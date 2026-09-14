package main

import (
	"bytes"
	"encoding/json"
)

// MCP clients commonly decode numbers as float64. Preserve metadata that exceeds
// that range as exact JSON text instead of making the entire request unreadable.
func inboxWireObject(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err = decoder.Decode(&out); err != nil {
		return nil, err
	}
	var visit func(any)
	visit = func(value any) {
		switch node := value.(type) {
		case map[string]any:
			if metadata, exists := node["metadata"]; exists {
				raw, marshalErr := json.Marshal(metadata)
				var compatible any
				if marshalErr == nil && json.Unmarshal(raw, &compatible) != nil {
					node["metadata"] = nil
					node["metadata_json"] = string(raw)
				}
			}
			for _, child := range node {
				visit(child)
			}
		case []any:
			for _, child := range node {
				visit(child)
			}
		}
	}
	visit(out)
	return out, nil
}
