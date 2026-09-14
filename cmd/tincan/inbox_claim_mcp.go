package main

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func addClaimTools(server *mcp.Server, resolve func(string) (*inbox, error)) {
	addRequestTools(server, resolve)
	type claimInput struct {
		Connection string `json:"connection,omitempty" jsonschema:"Private parent connection for plugin tools; omit for the standalone bridge"`
		Seq        int64  `json:"seq"`
		WorkerID   string `json:"worker_id" jsonschema:"Unique delegated worker ID; reuse only for this same worker's retry"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "inbox_claim", Description: "Claim one pending message inside its background worker before acting. Returns its body and a private claim token. If acquired=false, exit without acting. Claims survive restarts and never expire automatically."}, func(_ context.Context, _ *mcp.CallToolRequest, in claimInput) (*mcp.CallToolResult, map[string]any, error) {
		i, err := resolve(in.Connection)
		if err != nil {
			return nil, nil, err
		}
		v, err := i.claim(in.Seq, in.WorkerID)
		if err != nil {
			return nil, nil, err
		}
		// json.RawMessage is arbitrary JSON on the wire, not a byte array.
		// Avoid deriving a nested byte-slice schema for message metadata.
		out, err := inboxWireObject(v)
		return nil, out, err
	})
	type releaseInput struct {
		Connection string `json:"connection,omitempty"`
		Seq        int64  `json:"seq"`
		Claim      string `json:"claim"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "inbox_release", Description: "Release an unfinished request only after confirming its worker stopped. Requires its private claim. Leaves work pending; never use to race or retry an active/uncertain worker."}, func(_ context.Context, _ *mcp.CallToolRequest, in releaseInput) (*mcp.CallToolResult, map[string]any, error) {
		i, err := resolve(in.Connection)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"released": in.Seq, "pending": true}, i.release(in.Seq, in.Claim)
	})
}
