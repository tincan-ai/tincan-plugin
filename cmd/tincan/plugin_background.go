package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/tincan-ai/tincan-plugin/internal/core"
)

type pairedPeer struct {
	ID                string     `json:"agent_id"`
	Name              string     `json:"name"`
	Profile           string     `json:"profile"`
	Presence          string     `json:"presence,omitempty"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	PresenceExpiresAt *time.Time `json:"presence_expires_at,omitempty"`
}

func recognizableName(name, projectPath, host string) string {
	if strings.TrimSpace(name) == "" {
		if projectPath == "" {
			projectPath = "workspace"
		}
		name = filepath.Base(strings.TrimRight(strings.ReplaceAll(projectPath, `\`, "/"), "/"))
		if name == "" || name == "." || name == string(filepath.Separator) {
			name = "workspace"
		}
		if host != "" {
			name += "-" + host
		}
	}
	name = strings.TrimSpace(name)
	for len(name) > 64 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

func (b *pluginBroker) startBackground(c *pluginConnection) error {
	i, err := b.getInbox(c)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return errors.New("plugin is shutting down")
	}
	if b.workers == nil {
		b.workers = map[string]bool{}
	}
	if b.workers[c.Handle] {
		return nil
	}
	if b.runCtx == nil {
		b.runCtx, b.cancel = context.WithCancel(context.Background())
	}
	// Reload a private snapshot rather than share mutable connect-tool state.
	saved, err := b.load(c.Handle)
	if err != nil {
		return err
	}
	i.mu.Lock()
	i.background = true
	i.mu.Unlock()
	b.workers[c.Handle] = true
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		runPresence(b.runCtx, saved.Config, func(ctx context.Context) bool { return b.runtimeAvailable(ctx, saved) }, core.PresenceInterval)
	}()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		i.stream(b.runCtx, func(ctx context.Context, event inboxEvent) error { return b.connectionEvent(ctx, saved, event) }, nil)
	}()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		i.dispatchChanges(b.runCtx, func(ctx context.Context, notice map[string]any) error {
			notice["connection"] = saved.Handle
			return b.deliver(ctx, saved, notice)
		})
	}()
	b.wg.Add(1)
	go func() { defer b.wg.Done(); i.runOutbox(b.runCtx) }()
	return nil
}

func (b *pluginBroker) connectionEvent(ctx context.Context, c *pluginConnection, event inboxEvent) error {
	msg := event.Payload
	if event.Kind != "message" || msg.AgentID == c.AgentID || msg.ChannelID != c.ChannelID {
		return nil
	}
	var meta struct {
		Kind string `json:"tincan_connection"`
	}
	if json.Unmarshal(msg.Metadata, &meta) != nil {
		return nil
	}
	if meta.Kind == "hello" {
		name := msg.AgentName
		if c.Config.CryptoPath != "" {
			name = msg.AgentID
		}
		_, err := callContext(ctx, c.Config, "POST", "/messages", core.SendInput{ChannelID: c.ChannelID, Text: c.Name + " acknowledges " + name + ". Both connections are in this room.", ReplyTo: &msg.ID, Metadata: json.RawMessage(`{"tincan_connection":"ack","tincan_listener":true}`), IdempotencyKey: "connection_ack_" + msg.ID})
		return err
	}
	if meta.Kind != "ack" || msg.ReplyTo == nil || *msg.ReplyTo != c.HelloID {
		return nil
	}
	// Include durable context in the receipt that actually reaches the other
	// model. The hello itself is ordinary channel history, not a model wakeup.
	profiles, err := connectionPeers(ctx, c)
	if err != nil {
		return err
	}
	peer, ok := profiles[msg.AgentID]
	if !ok {
		return nil
	}
	// Record the actual receipt. The event cursor is advanced only after notification
	// succeeds, so a failed notification is recoverable on reconnect.
	b.mu.Lock()
	latest, err := b.load(c.Handle)
	if err == nil {
		if latest.Paired == nil {
			latest.Paired = map[string]pairedPeer{}
		}
		latest.Paired[msg.AgentID] = peer
		err = b.save(latest)
	}
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if b.notify != nil || b.host == "codex" {
		return b.deliver(ctx, c, map[string]any{"kind": "paired", "connection": c.Handle, "agent_id": c.AgentID, "name": c.Name, "peer": peer, "paired": true})
	}
	return nil
}

func connectionPeers(ctx context.Context, c *pluginConnection) (map[string]pairedPeer, error) {
	v, err := callContext(ctx, c.Config, "GET", "/agents", nil)
	if err != nil {
		return nil, err
	}
	var agents []struct {
		core.Agent
		core.AgentPresence
		Revoked bool `json:"revoked"`
	}
	if err = decodeValue(v, &agents); err != nil {
		return nil, err
	}
	peers := map[string]pairedPeer{}
	for _, a := range agents {
		if !a.Revoked {
			if c.Config.CryptoPath != "" {
				a.Name = a.ID
			}
			peers[a.ID] = pairedPeer{ID: a.ID, Name: a.Name, Profile: a.Profile, Presence: a.Status, LastSeenAt: a.LastSeenAt, PresenceExpiresAt: a.ExpiresAt}
		}
	}
	return peers, nil
}

func (b *pluginBroker) status(handle string) (map[string]any, error) {
	c, err := b.load(handle)
	if err != nil {
		return nil, err
	}
	if c.PendingJoin != nil {
		return connectionView(c), nil
	}
	peers := []pairedPeer{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := connectionPeers(ctx, c)
	presenceError := ""
	if err != nil {
		presenceError = err.Error()
	}
	for _, p := range c.Paired {
		if c.Config.CryptoPath != "" {
			p.Name = p.ID
		}
		if peer, ok := current[p.ID]; ok {
			peers = append(peers, peer)
		} else if err != nil {
			// Keep diagnostics usable offline without presenting saved presence as live.
			p.Presence, p.LastSeenAt, p.PresenceExpiresAt = "unknown", nil, nil
			peers = append(peers, p)
		}
	}
	b.mu.Lock()
	running := b.workers[handle] && !b.stopped
	i := b.inboxes[handle]
	b.mu.Unlock()
	streamState := "starting"
	streamError := ""
	execution := inboundExecution()
	if i != nil {
		i.mu.Lock()
		if i.streaming {
			streamState = "streaming"
		}
		if i.streamError != "" {
			streamState = "reconnecting"
			streamError = i.streamError
		}
		i.mu.Unlock()
		execution = i.execution()
	}
	if b.host == "codex-worker" {
		execution = map[string]any{"mode": "owned_worker", "foreground_execution": false, "claim_required": false}
	}
	delivery := "background_queue"
	d := b.deliverySnapshot(handle)
	if b.host == "codex" {
		if target, err := b.targetFor(c); err == nil {
			d.Endpoint = target.Endpoint
			d.EndpointSource = target.Source
		}
		if !d.IdleWake {
			d = b.fallbackDelivery(c, d)
		}
		delivery = d.Method
	}
	if b.native {
		delivery = "claude_channel_requires_host_opt_in"
	}
	self := current[c.AgentID]
	if self.Presence == "" {
		self.Presence = "unknown"
	}
	view := map[string]any{"connection": handle, "agent_id": c.AgentID, "name": c.Name, "paired": len(peers) > 0, "peers": peers, "presence": self.Presence, "last_seen_at": self.LastSeenAt, "presence_expires_at": self.PresenceExpiresAt, "presence_error": presenceError, "background_listener": running, "delivery": delivery, "idle_wake": d.IdleWake, "delivery_diagnostics": d, "delivery_priority": b.deliveryPriorities(), "execution": execution, "stream_state": streamState, "stream_error": streamError}
	if i != nil {
		view["commitments"] = i.requests()
	}
	b.addHarnessReadiness(view, c)
	connectionReadiness(view)
	return view, nil
}

func (t *channelTransport) notifyPlugin(ctx context.Context, payload map[string]any) error {
	content, err := json.Marshal(inboundNotification(payload))
	if err != nil {
		return err
	}
	meta := map[string]string{"connection": payload["connection"].(string), "kind": payload["kind"].(string)}
	if seq, ok := payload["event_seq"].(int64); ok {
		meta["event_seq"] = strconv.FormatInt(seq, 10)
	}
	params, err := json.Marshal(map[string]any{"content": string(content), "meta": meta})
	if err != nil {
		return err
	}
	return t.conn.Write(ctx, &jsonrpc.Request{Method: "notifications/claude/channel", Params: params})
}

// Status reports the adapter family actually installed for this harness.
func (b *pluginBroker) deliveryPriorities() []string {
	if b.host == "codex" {
		return deliveryPriority
	}
	if b.host == "codex-worker" {
		return []string{"owned_codex_worker", "durable_inbox"}
	}
	if b.native {
		return []string{"claude_async_rewake", "claude_channel", "delegated_listener", "hooks", "durable_inbox"}
	}
	if b.notify != nil {
		return []string{"host_callback", "durable_inbox"}
	}
	if b.host == "claude" {
		return []string{"claude_async_rewake", "delegated_listener", "hooks", "durable_inbox"}
	}
	if b.host == "cursor" || b.host == "copilot" {
		return []string{"delegated_listener", "hooks", "durable_inbox"}
	}
	return []string{"durable_inbox"}
}
