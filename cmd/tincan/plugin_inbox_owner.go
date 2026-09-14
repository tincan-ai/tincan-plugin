package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

// Hosts may create a new stdio MCP process for each native child. Forward only
// inbox operations to the process that owns the kernel lock and SSE stream.
// Each connection has a private capability, published only while holding its
// inbox lock. No server credential, endpoint argument, or global identity is used.
type inboxOwnerReference struct {
	ControllerOnly bool   `json:"controller_only,omitempty"`
	Port           int    `json:"port"`
	Secret         string `json:"secret"`
}

type pluginInboxOwner struct {
	server *http.Server
	client *mcp.ClientSession
	local  *mcp.ServerSession
	refs   map[string]inboxOwnerReference // guarded by pluginBroker.mu
	paths  []string                       // changed only under pluginBroker.mu before shutdown
	port   int
	once   sync.Once
}

func inboxOwnerTool(name string) bool {
	switch name {
	case "inbox_plan", "inbox_requests", "inbox_outcome", "inbox_decide", "inbox_policy_set", "inbox_wait", "inbox_next", "inbox_claim", "inbox_release", "inbox_reply", "inbox_ack", "tincan_status", "tincan_pairing_wait":
		return true
	}
	return false
}

func inboxOwnerHandle(args json.RawMessage) string {
	var in struct {
		Connection string `json:"connection"`
	}
	_ = json.Unmarshal(args, &in)
	return in.Connection
}

func inboxOwnerError(err error) *mcp.CallToolResult {
	r := &mcp.CallToolResult{}
	r.SetError(err)
	return r
}

func (b *pluginBroker) addInboxBridgeReadiness(view map[string]any, handle string) {
	b.mu.Lock()
	available := b.inboxOwner != nil && b.inboxOwner.refs[handle].Secret != "" && !b.stopped
	i := b.inboxes[handle]
	b.mu.Unlock()
	bridgeError := ""
	if i != nil {
		i.mu.Lock()
		bridgeError = i.bridgeError
		i.mu.Unlock()
	}
	view["inbox_bridge"] = map[string]any{"available": available, "error": bridgeError}
}

func (b *pluginBroker) addInboxOwnerRouting(server *mcp.Server) {
	b.toolServer = server
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			call, ok := req.(*mcp.CallToolRequest)
			if method != "tools/call" || !ok || !inboxOwnerTool(call.Params.Name) || b.host == "codex-worker" {
				return next(ctx, method, req)
			}
			handle := inboxOwnerHandle(call.Params.Arguments)
			path, err := b.path(handle)
			if err != nil {
				return next(ctx, method, req)
			}
			b.mu.Lock()
			local, stopped := b.inboxes[handle] != nil, b.stopped
			b.mu.Unlock()
			if stopped {
				return inboxOwnerError(errors.New("plugin is shutting down")), nil
			}
			if local {
				return next(ctx, method, req)
			}
			data, err := os.ReadFile(path + ".owner")
			if errors.Is(err, os.ErrNotExist) {
				return next(ctx, method, req)
			}
			if err != nil {
				return inboxOwnerError(errors.New("cannot read the parent's inbox bridge; stop without taking over")), nil
			}
			var ref inboxOwnerReference
			if json.Unmarshal(data, &ref) != nil || ref.Port < 1 || ref.Port > 65535 || len(ref.Secret) != 32 {
				return inboxOwnerError(errors.New("invalid parent inbox bridge; stop without taking over")), nil
			}
			return forwardInboxOwner(ctx, ref, call.Params), nil
		}
	})
}

// Called with b.mu held, after acquiring and registering the inbox. An owned
// standalone worker exposes only controller operations through its bridge;
// staged completion cannot be bypassed by another process.
func (b *pluginBroker) publishInboxOwner(c *pluginConnection) error {
	if b.toolServer == nil {
		return nil
	}
	if b.inboxOwner == nil {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("start local inbox bridge: %w", err)
		}
		a, z := mcp.NewInMemoryTransports()
		local, err := b.toolServer.Connect(context.Background(), a, nil)
		if err != nil {
			listener.Close()
			return err
		}
		client, err := mcp.NewClient(&mcp.Implementation{Name: "tincan-inbox-owner", Version: version}, nil).Connect(context.Background(), z, nil)
		if err != nil {
			listener.Close()
			local.Close()
			return err
		}
		owner := &pluginInboxOwner{client: client, local: local, refs: map[string]inboxOwnerReference{}, port: listener.Addr().(*net.TCPAddr).Port}
		owner.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b.serveInboxOwner(owner, w, r)
		}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second}
		b.inboxOwner = owner
		go owner.server.Serve(listener)
	}
	path, err := b.path(c.Handle)
	if err != nil {
		return err
	}
	ref := inboxOwnerReference{ControllerOnly: c.Worker, Port: b.inboxOwner.port, Secret: core.ID("")}
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(b.root, ".inbox-owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path+".owner"); err != nil {
		return err
	}
	b.inboxOwner.refs[c.Handle] = ref
	b.inboxOwner.paths = append(b.inboxOwner.paths, path+".owner")
	return nil
}

func (b *pluginBroker) serveInboxOwner(owner *pluginInboxOwner, w http.ResponseWriter, r *http.Request) {
	// The random per-connection capability protects even loopback access. Browser
	// requests and redirects cannot use this as an unauthenticated local proxy.
	if r.Method != http.MethodPost || r.URL.Path != "/inbox" || r.Header.Get("Origin") != "" {
		http.Error(w, "unsupported inbox request", http.StatusForbidden)
		return
	}
	var in mcp.CallToolParamsRaw
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&in); err != nil || !inboxOwnerTool(in.Name) {
		http.Error(w, "invalid inbox request", http.StatusBadRequest)
		return
	}
	handle := inboxOwnerHandle(in.Arguments)
	b.mu.Lock()
	ref, ok := owner.refs[handle]
	live := b.inboxes[handle] != nil && !b.stopped
	b.mu.Unlock()
	if !ok || !live || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+ref.Secret)) != 1 {
		http.Error(w, "inbox owner unavailable", http.StatusForbidden)
		return
	}
	if ref.ControllerOnly && in.Name != "inbox_requests" && in.Name != "inbox_decide" && in.Name != "inbox_policy_set" && in.Name != "inbox_plan" {
		http.Error(w, "owned worker bridge only accepts controller decisions", http.StatusForbidden)
		return
	}
	// Body reads are bounded, but the subsequent wait may last an hour.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	// This private session uses the same validated tool handlers as stdio. It
	// cannot reconnect, change bindings, or expose arbitrary remote tools.
	ctx, cancel := context.WithTimeout(r.Context(), maxInboxWait+10*time.Second)
	defer cancel()
	result, err := owner.client.CallTool(ctx, &mcp.CallToolParams{Name: in.Name, Arguments: in.Arguments})
	if err != nil {
		result = inboxOwnerError(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func forwardInboxOwner(ctx context.Context, ref inboxOwnerReference, in *mcp.CallToolParamsRaw) *mcp.CallToolResult {
	data, err := json.Marshal(mcp.CallToolParamsRaw{Name: in.Name, Arguments: in.Arguments})
	if err != nil {
		return inboxOwnerError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, maxInboxWait+10*time.Second)
	defer cancel()
	endpoint := "http://127.0.0.1:" + strconv.Itoa(ref.Port) + "/inbox"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return inboxOwnerError(err)
	}
	req.Header.Set("Authorization", "Bearer "+ref.Secret)
	req.Header.Set("Content-Type", "application/json")
	// A fresh, direct transport prevents environment proxies and automatic
	// replay on a reused connection. Never retry a possibly committed reply.
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return inboxOwnerError(errors.New("parent inbox bridge unavailable or cancelled; stop without retrying or taking over; retain any worker claim for recovery"))
	}
	defer res.Body.Close()
	var result mcp.CallToolResult
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&result) != nil {
		return inboxOwnerError(errors.New("parent inbox bridge failed; stop without retrying or taking over; retain any worker claim for recovery"))
	}
	return &result
}

func (o *pluginInboxOwner) close() {
	o.once.Do(func() {
		_ = o.server.Close()
		_ = o.client.Close()
		_ = o.local.Close()
		// The caller still owns the kernel locks, so a successor cannot have
		// published new references at these paths yet.
		for _, path := range o.paths {
			_ = os.Remove(path)
		}
	})
}
