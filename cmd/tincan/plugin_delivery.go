package main

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

// Delivery failure is independent of SSE health. A fallback never acknowledges
// pending work, and a failed push is not retried on an idle model loop.
type deliveryState struct {
	Method         string     `json:"method"`
	IdleWake       bool       `json:"idle_wake"`
	LastError      string     `json:"last_error,omitempty"`
	Experimental   string     `json:"experimental_mcp"`
	QueueError     string     `json:"queue_error,omitempty"`
	Endpoint       string     `json:"endpoint,omitempty"`
	EndpointSource string     `json:"endpoint_source,omitempty"`
	HookLastSeen   *time.Time `json:"hook_last_seen,omitempty"`
	RetryAfter     time.Time  `json:"-"`
}

var deliveryPriority = []string{"experimental_mcp", "codex_app_server", "codex_queue", "delegated_listener", "hooks", "durable_inbox"}

func (b *pluginBroker) deliverySnapshot(handle string) deliveryState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.deliveries[handle]; ok {
		return s
	}
	return deliveryState{Method: "durable_inbox", Experimental: "not_probed"}
}
func (b *pluginBroker) setDelivery(handle string, s deliveryState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.deliveries == nil {
		b.deliveries = map[string]deliveryState{}
	}
	b.deliveries[handle] = s
}

func (b *pluginBroker) probeDelivery(ctx context.Context, c *pluginConnection) {
	s := b.fallbackDelivery(c, deliveryState{Experimental: "not_probed_transport_unavailable"})
	if target, err := b.targetFor(c); err == nil {
		s.Endpoint = target.Endpoint
		s.EndpointSource = target.Source
	}
	// Codex 0.153.4's event/stream/start explicitly accepts only hosted apps.
	// Probe only through the official endpoint; never impersonate codex_apps.
	if err := b.codexDelivery(ctx, c, nil); err != nil {
		s.LastError = err.Error()
		s.RetryAfter = time.Now().Add(time.Minute)
	} else {
		s.Method = "codex_app_server"
		s.IdleWake = true
		if b.experimentalProbe != nil {
			s.Experimental = b.experimentalProbe(ctx, c.CodexThreadID)
		} else {
			s.Experimental = func() string {
				t, err := b.targetFor(c)
				if err != nil {
					return "unavailable"
				}
				return probeCodexEventsTarget(ctx, t, c.CodexThreadID)
			}()
		}
	}
	b.setDelivery(c.Handle, s)
}

func (b *pluginBroker) deliverCodex(ctx context.Context, c *pluginConnection, payload map[string]any) error {
	// Resolve bindings again: a live worker may have started before reconnection.
	latest, err := b.load(c.Handle)
	if err != nil {
		return err
	}
	c = latest
	s := b.deliverySnapshot(c.Handle)
	if c.CodexThreadID == "" {
		s.Method = "durable_inbox"
		b.setDelivery(c.Handle, s)
		return nil
	}
	mention := payload["kind"] == "semantic_attention" || payload["kind"] == "collaboration_attention" || payload["kind"] == "connection_notice" || payload["kind"] == "message" || payload["kind"] == "mention" || payload["kind"] == "join_request" || payload["kind"] == "approval_needed" || payload["kind"] == "needs_attention"
	seq, _ := payload["event_seq"].(int64)
	key, _ := payload["notice_key"].(string)
	if mention && seq > 0 {
		var receipt queueReceipt
		if data, e := os.ReadFile(queuePath(b.root, c.Handle)); e == nil && json.Unmarshal(data, &receipt) == nil && receipt.Seq == seq && receipt.Key == key {
			if receipt.State == "accepted" {
				s.Method = "codex_queue"
				s.IdleWake = true
				s.QueueError = ""
			} else {
				s = b.fallbackDelivery(c, s)
				s.QueueError = "previous queue attempt unconfirmed; pending mention retained"
			}
			b.setDelivery(c.Handle, s)
			return nil
		}
	}
	wasQueue := s.Method == "codex_queue"
	tryPush := s.Method == "codex_app_server" || (mention && time.Now().After(s.RetryAfter))
	if tryPush {
		if err = b.codexDelivery(ctx, c, payload); err == nil {
			s.Method = "codex_app_server"
			s.IdleWake = true
			s.LastError = ""
			s.QueueError = ""
			b.setDelivery(c.Handle, s)
			return nil
		}
		s = b.fallbackDelivery(c, s)
		s.LastError = err.Error()
		s.RetryAfter = time.Now().Add(time.Minute)
	}
	if mention && seq > 0 && (tryPush || wasQueue) {
		if err = b.queueCodex(ctx, c, seq, key); err == nil {
			s.Method = "codex_queue"
			s.IdleWake = true
			s.QueueError = ""
		} else {
			s = b.fallbackDelivery(c, s)
			s.QueueError = err.Error()
		}
	}
	b.setDelivery(c.Handle, s)
	// The durable pending record is the fallback. Hooks read it without changing
	// its acknowledgement state. No transport failure blocks protocol pairing.
	return nil
}

func (b *pluginBroker) fallbackDelivery(c *pluginConnection, s deliveryState) deliveryState {
	s.Method = "durable_inbox"
	s.IdleWake = false
	if b.delegatedListener(c.Handle) != nil {
		s.Method = "delegated_listener"
		return s
	}
	var h hookState
	if data, err := os.ReadFile(hookStatePath(b.root, c.CodexThreadID)); err == nil && json.Unmarshal(data, &h) == nil && !h.LastSeen.IsZero() {
		s.HookLastSeen = &h.LastSeen
		// Historical execution is evidence, not a promise that trust remains enabled.
		if time.Since(h.LastSeen) < 24*time.Hour {
			s.Method = "hooks"
		}
	}
	return s
}
