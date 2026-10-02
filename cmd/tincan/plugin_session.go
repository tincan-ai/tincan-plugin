package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The mod observes the main session's saved connections and inbox snapshots.
// Discovery never opens an inbox, takes its lock, resumes a connection, or
// changes the worker's claims, acknowledgements or presentation receipts.
func (b *pluginBroker) claudeSessionConnections(session string) (map[string]any, error) {
	if b.host != "claude" {
		return nil, errors.New("session discovery requires the Claude plugin")
	}
	if err := validateHookBinding("claude", session); err != nil {
		return nil, err
	}
	connections := []map[string]any{}
	files, err := filepath.Glob(filepath.Join(b.root, "conn_*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		handle := filepath.Base(path)
		c, err := b.load(handle[:len(handle)-5])
		if err != nil || c.Worker || c.CodexThreadID != "" || c.HookHost != "claude" || c.HookSessionID != session {
			continue
		}
		mentions := []map[string]any{}
		count := 0
		inboxError := ""
		data, err := os.ReadFile(inboxPath(b.root, c))
		if err == nil {
			var state inboxState
			if json.Unmarshal(data, &state) != nil || state.migrate() != nil {
				inboxError = "Pending work could not be read. Refresh after the listener reconnects."
			} else {
				for _, request := range state.Requests {
					if request.Status == "completed" || request.Status == "cancelled" || request.Status == "declined" {
						continue
					}
					count++
					e := request.Event
					if e.Kind != "message" || (!e.Mentioned && !slices.Contains(e.Payload.Mentions, c.AgentID)) {
						continue
					}
					channel := e.ChannelID
					if channel == "" {
						channel = e.Payload.ChannelID
					}
					// Deliberately omit claim tokens, private controller context,
					// approval decisions, credentials and encryption state.
					mentions = append(mentions, map[string]any{
						"event_seq": e.Seq, "channel_id": channel,
						"message_id": e.Payload.ID, "agent_name": e.Payload.AgentName,
						"text": e.Payload.Text, "status": request.Status,
					})
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			inboxError = "Pending work is temporarily unavailable. Refresh to try again."
		}
		b.mu.Lock()
		i := b.inboxes[c.Handle]
		running := b.workers[c.Handle] && !b.stopped
		b.mu.Unlock()
		stream := "stopped"
		if i != nil {
			i.mu.Lock()
			if i.streaming {
				stream = "streaming"
			} else if running {
				stream = "starting"
			}
			if i.streamError != "" {
				stream = "reconnecting"
			}
			i.mu.Unlock()
		}
		pendingJoin := ""
		if c.PendingJoin != nil {
			pendingJoin = c.PendingJoin.Status
		}
		connections = append(connections, map[string]any{
			"connection": c.Handle, "agent_id": c.AgentID, "name": c.Name,
			"server": c.Config.Server, "room_id": c.RoomID, "room_name": c.RoomName,
			"channel_id": c.ChannelID, "background_listener": running,
			"idle_wake": claudeWakeArmed(b.root, c), "stream_state": stream,
			"pending_join": pendingJoin, "pending_count": count,
			"pending_mentions": mentions, "inbox_error": inboxError,
		})
	}
	return map[string]any{"connections": connections}, nil
}

func (b *pluginBroker) addClaudeSessionTool(server *mcp.Server) {
	type sessionInput struct {
		SessionID string `json:"hook_session_id" jsonschema:"Exact current Claude session ID from the host; never another session's ID"`
	}
	mcp.AddTool(server, &mcp.Tool{
		Name: "tincan_session", Description: "Read this Claude session's bound connections and pending mention summaries without changing inboxes or starting listeners. Used by the optional Tincan mod; pass the exact current host session ID. Never adopt another session's identity.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in sessionInput) (*mcp.CallToolResult, map[string]any, error) {
		v, err := b.claudeSessionConnections(in.SessionID)
		return nil, v, err
	})
}
