package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

var errEventStreamStalled = errors.New("event stream stalled; using bounded JSON delivery")

const eventStreamIdleTimeout = 35 * time.Second // Server heartbeat is every 20s.

type eventActivityReader struct {
	io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (r eventActivityReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.idle)
	}
	return n, err
}

// stream holds a single SSE request open, with no model involvement or periodic
// tool calls. Receipt is persisted independently of work completion. Slow or suspended
// commitments never hold the SSE reader at the front of the inbox.
func (i *inbox) stream(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error) {
	i.streamWithIdleTimeout(ctx, control, notify, eventStreamIdleTimeout)
}

func (i *inbox) streamWithIdleTimeout(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error, idle time.Duration) {
	delay := time.Second
	batch := false
	for ctx.Err() == nil {
		var err error
		if batch {
			err = i.consumeBatch(ctx, control, notify)
		} else {
			err = i.consumeSSE(ctx, control, notify, idle)
		}
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errEventStreamStalled) {
			batch = true
			continue
		}
		if err == nil {
			delay = time.Second
			continue
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
	return i.consumeSSE(ctx, control, notify, eventStreamIdleTimeout)
}

func (i *inbox) consumeSSE(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error, idle time.Duration) error {
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
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := time.AfterFunc(idle, cancel)
	defer timer.Stop()
	req, err := http.NewRequestWithContext(streamCtx, "GET", fmt.Sprintf("%s/api/v1/events?after=%d", i.c.Server, after), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.Token)
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil && streamCtx.Err() != nil {
			return errEventStreamStalled
		}
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("stream HTTP %d", res.StatusCode)
	}
	i.mu.Lock()
	i.streaming = true
	i.streamTransport = "sse"
	i.streamError = ""
	i.mu.Unlock()
	defer func() { i.mu.Lock(); i.streaming = false; i.mu.Unlock() }()
	scan := bufio.NewScanner(eventActivityReader{Reader: res.Body, timer: timer, idle: idle})
	scan.Buffer(make([]byte, 4096), 2*1024*1024)
	for scan.Scan() {
		if !strings.HasPrefix(scan.Text(), "data: ") {
			continue
		}
		var event inboxEvent
		if err = json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &event); err != nil {
			return err
		}
		if err = i.acceptStreamEvent(ctx, event, control, notify); err != nil {
			return err
		}
	}
	if ctx.Err() == nil && streamCtx.Err() != nil {
		return errEventStreamStalled
	}
	if err = scan.Err(); err != nil {
		return err
	}
	return io.EOF
}

func (i *inbox) acceptStreamEvent(ctx context.Context, event inboxEvent, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error) error {
	i.mu.Lock()
	seen := event.Seq <= i.state.After
	i.mu.Unlock()
	if seen {
		return nil
	}
	valid, verifyErr := decryptInboxEvent(ctx, i.c, &event)
	if verifyErr != nil {
		return verifyErr
	}
	if !valid {
		_, err := i.ingest(event, false)
		return err
	}
	// Pairing is protocol work handled by the process, never by a model turn.
	if control != nil {
		if err := control(ctx, event); err != nil {
			return err
		}
	}
	event.Mentioned = event.Kind == "message" && slices.Contains(event.Payload.Mentions, i.agent)
	accepted, err := i.ingest(event, true)
	if err != nil {
		return err
	}
	if accepted && notify != nil {
		if err := notify(ctx, &event); err != nil {
			return err
		}
	}
	return nil
}

// Only the transport reissues bounded reads. Empty results never wake a model,
// and a received event follows the exact same durable claim/dispatch path as SSE.
func (i *inbox) consumeBatch(ctx context.Context, control func(context.Context, inboxEvent) error, notify func(context.Context, *inboxEvent) error) error {
	if err := validateServer(i.c.Server); err != nil {
		return err
	}
	i.mu.Lock()
	after := i.state.After
	i.streamTransport = "json_wait"
	i.mu.Unlock()
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, "GET", fmt.Sprintf("%s/api/v1/events?after=%d", i.c.Server, after), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.Token)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	i.mu.Lock()
	i.streaming = true
	i.mu.Unlock()
	defer func() { i.mu.Lock(); i.streaming = false; i.mu.Unlock() }()
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("event batch HTTP %d", res.StatusCode)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
		return errors.New("server does not support bounded JSON event delivery")
	}
	var events []inboxEvent
	if err = json.NewDecoder(io.LimitReader(res.Body, 16*1024*1024)).Decode(&events); err != nil {
		return err
	}
	i.mu.Lock()
	i.streamError = ""
	i.mu.Unlock()
	for _, event := range events {
		if err = i.acceptStreamEvent(ctx, event, control, notify); err != nil {
			return err
		}
	}
	return nil
}
