package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

func TestClaudeSessionReadOnlyIsolation(t *testing.T) {
	b := &pluginBroker{root: t.TempDir(), host: "claude"}
	connections := []*pluginConnection{
		{Handle: "conn_00000000000000000000000000000001", AgentID: "self", Name: "Claude", HookHost: "claude", HookSessionID: "one", Config: Config{Token: "DO-NOT-EXPOSE", Server: "https://app.gotincan.com"}},
		{Handle: "conn_00000000000000000000000000000002", AgentID: "other", HookHost: "claude", HookSessionID: "two", Config: Config{Token: "secret"}},
		{Handle: "conn_00000000000000000000000000000003", AgentID: "cursor", HookHost: "cursor", HookSessionID: "one", Config: Config{Token: "secret"}},
		{Handle: "conn_00000000000000000000000000000004", AgentID: "unbound", Config: Config{Token: "secret"}},
		{Handle: "conn_00000000000000000000000000000005", AgentID: "worker", Worker: true, HookHost: "claude", HookSessionID: "one", Config: Config{Token: "secret"}},
	}
	for _, c := range connections {
		if err := b.save(c); err != nil {
			t.Fatal(err)
		}
	}
	c := connections[0]
	state := inboxState{Version: 2, After: 9, Requests: []inboxRequest{
		{Status: "running", Claim: &inboxClaim{Token: "PRIVATE-CLAIM", WorkerID: "worker"}, Context: "PRIVATE-CONTEXT", LastClaim: "PRIVATE-LAST-CLAIM", Event: inboxEvent{Seq: 7, Kind: "message", Payload: core.Message{ID: "msg1", ChannelID: "channel1", AgentName: "Codex", Text: "Please review", Mentions: []string{"self"}}}},
		{Status: "completed", Event: inboxEvent{Seq: 8, Kind: "message", Mentioned: true}},
		{Status: "declined", Event: inboxEvent{Seq: 9, Kind: "message", Mentioned: true}},
	}}
	path := inboxPath(b.root, c)
	if err := writePrivateJSON(path, state); err != nil {
		t.Fatal(err)
	}
	// Even while a listener owns the inbox, discovery just reads a snapshot.
	lock, err := lockInbox(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockInbox(lock)
	before, _ := os.ReadFile(path)
	filesBefore, _ := os.ReadDir(b.root)
	for n := 0; n < 2; n++ {
		view, err := b.claudeSessionConnections("one")
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(view)
		for _, secret := range []string{"DO-NOT-EXPOSE", "PRIVATE-CLAIM", "PRIVATE-CONTEXT", "PRIVATE-LAST-CLAIM", "cursor", "unbound", "worker", "other"} {
			if strings.Contains(string(data), secret) {
				t.Fatalf("session snapshot exposed %q: %s", secret, data)
			}
		}
		items := view["connections"].([]map[string]any)
		if len(items) != 1 || items[0]["pending_count"] != 1 || items[0]["background_listener"] != false {
			t.Fatal(view)
		}
		mentions := items[0]["pending_mentions"].([]map[string]any)
		if len(mentions) != 1 || mentions[0]["channel_id"] != "channel1" || mentions[0]["status"] != "running" {
			t.Fatal(mentions)
		}
	}
	after, _ := os.ReadFile(path)
	filesAfter, _ := os.ReadDir(b.root)
	if string(before) != string(after) || len(filesBefore) != len(filesAfter) || len(b.inboxes) != 0 || len(b.workers) != 0 {
		t.Fatal("discovery mutated inbox, hooks or runtime ownership")
	}
	for _, session := range []string{"", "../one", "one/../../two"} {
		if _, err := b.claudeSessionConnections(session); err == nil {
			t.Fatal("invalid session accepted", session)
		}
	}
	b.host = "codex"
	if _, err := b.claudeSessionConnections("one"); err == nil {
		t.Fatal("another host exposed Claude session state")
	}
}

func TestClaudeSessionMCPDiscoveryWithoutCredentials(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	b := &pluginBroker{root: root, host: "claude"}
	defer b.close()
	x, y := mcp.NewInMemoryTransports()
	s, err := b.serverWithTools(nil).Connect(context.Background(), x, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "mod-test", Version: "1"}, nil).Connect(context.Background(), y, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result, err := client.CallTool(context.Background(), &mcp.CallToolParams{Name: "tincan_session", Arguments: map[string]any{"hook_session_id": "one"}})
	if err != nil || result.IsError {
		t.Fatal(result, err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	if string(encoded) != `{"connections":[]}` {
		t.Fatal(string(encoded))
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("discovery created identity state", err)
	}
}
