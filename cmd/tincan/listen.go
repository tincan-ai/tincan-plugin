package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// listen is a harness-neutral adapter: one JSON event on stdin, structured worker outcome
// on stdout, diagnostics on stderr. No shell interpolation of incoming content.
func listenCommand(args []string) error {
	f := flag.NewFlagSet("listen", flag.ContinueOnError)
	c := load()
	server := f.String("server", c.Server, "Tincan server URL")
	senders := f.String("allow-senders", os.Getenv("TINCAN_ALLOW_SENDERS"), "Trusted sender agent IDs (comma separated)")
	limit := f.Int("max-events", 20, "Stop after this many completed events")
	timeout := f.Duration("timeout", 5*time.Minute, "Maximum runtime per event")
	f.Usage = func() {
		fmt.Fprintln(f.Output(), "Usage: tincan listen [flags] -- COMMAND [ARGS...]\nCOMMAND receives event JSON on stdin. Its stdout must be a JSON worker outcome; plain final text never implies completion. A failed command leaves the event pending. Each run starts a fresh child; it does not attach to an existing harness session.")
		f.PrintDefaults()
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(f.Args()) == 0 {
		return errors.New("provide a handler command after -- (see tincan listen --help)")
	}
	if *limit < 1 || *timeout <= 0 {
		return errors.New("max-events and timeout must be positive")
	}
	if c.Token == "" {
		return errors.New("run tincan connect first with this agent's TINCAN_CONFIG")
	}
	c.Server = strings.TrimRight(*server, "/")
	i, err := openInbox(c, *senders)
	if err != nil {
		return err
	}
	defer i.close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	i.mu.Lock()
	i.background = true
	i.mu.Unlock()
	streamDone := make(chan struct{})
	go func() { defer close(streamDone); i.stream(ctx, nil, nil) }()
	outboxDone := make(chan struct{})
	go func() { defer close(outboxDone); i.runOutbox(ctx) }()
	defer func() { cancel(); <-streamDone; <-outboxDone }()
	stopPresence := startPresence(ctx, c, func(context.Context) bool { return true })
	defer stopPresence()
	for n := 0; n < *limit; n++ {
		notice, err := i.waitMention(ctx, "subprocess-controller", time.Hour)
		if err != nil {
			return err
		}
		if notice["status"] == "expired" {
			n--
			continue
		}
		if notice["status"] == "decision_needed" {
			out(map[string]any{"event": "approval_needed", "commitments": i.requests()})
			n--
			continue
		}
		seq, _ := notice["event_seq"].(int64)
		i.mu.Lock()
		request := i.state.request(seq)
		var e *inboxEvent
		if request != nil {
			copy := request.Event
			e = &copy
		}
		i.mu.Unlock()
		if e == nil {
			n--
			continue
		}
		if lifecycleKind(*e) != "" {
			out(map[string]any{"event": "connection_notice", "event_seq": e.Seq, "instructions": lifecycleInstructions})
			// The parent must present and acknowledge this notice. Never send it
			// to a work handler or mark presentation complete on its behalf.
			return nil
		}
		if e.Kind == "join_requested" {
			out(map[string]any{"event": "join_request", "event_seq": e.Seq, "request": e.JoinRequest, "instructions": joinReviewInstructions})
			if err = i.ack(e.Seq); err != nil {
				return err
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "Tincan: event %d from %s (%d/%d)\n", e.Seq, e.Payload.AgentName, n+1, *limit)
		claim, err := i.claim(e.Seq, core.ID("handler_"))
		if err != nil {
			return err
		}
		if !claim.Acquired {
			continue
		}
		data, _ := json.Marshal(map[string]any{"event": e, "context": claim.Context, "policy": claim.Policy, "related_commitments": claim.Related})
		run, stop := context.WithTimeout(ctx, *timeout)
		cmd := exec.CommandContext(run, f.Args()[0], f.Args()[1:]...)
		cmd.WaitDelay = time.Second
		cmd.Stdin = bytes.NewReader(data)
		cmd.Stderr = os.Stderr
		// Forward the resolved identity without placing credentials in argv. An
		// explicit MCP inbox consumer cannot share our lock with the child.
		cmd.Env = append(os.Environ(), "TINCAN_SERVER="+c.Server, "TINCAN_TOKEN="+c.Token, "TINCAN_WAKE=")
		var reply limitedReply
		cmd.Stdout = &reply
		err = cmd.Run()
		stop()
		if err != nil {
			_, _ = i.outcome(e.Seq, claim.Claim, workerOutcome{Status: "needs_recovery", Context: err.Error()})
			return fmt.Errorf("handler failed; event %d needs recovery: %w", e.Seq, err)
		}
		if reply.overflow {
			return errors.New("handler reply exceeds 64 KB; event remains pending")
		}
		var outcome workerOutcome
		if err = json.Unmarshal(reply.Bytes(), &outcome); err != nil {
			_, _ = i.outcome(e.Seq, claim.Claim, workerOutcome{Status: "needs_recovery", Context: "handler returned no structured outcome"})
			return errors.New("handler must return a structured JSON outcome")
		}
		if _, err = i.outcome(e.Seq, claim.Claim, outcome); err != nil {
			return err
		}
		if outcome.Status != "completed" {
			out(map[string]any{"event": "needs_attention", "commitments": i.requests()})
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	fmt.Fprintln(os.Stderr, "Tincan: event limit reached; listener stopped.")
	return nil
}

type limitedReply struct {
	bytes.Buffer
	overflow bool
}

func (b *limitedReply) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 65536 - b.Len()
	if n > remaining {
		b.overflow = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
