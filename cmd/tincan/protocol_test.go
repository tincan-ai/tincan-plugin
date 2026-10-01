package main

import (
	"context"
	"encoding/json"
	"github.com/tincan-ai/tincan-plugin/internal/protocol"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func protocolDocument() protocol.Descriptor {
	return protocol.Descriptor{Protocol: "tincan", Versions: []string{protocol.Version}, Profiles: []string{protocol.CoreProfile, protocol.Profile}, Capabilities: []string{}, Tools: append([]string{}, protocol.RequiredTools...)}
}

func TestProtocolDiscovery(t *testing.T) {
	for _, test := range []struct {
		name              string
		status            int
		body              string
		wantError, legacy bool
	}{
		{"legacy", 404, `{}`, false, true},
		{"unavailable", 503, `{}`, true, false},
		{"invalid", 200, `{`, true, false},
		{"oversize", 200, strings.Repeat(" ", 128*1024+1), true, false},
		{"redirect", 302, `{}`, true, false},
		{"missing-required-tools", 200, `{"protocol":"tincan","versions":["0.1"],"profiles":["tincan-core/0.1","tincan-plugin/0.1"],"capabilities":[],"tools":[]}`, true, false},
		{"incompatible-version", 200, `{"protocol":"tincan","versions":["99"],"profiles":[],"capabilities":[],"tools":[]}`, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != protocol.Path {
					t.Error("discovery followed a redirect")
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("discovery leaked credentials")
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(test.status)
				w.Write([]byte(test.body))
			}))
			defer s.Close()
			d, err := discoverProtocol(context.Background(), s.URL)
			if (err != nil) != test.wantError || (err == nil && (d == nil) != test.legacy) {
				t.Fatalf("descriptor=%v error=%v", d, err)
			}
		})
	}
}

func TestProtocolCapabilitiesAndDowngrade(t *testing.T) {
	d := protocolDocument()
	missing := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if missing {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(d)
	}))
	defer s.Close()
	c := &pluginConnection{Config: Config{Server: s.URL}}
	b := &pluginBroker{}
	if err := b.refreshProtocol(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err := checkServerTool(c, "message_send", nil); err != nil {
		t.Fatal(err)
	}
	if err := checkServerTool(c, "pages_list", nil); err == nil {
		t.Fatal("unsupported tool accepted")
	}
	if err := checkServerTool(c, "room_create", map[string]any{"e2ee": true}); err == nil {
		t.Fatal("unsupported encryption accepted")
	}
	missing = true
	if err := b.refreshProtocol(context.Background(), c); err == nil {
		t.Fatal("negotiated connection silently downgraded")
	}
}
