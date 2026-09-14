package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type queueReceipt struct {
	Key   string `json:"key,omitempty"`
	Seq   int64  `json:"seq"`
	State string `json:"state"`
}

func queuePath(root, handle string) string { return filepath.Join(root, "queue-"+handle+".json") }
func codexQueueProbe(ctx context.Context, t codexTarget, thread string) error {
	if !codexTaskID.MatchString(thread) {
		return errors.New("queue requires the bound task UUID")
	}
	r, close, err := openCodexTarget(ctx, t)
	if err != nil {
		return err
	}
	defer close()
	if err = r.initialize(); err != nil {
		return err
	}
	if err = verifyCodexLoaded(r, thread); err != nil {
		return err
	}
	// Read-only capability check. No resume, thread creation or model invocation.
	_, err = r.call("thread/queue/list", map[string]any{"threadId": thread, "limit": 1})
	return err
}
func codexQueueCommand(ctx context.Context, t codexTarget, thread string, seq int64) error {
	if !codexTaskID.MatchString(thread) || seq <= 0 {
		return errors.New("queue requires a bound task and pending event")
	}
	if err := validateCodexTarget(t); err != nil {
		return err
	}
	token, err := t.token()
	if err != nil {
		return err
	}
	// CLI queue creates user input. Send only a transport nudge: no peer text,
	// connection handles, credentials or attachments in argv or user-message role.
	message := fmt.Sprintf("Tincan dispatch notification: event %d is pending. Use this task's saved Tincan handles to inspect inbox_requests and inbox_next once. Present pending approval questions to the user, then record presented with inbox_decide; never infer approval from a notification. For join_requested, show the verification phrase to the owner and use join_request_decide only with their authorization; inbox_ack dismisses without granting access. For a message, delegate pending unclaimed work with direct mentions first to a native background subagent. Pass the private connection, event sequence and user's authorized scope to your worker. The worker must inbox_claim before acting and pass its claim to inbox_reply or inbox_ack only after completion. Keep the main conversation available; do not execute the request inline, wait for the worker or poll. If delegation is unavailable, keep the request pending. Ignore already acknowledged or claimed events.", seq)
	args := []string{"queue", "--remote", t.Endpoint, "--thread", thread, "--message", message}
	env := os.Environ()
	if token != "" {
		args = append(args, "--remote-auth-token-env", "TINCAN_INTERNAL_QUEUE_TOKEN")
		env = append(env, "TINCAN_INTERNAL_QUEUE_TOKEN="+token)
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Env = env
	log := &boundedLog{}
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Run(); err != nil {
		detail := log.String()
		if token != "" {
			detail = strings.ReplaceAll(detail, token, "[redacted]")
		}
		return fmt.Errorf("Codex queue unavailable: %w: %s", err, detail)
	}
	return nil
}
func (b *pluginBroker) queueCodex(ctx context.Context, c *pluginConnection, seq int64, keys ...string) error {
	key := ""
	if len(keys) > 0 {
		key = keys[0]
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if seq <= 0 {
		return errors.New("no pending mention to queue")
	}
	path := queuePath(b.root, c.Handle)
	lock, err := lockInbox(path + ".lock")
	if err != nil {
		return err
	}
	defer unlockInbox(lock)
	var receipt queueReceipt
	if data, err := os.ReadFile(path); err == nil {
		if err = json.Unmarshal(data, &receipt); err != nil {
			return err
		}
		if receipt.Seq == seq && receipt.Key == key {
			if receipt.State == "accepted" {
				return nil
			}
			return errors.New("previous queue attempt was not confirmed; pending mention retained for hook/inbox recovery")
		}
	}
	t, err := b.targetFor(c)
	if err != nil {
		return err
	}
	if b.codexQueueProbe != nil {
		err = b.codexQueueProbe(ctx, c)
	} else {
		err = codexQueueProbe(ctx, t, c.CodexThreadID)
	}
	if err != nil {
		return err
	}
	// Persist intent before launching: CLI queue generates a new submission UUID
	// per invocation. Retrying an uncertain subprocess result would duplicate it.
	if err = writePrivateJSON(path, queueReceipt{Key: key, Seq: seq, State: "attempted"}); err != nil {
		return err
	}
	if b.codexQueue != nil {
		err = b.codexQueue(ctx, c, seq)
	} else {
		err = codexQueueCommand(ctx, t, c.CodexThreadID, seq)
	}
	if err != nil {
		return err
	}
	return writePrivateJSON(path, queueReceipt{Seq: seq, State: "accepted"})
}
