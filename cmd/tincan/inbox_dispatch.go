package main

import (
	"context"
	"fmt"
	"time"
)

// Notice delivery is a separate consumer of durable state, never the SSE reader.
// A notification is an attention hint, not acknowledgment of work or proof that
// a human saw a question. No idle model polls or retry loops are required.
func (i *inbox) dispatchChanges(ctx context.Context, notify func(context.Context, map[string]any) error) {
	seen := map[int64]string{}
	for {
		i.mu.Lock()
		if i.updates == nil {
			i.updates = make(chan struct{})
		}
		updates := i.updates
		s := i.state.copy()
		i.mu.Unlock()
		live := map[int64]string{}
		for _, r := range s.Requests {
			kind, key := "", ""
			if s.eligible(&r) {
				kind = "message"
				if r.Event.Mentioned {
					kind = "mention"
				}
				if r.Event.Kind == "join_requested" {
					kind = "join_request"
				}
				if lifecycleKind(r.Event) != "" {
					kind = "connection_notice"
				}
				key = fmt.Sprintf("%d:ready:%d:%d", r.Event.Seq, r.Attempt, s.Revision)
			} else if r.Approval != nil && r.Approval.Delivery == "pending" && (r.Status == "awaiting_approval" || r.Status == "awaiting_information") {
				kind, key = "approval_needed", r.Approval.ID
			} else if r.Status == "needs_recovery" || r.Status == "failed" || (r.Status == "reply_pending" && r.DeliveryError != "") {
				kind, key = "needs_attention", fmt.Sprintf("%d:%s:%d", r.Event.Seq, r.Status, r.Attempt)
			}
			if kind != "" {
				live[r.Event.Seq] = key
			}
			if kind == "" || seen[r.Event.Seq] == key {
				continue
			}
			seen[r.Event.Seq] = key
			// Only identifiers enter host wake instructions. Private details are read
			// through inbox_requests, not injected as trusted instructions.
			if notify != nil {
				_ = notify(ctx, map[string]any{"kind": kind, "event_seq": r.Event.Seq, "notice_key": key, "mentioned": r.Event.Mentioned})
			}
		}
		seen = live
		select {
		case <-ctx.Done():
			return
		case <-updates:
		}
	}
}

// Retry only the saved, idempotent outbound message, never its model execution.
func (i *inbox) runOutbox(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		i.mu.Lock()
		if i.updates == nil {
			i.updates = make(chan struct{})
		}
		updates := i.updates
		s := i.state.copy()
		i.mu.Unlock()
		failed := false
		for _, r := range s.Requests {
			if r.Reply != nil {
				if _, err := i.deliverReplyContext(ctx, r.Event.Seq); err != nil {
					failed = true
				}
			}
		}
		if failed {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if delay < 30*time.Second {
				delay *= 2
			}
			continue
		}
		delay = time.Second
		select {
		case <-ctx.Done():
			return
		case <-updates:
		}
	}
}
