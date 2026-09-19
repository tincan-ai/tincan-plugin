package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

// Run a real, separate stdio MCP process with the same connection vault as the
// parent, matching hosts that do not share their parent's MCP process with a child.
func TestInboxOwnerChildProcess(t *testing.T) {
	if os.Getenv("TINCAN_TEST_INBOX_CHILD") != "1" {
		return
	}
	b := &pluginBroker{root: os.Getenv("TINCAN_TEST_INBOX_ROOT"), host: "codex"}
	err := b.serverWithTools(nil).Run(context.Background(), &mcp.StdioTransport{})
	b.close()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func inboxOwnerChild(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInboxOwnerChildProcess$")
	cmd.Env = append(os.Environ(), "TINCAN_TEST_INBOX_CHILD=1", "TINCAN_TEST_INBOX_ROOT="+root)
	client, err := mcp.NewClient(&mcp.Implementation{Name: "child-test", Version: "1"}, nil).Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

func ownerCall(t *testing.T, client *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := client.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || r.IsError {
		t.Fatalf("%s: %v %v", name, r, err)
	}
	var out map[string]any
	if err := decodeValue(r.StructuredContent, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func ownerFixture(t *testing.T) (*pluginBroker, *pluginConnection, *inbox, chan inboxEvent, *atomic.Int32, chan core.SendInput) {
	t.Helper()
	events := make(chan inboxEvent, 4)
	replies := make(chan core.SendInput, 4)
	streams := new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/me":
			fmt.Fprint(w, `{"agent":{"id":"self"}}`)
		case "/api/v1/agents":
			fmt.Fprint(w, `[]`)
		case "/api/v1/events":
			streams.Add(1)
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
		case "/api/v1/channels":
			fmt.Fprint(w, `[{"id":"channel","encryption_mode":"standard"}]`)
		case "/api/v1/messages":
			var in core.SendInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			replies <- in
			fmt.Fprint(w, `{"id":"reply"}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(s.Close)
	b := &pluginBroker{root: t.TempDir(), host: "mcp"}
	t.Cleanup(b.close)
	c := &pluginConnection{Handle: "conn_11111111111111111111111111111111", CodexThreadID: testCodexThread, AgentID: "self", Config: Config{Server: s.URL, Token: "saved"}}
	if err := b.save(c); err != nil {
		t.Fatal(err)
	}
	b.serverWithTools(nil)
	if err := b.startBackground(c); err != nil {
		t.Fatal(err)
	}
	i, err := b.getInbox(c)
	if err != nil {
		t.Fatal(err)
	}
	return b, c, i, events, streams, replies
}

func TestInboxOwnerAcrossProcesses(t *testing.T) {
	b, c, i, events, streams, replies := ownerFixture(t)
	child := inboxOwnerChild(t, b.root)
	path, _ := b.path(c.Handle)
	if info, err := os.Stat(path + ".owner"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("owner capability is not private", info, err)
	}
	for seq := int64(1); seq <= 2; seq++ {
		result := make(chan *mcp.CallToolResult, 1)
		go func() {
			r, err := child.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "child", "wait_seconds": 30}})
			if err != nil {
				t.Error(err)
			}
			result <- r
		}()
		waitUntilArmed(t, i)
		// One child's wait cannot block other MCP calls or acquire a second lock.
		status := ownerCall(t, child, "tincan_status", map[string]any{"connection": c.Handle})
		if status["background_listener"] != true {
			t.Fatal("child status did not reach owner", status)
		}
		if lock, err := lockInbox(i.path + ".lock"); err == nil {
			unlockInbox(lock)
			t.Fatal("parent released its inbox lock")
		}
		select {
		case r := <-result:
			t.Fatal("empty wait returned to the model", r)
		case <-time.After(30 * time.Millisecond):
		}
		events <- event(seq, "peer", "self")
		select {
		case r := <-result:
			if r == nil || r.IsError {
				t.Fatal("separate child could not wait on owner", r)
			}
			var out map[string]any
			_ = decodeValue(r.StructuredContent, &out)
			if out["event_seq"] != float64(seq) || out["status"] != "event" || out["event"] != nil {
				t.Fatal(out)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("child did not receive mention")
		}
		claim := ownerCall(t, child, "inbox_claim", map[string]any{"connection": c.Handle, "seq": seq, "worker_id": "child"})
		if claim["acquired"] != true || claim["event"] == nil {
			t.Fatal("child failed to claim", claim)
		}
		other := ownerCall(t, child, "inbox_claim", map[string]any{"connection": c.Handle, "seq": seq, "worker_id": "other"})
		if other["acquired"] != false {
			t.Fatal("duplicate claim accepted", other)
		}
		ownerCall(t, child, "inbox_reply", map[string]any{"connection": c.Handle, "seq": seq, "text": "handled", "claim": claim["claim"]})
		reply := <-replies
		if reply.Text != "handled" || reply.ReplyTo == nil || *reply.ReplyTo != fmt.Sprintf("message-%d", seq) || !strings.Contains(string(reply.Metadata), "tincan_listener") {
			t.Fatal("reply lost routing or loop protection", reply)
		}
	}
	if streams.Load() != 1 {
		t.Fatal("child opened another SSE stream", streams.Load())
	}
}

func TestInboxOwnerCancellationAndShutdown(t *testing.T) {
	b, c, i, _, _, _ := ownerFixture(t)
	child := inboxOwnerChild(t, b.root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := child.CallTool(ctx, &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "child"}})
		done <- err
	}()
	waitUntilArmed(t, i)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation did not reach child", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for i.waiting() != nil {
		if time.Now().After(deadline) {
			t.Fatal("cancellation stranded owner waiter")
		}
		time.Sleep(time.Millisecond)
	}
	// Closing the owner wakes a delegated call and cannot let the child become
	// a replacement stream owner when it next tries to wait.
	result := make(chan *mcp.CallToolResult, 1)
	go func() {
		r, _ := child.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "child"}})
		result <- r
	}()
	waitUntilArmed(t, i)
	b.close()
	select {
	case r := <-result:
		if r == nil || !r.IsError {
			t.Fatal("owner shutdown did not stop child", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stranded child")
	}
	r, err := child.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_wait", Arguments: map[string]any{"connection": c.Handle, "worker_id": "child"}})
	if err != nil || !r.IsError {
		t.Fatal("child took over a stopped owner's inbox", r, err)
	}
	path, _ := b.path(c.Handle)
	if _, err := os.Stat(path + ".owner"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("shutdown retained bridge capability", err)
	}
}

func TestInboxOwnerRejectsUnauthorizedRequests(t *testing.T) {
	b, c, i, _, _, _ := ownerFixture(t)
	ref := b.inboxOwner.refs[c.Handle]
	for _, tc := range []struct{ name, handle, secret, origin string }{
		{"inbox_next", c.Handle, "wrong", ""},
		{"inbox_next", "conn_22222222222222222222222222222222", ref.Secret, ""},
		{"tincan_connect", c.Handle, ref.Secret, ""},
		{"message_send", c.Handle, ref.Secret, ""},
		{"inbox_next", c.Handle, ref.Secret, "http://example.com"},
	} {
		data, _ := json.Marshal(map[string]any{"name": tc.name, "arguments": map[string]any{"connection": tc.handle}})
		req, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(ref.Port)+"/inbox", bytes.NewReader(data))
		req.Header.Set("Authorization", "Bearer "+tc.secret)
		req.Header.Set("Origin", tc.origin)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode < 400 {
			t.Fatal("bridge accepted unauthorized request", tc.name)
		}
	}
	if i.waiting() != nil {
		t.Fatal("unauthorized request armed listener")
	}
}

func TestInboxOwnerPreservesClaimsAcrossRestart(t *testing.T) {
	b, c, i, _, _, _ := ownerFixture(t)
	child := inboxOwnerChild(t, b.root)
	saveWaitingEvent(t, i, event(7, "peer", "self"))
	claim := ownerCall(t, child, "inbox_claim", map[string]any{"connection": c.Handle, "seq": 7, "worker_id": "original"})
	if claim["acquired"] != true {
		t.Fatal(claim)
	}
	b.close()
	// Only an explicit parent resume acquires the released kernel lock and
	// publishes a new capability. The child follows the new owner automatically.
	restarted := &pluginBroker{root: b.root, host: "mcp"}
	t.Cleanup(restarted.close)
	restarted.serverWithTools(nil)
	if err := restarted.startBackground(c); err != nil {
		t.Fatal(err)
	}
	status := ownerCall(t, child, "inbox_requests", map[string]any{"connection": c.Handle})
	data, _ := json.Marshal(status)
	if !strings.Contains(string(data), "needs_recovery") {
		t.Fatal("restart lost the original claim", status)
	}
	other := ownerCall(t, child, "inbox_claim", map[string]any{"connection": c.Handle, "seq": 7, "worker_id": "replacement"})
	if other["acquired"] != false {
		t.Fatal("restart reran uncertain work", other)
	}
	// The test controls both workers and knows the old one stopped. Its exact
	// private claim, not a new worker ID, is required for explicit recovery.
	ownerCall(t, child, "inbox_release", map[string]any{"connection": c.Handle, "seq": 7, "claim": claim["claim"]})
	claim = ownerCall(t, child, "inbox_claim", map[string]any{"connection": c.Handle, "seq": 7, "worker_id": "replacement"})
	ownerCall(t, child, "inbox_ack", map[string]any{"connection": c.Handle, "seq": 7, "claim": claim["claim"]})
	pending := ownerCall(t, child, "inbox_next", map[string]any{"connection": c.Handle})
	if pending["event"] != nil {
		t.Fatal("acknowledged event remained pending", pending)
	}
}

func TestInboxOwnerDoesNotReplayUncertainReply(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		// Simulate a committed mutation whose response is lost.
		conn.Close()
	}))
	defer server.Close()
	ref := inboxOwnerReference{Port: server.Listener.Addr().(*net.TCPAddr).Port, Secret: core.ID("")}
	result := forwardInboxOwner(context.Background(), ref, &mcp.CallToolParamsRaw{Name: "inbox_reply", Arguments: json.RawMessage(`{"connection":"conn_11111111111111111111111111111111","seq":1,"claim":"private","text":"done"}`)})
	if !result.IsError || calls.Load() != 1 {
		t.Fatal("uncertain reply was retried or reported successful", result, calls.Load())
	}
}

func TestInboxOwnerUnavailableDoesNotTakeOver(t *testing.T) {
	b, c, i, _, _, _ := ownerFixture(t)
	child := inboxOwnerChild(t, b.root)
	path, _ := b.path(c.Handle)
	data, err := os.ReadFile(path + ".owner")
	if err != nil {
		t.Fatal(err)
	}
	b.close()
	// A crash can leave a stale descriptor. Its absence or failure is never
	// permission for a worker to acquire the free lock and start a new stream.
	if err := os.WriteFile(path+".owner", data, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := child.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_next", Arguments: map[string]any{"connection": c.Handle}})
	if err != nil || !r.IsError {
		t.Fatal("stale owner fell back to local inbox ownership", r, err)
	}
	lock, err := lockInbox(i.path + ".lock")
	if err != nil {
		t.Fatal("child took the stopped owner's lock", err)
	}
	unlockInbox(lock)
}

func TestInboxOwnerBridgeFailureKeepsBackgroundInbox(t *testing.T) {
	b, c, _, _, _, _ := ownerFixture(t)
	b.close()
	path, _ := b.path(c.Handle)
	// A directory at the capability path deterministically blocks publication.
	if err := os.Mkdir(path+".owner", 0700); err != nil {
		t.Fatal(err)
	}
	restarted := &pluginBroker{root: b.root, host: "mcp"}
	t.Cleanup(restarted.close)
	restarted.serverWithTools(nil)
	if err := restarted.startBackground(c); err != nil {
		t.Fatal("optional bridge failure stopped SSE", err)
	}
	view, err := restarted.status(c.Handle)
	if err != nil || view["background_listener"] != true {
		t.Fatal("bridge failure removed existing delivery", view, err)
	}
	bridge := view["inbox_bridge"].(map[string]any)
	if bridge["available"] != false || bridge["error"] == "" {
		t.Fatal("bridge limitation was hidden", bridge)
	}
}
