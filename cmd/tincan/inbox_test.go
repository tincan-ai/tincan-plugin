package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

func event(seq int64, sender string, mentions ...string) inboxEvent {
	return inboxEvent{Seq: seq, Kind: "message", ChannelID: "channel", Payload: core.Message{ID: fmt.Sprintf("message-%d", seq), ChannelID: "channel", AgentID: sender, Mentions: mentions, Text: "Please inspect the build"}}
}
func fixture(t *testing.T, events []inboxEvent) (Config, *[]core.SendInput) {
	t.Helper()
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "agent.json"))
	replies := []core.SendInput{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/api/v1/me":
			fmt.Fprint(w, `{"agent":{"id":"self"}}`)
		case "/api/v1/events":
			w.Header().Set("Content-Type", "text/event-stream")
			var after int64
			fmt.Sscan(r.URL.Query().Get("after"), &after)
			for _, e := range events {
				if e.Seq > after {
					b, _ := json.Marshal(e)
					fmt.Fprintf(w, "data: %s\n\n", b)
				}
			}
		case "/api/v1/messages":
			var in core.SendInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			replies = append(replies, in)
			fmt.Fprint(w, `{"id":"reply"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return Config{Server: server.URL, Token: "test-token"}, &replies
}
func TestInboxFiltering(t *testing.T) {
	i := &inbox{agent: "self", allowed: []string{"peer"}}
	cases := []struct {
		name string
		e    inboxEvent
		want bool
	}{
		{"mention", event(1, "peer", "self"), true},
		{"self", event(1, "self", "self"), false},
		{"untrusted", event(1, "stranger", "self"), false},
		{"not mentioned", event(1, "peer", "someone"), false},
	}
	automated := event(2, "peer", "self")
	automated.Payload.Metadata = json.RawMessage(`{"tincan_listener":true}`)
	cases = append(cases, struct {
		name string
		e    inboxEvent
		want bool
	}{"automated", automated, false})
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := i.accepts(tt.e); got != tt.want {
				t.Fatalf("accepts=%v", got)
			}
		})
	}
}
func TestInboxRecoveryLockAndReply(t *testing.T) {
	c, replies := fixture(t, []inboxEvent{event(1, "other", "self"), event(2, "peer", "self"), event(3, "peer", "self")})
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openInbox(c, "peer"); err == nil {
		t.Fatal("second listener acquired lock")
	}
	e, err := i.next(context.Background())
	if err != nil || e.Seq != 2 {
		t.Fatalf("next: %v %v", e, err)
	}
	if err = i.ack(3); err == nil {
		t.Fatal("out of order ack succeeded")
	}
	path := i.path
	i.close()
	i, err = openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	e, err = i.next(context.Background())
	if err != nil || e.Seq != 2 {
		t.Fatalf("lost pending event: %v %v", e, err)
	}
	// Simulate a crash after the server accepted a reply but before local ack.
	pending := i.state
	if _, err = i.reply(2, "done"); err != nil {
		t.Fatal(err)
	}
	if err = i.save(pending); err != nil {
		t.Fatal(err)
	}
	if _, err = i.reply(2, "done"); err != nil {
		t.Fatal(err)
	}
	if len(*replies) != 2 || (*replies)[0].IdempotencyKey != (*replies)[1].IdempotencyKey {
		t.Fatal("retry was not idempotent")
	}
	r := (*replies)[0]
	if r.ChannelID != "channel" || r.ReplyTo == nil || *r.ReplyTo != "message-2" || len(r.Mentions) != 0 || !strings.Contains(string(r.Metadata), "tincan_listener") {
		t.Fatalf("bad reply routing: %+v", r)
	}
	e, err = i.next(context.Background())
	if err != nil || e.Seq != 3 {
		t.Fatalf("did not advance: %v %v", e, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
}
func TestInboxRejectsMissingAllowlistAndRevalidatesPending(t *testing.T) {
	c, _ := fixture(t, []inboxEvent{event(1, "peer", "self"), event(2, "new-peer", "self")})
	if _, err := openInbox(c, ""); err == nil {
		t.Fatal("missing allowlist accepted")
	}
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = i.next(context.Background()); err != nil {
		t.Fatal(err)
	}
	i.close()
	i, err = openInbox(c, "new-peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	e, err := i.next(context.Background())
	if err != nil || e.Seq != 2 {
		t.Fatalf("revoked sender replayed: %v %v", e, err)
	}
}
func TestNativePushReceivesWhileWorkIsPending(t *testing.T) {
	c, _ := fixture(t, []inboxEvent{event(1, "peer", "self"), event(2, "peer", "self")})
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan int64, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pushInbox(ctx, i, func(_ context.Context, e *inboxEvent) error { got <- e.Seq; return nil })
	}()
	select {
	case seq := <-got:
		if seq != 1 {
			t.Fatal(seq)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	select {
	case seq := <-got:
		if seq != 2 {
			t.Fatal(seq)
		}
	case <-time.After(time.Second):
		t.Fatal("no second notification")
	}
	if err := i.ack(1); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
}

type captureConnection struct{ message jsonrpc.Message }

func (c *captureConnection) Read(context.Context) (jsonrpc.Message, error) { panic("unused") }
func (c *captureConnection) Write(_ context.Context, m jsonrpc.Message) error {
	c.message = m
	return nil
}
func (c *captureConnection) Close() error      { return nil }
func (c *captureConnection) SessionID() string { return "" }
func TestClaudeNotificationContract(t *testing.T) {
	c := &captureConnection{}
	transport := &channelTransport{conn: c}
	e := event(42, "peer", "self")
	if err := transport.notify(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	b, err := jsonrpc.EncodeMessage(c.message)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Method string
		ID     any
		Params struct {
			Content string
			Meta    map[string]string
		}
	}
	if err = json.Unmarshal(b, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Method != "notifications/claude/channel" || wire.ID != nil || wire.Params.Meta["event_seq"] != "42" || strings.Contains(wire.Params.Content, "Please inspect") || !strings.Contains(wire.Params.Content, "background_delegate") {
		t.Fatalf("invalid notification: %s", b)
	}
}
func TestPortableToolsOverMCP(t *testing.T) {
	c, _ := fixture(t, []inboxEvent{event(1, "peer", "self")})
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	addInboxTools(server, i)
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "portable", Version: "1"}, nil)
	cs, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	claim := ""
	for _, call := range []struct {
		name string
		args map[string]any
	}{{"inbox_next", map[string]any{}}, {"inbox_claim", map[string]any{"seq": 1, "worker_id": "portable-child"}}, {"inbox_reply", map[string]any{"seq": 1, "text": "done"}}} {
		if call.name == "inbox_reply" {
			call.args["claim"] = claim
		}
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: call.name, Arguments: call.args})
		if err != nil {
			t.Fatal(err)
		}
		if result.IsError {
			t.Fatalf("%s failed: %+v", call.name, result)
		}
		if call.name == "inbox_claim" {
			var claimed claimResult
			data, _ := json.Marshal(result.StructuredContent)
			if err := json.Unmarshal(data, &claimed); err != nil || !claimed.Acquired {
				t.Fatal(result, err)
			}
			claim = claimed.Claim
		}
	}
	if i.state.After != 1 || i.state.Pending != nil {
		t.Fatal("MCP reply did not acknowledge")
	}
}
func TestListenHandlerFailureRetainsWork(t *testing.T) {
	c, _ := fixture(t, []inboxEvent{event(1, "peer", "self")})
	t.Setenv("TINCAN_TOKEN", c.Token)
	t.Setenv("TINCAN_SERVER", c.Server)
	// This test binary acts as a deterministic harness adapter.
	t.Setenv("TINCAN_TEST_HANDLER", "fail")
	err := listenCommand([]string{"--allow-senders", "peer", "--max-events", "1", "--", os.Args[0], "-test.run=TestHandlerHelper"})
	if err == nil {
		t.Fatal("failed handler was acknowledged")
	}
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	if i.state.Pending == nil {
		t.Fatal("failed work lost")
	}
	if err := i.decision(1, "", "resume", "test operator confirmed subprocess exited", "", true); err != nil {
		t.Fatal(err)
	}
	i.close()
	t.Setenv("TINCAN_TEST_HANDLER", "ok")
	if err = listenCommand([]string{"--allow-senders", "peer", "--max-events", "1", "--", os.Args[0], "-test.run=TestHandlerHelper"}); err != nil {
		t.Fatal(err)
	}
}
func TestHandlerHelper(t *testing.T) {
	switch os.Getenv("TINCAN_TEST_HANDLER") {
	case "fail":
		os.Exit(7)
	case "ok":
		var e struct {
			Event inboxEvent `json:"event"`
		}
		if json.NewDecoder(os.Stdin).Decode(&e) != nil || e.Event.Seq != 1 {
			os.Exit(8)
		}
		fmt.Print(`{"status":"completed","reply":"Completed the requested check."}`)
		os.Exit(0)
	}
}

func TestReplyIntentSurvivesNetworkFailure(t *testing.T) {
	t.Setenv("TINCAN_CONFIG", filepath.Join(t.TempDir(), "agent.json"))
	texts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me":
			fmt.Fprint(w, `{"agent":{"id":"self"}}`)
		case "/api/v1/events":
			e := event(2, "peer", "self")
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
		case "/api/v1/messages":
			var in core.SendInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			texts = append(texts, in.Text)
			if len(texts) == 1 {
				http.Error(w, "retry later", 503)
				return
			}
			fmt.Fprint(w, `{"id":"reply"}`)
		}
	}))
	defer server.Close()
	c := Config{Server: server.URL, Token: "test"}
	i, err := openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	e := event(1, "peer", "self")
	if err = i.save(inboxState{Pending: &e}); err != nil {
		t.Fatal(err)
	}
	if _, err = i.reply(1, "original result"); err == nil {
		t.Fatal("expected failed POST")
	}
	if err = i.ack(1); err == nil {
		t.Fatal("ack dropped an unsent reply")
	}
	i.close()
	i, err = openInbox(c, "peer")
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	next, err := i.next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if next.Seq != 2 || len(texts) != 2 || texts[0] != texts[1] {
		t.Fatalf("reply intent lost: next=%+v texts=%v", next, texts)
	}
}

func TestNativeMCPInitializationAndDelivery(t *testing.T) {
	c, _ := fixture(t, []inboxEvent{event(1, "peer", "self")})
	t.Setenv("TINCAN_WAKE", "claude")
	t.Setenv("TINCAN_ALLOW_SENDERS", "peer")
	opts, transport, cleanup, err := inboxBridge(c)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	transport.(*channelTransport).base = a
	server := mcp.NewServer(&mcp.Implementation{Name: "tincan", Version: "test"}, opts.options)
	addInboxTools(server, opts.inbox)
	ss, err := server.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	raw, err := b.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	init, _ := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if err = raw.Write(ctx, init); err != nil {
		t.Fatal(err)
	}
	response, err := raw.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := jsonrpc.EncodeMessage(response)
	if !strings.Contains(string(encoded), `"claude/channel":{}`) {
		t.Fatalf("channel not advertised: %s", encoded)
	}
	initialized, _ := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err = raw.Write(ctx, initialized); err != nil {
		t.Fatal(err)
	}
	pushed, err := raw.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = jsonrpc.EncodeMessage(pushed)
	if !strings.Contains(string(encoded), `notifications/claude/channel`) {
		t.Fatalf("missing native push: %s", encoded)
	}
	// Disconnect without acknowledging. The event must remain durable for restart.
	opts.inbox.mu.Lock()
	pending := opts.inbox.state.Pending
	opts.inbox.mu.Unlock()
	if pending == nil || pending.Seq != 1 {
		t.Fatal("transport write incorrectly acknowledged work")
	}
}
