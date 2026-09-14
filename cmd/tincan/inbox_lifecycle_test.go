package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConnectionNoticeWaitPersistAndAcknowledge(t *testing.T) {
	for _, sender := range []string{"self", "peer"} {
		t.Run(sender, func(t *testing.T) {
			i := emptyWaitingInbox(t)
			e := event(42, sender, "")
			e.Payload.Metadata = json.RawMessage(`{"tincan_connection":"hello","tincan_listener":true}`)
			done := make(chan map[string]any, 1)
			go func() { v, _ := i.waitMention(t.Context(), "child", time.Second); done <- v }()
			waitUntilArmed(t, i)
			if ok, err := i.ingest(e, true); err != nil || !ok {
				t.Fatal(ok, err)
			}
			v := <-done
			if v["status"] != "connection_notice" || v["instructions"] != lifecycleInstructions {
				t.Fatal(v)
			}
			if !i.isConnectionNotice(42) || i.execution()["claim_required"] != false {
				t.Fatal("notice requires work delegation")
			}
			if _, err := i.reply(42, "acknowledged"); err == nil {
				t.Fatal("allowed a reply loop")
			}
			data, err := os.ReadFile(i.path)
			if err != nil {
				t.Fatal(err)
			}
			var saved inboxState
			if err = json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.runnable() == nil {
				t.Fatal("notice lost on reload")
			}
			if err := i.ack(42); err != nil {
				t.Fatal(err)
			}
			if ok, err := i.ingest(e, true); err != nil || ok {
				t.Fatal("duplicate replay", ok, err)
			}
			v, err = i.waitMention(context.Background(), "child", time.Millisecond)
			if err != nil || v["status"] != "expired" {
				t.Fatal("reannounced", v, err)
			}
		})
	}
}

func TestConnectionNoticeHookAndDispatch(t *testing.T) {
	root, c, path := hookFixture(t)
	e := event(7, "peer", "")
	e.Payload.Text = "PRIVATE ANNOUNCEMENT"
	e.Payload.Metadata = json.RawMessage(`{"tincan_connection":"hello","tincan_listener":true}`)
	i := emptyWaitingInbox(t)
	i.path = path
	if _, err := i.ingest(e, true); err != nil {
		t.Fatal(err)
	}
	got, err := runHook(root, hookInput{SessionID: c.CodexThreadID, Event: "PostToolUse"})
	data, _ := json.Marshal(got)
	if err != nil || !strings.Contains(string(data), "connection_notice") || strings.Contains(string(data), e.Payload.Text) {
		t.Fatal(got, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan map[string]any, 1)
	go i.dispatchChanges(ctx, func(_ context.Context, p map[string]any) error { done <- inboundNotification(p); return nil })
	select {
	case p := <-done:
		if p["kind"] != "connection_notice" || p["instructions"] != lifecycleInstructions {
			t.Fatal(p)
		}
	case <-time.After(time.Second):
		t.Fatal("notice did not dispatch")
	}
}

func TestConnectionNoticeWakesIdleClaude(t *testing.T) {
	for _, sender := range []string{"agent_self", "peer"} {
		t.Run(sender, func(t *testing.T) {
			root, c, path := claudeWakeFixture(t)
			if err := writePrivateJSON(path, inboxState{Version: 2}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			var out bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- waitClaudeWake(ctx, root, hookInput{SessionID: c.HookSessionID}, time.Millisecond, &out)
			}()
			for !claudeWakeArmed(root, c) {
				if ctx.Err() != nil {
					t.Fatal("waiter never armed")
				}
				time.Sleep(time.Millisecond)
			}
			i := emptyWaitingInbox(t)
			i.path = path
			i.agent = c.AgentID
			e := event(8, sender, "")
			e.Payload.Metadata = json.RawMessage(`{"tincan_connection":"hello","tincan_listener":true}`)
			if ok, err := i.ingest(e, true); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if code := <-done; code != 2 || !strings.Contains(out.String(), "connection_notice") {
				t.Fatal("idle Claude not woken", code, out.String())
			}
			if i.state.runnable() == nil {
				t.Fatal("wake incorrectly acknowledged notice")
			}
		})
	}
}
