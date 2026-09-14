package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type inboxEvent struct {
	Mentioned   bool         `json:"mentioned,omitempty"`
	JoinRequest *joinNotice  `json:"join_request,omitempty"`
	Seq         int64        `json:"seq"`
	Kind        string       `json:"kind"`
	ChannelID   string       `json:"channel_id"`
	Payload     core.Message `json:"payload"`
}
type inboxState struct {
	Revision int             `json:"revision,omitempty"`
	Version  int             `json:"version,omitempty"`
	Requests []inboxRequest  `json:"requests,omitempty"`
	Policy   *standingPolicy `json:"policy,omitempty"`
	After    int64           `json:"after"`
	Pending  *inboxEvent     `json:"pending,omitempty"`
	Reply    *string         `json:"reply,omitempty"`
	Claim    *inboxClaim     `json:"claim,omitempty"`
}
type inbox struct {
	creator         bool
	mu              sync.Mutex
	replyMu         sync.Mutex
	c               Config
	agent           string
	allowed         []string
	workspacePeers  bool
	background      bool
	streamError     string
	bridgeError     string
	streaming       bool
	path            string
	state           inboxState
	changed         chan struct{}
	updates         chan struct{}
	waiter          *inboxWaiter
	questionNotices map[string]bool
	closed          bool
	lock            *os.File
}

func openInbox(c Config, senders string) (*inbox, error) {
	return openInboxAt(c, senders, configPath(), false)
}

// Plugin connections derive peer access from the joined workspace, rather than
// requiring users to maintain a second list of agent IDs.
func openInboxAt(c Config, senders, identityPath string, workspacePeers bool) (*inbox, error) {
	allowed := []string{}
	for _, id := range strings.Split(senders, ",") {
		if id = strings.TrimSpace(id); id != "" {
			allowed = append(allowed, id)
		}
	}
	if len(allowed) == 0 && !workspacePeers {
		return nil, errors.New("set TINCAN_ALLOW_SENDERS to trusted agent IDs (comma separated) before listening")
	}
	v, err := call(c, "GET", "/me", nil)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(v)
	var identity struct {
		Agent core.Agent `json:"agent"`
	}
	if err = json.Unmarshal(b, &identity); err != nil || identity.Agent.ID == "" {
		return nil, errors.New("server returned no agent identity")
	}
	return openInboxIdentity(c, allowed, identityPath, workspacePeers, identity.Agent.ID, identity.Agent.Admin)
}

// Saved plugin identities can inspect/resolve private commitments while offline.
// Network authentication still applies when the stream or outbox reconnects.
func openInboxIdentity(c Config, allowed []string, identityPath string, workspacePeers bool, agent string, admin bool) (*inbox, error) {
	if agent == "" {
		return nil, errors.New("saved agent identity required")
	}
	var err error
	sum := sha256.Sum256([]byte(c.Server + "\n" + agent))
	path := filepath.Join(filepath.Dir(identityPath), fmt.Sprintf("inbox-%x.json", sum[:12]))
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	// Kernel locks release automatically even after a crash. The lock file stays
	// in place to avoid racing consumers that already opened the same inode.
	lock, err := lockInbox(path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("inbox already in use or unavailable (%s); stop the other consumer before restarting: %w", path, err)
	}
	i := &inbox{creator: admin, c: c, agent: agent, allowed: allowed, workspacePeers: workspacePeers, path: path, changed: make(chan struct{}, 1), lock: lock}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &i.state)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		i.close()
		return nil, err
	}
	if len(b) > 0 && i.state.Version < 2 {
		backup, backupErr := os.OpenFile(path+".v1.bak", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if backupErr == nil {
			_, backupErr = backup.Write(b)
			if backupErr == nil {
				backupErr = backup.Sync()
			}
			closeErr := backup.Close()
			if backupErr == nil {
				backupErr = closeErr
			}
		}
		if backupErr != nil && !errors.Is(backupErr, os.ErrExist) {
			i.close()
			return nil, backupErr
		}
	}
	if err = i.state.migrate(); err != nil {
		i.close()
		return nil, err
	}
	for n := range i.state.Requests {
		e := &i.state.Requests[n].Event
		e.Mentioned = e.Kind == "message" && slices.Contains(e.Payload.Mentions, agent)
	}
	if c.CryptoPath != "" {
		for n := range i.state.Requests {
			i.state.Requests[n].Event.Payload.AgentName = i.state.Requests[n].Event.Payload.AgentID
		}
	}
	recovered := false
	for n := range i.state.Requests {
		r := &i.state.Requests[n]
		if r.Status == "running" {
			r.Status = "needs_recovery"
			recovered = true
		}
	}
	i.state.project()
	if recovered {
		if err := i.save(i.state.copy()); err != nil {
			i.close()
			return nil, err
		}
	}
	return i, nil
}
func (i *inbox) close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.closed {
		i.closed = true
		i.signalUpdate()
		if i.lock != nil {
			unlockInbox(i.lock)
		}
	}
}
func (i *inbox) save(s inboxState) error {
	if err := s.migrate(); err != nil {
		return err
	}
	s.Pending, s.Reply = nil, nil
	// Old readers reject a claim without a pending event instead of silently
	// dropping v2 commitments on their next legacy save. This is not a capability.
	s.Claim = &inboxClaim{WorkerID: "controller-v2-required", Token: "format-guard-not-a-claim"}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(i.path), ".inbox-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), i.path); err != nil {
		return err
	}
	s.project()
	i.state = s
	i.signalUpdate()
	return nil
}
func (i *inbox) accepts(e inboxEvent) bool {
	if e.Kind == "join_requested" {
		return i.creator && e.JoinRequest != nil && e.JoinRequest.RequestID != ""
	}
	if e.Kind != "message" || e.Payload.ID == "" || e.Payload.AgentID == i.agent || (!i.workspacePeers && !slices.Contains(i.allowed, e.Payload.AgentID)) {
		return false
	}
	var meta map[string]json.RawMessage
	_ = json.Unmarshal(e.Payload.Metadata, &meta)
	// Replies from this workflow never start another automated turn, even if mentioned.
	_, automated := meta["tincan_listener"]
	return !automated
}
func (i *inbox) next(ctx context.Context) (*inboxEvent, error) {
	i.mu.Lock()
	saved := i.state.copy()
	i.mu.Unlock()
	for _, r := range saved.Requests {
		if r.Reply != nil {
			_, _ = i.deliverReply(r.Event.Seq)
		}
	}
	i.mu.Lock()
	if err := i.state.migrate(); err != nil {
		i.mu.Unlock()
		return nil, err
	}
	// Revalidate eligibility after an operator changes the sender allowlist.
	s := i.state.copy()
	changed := false
	for n := range s.Requests {
		r := &s.Requests[n]
		if r.Status == "ready" && !i.accepts(r.Event) {
			r.Status = "cancelled"
			changed = true
		}
	}
	if changed {
		if err := i.save(s); err != nil {
			i.mu.Unlock()
			return nil, err
		}
	}
	if r := i.state.runnable(); r != nil {
		e := r.Event
		i.mu.Unlock()
		return &e, nil
	}
	if i.background {
		i.state.project()
		e := i.state.Pending
		i.mu.Unlock()
		return e, nil
	}
	after := i.state.After
	i.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, "GET", i.c.Server+fmt.Sprintf("/api/v1/events?after=%d", after), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.Token)
	if err = validateServer(i.c.Server); err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("inbox HTTP %d", res.StatusCode)
	}
	scan := bufio.NewScanner(res.Body)
	scan.Buffer(make([]byte, 4096), 2*1024*1024)
	for scan.Scan() {
		if !strings.HasPrefix(scan.Text(), "data: ") {
			continue
		}
		var e inboxEvent
		if err = json.Unmarshal([]byte(strings.TrimPrefix(scan.Text(), "data: ")), &e); err != nil {
			return nil, err
		}
		valid, err := decryptInboxEvent(ctx, i.c, &e)
		if err != nil {
			return nil, err
		}
		e.Mentioned = e.Kind == "message" && slices.Contains(e.Payload.Mentions, i.agent)
		accepted, err := i.ingest(e, valid)
		if err != nil {
			return nil, err
		}
		if accepted {
			return &e, nil
		}
	}
	if err = scan.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
func (i *inbox) ack(seq int64, claim ...string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	r := i.state.request(seq)
	if r != nil {
		if err := checkRequestClaim(r, claim); err != nil {
			return err
		}
		if r.Reply != nil {
			return errors.New("reply delivery is pending; retry inbox_reply before acknowledging")
		}
	}
	return i.ackLocked(seq)
}
func (i *inbox) ackLocked(seq int64) error {
	s := i.state.copy()
	r := s.request(seq)
	if r == nil {
		return errors.New("no matching pending event")
	}
	if r.Status == "completed" || r.Status == "declined" {
		return nil
	}
	if r.Claim != nil {
		r.LastClaim = r.Claim.Token
	}
	r.Status, r.Claim, r.Reply = "completed", nil, nil
	r.DeliveryError = ""
	if r.Decision == "decline" {
		r.Status = "declined"
	}
	s.Revision++
	if err := i.save(s); err != nil {
		return err
	}
	select {
	case i.changed <- struct{}{}:
	default:
	}
	return nil
}
func (i *inbox) reply(seq int64, text string, claim ...string) (any, error) {
	i.mu.Lock()
	r := i.state.request(seq)
	if r == nil {
		i.mu.Unlock()
		return nil, errors.New("no matching pending event")
	}
	if r.Status == "completed" {
		i.mu.Unlock()
		return map[string]any{"acknowledged": seq}, nil
	}
	if err := checkRequestClaim(r, claim); err != nil {
		i.mu.Unlock()
		return nil, err
	}
	if r.Event.Kind == "join_requested" {
		i.mu.Unlock()
		return nil, errors.New("use join_request_decide to approve or deny; inbox_ack dismisses this notification without granting access")
	}
	if r.Reply == nil {
		if strings.TrimSpace(text) == "" || len(text) > 65536 {
			i.mu.Unlock()
			return nil, errors.New("reply must contain 1 to 65536 bytes")
		}
		s := i.state.copy()
		r = s.request(seq)
		r.Reply = &text
		r.Status = "reply_pending"
		if err := i.save(s); err != nil {
			i.mu.Unlock()
			return nil, err
		}
	}
	i.mu.Unlock()
	return i.deliverReply(seq)
}

// A separate outbox lock serializes delivery without blocking event ingestion.
func (i *inbox) deliverReply(seq int64) (any, error) {
	return i.deliverReplyContext(context.Background(), seq)
}
func (i *inbox) deliverReplyContext(ctx context.Context, seq int64) (any, error) {
	i.replyMu.Lock()
	defer i.replyMu.Unlock()
	i.mu.Lock()
	r := i.state.request(seq)
	if r == nil || r.Reply == nil {
		i.mu.Unlock()
		return nil, nil
	}
	e, text := r.Event, *r.Reply
	i.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	v, err := callContext(ctx, i.c, "POST", "/messages", core.SendInput{ChannelID: e.ChannelID, Text: text, ReplyTo: &e.Payload.ID, Metadata: json.RawMessage(`{"tincan_listener":true}`), IdempotencyKey: fmt.Sprintf("listen_%s_%d", i.agent, e.Seq)})
	if err != nil {
		i.mu.Lock()
		s := i.state.copy()
		r := s.request(seq)
		if r != nil && r.Reply != nil && r.DeliveryError == "" {
			r.DeliveryError = "Saved reply delivery failed; automatic idempotent retry is pending"
			_ = i.save(s)
		}
		i.mu.Unlock()
		return nil, err
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return v, i.ackLocked(seq)
}

// Legacy internal callers hold mu. Preserve lock ordering with deliverReply.
func (i *inbox) finishReplyFor(seq int64) (any, error) {
	i.mu.Unlock()
	v, err := i.deliverReply(seq)
	i.mu.Lock()
	return v, err
}
func (i *inbox) finishReply() (any, error) {
	if i.state.Pending == nil {
		return nil, errors.New("no saved reply")
	}
	return i.finishReplyFor(i.state.Pending.Seq)
}
func (i *inbox) waitNext(ctx context.Context) (*inboxEvent, error) {
	for {
		poll, cancel := context.WithTimeout(ctx, 25*time.Second)
		e, err := i.next(poll)
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err == nil {
			return e, nil
		}
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
}
