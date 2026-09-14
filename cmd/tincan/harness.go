package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

var harnessSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

func validateHookBinding(host, session string) error {
	if host == "" && session == "" {
		return nil
	}
	if (host != "cursor" && host != "copilot" && host != "claude") || !harnessSessionID.MatchString(session) {
		return errors.New("hook binding requires cursor, copilot, or claude and the exact session ID from this task's SessionStart hook")
	}
	return nil
}
func (b *pluginBroker) bindHook(c *pluginConnection, host, session string) error {
	if host == "" && session == "" {
		return nil
	}
	if err := validateHookBinding(host, session); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	latest, err := b.load(c.Handle)
	if err != nil {
		return err
	}
	if latest.CodexThreadID != "" || latest.Worker {
		return errors.New("connection already belongs to a different runtime")
	}
	if latest.HookSessionID != "" && (latest.HookSessionID != session || latest.HookHost != host) {
		return errors.New("connection belongs to another harness session")
	}
	latest.HookHost, latest.HookSessionID = host, session
	if err = b.save(latest); err != nil {
		return err
	}
	*c = *latest
	return nil
}

// No receipt of a lifecycle hook proves idle delivery. The hook carries only
// routing pointers, and the binding is supplied by the session, not shared MCP env.
func (b *pluginBroker) addHarnessReadiness(view map[string]any, c *pluginConnection) {
	b.addInboxBridgeReadiness(view, c.Handle)
	host := c.HookHost
	if host == "" {
		host = b.host
	}
	view["harness"] = host
	if b.notify != nil && !b.native && b.host != "codex" {
		b.mu.Lock()
		presence := b.hostPresence[c.Handle]
		stopped := b.stopped
		b.mu.Unlock()
		if !stopped && presence.Available && time.Since(presence.SeenAt) < 90*time.Second {
			view["delivery"] = "host_callback"
			view["idle_wake"] = true
		}
	}
	if c.HookSessionID != "" && !b.native && b.host != "codex" {
		var state hookState
		data, err := os.ReadFile(hookStatePath(b.root, c.HookHost+"-"+c.HookSessionID))
		if err == nil && json.Unmarshal(data, &state) == nil && time.Since(state.LastSeen) < 24*time.Hour {
			view["delivery"] = "hooks"
			view["idle_wake"] = false
		}
	}
	switch host {
	case "claude":
		view["automatic_replies_setup"] = "Enable the bundled Claude hooks in a CLI supporting asyncRewake (tested on 2.1.268), and include the SessionStart hook's exact session binding when connecting. No channel launch flag is needed for hook delivery. A waiter lasts up to 24 hours; normal session activity re-arms it after it exits. Native channels remain an optional alternative."
		if claudeWakeArmed(b.root, c) {
			view["delivery"] = "claude_async_rewake"
			view["idle_wake"] = true
		}
	case "cursor":
		view["automatic_replies_setup"] = "Bundled Cursor hooks deliver pending mentions at session start and after a completed turn. For idle replies, launch the bundled Cursor SDK adapter as a dedicated local agent; IDE hooks cannot wake an already-idle chat."
	case "copilot":
		view["automatic_replies_setup"] = "Bundled Copilot hooks deliver mentions on session start, tool completion, stop, and host notifications. Independent idle replies use the bundled Copilot SDK adapter in a separately launched runtime."
	case "openclaw", "hermes":
		view["automatic_replies_setup"] = "Enable the bundled native gateway adapter and configure its dedicated Tincan connection. The MCP-only connection cannot wake this host."
	}
	b.addListenerReadiness(view, c)
	if d, ok := view["delivery_diagnostics"].(deliveryState); ok {
		d.Method, _ = view["delivery"].(string)
		d.IdleWake, _ = view["idle_wake"].(bool)
		view["delivery_diagnostics"] = d
	}
}

func harnessHookCommand(args []string, input io.Reader, output io.Writer) error {
	result := map[string]any{}
	if len(args) < 1 || len(args) > 2 {
		return json.NewEncoder(output).Encode(result)
	}
	host := args[0]
	var raw struct {
		SessionID    string `json:"session_id"`
		SessionCamel string `json:"sessionId"`
		Conversation string `json:"conversation_id"`
		Event        string `json:"hook_event_name"`
		AgentID      string `json:"agent_id"`
		TurnID       string `json:"generation_id"`
		StopActive   bool   `json:"stop_hook_active"`
		LoopCount    int    `json:"loop_count"`
		Status       string `json:"status"`
	}
	if json.NewDecoder(io.LimitReader(input, 2*1024*1024)).Decode(&raw) != nil {
		return json.NewEncoder(output).Encode(result)
	}
	session := raw.SessionID
	if session == "" {
		session = raw.SessionCamel
	}
	if session == "" {
		session = raw.Conversation
	}
	if validateHookBinding(host, session) != nil || raw.AgentID != "" {
		return json.NewEncoder(output).Encode(result)
	}
	event := raw.Event
	if len(args) == 2 {
		event = args[1]
	}
	switch event {
	case "sessionStart":
		event = "SessionStart"
	case "postToolUse":
		event = "PostToolUse"
	case "agentStop", "stop":
		event = "Stop"
	case "notification", "Notification":
		event = "PostToolUse"
	}
	if host == "cursor" && event != "SessionStart" && event != "Stop" {
		return json.NewEncoder(output).Encode(result)
	}
	if host == "cursor" && event == "Stop" && raw.Status != "completed" {
		return json.NewEncoder(output).Encode(result)
	}
	root, err := runtimeStateDirectory(os.Getenv("TINCAN_STATE_DIR"))
	if err == nil {
		result, err = runHook(root, hookInput{Host: host, SessionID: session, Event: event, TurnID: raw.TurnID, StopActive: raw.StopActive || raw.LoopCount > 0})
		if err != nil {
			result = map[string]any{}
		}
	}
	context := ""
	if specific, ok := result["hookSpecificOutput"].(map[string]any); ok {
		context, _ = specific["additionalContext"].(string)
	}
	if reason, ok := result["reason"].(string); ok {
		context = reason
	}
	if event == "SessionStart" {
		binding, _ := json.Marshal(map[string]string{"hook_host": host, "hook_session_id": session})
		context = "When this task connects to Tincan, include these exact session binding fields in tincan_connect: " + string(binding) + ". Do not connect merely because this hook ran. " + context
	}
	result = map[string]any{}
	if context != "" {
		switch host {
		case "cursor":
			if event == "Stop" {
				result["followup_message"] = context
			} else {
				result["additional_context"] = context
			}
		case "copilot":
			if event == "Stop" {
				result["decision"] = "block"
				result["reason"] = context
			} else {
				result["additionalContext"] = context
			}
		case "claude":
			if event == "Stop" {
				result["decision"] = "block"
				result["reason"] = context
			} else {
				result["hookSpecificOutput"] = map[string]any{"hookEventName": event, "additionalContext": context}
			}
		}
	}
	return json.NewEncoder(output).Encode(result)
}

// This launcher opts in only the selected plugin. It never edits global Claude
// settings, consumes an invite, changes identity, or enables permission relay.
func claudePluginArgs(args []string) ([]string, error) {
	identity := "plugin:tincan@tincan"
	var out []string
	for len(args) > 0 {
		switch args[0] {
		case "--":
			out = append(out, args[1:]...)
			args = nil
		case "--marketplace":
			if len(args) < 2 || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(args[1]) {
				return nil, errors.New("--marketplace requires an installed marketplace name")
			}
			identity = "plugin:tincan@" + args[1]
			args = args[2:]
		case "--plugin-dir":
			if len(args) < 2 || args[1] == "" {
				return nil, errors.New("--plugin-dir requires the Tincan plugin directory")
			}
			out = append(out, args[:2]...)
			identity = "plugin:tincan@inline"
			args = args[2:]
		default:
			out = append(out, args...)
			args = nil
		}
	}
	for _, arg := range out {
		if strings.HasPrefix(arg, "--channels") || strings.HasPrefix(arg, "--dangerously-load-development-channels") {
			return nil, errors.New("launch supplies the Tincan channel flag; pass other Claude options after --")
		}
	}
	return append([]string{"--dangerously-load-development-channels", identity}, out...), nil
}
func launchCommand(args []string) error {
	if len(args) == 0 || args[0] == "--help" {
		fmt.Println("Usage: tincan launch claude [--marketplace NAME | --plugin-dir PATH] [-- Claude flags]\nStarts Claude with the installed Tincan plugin channel enabled. Resume the same conversation with -- --continue. Claude still applies channel consent and organization policy.")
		return nil
	}
	if args[0] != "claude" {
		return errors.New("launch currently supports Claude; see docs/HARNESS_DELIVERY.md for native gateway and SDK adapters")
	}
	argv, err := claudePluginArgs(args[1:])
	if err != nil {
		return err
	}
	cmd := exec.Command("claude", argv...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
