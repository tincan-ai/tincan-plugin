package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestInboxAllMessagesMentionPriority(t *testing.T) {
	i := emptyWaitingInbox(t)
	for _, e := range []inboxEvent{event(1, "peer"), event(2, "peer", "someone"), event(3, "peer", "self"), event(4, "peer", "self")} {
		if ok, err := i.ingest(e, true); err != nil || !ok {
			t.Fatalf("ingest %d: %v %v", e.Seq, ok, err)
		}
	}
	// Priority survives persistence and still preserves FIFO within each class.
	data, err := os.ReadFile(i.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved inboxState
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if err := saved.migrate(); err != nil {
		t.Fatal(err)
	}
	for n, seq := range []int64{3, 4, 1, 2} {
		if saved.Requests[n].Event.Seq != seq {
			t.Fatal(saved.Requests)
		}
	}
	for _, seq := range []int64{3, 4, 1, 2} {
		e, err := i.next(context.Background())
		if err != nil || e == nil || e.Seq != seq {
			t.Fatalf("next want %d: %v %v", seq, e, err)
		}
		got, err := i.waitMention(context.Background(), "worker", time.Second)
		if err != nil || got["event_seq"] != seq {
			t.Fatalf("wait want %d: %v %v", seq, got, err)
		}
		if err := i.ack(seq); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInboxMentionPriorityIsLocallyDerived(t *testing.T) {
	i := emptyWaitingInbox(t)
	e := event(1, "peer")
	e.Mentioned = true
	if ok, err := i.ingest(e, true); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if i.state.Requests[0].Event.Mentioned {
		t.Fatal("peer supplied priority trusted")
	}
	n := inboundNotification(map[string]any{"kind": "message", "event_seq": int64(1), "payload": "untrusted body"})
	if n["execution"] == nil || n["payload"] != nil {
		t.Fatal(n)
	}
}

func TestInboxDispatchPrioritizesMentionsWithoutDroppingMessages(t *testing.T) {
	i := emptyWaitingInbox(t)
	for _, e := range []inboxEvent{event(1, "peer"), event(2, "peer", "self")} {
		if ok, err := i.ingest(e, true); !ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	notices := make(chan map[string]any, 2)
	done := make(chan struct{})
	go func() {
		defer close(done)
		i.dispatchChanges(ctx, func(_ context.Context, n map[string]any) error { notices <- n; return nil })
	}()
	defer func() { cancel(); <-done }()
	for n, seq := range []int64{2, 1} {
		select {
		case got := <-notices:
			kinds := []string{"mention", "message"}
			if got["event_seq"] != seq || got["kind"] != kinds[n] {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("missing notice")
		}
	}
}

func TestHookContextPrioritizesMentions(t *testing.T) {
	root, c, path := hookFixture(t)
	ordinary := event(1, "peer")
	mention := event(2, "peer", c.AgentID)
	mention.Mentioned = true
	s := inboxState{Version: 2, After: 2, Requests: []inboxRequest{{Event: ordinary, Status: "ready"}, {Event: mention, Status: "ready"}}}
	if err := writePrivateJSON(path, s); err != nil {
		t.Fatal(err)
	}
	got, err := runHook(root, hookInput{SessionID: c.CodexThreadID, Event: "SessionStart"})
	if err != nil {
		t.Fatal(err)
	}
	output := got["hookSpecificOutput"].(map[string]any)["additionalContext"].(string)
	first, second := strings.Index(output, `"event_seq":2`), strings.Index(output, `"event_seq":1`)
	if first < 0 || second < first || strings.Contains(output, "Please inspect") {
		t.Fatal(output)
	}
}
