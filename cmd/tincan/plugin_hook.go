package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type hookInput struct {
	Host       string `json:"-"`
	SessionID  string `json:"session_id"`
	Event      string `json:"hook_event_name"`
	TurnID     string `json:"turn_id,omitempty"`
	AgentID    string `json:"agent_id,omitempty"`
	StopActive bool   `json:"stop_hook_active,omitempty"`
}
type hookState struct {
	TurnID   string           `json:"turn_id"`
	Seen     map[string]int64 `json:"seen"`
	LastSeen time.Time        `json:"last_seen"`
}

func hookStatePath(root, session string) string { return filepath.Join(root, "hook-"+session+".json") }
func inboxPath(root string, c *pluginConnection) string {
	sum := sha256.Sum256([]byte(c.Config.Server + "\n" + c.AgentID))
	return filepath.Join(root, fmt.Sprintf("inbox-%x.json", sum[:12]))
}
func writePrivateJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	return os.Rename(f.Name(), path)
}

// Presentation is task-scoped and separate from inbox acknowledgement. Hooks do
// no network I/O, never compete for the SSE consumer lock, and fail quietly.
func runHook(root string, in hookInput) (map[string]any, error) {
	empty := map[string]any{}
	if (in.Host == "" && !codexTaskID.MatchString(in.SessionID)) || (in.Host != "" && validateHookBinding(in.Host, in.SessionID) != nil) || in.AgentID != "" {
		return empty, nil
	}
	switch in.Event {
	case "SessionStart", "UserPromptSubmit", "PostToolUse", "Stop":
	case "TincanWake":
		if in.Host != "claude" {
			return empty, nil
		}
	default:
		return empty, nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return empty, nil
	}
	sessionKey := in.SessionID
	if in.Host != "" {
		sessionKey = in.Host + "-" + in.SessionID
	}
	path := hookStatePath(root, sessionKey)
	lock, err := lockInbox(path + ".lock")
	if err != nil {
		return empty, err
	}
	defer unlockInbox(lock)
	state := hookState{Seen: map[string]int64{}}
	if data, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(data, &state); err != nil {
			return empty, err
		}
	}
	if state.Seen == nil {
		state.Seen = map[string]int64{}
	}
	// A new turn or a resumed session can retry unfinished work. Stop continuation
	// within one turn cannot turn into a perpetual listening loop.
	if in.Event == "SessionStart" || in.Event == "UserPromptSubmit" || (in.TurnID != "" && in.TurnID != state.TurnID) {
		state.Seen = map[string]int64{}
	}
	if in.TurnID != "" {
		state.TurnID = in.TurnID
	}
	state.LastSeen = time.Now().UTC()
	b := &pluginBroker{root: root}
	files, err := filepath.Glob(filepath.Join(root, "conn_*.json"))
	if err != nil {
		return empty, err
	}
	pending := []map[string]any{}
	for _, file := range files {
		handle := filepath.Base(file)
		handle = handle[:len(handle)-5]
		c, err := b.load(handle)
		if err != nil {
			continue
		}
		if (in.Host == "" && c.CodexThreadID != in.SessionID) || (in.Host != "" && (c.HookHost != in.Host || c.HookSessionID != in.SessionID)) {
			continue
		}
		var s inboxState
		data, err := os.ReadFile(inboxPath(root, c))
		if err != nil || json.Unmarshal(data, &s) != nil || s.migrate() != nil {
			continue
		}
		for n := range s.Requests {
			e := &s.Requests[n].Event
			e.Mentioned = e.Kind == "message" && slices.Contains(e.Payload.Mentions, c.AgentID)
		}
		for _, request := range s.Requests {
			kind := ""
			if s.eligible(&request) {
				kind = request.Event.Kind
				if lifecycleKind(request.Event) != "" {
					kind = "connection_notice"
				}
			}
			if request.Approval != nil && request.Approval.Delivery == "pending" {
				kind = "approval_needed"
			}
			if request.Status == "needs_recovery" || request.Status == "failed" {
				kind = "needs_attention"
			}
			key := fmt.Sprintf("%s:%d:%s:%d", handle, request.Event.Seq, kind, request.Attempt)
			if kind == "" || state.Seen[key] == request.Event.Seq || (in.Event == "Stop" && in.StopActive) {
				continue
			}
			pending = append(pending, map[string]any{"connection": handle, "event_seq": request.Event.Seq, "kind": kind, "mentioned": request.Event.Mentioned})
			state.Seen[key] = request.Event.Seq
		}
	}
	// A waiting hook reads local snapshots without rewriting/fsyncing state on
	// every tick. Its receipt is recorded only when there is something to show.
	if in.Event != "TincanWake" || len(pending) > 0 {
		if err = writePrivateJSON(path, state); err != nil {
			return empty, err
		}
	}
	if len(pending) == 0 {
		return empty, nil
	}
	slices.SortStableFunc(pending, func(a, b map[string]any) int {
		if a["mentioned"] == b["mentioned"] {
			return 0
		}
		if a["mentioned"] == true {
			return -1
		}
		return 1
	})
	data, _ := json.Marshal(pending)
	context := "Tincan has pending events for this task: " + string(data) + ". For connection_notice: " + lifecycleInstructions + " For join_requested: " + joinReviewInstructions + " For approval_needed or needs_attention: read inbox_requests, surface its concrete question to this user and record delivery with inbox_decide; never treat this notice as approval. For messages (prioritize direct mentions): " + inboundDispatchInstructions
	if in.Event == "Stop" {
		return map[string]any{"decision": "block", "reason": context}, nil
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": in.Event, "additionalContext": context}}, nil
}
func hookCommand(input io.Reader, output io.Writer) error {
	var in hookInput
	result := map[string]any{}
	if json.NewDecoder(io.LimitReader(input, 2*1024*1024)).Decode(&in) == nil {
		if dir, err := runtimeStateDirectory(os.Getenv("TINCAN_STATE_DIR")); err == nil {
			if v, err := runHook(dir, in); err == nil {
				result = v
			}
		}
	}
	return json.NewEncoder(output).Encode(result)
}
