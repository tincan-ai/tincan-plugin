package main

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type inboxWaitInput struct {
	Connection  string `json:"connection" jsonschema:"Private parent connection; never reconnect inside the child"`
	WorkerID    string `json:"worker_id" jsonschema:"Unique worker ID assigned to this native child by the parent; never the parent task ID"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Maximum wait, 1 to 3600 seconds; default 900. Verify the host tool timeout exceeds this. On empty expiry or cancellation stop, never poll."`
}

func (b *pluginBroker) addWaitTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "inbox_wait", Description: "Experimental: block inside one automatic native background listener until an eligible message arrives (direct mentions first). Uses the existing SSE inbox without model polling. Returns routing metadata; claim before acting. Never call in the main conversation. Host must preserve the child and support the wait duration. An armed tool is not proof of idle wake support. Stop on timeout or cancellation. Claimed/suspended work is skipped; use inbox_requests for its recovery."}, func(ctx context.Context, _ *mcp.CallToolRequest, in inboxWaitInput) (*mcp.CallToolResult, map[string]any, error) {
		result, err := b.waitInbox(ctx, in)
		return nil, result, err
	})
}

func (b *pluginBroker) waitInbox(ctx context.Context, in inboxWaitInput) (map[string]any, error) {
	duration := defaultInboxWait
	if in.WaitSeconds != 0 {
		if in.WaitSeconds < 1 || in.WaitSeconds > int(maxInboxWait/time.Second) {
			return nil, errors.New("wait_seconds must be between 1 and 3600")
		}
		duration = time.Duration(in.WaitSeconds) * time.Second
	}
	c, err := b.load(in.Connection)
	if err != nil {
		return nil, err
	}
	bound := c.CodexThreadID
	if bound == "" && (c.HookHost == "claude" || c.HookHost == "cursor" || c.HookHost == "copilot") {
		bound = c.HookSessionID
	}
	if c.Worker || c.PendingJoin != nil || bound == "" || in.WorkerID == bound {
		return nil, errors.New("inbox_wait requires a native child of this connection's bound parent; owned workers and unbound sessions cannot use it")
	}
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil, errors.New("plugin is shutting down")
	}
	i := b.inboxes[c.Handle]
	if i == nil || !b.workers[c.Handle] {
		b.mu.Unlock()
		return nil, errors.New("the parent's background inbox is unavailable in this process; use the existing owner's inbox bridge, or resume the saved connection from the parent after updating the plugin; never take over the inbox from a child")
	}
	b.wg.Add(1)
	runCtx := b.runCtx
	b.mu.Unlock()
	defer b.wg.Done()
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(runCtx, cancel)
	defer stop()
	return i.waitMention(waitCtx, in.WorkerID, duration)
}

func (b *pluginBroker) delegatedListener(handle string) *inboxWaiter {
	b.mu.Lock()
	i, stopped := b.inboxes[handle], b.stopped
	b.mu.Unlock()
	if stopped || i == nil {
		return nil
	}
	return i.waiting()
}

func (b *pluginBroker) addListenerReadiness(view map[string]any, c *pluginConnection) {
	host := c.HookHost
	if host == "" {
		host = b.host
	}
	if host != "codex" && host != "claude" && host != "cursor" && host != "copilot" {
		return
	}
	waiter := b.delegatedListener(c.Handle)
	view["delegated_listener"] = map[string]any{
		"experimental": true, "armed": waiter != nil, "waiter": waiter,
		"host_lifetime_verified": false, "default_wait_seconds": int(defaultInboxWait / time.Second),
		"max_wait_seconds": int(maxInboxWait / time.Second),
		"required":         !c.Worker && c.PendingJoin == nil && (c.CodexThreadID != "" || c.HookSessionID != "") && view["idle_wake"] != true && waiter == nil,
		"setup":            "Automatically use tincan-listen's experimental persistent-listener workflow. Verify native background execution and a host tool timeout longer than wait_seconds. Spawn one child with the parent's model, permissions, connection, scope and listening deadline. Do not change global settings or fall back to foreground waiting. Stop on empty expiry or cancellation.",
	}
	if wake, _ := view["idle_wake"].(bool); !wake && waiter != nil {
		view["delivery"] = "delegated_listener"
	}
}
