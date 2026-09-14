package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SDK schema inference must not inspect domain structs (RawMessage is []byte).
// Keep output inference on JSON wire objects; validate richer schemas explicitly.
func TestMCPHandlersUseJSONWireOutputs(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AddTool" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "mcp" {
				return true
			}
			handler, ok := call.Args[len(call.Args)-1].(*ast.FuncLit)
			if !ok {
				t.Errorf("%s: MCP handler must expose its wire result type for review", path)
				return true
			}
			results := handler.Type.Results.List
			if len(results) != 3 {
				t.Errorf("%s: unexpected MCP handler signature", path)
				return true
			}
			output := results[1].Type
			if id, ok := output.(*ast.Ident); ok && id.Name == "any" {
				return true
			}
			if m, ok := output.(*ast.MapType); ok {
				key, kok := m.Key.(*ast.Ident)
				value, vok := m.Value.(*ast.Ident)
				if kok && vok && key.Name == "string" && value.Name == "any" {
					return true
				}
			}
			t.Errorf("%s: domain output type may infer an incorrect MCP schema; return a JSON wire object", path)
			return true
		})
	}
}

// Exercise the SDK boundary, not just inbox.claim, with arbitrary JSON metadata.
func FuzzMCPClaimMetadata(f *testing.F) {
	for _, s := range []string{`{}`, `null`, `[]`, `{"nested":[{},null,true,"text"]}`, `"text"`, `42`, `1e1000`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, metadata string) {
		if len(metadata) > 16384 || !json.Valid([]byte(metadata)) {
			t.Skip()
		}
		i := claimedInbox(t)
		i.state.Pending.Payload.Metadata = json.RawMessage(metadata)
		server := mcp.NewServer(&mcp.Implementation{Name: "contract", Version: "1"}, nil)
		addClaimTools(server, func(string) (*inbox, error) { return i, nil })
		a, b := mcp.NewInMemoryTransports()
		ctx := context.Background()
		ss, err := server.Connect(ctx, a, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer ss.Close()
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "worker", Version: "1"}, nil).Connect(ctx, b, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer cs.Close()
		claim := func() claimResult {
			r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "inbox_claim", Arguments: map[string]any{"seq": 42, "worker_id": "same-worker"}})
			if err != nil || r.IsError {
				t.Fatalf("claim response rejected: %v %v", r, err)
			}
			data, err := json.Marshal(r.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			var v claimResult
			if err = json.Unmarshal(data, &v); err != nil || !v.Acquired || v.Claim == "" {
				t.Fatalf("claim token missing: %v", err)
			}
			return v
		}
		first := claim()
		retry := claim()
		if first.Claim != retry.Claim {
			t.Fatal("lost-response retry changed ownership")
		}
		r, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "inbox_release", Arguments: map[string]any{"seq": 42, "claim": retry.Claim}})
		if err != nil || r.IsError || i.state.Claim != nil || i.state.Pending == nil {
			t.Fatal("could not release recovered claim", err)
		}
	})
}
