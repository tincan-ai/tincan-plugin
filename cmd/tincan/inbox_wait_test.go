package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

func emptyWaitingInbox(t *testing.T) *inbox {
	t.Helper()
	i := &inbox{agent: "self", allowed: []string{"peer"}, background: true, path: filepath.Join(t.TempDir(), "inbox.json"), changed: make(chan struct{}, 1)}
	t.Cleanup(i.close)
	return i
}

func waitUntilArmed(t *testing.T, i *inbox) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for i.waiting() == nil {
		select {
		case <-deadline:
			t.Fatal("listener never armed")
		case <-time.After(time.Millisecond):
		}
	}
}

func saveWaitingEvent(t *testing.T, i *inbox, e inboxEvent) {
	t.Helper()
	i.mu.Lock()
	err := i.save(inboxState{After: i.state.After, Pending: &e})
	i.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestInboxWaitCancellationAndExclusivity(t *testing.T) {
	i := emptyWaitingInbox(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := i.waitMention(ctx, "child", time.Hour); done <- err }()
	waitUntilArmed(t, i)
	for _, worker := range []string{"child", "other-child"} {
		if _, err := i.waitMention(ctx, worker, time.Minute); err == nil {
			t.Fatal("accepted overlapping wait", worker)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || i.waiting() != nil {
			t.Fatal("cancellation retained readiness", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop waiter")
	}
	r, err := i.waitMention(context.Background(), "replacement", time.Millisecond)
	if err != nil || r["status"] != "expired" || i.waiting() != nil {
		t.Fatal(r, err)
	}
}

func TestInboxWaitClaimAndRestartSafety(t *testing.T) {
	i := emptyWaitingInbox(t)
	e := event(42, "peer", "self")
	saveWaitingEvent(t, i, e)
	r, err := i.waitMention(context.Background(), "child", time.Second)
	if err != nil || r["status"] != "event" || r["event_seq"] != int64(42) {
		t.Fatal(r, err)
	}
	wire, _ := json.Marshal(r)
	if strings.Contains(string(wire), e.Payload.Text) || r["event"] != nil || r["claim"] != nil {
		t.Fatal("wait exposed peer body or claim")
	}
	claim, err := i.claim(42, "native-delivery-worker")
	if err != nil || !claim.Acquired {
		t.Fatal(claim, err)
	}
	r, err = i.waitMention(context.Background(), "child", time.Millisecond)
	if err != nil || r["status"] != "expired" || r["claim"] != nil {
		t.Fatal("wait stole claimed work", r, err)
	}
	data, err := os.ReadFile(i.path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := emptyWaitingInbox(t)
	if err := json.Unmarshal(data, &restarted.state); err != nil {
		t.Fatal(err)
	}
	if restarted.waiting() != nil || restarted.state.request(42).Claim.Token != claim.Claim {
		t.Fatal("restart retained waiter or lost durable claim")
	}
	r, err = restarted.waitMention(context.Background(), "replacement", time.Millisecond)
	if err != nil || r["status"] != "expired" {
		t.Fatal(r, err)
	}
	if err := i.ack(42, claim.Claim); err != nil {
		t.Fatal(err)
	}
	r, err = i.waitMention(context.Background(), "child", time.Millisecond)
	if err != nil || r["status"] != "expired" {
		t.Fatal("acknowledged event redelivered", r, err)
	}
}

func TestInboxWaitClosedAndOwnerReview(t *testing.T) {
	i := emptyWaitingInbox(t)
	done := make(chan error, 1)
	go func() { _, err := i.waitMention(context.Background(), "child", time.Hour); done <- err }()
	waitUntilArmed(t, i)
	i.close()
	select {
	case err := <-done:
		if err == nil || i.waiting() != nil {
			t.Fatal("closed inbox still waiting")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not wake listener")
	}
	i = emptyWaitingInbox(t)
	i.creator = true
	saveWaitingEvent(t, i, inboxEvent{Seq: 5, Kind: "join_requested", JoinRequest: &joinNotice{RequestID: "join"}})
	r, err := i.waitMention(context.Background(), "child", time.Second)
	if err != nil || r["status"] != "owner_review" || i.state.Claim != nil || i.state.After != 5 {
		t.Fatal("join request was executed or acknowledged", r, err)
	}
}

func TestDelegatedWaitTwoDelayedSSEMentions(t *testing.T) {
	var requests atomic.Int32
	events := make(chan inboxEvent)
	replies := make(chan core.SendInput, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/me" {
			fmt.Fprint(w, `{"agent":{"encryption_mode":"standard"}}`)
			return
		}
		if r.URL.Path == "/api/v1/channels" {
			fmt.Fprint(w, `[{"id":"channel","encryption_mode":"standard"}]`)
			return
		}
		if r.URL.Path == "/api/v1/messages" && r.Method == "POST" {
			var reply core.SendInput
			if err := json.NewDecoder(r.Body).Decode(&reply); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			replies <- reply
			fmt.Fprint(w, `{"id":"reply"}`)
			return
		}
		if r.URL.Path != "/api/v1/events" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-events:
				data, _ := json.Marshal(e)
				fmt.Fprintf(w, "data: %s\n\n", data)
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer server.Close()
	i := emptyWaitingInbox(t)
	i.c = Config{Server: server.URL, Token: "test-token"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); i.stream(ctx, nil, nil) }()
	defer func() { cancel(); <-done }()
	for seq := int64(1); seq <= 2; seq++ {
		result := make(chan map[string]any, 1)
		go func() {
			r, err := i.waitMention(ctx, "child", time.Minute)
			if err != nil {
				t.Error(err)
			}
			result <- r
		}()
		waitUntilArmed(t, i)
		// Real delays occur while only the SSE process and tool are running.
		select {
		case r := <-result:
			t.Fatal("empty inbox returned to model", r)
		case <-time.After(30 * time.Millisecond):
		}
		select {
		case events <- event(seq, "peer", "self"):
		case <-time.After(time.Second):
			t.Fatal("SSE stopped reading")
		}
		select {
		case r := <-result:
			if r["event_seq"] != seq || r["status"] != "event" {
				t.Fatal(r)
			}
		case <-time.After(time.Second):
			t.Fatal("mention did not release tool wait")
		}
		claim, err := i.claim(seq, "child")
		if err != nil || !claim.Acquired {
			t.Fatal(claim, err)
		}
		if _, err = i.reply(seq, "handled", claim.Claim); err != nil {
			t.Fatal(err)
		}
		reply := <-replies
		if reply.ReplyTo == nil || *reply.ReplyTo != fmt.Sprintf("message-%d", seq) || reply.Text != "handled" || !strings.Contains(string(reply.Metadata), "tincan_listener") {
			t.Fatal("reply lost routing or loop protection", reply)
		}
	}
	if requests.Load() != 1 {
		t.Fatal("wait opened extra SSE connections", requests.Load())
	}
}

func TestPluginWaitMCPAndShutdown(t *testing.T) {
	i := emptyWaitingInbox(t)
	c := &pluginConnection{Handle: "conn_11111111111111111111111111111111", CodexThreadID: testCodexThread, Config: Config{Token: "saved"}}
	runCtx, cancel := context.WithCancel(context.Background())
	b := &pluginBroker{root: t.TempDir(), host: "codex", inboxes: map[string]*inbox{c.Handle: i}, workers: map[string]bool{c.Handle: true}, runCtx: runCtx, cancel: cancel}
	t.Cleanup(b.close)
	if err := b.save(c); err != nil {
		t.Fatal(err)
	}
	srv := b.serverWithTools(nil)
	a, z := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(context.Background(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "listener-test", Version: "1"}, nil).Connect(context.Background(), z, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	for _, in := range []inboxWaitInput{{Connection: c.Handle, WorkerID: testCodexThread}, {Connection: c.Handle, WorkerID: "child", WaitSeconds: -1}, {Connection: c.Handle, WorkerID: "child", WaitSeconds: 3601}, {Connection: "wrong", WorkerID: "child"}} {
		if _, err := b.waitInbox(context.Background(), in); err == nil {
			t.Fatal("invalid waiter accepted", in)
		}
	}
	// Native child interruption cancels the MCP call, not the whole broker.
	callCtx, stopCall := context.WithCancel(context.Background())
	defer stopCall()
	cancelled := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(callCtx, &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "cancelled-child"}})
		cancelled <- err
	}()
	waitUntilArmed(t, i)
	stopCall()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("MCP cancellation did not reach client", err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP cancellation blocked")
	}
	deadline := time.Now().Add(time.Second)
	for i.waiting() != nil {
		if time.Now().After(deadline) {
			t.Fatal("MCP cancellation left an orphaned waiter")
		}
		time.Sleep(time.Millisecond)
	}
	result := make(chan *mcp.CallToolResult, 1)
	go func() {
		r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "child", "wait_seconds": 120}})
		if err != nil {
			t.Error(err)
		}
		result <- r
	}()
	waitUntilArmed(t, i)
	// Another MCP request must remain usable while inbox_wait is outstanding.
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	d := b.fallbackDelivery(c, deliveryState{})
	view := map[string]any{"background_listener": true, "idle_wake": d.IdleWake, "delivery": d.Method}
	b.addListenerReadiness(view, c)
	connectionReadiness(view)
	if d.Method != "delegated_listener" || d.IdleWake || view["readiness"] != "experimental" {
		t.Fatal("wait overstated host capability", view)
	}
	b.close()
	select {
	case r := <-result:
		if r == nil || !r.IsError || b.delegatedListener(c.Handle) != nil {
			t.Fatal("shutdown retained waiter", r)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel tool")
	}
	if d := b.fallbackDelivery(c, deliveryState{}); d.Method != "durable_inbox" || d.IdleWake {
		t.Fatal("stale readiness after shutdown", d)
	}
}

func TestDelegatedListenerKeepsVerifiedRoutesAndBindings(t *testing.T) {
	for _, host := range []string{"codex", "claude", "cursor", "copilot"} {
		t.Run(host, func(t *testing.T) {
			i := emptyWaitingInbox(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &pluginConnection{Handle: "conn_22222222222222222222222222222222", Config: Config{Token: "saved"}}
			if host == "codex" {
				c.CodexThreadID = testCodexThread
			} else {
				c.HookHost, c.HookSessionID = host, "parent-session"
			}
			b := &pluginBroker{root: t.TempDir(), host: host, inboxes: map[string]*inbox{c.Handle: i}, workers: map[string]bool{c.Handle: true}, runCtx: ctx, cancel: cancel}
			defer b.close()
			if err := b.save(c); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := b.waitInbox(ctx, inboxWaitInput{Connection: c.Handle, WorkerID: "child"})
				done <- err
			}()
			waitUntilArmed(t, i)
			view := map[string]any{"delivery": "verified-route", "idle_wake": true}
			b.addListenerReadiness(view, c)
			if view["delivery"] != "verified-route" || view["idle_wake"] != true {
				t.Fatal("experimental route replaced verified delivery", view)
			}
			cancel()
			<-done
			for _, change := range []func(){func() { c.Worker = true }, func() { c.Worker = false; c.CodexThreadID = ""; c.HookSessionID = "" }} {
				change()
				if err := b.save(c); err != nil {
					t.Fatal(err)
				}
				if _, err := b.waitInbox(context.Background(), inboxWaitInput{Connection: c.Handle, WorkerID: "child"}); err == nil {
					t.Fatal("owned or unbound runtime accepted")
				}
			}
		})
	}
}

func TestDelegatedListenerAutomaticSetup(t *testing.T) {
	for _, host := range []string{"codex", "claude", "cursor", "copilot"} {
		t.Run(host, func(t *testing.T) {
			b := &pluginBroker{host: host}
			c := &pluginConnection{Handle: "connection", HookHost: host, HookSessionID: "parent"}
			for _, tc := range []struct {
				name        string
				wake, owned bool
				want        string
			}{
				{"fallback", false, false, "starting"},
				{"native", true, false, "ready"},
				{"owned", false, true, "listening"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					c.Worker = tc.owned
					view := map[string]any{"background_listener": true, "idle_wake": tc.wake}
					b.addListenerReadiness(view, c)
					connectionReadiness(view)
					if view["readiness"] != tc.want {
						t.Fatalf("got %v, want %s", view, tc.want)
					}
					if tc.want == "starting" && view["next"] == nil {
						t.Fatal("fallback omitted startup action")
					}
				})
			}
		})
	}
}
