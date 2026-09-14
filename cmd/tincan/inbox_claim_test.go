package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func claimedInbox(t *testing.T) *inbox {
	t.Helper()
	e := event(42, "peer", "self")
	i := &inbox{agent: "self", allowed: []string{"peer"}, path: filepath.Join(t.TempDir(), "inbox.json"), changed: make(chan struct{}, 1)}
	if err := i.save(inboxState{Pending: &e}); err != nil {
		t.Fatal(err)
	}
	return i
}

func TestOneDelegatedWorkerOwnsRequest(t *testing.T) {
	i := claimedInbox(t)
	var wg sync.WaitGroup
	winners := make(chan claimResult, 12)
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, err := i.claim(42, fmt.Sprintf("child-%d", n))
			if err != nil {
				t.Error(err)
				return
			}
			if r.Acquired {
				winners <- r
			} else if r.Claim != "" || r.Event != nil {
				t.Error("busy claim exposed worker data")
			}
		}(n)
	}
	wg.Wait()
	close(winners)
	if len(winners) != 1 {
		t.Fatalf("started %d workers", len(winners))
	}
	winner := <-winners
	if winner.Event == nil || winner.Claim == "" {
		t.Fatal(winner)
	}
	if err := i.ack(42); err == nil {
		t.Fatal("foreground acknowledged active worker")
	}
	if _, err := i.reply(42, "wrong", "wrong-token"); err == nil {
		t.Fatal("another worker replied")
	}
	if err := i.release(42, "wrong-token"); err == nil {
		t.Fatal("another worker released ownership")
	}
	var recovered inboxState
	data, err := os.ReadFile(i.path)
	if err != nil || json.Unmarshal(data, &recovered) != nil {
		t.Fatal(err)
	}
	i.state = recovered
	r, err := i.claim(42, winner.WorkerID)
	if err != nil || !r.Acquired || r.Claim != winner.Claim {
		t.Fatal("same worker lost claim on restart", r, err)
	}
	if err = i.release(42, winner.Claim); err != nil {
		t.Fatal(err)
	}
	if i.state.Pending == nil || i.state.After != 42 {
		t.Fatal("release acknowledged work")
	}
	r, err = i.claim(42, "replacement")
	if err != nil || !r.Acquired || r.Claim == winner.Claim {
		t.Fatal(r, err)
	}
	if err = i.ack(42, winner.Claim); err == nil {
		t.Fatal("stale worker committed")
	}
	if err = i.ack(42, r.Claim); err != nil {
		t.Fatal(err)
	}
	if i.state.Pending != nil || i.state.Claim != nil || i.state.After != 42 {
		t.Fatal(i.state)
	}
	r, err = i.claim(42, "duplicate")
	if err != nil || r.Acquired {
		t.Fatal("acknowledged event ran twice", r, err)
	}
}

func TestDelegatedMCPClaimAndReply(t *testing.T) {
	for _, metadata := range []string{`{}`, `{"nested":{"enabled":true},"values":[1,"two"],"empty":{}}`, `null`, `[]`} {
		t.Run(metadata, func(t *testing.T) {
			e := event(42, "peer", "self")
			e.Payload.Metadata = json.RawMessage(metadata)
			c, replies := fixture(t, []inboxEvent{e})
			i, err := openInbox(c, "peer")
			if err != nil {
				t.Fatal(err)
			}
			defer i.close()
			if _, err = i.next(context.Background()); err != nil {
				t.Fatal(err)
			}
			server := mcp.NewServer(&mcp.Implementation{Name: "claims", Version: "1"}, nil)
			addInboxTools(server, i)
			a, b := mcp.NewInMemoryTransports()
			ss, err := server.Connect(context.Background(), a, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			cs, err := mcp.NewClient(&mcp.Implementation{Name: "child", Version: "1"}, nil).Connect(context.Background(), b, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cs.Close()
			r, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_claim", Arguments: map[string]any{"seq": 42, "worker_id": "child"}})
			if err != nil || r.IsError {
				t.Fatal(r, err)
			}
			var claimed claimResult
			data, _ := json.Marshal(r.StructuredContent)
			if json.Unmarshal(data, &claimed) != nil || !claimed.Acquired {
				t.Fatal(string(data))
			}
			// A response lost after persistence is recoverable by the SAME worker.
			retry, retryErr := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_claim", Arguments: map[string]any{"seq": 42, "worker_id": "child"}})
			if retryErr != nil || retry.IsError {
				t.Fatal(retry, retryErr)
			}
			var recovered claimResult
			retryData, _ := json.Marshal(retry.StructuredContent)
			if json.Unmarshal(retryData, &recovered) != nil || recovered.Claim != claimed.Claim || recovered.Claim == "" {
				t.Fatal("claim retry lost ownership")
			}
			if string(recovered.Event.Payload.Metadata) != string(claimed.Event.Payload.Metadata) {
				t.Fatal("metadata changed on retry")
			}
			r, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_reply", Arguments: map[string]any{"seq": 42, "text": "done"}})
			if err != nil || !r.IsError || len(*replies) != 0 {
				t.Fatal("unclaimed completion succeeded", r, err)
			}
			r, err = cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "inbox_reply", Arguments: map[string]any{"seq": 42, "text": "done", "claim": claimed.Claim}})
			if err != nil || r.IsError || len(*replies) != 1 || i.state.Pending != nil {
				t.Fatal(r, err)
			}
		})
	}
}

func TestDispatchNoticeOmitsPeerBody(t *testing.T) {
	for _, host := range []string{"claude", "codex", "custom", "codex-worker"} {
		t.Run(host, func(t *testing.T) {
			b := &pluginBroker{host: host}
			var got map[string]any
			if host == "codex" {
				root, c, _ := hookFixture(t)
				b.root = root
				b.codexSend = func(_ context.Context, _ string, p map[string]any) error { got = p; return nil }
				b.setDelivery(c.Handle, deliveryState{Method: "codex_app_server"})
				if err := b.deliver(context.Background(), c, map[string]any{"kind": "mention", "event_seq": int64(42), "connection": c.Handle, "payload": "PEER BODY"}); err != nil {
					t.Fatal(err)
				}
			} else {
				b.notify = func(_ context.Context, p map[string]any) error { got = p; return nil }
				if err := b.deliver(context.Background(), &pluginConnection{}, map[string]any{"kind": "mention", "event_seq": int64(42), "payload": "PEER BODY"}); err != nil {
					t.Fatal(err)
				}
			}
			data, _ := json.Marshal(got)
			if got["execution"].(map[string]any)["foreground_execution"] != false || strings.Contains(string(data), "PEER BODY") {
				t.Fatal(string(data))
			}
		})
	}
}

func TestClaimedWorkDoesNotBlockMainStop(t *testing.T) {
	root, c, path := hookFixture(t)
	var s inboxState
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	s.Claim = &inboxClaim{WorkerID: "background-child", Token: "private-token"}
	if err := writePrivateJSON(path, s); err != nil {
		t.Fatal(err)
	}
	for _, event := range []string{"PostToolUse", "Stop", "UserPromptSubmit"} {
		v, err := runHook(root, hookInput{SessionID: c.CodexThreadID, Event: event})
		if err != nil || len(v) != 0 {
			t.Fatal("hook redispatched or waited for active worker", v, err)
		}
	}
}
