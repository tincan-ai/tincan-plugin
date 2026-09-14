package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/httpapi"
)

// sidecar serves newline-delimited JSON. Responses and pushed events share a
// serialized writer so a mention can arrive while a command is executing.
func sidecar(ctx context.Context, b *pluginBroker, remoteTools []*mcp.Tool, input io.Reader, output io.Writer) error {
	var mu sync.Mutex
	emit := func(v any) error { mu.Lock(); defer mu.Unlock(); return json.NewEncoder(output).Encode(v) }
	b.notify = func(ctx context.Context, p map[string]any) error {
		return emit(map[string]any{"event": p["kind"], "data": p})
	}
	server := b.serverWithTools(remoteTools)
	left, right := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, left, nil)
	if err != nil {
		return err
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "tincan-sidecar", Version: "1"}, nil)
	cs, err := client.Connect(ctx, right, nil)
	if err != nil {
		return err
	}
	defer cs.Close()
	if err = emit(map[string]any{"event": "ready", "protocol": "tincan/1", "instructions": core.AgentInstructions + "\n" + pluginInstructions, "execution": inboundExecution(), "capabilities": map[string]bool{"end_to_end_encryption": true, "forward_secrecy": true, "encrypted_history_recovery": true, "push_mentions": true, "durable_inbox": true, "host_enqueue_required": true, "background_worker_required": true, "worker_claims": true, "commitments": true, "structured_outcomes": true, "approval_queue": true, "presence_heartbeat": true, "host_status_required": true}}); err != nil {
		return err
	}
	scan := bufio.NewScanner(input)
	scan.Buffer(make([]byte, 4096), 2*1024*1024)
	for scan.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		err := json.Unmarshal(scan.Bytes(), &req)
		if err != nil || len(req.ID) == 0 || string(req.ID) == "null" {
			if err = emit(map[string]any{"id": nil, "error": "request requires id, method, and optional params object"}); err != nil {
				return err
			}
			continue
		}
		respond := func(result any, err error) error {
			if err != nil {
				return emit(map[string]any{"id": req.ID, "error": err.Error()})
			}
			return emit(map[string]any{"id": req.ID, "result": result})
		}
		if req.Method == "host_status" {
			handle, _ := req.Params["connection"].(string)
			available, ok := req.Params["available"].(bool)
			err := errors.New("host_status requires connection and boolean available")
			if ok {
				err = b.hostStatus(handle, available)
			}
			if err = respond(map[string]any{"ok": err == nil, "expires_in_seconds": core.PresenceTTL.Seconds()}, err); err != nil {
				return err
			}
			continue
		}
		if req.Method == "tools" {
			list, err := cs.ListTools(ctx, nil)
			if err = respond(list, err); err != nil {
				return err
			}
			continue
		}
		aliases := map[string]string{"connect": "tincan_connect", "status": "tincan_status", "pending": "inbox_next", "claim": "inbox_claim", "release": "inbox_release", "ack": "inbox_ack", "reply": "inbox_reply", "requests": "inbox_requests", "plan": "inbox_plan", "outcome": "inbox_outcome", "decide": "inbox_decide", "policy_set": "inbox_policy_set"}
		method := req.Method
		if alias := aliases[method]; alias != "" {
			method = alias
		}
		if req.Params == nil {
			req.Params = map[string]any{}
		}
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: method, Arguments: req.Params})
		var value any
		if err == nil {
			value = result.StructuredContent
			if value == nil && len(result.Content) > 0 {
				if text, ok := result.Content[0].(*mcp.TextContent); ok {
					if json.Unmarshal([]byte(text.Text), &value) != nil {
						value = text.Text
					}
				}
			}
			if result.IsError {
				data, _ := json.Marshal(value)
				err = errors.New(string(data))
			}
		}
		if err = respond(value, err); err != nil {
			return err
		}
	}
	return scan.Err()
}

func sidecarCommand(args []string) error {
	b, err := runtimeBroker("sidecar", args)
	if err != nil {
		return err
	}
	defer b.close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Unblock stdin on termination. Closing the host's pipe also ends the sidecar.
	go func() { <-ctx.Done(); os.Stdin.Close() }()
	return sidecar(ctx, b, httpapi.MCPTools(), os.Stdin, os.Stdout)
}
