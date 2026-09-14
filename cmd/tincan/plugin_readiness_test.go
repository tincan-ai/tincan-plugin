package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectionReadiness(t *testing.T) {
	for _, tc := range []struct {
		name string
		view map[string]any
		want string
	}{
		{"verified wake", map[string]any{"background_listener": true, "idle_wake": true}, "ready"},
		{"delegated listener starting", map[string]any{"background_listener": true, "delegated_listener": map[string]any{"required": true}}, "starting"},
		{"delegated listener armed", map[string]any{"background_listener": true, "idle_wake": false, "delivery": "delegated_listener"}, "experimental"},
		{"delegated listener stopped", map[string]any{"background_listener": false, "delivery": "delegated_listener"}, "needs_attention"},
		{"listener alone", map[string]any{"background_listener": true}, "listening"},
		{"Claude needs opt in", map[string]any{"background_listener": true, "delivery": "claude_channel_requires_host_opt_in"}, "listening"},
		{"setup failed", map[string]any{"setup_error": "offline", "idle_wake": true}, "needs_attention"},
		{"stopped", map[string]any{}, "needs_attention"},
		{"stale delivery", map[string]any{"idle_wake": true}, "needs_attention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connectionReadiness(tc.view)
			if tc.view["readiness"] != tc.want || tc.view["user_message"] == "" {
				t.Fatal(tc.view)
			}
		})
	}
}

func TestImportDirectKeepsIdentityWithoutRedeemingInvite(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
			http.Error(w, "unexpected write", 400)
			return
		}
		if r.Header.Get("Authorization") != "Bearer saved-token" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/me":
			w.Write([]byte(`{"agent":{"id":"same-agent","name":"Codex","profile":"Existing profile"}}`))
		case "/api/v1/rooms":
			w.Write([]byte(`[{"id":"shared","name":"Dev"},{"id":"private","private":true},{"id":"archived","archived":true}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	b := &pluginBroker{root: t.TempDir()}
	c, err := b.importDirect(server.URL, "saved-token", "shared")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := b.load(c.Handle)
	if err != nil || saved.AgentID != "same-agent" || saved.Config.Token != "saved-token" || saved.HelloID == "" {
		t.Fatal("identity not preserved", err)
	}
	for _, room := range []string{"private", "archived", "foreign"} {
		if _, err := b.importDirect(server.URL, "saved-token", room); err == nil {
			t.Fatal("accepted", room)
		}
	}
	if _, err := b.importDirect(server.URL, "wrong-token", "shared"); err == nil {
		t.Fatal("accepted invalid credential")
	}
	if writes != 0 {
		t.Fatal("import wrote to server")
	}
}
