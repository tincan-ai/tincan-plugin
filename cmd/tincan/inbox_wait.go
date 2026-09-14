package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const defaultInboxWait = 15 * time.Minute
const maxInboxWait = time.Hour

// A waiter is a live tool call, not a durable worker claim or proof that the
// host preserves subagents after the parent finishes. Never persist readiness.
type inboxWaiter struct {
	WorkerID string    `json:"worker_id"`
	Until    time.Time `json:"until"`
}

// save and close call this with mu held. Broadcast separately from changed:
// consuming the SSE acknowledgement channel here could strand the stream.
func (i *inbox) signalUpdate() {
	if i.updates != nil {
		close(i.updates)
	}
	i.updates = make(chan struct{})
}

func (i *inbox) waiting() *inboxWaiter {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed || i.waiter == nil || !time.Now().Before(i.waiter.Until) {
		return nil
	}
	w := *i.waiter
	return &w
}

// waitMention only observes the durable inbox. The child must claim separately
// before execution, so concurrent native delivery cannot duplicate the work.
// No network polling, progress pings, or periodic returns to the model.
func (i *inbox) waitMention(ctx context.Context, worker string, duration time.Duration) (map[string]any, error) {
	if strings.TrimSpace(worker) == "" || len(worker) > 200 || strings.ContainsAny(worker, "\x00\r\n") {
		return nil, errors.New("worker_id must identify this delegated listener")
	}
	if duration <= 0 || duration > maxInboxWait {
		return nil, errors.New("wait duration must be positive and at most one hour")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	until := time.Now().Add(duration)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
		until = deadline
	}
	w := &inboxWaiter{WorkerID: worker, Until: until}
	i.mu.Lock()
	if i.closed || !i.background {
		i.mu.Unlock()
		return nil, errors.New("background inbox is not running")
	}
	if i.waiter != nil {
		i.mu.Unlock()
		return nil, errors.New("a delegated listener is already waiting; do not start another")
	}
	i.waiter = w
	i.mu.Unlock()
	defer func() {
		i.mu.Lock()
		i.waiter = nil
		i.mu.Unlock()
	}()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		i.mu.Lock()
		if i.closed {
			i.mu.Unlock()
			return nil, errors.New("inbox closed; stop the delegated listener")
		}
		_ = i.state.migrate()
		var candidate *inboxRequest
		for n := range i.state.Requests {
			r := &i.state.Requests[n]
			if !i.state.eligible(r) || !i.accepts(r.Event) {
				continue
			}
			if r.Event.Kind == "join_requested" {
				key := worker + ":join:" + fmt.Sprint(r.Event.Seq)
				if i.questionNotices == nil {
					i.questionNotices = map[string]bool{}
				}
				if i.questionNotices[key] {
					continue
				}
				i.questionNotices[key] = true
			}
			candidate = r
			break
		}
		if r := candidate; r != nil {
			result := map[string]any{"event_seq": r.Event.Seq, "kind": r.Event.Kind, "experimental": true, "status": "event", "instructions": "Claim this message before acting. Use inbox_outcome to save completion or safely suspend a commitment, then continue listening. Other commitments do not block this message."}
			if r.Event.Kind == "join_requested" {
				result["status"] = "owner_review"
				result["instructions"] = joinReviewInstructions
			}
			i.mu.Unlock()
			return result, nil
		}
		for _, r := range i.state.Requests {
			if r.Approval == nil || r.Approval.Delivery != "pending" {
				continue
			}
			key := worker + ":" + r.Approval.ID
			if i.questionNotices == nil {
				i.questionNotices = map[string]bool{}
			}
			if i.questionNotices[key] {
				continue
			}
			i.questionNotices[key] = true
			i.mu.Unlock()
			return map[string]any{"status": "decision_needed", "event_seq": r.Event.Seq, "instructions": "Read inbox_requests privately and hand its concrete question to the parent for user presentation. Record presented only after actual delivery. Then continue listening; this commitment alone is suspended."}, nil
		}
		if i.updates == nil {
			i.updates = make(chan struct{})
		}
		updates := i.updates
		i.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return map[string]any{"status": "expired", "experimental": true, "instructions": "Stop the delegated listener. Do not loop on an empty timeout or start a replacement. Re-arm only on later user activity or explicit host scheduling."}, nil
		case <-updates:
		}
	}
}
