package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// stream holds a single SSE request open, with no model involvement or periodic
// tool calls. Receipt is persisted independently of work completion. Slow or suspended
// commitments never hold the SSE reader at the front of the inbox.
func (i *inbox) stream(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error) {
	delay := time.Second
	for ctx.Err() == nil {
		err := i.consumeStream(ctx, control, notify)
		if ctx.Err() != nil {
			return
		}
		// Reconnect only on EOF/failure, never on an idle timer.
		i.mu.Lock()
		if err != nil {
			i.streamError = err.Error()
		}
		i.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func (i *inbox) pendingEvent() (*inboxEvent, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if r := i.state.runnable(); r != nil {
		e := r.Event
		return &e, nil
	}
	return nil, nil
}
func (i *inbox) waitAcknowledged(ctx context.Context, seq int64) error {
	for {
		i.mu.Lock()
		r := i.state.request(seq)
		pending := r != nil && !terminal(r.Status)
		updates := i.updates
		i.mu.Unlock()
		if !pending {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-updates:
		}
	}
}
func (i *inbox) consumeStream(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error) error {
	// Replay eligible saved notices after restart or a failed notification.
	i.mu.Lock()
	saved := i.state.copy()
	i.mu.Unlock()
	for _, r := range saved.Requests {
		if saved.eligible(&r) && notify != nil {
			e := r.Event
			if err := notify(ctx, &e); err != nil {
				return err
			}
		}
	}
	var err error
	i.mu.Lock()
	after := i.state.After
	i.mu.Unlock()
	if err = validateServer(i.c.Server); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/api/v1/events?after=%d", i.c.Server, after), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.Token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("stream HTTP %d", res.StatusCode)
	}
	i.mu.Lock()
	i.streaming = true
	i.streamError = ""
	i.mu.Unlock()
	defer func() { i.mu.Lock(); i.streaming = false; i.mu.Unlock() }()
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 2*1024*1024)
	for scan.Scan() {
		if !strings.HasPrefix(scan.Text(), "data: ") {
			continue
		}
		var event inboxEvent
		if err = json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &event); err != nil {
			return err
		}
		i.mu.Lock()
		seen := event.Seq <= i.state.After
		i.mu.Unlock()
		if seen {
			continue
		}
		valid, verifyErr := decryptInboxEvent(ctx, i.c, &event)
		if verifyErr != nil {
			return verifyErr
		}
		if !valid {
			if _, err = i.ingest(event, false); err != nil {
				return err
			}
			continue
		}
		// Pairing is protocol work handled by the process, never by a model turn.
		if control != nil {
			if err = control(ctx, event); err != nil {
				return err
			}
		}
		accepted, err := i.ingest(event, true)
		if err != nil {
			return err
		}
		if accepted && notify != nil {
			if err = notify(ctx, &event); err != nil {
				return err
			}
		}

	}
	if err = scan.Err(); err != nil {
		return err
	}
	return io.EOF
}
