package main

import (
	"encoding/json"
	"testing"
)

func TestInboxWirePreservesExtremeMetadata(t *testing.T) {
	raw := json.RawMessage(`{"large":1e1000,"precise":9007199254740993}`)
	out, err := inboxWireObject(map[string]any{"payload": map[string]any{"metadata": raw}})
	if err != nil {
		t.Fatal(err)
	}
	payload := out["payload"].(map[string]any)
	if payload["metadata"] != nil || payload["metadata_json"] != string(raw) {
		t.Fatal(payload)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal("MCP client cannot decode fallback", err)
	}
}
