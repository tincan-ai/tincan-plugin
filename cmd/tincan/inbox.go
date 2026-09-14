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
	JoinRequest *joinNotice  `json:"join_request,omitempty"`
	Seq         int64        `json:"seq"`
	Kind        string       `json:"kind"`
	ChannelID   string       `json:"channel_id"`
	Payload     core.Message `json:"payload"`
}
type inboxState struct {
	After   int64       `json:"after"`
	Pending *inboxEvent `json:"pending,omitempty"`
	Reply   *string     `json:"reply,omitempty"`
	Claim   *inboxClaim `json:"claim,omitempty"`
}
type inbox struct {
	creator        bool
	mu             sync.Mutex
	c              Config
	agent          string
	allowed        []string
	workspacePeers bool
	background     bool
	streamError    string
	bridgeError    string
	streaming      bool
	path           string
	state          inboxState
	changed        chan struct{}
	updates        chan struct{}
	waiter         *inboxWaiter
	closed         bool
	lock           *os.File
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
	sum := sha256.Sum256([]byte(c.Server + "\n" + identity.Agent.ID))
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
	i := &inbox{creator: identity.Agent.Admin, c: c, agent: identity.Agent.ID, allowed: allowed, workspacePeers: workspacePeers, path: path, changed: make(chan struct{}, 1), lock: lock}
	b, err = os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &i.state)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		i.close()
		return nil, err
	}
	if ((i.state.Reply != nil || i.state.Claim != nil) && i.state.Pending == nil) || i.state.After < 0 || (i.state.Pending != nil && i.state.Pending.Seq <= i.state.After) {
		i.close()
		return nil, errors.New("invalid inbox cursor")
	}
	if i.state.Claim != nil && (i.state.Claim.WorkerID == "" || i.state.Claim.Token == "") {
		i.close()
		return nil, errors.New("invalid inbox worker claim")
	}
	if c.CryptoPath != "" && i.state.Pending != nil && i.state.Pending.Kind == "message" {
		// Upgrade already decrypted, durable inbox entries from older clients.
		i.state.Pending.Payload.AgentName = i.state.Pending.Payload.AgentID
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
	i.state = s
	i.signalUpdate()
	return nil
}
func (i *inbox) accepts(e inboxEvent) bool {
	if e.Kind == "join_requested" {
		return i.creator && e.JoinRequest != nil && e.JoinRequest.RequestID != ""
	}
	if e.Kind != "message" || e.Payload.ID == "" || e.Payload.AgentID == i.agent || (!i.workspacePeers && !slices.Contains(i.allowed, e.Payload.AgentID)) || !slices.Contains(e.Payload.Mentions, i.agent) {
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
	defer i.mu.Unlock()
	if i.state.Pending != nil && i.state.Reply != nil {
		if _, err := i.finishReply(); err != nil {
			return nil, err
		}
	}
	if i.state.Pending != nil {
		if i.accepts(*i.state.Pending) {
			return i.state.Pending, nil
		}
		if err := i.save(inboxState{After: i.state.Pending.Seq}); err != nil {
			return nil, err
		}
	}
	if i.background {
		return nil, nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", i.c.Server+fmt.Sprintf("/api/v1/events?after=%d", i.state.After), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+i.c.Token)
	// Validate origin before sending credentials, just as the ordinary CLI does.
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
		if e.Seq <= i.state.After {
			continue
		}
		valid, verifyErr := decryptInboxEvent(ctx, i.c, &e)
		if verifyErr != nil {
			return nil, verifyErr
		}
		if valid && i.accepts(e) {
			if err = i.save(inboxState{After: i.state.After, Pending: &e}); err != nil {
				return nil, err
			}
			return &e, nil
		}
		if err = i.save(inboxState{After: e.Seq}); err != nil {
			return nil, err
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
	if err := i.checkClaim(claim); err != nil {
		return err
	}
	if i.state.Reply != nil {
		return errors.New("reply delivery is pending; retry inbox_reply before acknowledging")
	}
	return i.ackLocked(seq)
}
func (i *inbox) ackLocked(seq int64) error {
	if i.state.Pending == nil {
		if seq == i.state.After {
			return nil
		}
		return errors.New("no matching pending event")
	}
	if i.state.Pending.Seq != seq {
		return errors.New("acknowledgement does not match pending event")
	}
	if err := i.save(inboxState{After: seq}); err != nil {
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
	defer i.mu.Unlock()
	if err := i.checkClaim(claim); err != nil {
		return nil, err
	}
	if i.state.Pending == nil || i.state.Pending.Seq != seq {
		return nil, errors.New("no matching pending event")
	}
	if i.state.Pending.Kind == "join_requested" {
		return nil, errors.New("use join_request_decide to approve or deny; inbox_ack dismisses this notification without granting access")
	}
	if i.state.Reply == nil {
		if strings.TrimSpace(text) == "" || len(text) > 65536 {
			return nil, errors.New("reply must contain 1 to 65536 bytes")
		}
		s := i.state
		s.Reply = &text
		if err := i.save(s); err != nil {
			return nil, err
		}
	}
	return i.finishReply()
}

// Persist the exact reply before posting. Recovery retries this text without
// invoking the model again, including a crash between server commit and ack.
// Caller holds i.mu.
func (i *inbox) finishReply() (any, error) {
	e := i.state.Pending
	v, err := call(i.c, "POST", "/messages", core.SendInput{ChannelID: e.ChannelID, Text: *i.state.Reply, ReplyTo: &e.Payload.ID, Metadata: json.RawMessage(`{"tincan_listener":true}`), IdempotencyKey: fmt.Sprintf("listen_%s_%d", i.agent, e.Seq)})
	if err != nil {
		return nil, err
	}
	return v, i.ackLocked(e.Seq)
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
