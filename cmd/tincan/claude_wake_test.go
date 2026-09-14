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

func claudeWakeFixture(t *testing.T) (string, *pluginConnection, string) {
	t.Helper()
	root, c, path := hookFixture(t)
	c.CodexThreadID = ""
	if err := (&pluginBroker{root: root}).save(c); err != nil {
		t.Fatal(err)
	}
	if err := (&pluginBroker{root: root}).bindHook(c, "claude", "session-one"); err != nil {
		t.Fatal(err)
	}
	return root, c, path
}

func TestClaudeWakeRoutesOnceAndRearms(t *testing.T) {
	root, c, path := claudeWakeFixture(t)
	in := hookInput{SessionID: c.HookSessionID}
	before, _ := os.ReadFile(path)
	for _, seq := range []int64{7, 8} {
		if seq == 8 {
			var state inboxState
			json.Unmarshal(before, &state)
			state.After, state.Pending.Seq = 7, 8
			if err := writePrivateJSON(path, state); err != nil {
				t.Fatal(err)
			}
			before, _ = os.ReadFile(path)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		var out bytes.Buffer
		code := waitClaudeWake(ctx, root, in, time.Millisecond, &out)
		cancel()
		if code != 2 || !strings.Contains(out.String(), c.Handle) || strings.Contains(out.String(), "UNTRUSTED") || strings.Contains(out.String(), "secret") {
			t.Fatal(code, out.String())
		}
		after, _ := os.ReadFile(path)
		if !bytes.Equal(before, after) || claudeWakeArmed(root, c) {
			t.Fatal("wake changed inbox or retained readiness")
		}
		// Re-arming must not repeatedly wake for an unclaimed, already shown event.
		ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
		out.Reset()
		code = waitClaudeWake(ctx, root, in, time.Millisecond, &out)
		cancel()
		if code != 0 || out.Len() != 0 {
			t.Fatal("duplicate wake", code, out.String())
		}
	}
}

func TestClaudeWakeOwnershipReadinessAndCancellation(t *testing.T) {
	root, c, path := claudeWakeFixture(t)
	os.Remove(path) // Idle before the first message arrives.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- waitClaudeWake(ctx, root, hookInput{SessionID: c.HookSessionID}, time.Millisecond, &bytes.Buffer{})
	}()
	for !claudeWakeArmed(root, c) {
		if ctx.Err() != nil {
			t.Fatal("waiter did not arm")
		}
		time.Sleep(time.Millisecond)
	}
	b := &pluginBroker{root: root, host: "claude", native: true}
	view := map[string]any{"background_listener": true, "delivery_diagnostics": deliveryState{Method: "durable_inbox"}}
	b.addHarnessReadiness(view, c)
	connectionReadiness(view)
	if d := view["delivery_diagnostics"].(deliveryState); !d.IdleWake || d.Method != view["delivery"] {
		t.Fatal("inconsistent diagnostics", view)
	}
	if view["delivery"] != "claude_async_rewake" || view["readiness"] != "ready" || !b.runtimeAvailable(ctx, c) {
		t.Fatal(view)
	}
	var duplicate bytes.Buffer
	if code := waitClaudeWake(ctx, root, hookInput{SessionID: c.HookSessionID}, time.Millisecond, &duplicate); code != 0 || duplicate.Len() != 0 {
		t.Fatal("duplicate waiter", code)
	}
	other := *c
	other.HookSessionID = "other"
	if claudeWakeArmed(root, &other) {
		t.Fatal("readiness leaked across sessions")
	}
	cancel()
	if code := <-done; code != 0 || claudeWakeArmed(root, c) || b.runtimeAvailable(t.Context(), c) {
		t.Fatal("cancelled waiter advertised availability", code)
	}
	// A crash can leave a receipt, but it cannot retain the OS lock.
	writePrivateJSON(claudeWakePath(root, c.HookSessionID), claudeWakeLease{ExpiresAt: time.Now().Add(time.Hour)})
	if claudeWakeArmed(root, c) {
		t.Fatal("stale receipt advertised availability")
	}
}

func TestClaudeWakeSkipsForeignClaimedAndInvalidWork(t *testing.T) {
	for _, kind := range []string{"foreign", "claimed", "subagent", "invalid", "acknowledged"} {
		t.Run(kind, func(t *testing.T) {
			root, c, path := claudeWakeFixture(t)
			in := hookInput{SessionID: c.HookSessionID}
			var state inboxState
			data, _ := os.ReadFile(path)
			json.Unmarshal(data, &state)
			switch kind {
			case "foreign":
				in.SessionID = "different-session"
			case "claimed":
				state.Claim = &inboxClaim{WorkerID: "worker", Token: "private-claim"}
			case "subagent":
				in.AgentID = "child"
			case "invalid":
				in.SessionID = "../escape"
			case "acknowledged":
				state.After = state.Pending.Seq
			}
			writePrivateJSON(path, state)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			var out bytes.Buffer
			if code := waitClaudeWake(ctx, root, in, time.Millisecond, &out); code != 0 || out.Len() != 0 {
				t.Fatal(code, out.String())
			}
		})
	}
}

func TestClaudeWakeLateConnectionAndExpiredLease(t *testing.T) {
	root, c, path := claudeWakeFixture(t)
	data, _ := os.ReadFile(path)
	os.Remove(path)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan int, 1)
	var out bytes.Buffer
	go func() {
		done <- waitClaudeWake(ctx, root, hookInput{SessionID: c.HookSessionID}, time.Millisecond, &out)
	}()
	for !claudeWakeArmed(root, c) {
		if ctx.Err() != nil {
			t.Fatal("waiter did not arm")
		}
		time.Sleep(time.Millisecond)
	}
	writePrivateJSON(claudeWakePath(root, c.HookSessionID), claudeWakeLease{ExpiresAt: time.Now().Add(-time.Second)})
	if claudeWakeArmed(root, c) {
		t.Fatal("expired lease advertised readiness")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 2 || !strings.Contains(out.String(), c.Handle) {
		t.Fatal(code, out.String())
	}
}
