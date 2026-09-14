package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestBackgroundSSEStaysOpenAndRecoversPending(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/me" {
			fmt.Fprint(w, `{"agent":{"id":"self"}}`)
			return
		}
		if r.URL.Path != "/api/v1/events" {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		var after int64
		fmt.Sscan(r.URL.Query().Get("after"), &after)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range []inboxEvent{event(1, "peer", "other"), event(2, "peer", "self"), event(3, "peer", "self")} {
			if e.Seq > after {
				data, _ := json.Marshal(e)
				fmt.Fprintf(w, "data: %s\n\n", data)
			}
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	config := Config{Server: server.URL, Token: "test-token"}
	path := filepath.Join(t.TempDir(), "agent.json")
	i, err := openInboxAt(config, "", path, true)
	if err != nil {
		t.Fatal(err)
	}
	i.background = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	delivered := make(chan int64, 4)
	notify := func(ctx context.Context, e *inboxEvent) error { delivered <- e.Seq; return nil }
	go func() { defer close(done); i.stream(ctx, nil, notify) }()
	receive := func(want int64) {
		t.Helper()
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("got %d want %d", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no push")
		}
	}
	receive(2)
	receive(3) // Both messages arrive before either commitment finishes.
	if requests.Load() != 1 {
		t.Fatalf("reopened stream while idle: %d", requests.Load())
	}
	if err = i.ack(2); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("reconnected between messages")
	}
	cancel()
	<-done
	i.close()
	// Pending delivery survives a restart before ack. It is recovered before any
	// new HTTP request, and inbox_next never competes with the stream for reads.
	i, err = openInboxAt(config, "", path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer i.close()
	i.background = true
	started := time.Now()
	pending, err := i.next(context.Background())
	if err != nil || pending == nil || pending.Seq != 3 || time.Since(started) > time.Second {
		t.Fatal("pending retrieval did not return immediately")
	}
	if err = i.ack(3); err != nil {
		t.Fatal(err)
	}
	pending, err = i.next(context.Background())
	if err != nil || pending != nil {
		t.Fatal("empty background inbox should return immediately")
	}
	if requests.Load() != 1 {
		t.Fatal("snapshot opened another stream")
	}
}

func TestPluginClaudeCapabilityAndNotificationMetadata(t *testing.T) {
	broker := &pluginBroker{root: t.TempDir(), native: true}
	server := broker.serverWithTools(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	left, right := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	caps, _ := json.Marshal(cs.InitializeResult().Capabilities)
	if !strings.Contains(string(caps), `"claude/channel":{}`) {
		t.Fatalf("missing capability: %s", caps)
	}
	capture := &captureConnection{}
	transport := &channelTransport{conn: capture}
	if err = transport.notifyPlugin(ctx, map[string]any{"kind": "mention", "connection": "private-handle", "event_seq": int64(42), "payload": event(42, "peer", "self").Payload}); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(capture.message)
	if !strings.Contains(string(data), "notifications/claude/channel") || !strings.Contains(string(data), "private-handle") || !strings.Contains(string(data), `"event_seq":"42"`) {
		t.Fatalf("missing routing metadata: %s", data)
	}
}
