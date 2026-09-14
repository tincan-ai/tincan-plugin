package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Only the private path, never keys, travels through Config or a tool response.
// The file lock protects replay state and retries across independent processes.
type cryptoState struct {
	Identity            e2ee.Identity                 `json:"identity"`
	Root                []byte                        `json:"root"`
	Server              string                        `json:"server"`
	WorkspaceID         string                        `json:"workspace_id"`
	AgentID             string                        `json:"agent_id"`
	Epoch               int64                         `json:"epoch"`
	RosterHash          string                        `json:"roster_hash"`
	Protocol            int                           `json:"protocol,omitempty"`
	KeyPackage          []byte                        `json:"key_package,omitempty"`
	MLS                 json.RawMessage               `json:"mls,omitempty"`
	PrivateMLS          json.RawMessage               `json:"private_mls,omitempty"`
	MLSEpoch            int64                         `json:"mls_epoch,omitempty"`
	SyncSeq             int64                         `json:"sync_seq,omitempty"`
	ActiveRoster        e2ee.Roster                   `json:"active_roster,omitempty"`
	PendingRoster       *pendingMLSRoster             `json:"pending_roster,omitempty"`
	Outbox              map[string]savedSend          `json:"outbox,omitempty"`
	Archive             map[string][]byte             `json:"history_archive,omitempty"`
	SearchIndex         map[string][]byte             `json:"history_search_index,omitempty"`
	Consumed            map[string]bool               `json:"consumed_capsules,omitempty"`
	Rejected            map[string]rejectedMLSMessage `json:"rejected_messages,omitempty"`
	MessagesSinceUpdate int                           `json:"messages_since_update,omitempty"`
	LastMLSUpdate       time.Time                     `json:"last_mls_update,omitempty"`
}

var cryptoMu sync.Mutex

func privateJSON(path string, v any) error {
	return privateJSONWithReplacement(path, v, replacePrivate)
}

func privateJSONWithReplacement(path string, v any, replace func(string, string) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	defer clear(b)
	if err := cleanupPrivateTemps(path, nil); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), privateTempPrefix(path)+"*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
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
	return replace(temp, path)
}
func newCrypto(path, server string, root []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := lockInbox(path + ".lock")
	if err != nil {
		return err
	}
	defer unlockInbox(lock)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return errors.New("encryption identity already exists; resume the saved connection")
	}
	i, err := e2ee.NewIdentity()
	if err != nil {
		return err
	}
	d, err := i.Device("")
	if err != nil {
		return err
	}
	if len(root) == 0 {
		root = d.SigningKey
	}
	st := cryptoState{Identity: *i, Root: root, Server: server, Protocol: 2}
	keys, err := e2ee.MLS(context.Background(), e2ee.MLSRequest{Op: "key_package", SigningKey: i.SigningKey})
	if err != nil {
		return err
	}
	st.MLS, st.KeyPackage = keys.State, keys.Data
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".history.key", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(key)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return privateJSON(path, st)
}
func withCrypto(ctx context.Context, c Config, fn func(*cryptoState) (any, error)) (any, error) {
	if c.CryptoPath == "" {
		return nil, errors.New("this connection has no local encryption keys")
	}
	cryptoMu.Lock()
	defer cryptoMu.Unlock()
	var lock *os.File
	var err error
	for {
		lock, err = lockInbox(c.CryptoPath + ".lock")
		if err == nil {
			break
		}
		if !isWakeLockBusy(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer unlockInbox(lock)
	info, err := os.Lstat(c.CryptoPath)
	if err != nil {
		return nil, errors.New("encryption state is missing; rejoin as a fresh device and import a history backup")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("encryption identity must be a private regular file (0600)")
	}
	b, err := os.ReadFile(c.CryptoPath)
	if err != nil {
		return nil, err
	}
	defer clear(b)
	var st cryptoState
	if err = json.Unmarshal(b, &st); err != nil {
		return nil, errors.New("invalid saved encryption state")
	}
	if _, err = st.Identity.Device(st.AgentID); err != nil {
		return nil, err
	}
	if st.Server != c.Server {
		return nil, errors.New("encryption identity belongs to another server")
	}
	if err := cleanupPrivateTemps(c.CryptoPath, &st.Identity); err != nil {
		return nil, err
	}
	v, err := fn(&st)
	// Persist observed membership even if a later operation fails.
	saveErr := privateJSON(c.CryptoPath, st)
	if err == nil {
		err = saveErr
	}
	return v, err
}
func cryptoJoinAddress(raw string) (string, []byte, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, err
	}
	token, pin, has := strings.Cut(u.Fragment, ".e2ee.")
	if !has {
		return raw, nil, nil
	}
	root, err := base64.RawURLEncoding.DecodeString(pin)
	if err != nil || len(root) != 32 || token == "" {
		return "", nil, errors.New("invalid encrypted invitation")
	}
	u.Fragment = token
	return u.String(), root, nil
}
func cryptoInvite(raw string, root []byte) string {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment == "" {
		return raw
	}
	token, _, _ := strings.Cut(u.Fragment, ".e2ee.")
	u.Fragment = token + ".e2ee." + base64.RawURLEncoding.EncodeToString(root)
	return u.String()
}
func cryptoConnectionInput(ctx context.Context, c Config, invite string, input map[string]any) error {
	_, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		d, err := st.device("")
		if err != nil {
			return nil, err
		}
		input["device"] = d
		if invite != "" {
			input["root"] = st.Root
			input["proof"], err = e2ee.JoinProof(st.Identity, invite, d)
		}
		return nil, err
	})
	return err
}
func ensureCrypto(ctx context.Context, c Config, st *cryptoState) error {
	var me struct {
		Agent core.Agent `json:"agent"`
	}
	v, err := rawCallContext(ctx, c, "GET", "/me", nil)
	if err != nil {
		return err
	}
	if err = decodeValue(v, &me); err != nil {
		return err
	}
	if me.Agent.EncryptionMode != "e2ee" {
		return errors.New("encrypted connection refused a plaintext workspace; no fallback")
	}
	if st.WorkspaceID != "" && (st.WorkspaceID != me.Agent.WorkspaceID || st.AgentID != me.Agent.ID) {
		return errors.New("encryption identity does not match this connection")
	}
	st.WorkspaceID, st.AgentID = me.Agent.WorkspaceID, me.Agent.ID
	var status struct {
		Epoch    int64  `json:"epoch"`
		Root     []byte `json:"root"`
		Protocol int    `json:"protocol"`
	}
	v, err = rawCallContext(ctx, c, "GET", "/e2ee/roster", nil)
	if err != nil {
		return err
	}
	if err = decodeValue(v, &status); err != nil {
		return err
	}
	if !bytes.Equal(status.Root, st.Root) {
		return errors.New("workspace encryption identity changed")
	}
	if status.Protocol != st.Protocol {
		return errors.New("workspace encryption protocol changed; no downgrade is allowed")
	}
	if st.Protocol == 2 {
		return ensureMLS(ctx, c, st, status.Epoch)
	}
	if status.Epoch == 0 {
		d, err := st.Identity.Device(st.AgentID)
		if err != nil {
			return err
		}
		if !me.Agent.Admin || !bytes.Equal(d.SigningKey, st.Root) {
			return errors.New("creator has not initialized encrypted membership")
		}
		r := e2ee.Roster{WorkspaceID: st.WorkspaceID, Epoch: 1, Members: []e2ee.Device{d}}
		if err = r.Sign(st.Identity); err != nil {
			return err
		}
		if _, err = rawCallContext(ctx, c, "POST", "/e2ee/roster", map[string]any{"roster": r}); err != nil {
			return err
		}
	}
	r, err := cryptoRoster(ctx, c, st, 0)
	if err != nil {
		return err
	}
	d, ok := r.Device(st.AgentID)
	local, _ := st.Identity.Device(st.AgentID)
	if !ok || d.Fingerprint() != local.Fingerprint() {
		return errors.New("this device is not in the trusted encryption membership")
	}
	return nil
}
func cryptoRoster(ctx context.Context, c Config, st *cryptoState, epoch int64) (e2ee.Roster, error) {
	var out struct {
		Roster e2ee.Roster `json:"roster"`
	}
	v, err := rawCallContext(ctx, c, "GET", "/e2ee/roster?epoch="+strconv.FormatInt(epoch, 10), nil)
	if err != nil {
		return out.Roster, err
	}
	if err = decodeValue(v, &out); err != nil {
		return out.Roster, err
	}
	r := out.Roster
	if err = r.Verify(st.Root, st.WorkspaceID); err != nil {
		return r, err
	}
	if epoch != 0 && r.Epoch != epoch {
		return r, errors.New("server returned the wrong historical membership")
	}
	if epoch == 0 {
		if r.Epoch < st.Epoch || (r.Epoch == st.Epoch && st.RosterHash != "" && r.Hash() != st.RosterHash) {
			return r, errors.New("encryption membership rollback detected")
		}
		st.Epoch, st.RosterHash = r.Epoch, r.Hash()
	}
	return r, nil
}
func cryptoRecipients(ctx context.Context, c Config, st *cryptoState, r e2ee.Roster, channel string) ([]e2ee.Device, error) {
	v, err := rawCallContext(ctx, c, "GET", "/channels", nil)
	if err != nil {
		return nil, err
	}
	var channels []core.Channel
	if err = decodeValue(v, &channels); err != nil {
		return nil, err
	}
	for _, ch := range channels {
		if ch.ID == channel {
			if ch.Private {
				d, ok := r.Device(st.AgentID)
				if !ok {
					return nil, errors.New("device no longer authorized")
				}
				return []e2ee.Device{d}, nil
			}
			return r.Members, nil
		}
	}
	return nil, errors.New("channel is unavailable")
}

// Private channel IDs carry the owning agent. Their encryption scope cannot
// be changed by an unauthenticated server label or a membership update.
func pinnedRecipients(ctx context.Context, c Config, st *cryptoState, r e2ee.Roster, channel string) ([]e2ee.Device, error) {
	if strings.HasPrefix(channel, "ch_private_") {
		if !strings.HasPrefix(channel, "ch_private_"+st.AgentID+"_") {
			return nil, errors.New("another agent owns this memory vault")
		}
		d, ok := r.Device(st.AgentID)
		if !ok {
			return nil, errors.New("device no longer authorized")
		}
		return []e2ee.Device{d}, nil
	}
	recipients, err := cryptoRecipients(ctx, c, st, r, channel)
	if err != nil {
		return nil, err
	}
	if len(recipients) != len(r.Members) {
		return nil, errors.New("untrusted private channel identifier")
	}
	return r.Members, nil
}

type encryptedBody struct {
	Text        string            `json:"text"`
	Metadata    json.RawMessage   `json:"metadata"`
	Attachments []core.Attachment `json:"attachments"`
}
type savedSend struct {
	Digest string         `json:"digest"`
	Input  core.SendInput `json:"input"`
}

func cryptoSend(ctx context.Context, c Config, st *cryptoState, in core.SendInput) (any, error) {
	if in.Encrypted != nil {
		return nil, errors.New("supply plaintext to the local plugin; it owns encryption")
	}
	if len(in.Text) > 65536 || len(in.Metadata) > 16384 {
		return nil, errors.New("message is too large")
	}
	if len(in.Metadata) == 0 {
		in.Metadata = json.RawMessage(`{}`)
	}
	if !json.Valid(in.Metadata) {
		return nil, errors.New("invalid message metadata")
	}
	if strings.TrimSpace(in.Text) == "" && len(in.AttachmentIDs) == 0 {
		return nil, errors.New("empty message")
	}
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = core.ID("enc_")
	}
	plain, _ := json.Marshal(in)
	sum := sha256.Sum256(plain)
	digest := hex.EncodeToString(sum[:])
	keysum := sha256.Sum256([]byte(in.IdempotencyKey))
	path := c.CryptoPath + ".outbox/" + hex.EncodeToString(keysum[:]) + ".json"
	var saved savedSend
	readSaved := func() ([]byte, error) {
		if st.Protocol == 2 {
			if value, ok := st.Outbox[in.IdempotencyKey]; ok {
				return json.Marshal(value)
			}
			return nil, os.ErrNotExist
		}
		return os.ReadFile(path)
	}
	if data, err := readSaved(); err == nil {
		if json.Unmarshal(data, &saved) != nil || saved.Digest != digest {
			return nil, errors.New("idempotency key already used for different content")
		}
		// Reuse the exact envelope across failures and restarts.
		v, err := rawCallContext(ctx, c, "POST", "/messages", saved.Input)
		if err == nil {
			return cryptoMessageValue(ctx, c, st, v)
		}
		if !strings.Contains(err.Error(), `"code":"encryption_epoch_conflict"`) {
			return nil, err
		}
		// This explicit rejection guarantees no message was committed. Rebuild
		// for the current recipients; uncertain network outcomes keep the outbox.
		if st.Protocol == 2 {
			delete(st.Outbox, in.IdempotencyKey)
		} else if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	r, err := cryptoRoster(ctx, c, st, 0)
	if err != nil {
		return nil, err
	}
	recipients, err := pinnedRecipients(ctx, c, st, r, in.ChannelID)
	if err != nil {
		return nil, err
	}
	body := encryptedBody{Text: in.Text, Metadata: in.Metadata, Attachments: []core.Attachment{}}
	for _, id := range in.AttachmentIDs {
		at, _, err := cryptoDownload(ctx, c, st, id, in.ChannelID)
		if err != nil {
			return nil, err
		}
		body.Attachments = append(body.Attachments, at)
	}
	data, _ := json.Marshal(body)
	envelope := &e2ee.Envelope{WorkspaceID: st.WorkspaceID, ChannelID: in.ChannelID, SenderID: st.AgentID, Key: in.IdempotencyKey, Mentions: in.Mentions, ReplyTo: in.ReplyTo, AttachmentIDs: in.AttachmentIDs}
	if st.Protocol == 2 {
		err = sealMLS(ctx, c, st, envelope, r, data)
	} else {
		err = envelope.Seal(st.Identity, r, recipients, data)
	}
	if err != nil {
		return nil, err
	}
	wire := in
	wire.Text = ""
	wire.Metadata = json.RawMessage(`{}`)
	wire.Encrypted = envelope
	if st.Protocol == 2 {
		if st.Outbox == nil {
			st.Outbox = map[string]savedSend{}
		}
		st.Outbox[in.IdempotencyKey] = savedSend{Digest: digest, Input: wire}
		err = privateJSON(c.CryptoPath, st)
	} else {
		err = privateJSON(path, savedSend{Digest: digest, Input: wire})
	}
	if err != nil {
		return nil, err
	}
	v, err := rawCallContext(ctx, c, "POST", "/messages", wire)
	if err != nil {
		return nil, err
	}
	return cryptoMessageValue(ctx, c, st, v)
}

var errBeforeJoin = errors.New("message predates this device's membership")

func decryptMessage(ctx context.Context, c Config, st *cryptoState, m *core.Message) error {
	e := m.Encrypted
	if e == nil {
		return errors.New("plaintext message rejected in encrypted workspace")
	}
	if e.Kind != "message" || e.WorkspaceID != st.WorkspaceID || e.ChannelID != m.ChannelID || e.SenderID != m.AgentID || e.MessageID() != m.ID || !sameStringList(e.Mentions, m.Mentions) || !equalJSON(e.ReplyTo, m.ReplyTo) {
		return errors.New("encrypted message routing was modified")
	}
	ids := []string{}
	for _, a := range m.Attachments {
		ids = append(ids, a.ID)
	}
	if !sameStringList(ids, e.AttachmentIDs) {
		return errors.New("encrypted attachments were modified")
	}
	r, err := cryptoRoster(ctx, c, st, e.Epoch)
	if err != nil {
		return err
	}
	if err = e.Verify(r); err != nil {
		return err
	}
	// Display only the identity authenticated by the roster and signature.
	// Relay-supplied names are never an identity assertion, including old history.
	m.AgentName = e.SenderID
	m.EncryptionError = ""
	if _, rejected := st.Rejected[rejectedEnvelopeID(*e)]; rejected {
		m.Text = "[Encrypted message rejected]"
		m.Metadata = json.RawMessage(`{}`)
		m.Attachments = []core.Attachment{}
		m.ReplyPreview = nil
		m.EncryptionError = e2ee.ErrMLSApplicationRejected.Error()
		return nil
	}
	if _, ok := r.Device(st.AgentID); !ok {
		if e.Version != 2 {
			return errBeforeJoin
		}
		if _, imported, err := st.archiveGet(c, *e); err != nil {
			return err
		} else if !imported {
			return errBeforeJoin
		}
	}
	var data []byte
	if e.Version == 2 {
		data, err = openMLSPayload(ctx, c, st, *e)
	} else {
		data, err = e2ee.Decrypt(st.Identity, e.Ciphertext, e2ee.MaxPlaintextBytes)
	}
	if err != nil {
		return err
	}
	var body encryptedBody
	if err = json.Unmarshal(data, &body); err != nil {
		return errors.New("invalid decrypted message")
	}
	bodyIDs := []string{}
	for _, a := range body.Attachments {
		bodyIDs = append(bodyIDs, a.ID)
	}
	if !sameStringList(bodyIDs, e.AttachmentIDs) {
		return errors.New("encrypted attachment manifest mismatch")
	}
	m.Text, m.Metadata, m.Attachments = body.Text, body.Metadata, body.Attachments
	// Server-generated previews are not authenticated content.
	m.ReplyPreview = nil
	return nil
}
func equalJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}
func sameStringList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for n := range a {
		if a[n] != b[n] {
			return false
		}
	}
	return true
}
func cryptoMessageValue(ctx context.Context, c Config, st *cryptoState, v any) (any, error) {
	var m core.Message
	if err := decodeValue(v, &m); err != nil {
		return nil, err
	}
	if err := decryptMessage(ctx, c, st, &m); err != nil {
		return nil, err
	}
	return m, nil
}
func encryptedCall(ctx context.Context, c Config, method, path string, v any) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		if method == "POST" && path == "/messages" {
			var in core.SendInput
			if err := decodeValue(v, &in); err != nil {
				return nil, err
			}
			return cryptoSend(ctx, c, st, in)
		}
		if method == "GET" && (path == "/messages" || strings.HasPrefix(path, "/messages?")) {
			return cryptoHistory(ctx, c, st, path)
		}
		out, err := rawCallContext(ctx, c, method, path, v)
		if err != nil {
			return nil, err
		}
		if method == "POST" && path == "/invites" {
			m, ok := out.(map[string]any)
			if ok {
				raw, _ := m["url"].(string)
				m["url"] = cryptoInvite(raw, st.Root)
				delete(m, "invite")
			}
		}
		return out, nil
	})
}
func cryptoHistory(ctx context.Context, c Config, st *cryptoState, path string) (any, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	query := strings.ToLower(q.Get("q"))
	q.Del("q")
	var filter any
	if raw := q.Get("metadata"); raw != "" {
		if err = json.Unmarshal([]byte(raw), &filter); err != nil {
			return nil, err
		}
		q.Del("metadata")
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q.Set("limit", "100")
	out := []core.Message{}
	for {
		v, err := rawCallContext(ctx, c, "GET", "/messages?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var page []core.Message
		if err = decodeValue(v, &page); err != nil {
			return nil, err
		}
		for _, m := range page {
			if st.Protocol == 2 && query != "" && m.Encrypted != nil {
				if filter, ok := st.SearchIndex[archiveID(*m.Encrypted)]; ok {
					key, err := historyKey(c)
					if err != nil {
						return nil, err
					}
					if !historyIndexMayMatch(filter, key, query) {
						continue
					}
				}
			}
			if err = decryptMessage(ctx, c, st, &m); errors.Is(err, errBeforeJoin) {
				continue
			} else if err != nil {
				return nil, err
			}
			if query != "" && !strings.Contains(strings.ToLower(m.Text), query) {
				continue
			}
			if filter != nil {
				var metadata any
				if json.Unmarshal(m.Metadata, &metadata) != nil || !jsonContains(metadata, filter) {
					continue
				}
			}
			out = append(out, m)
			if len(out) >= limit {
				return out, nil
			}
		}
		if len(page) < 100 {
			return out, nil
		}
		before := strconv.FormatInt(page[len(page)-1].Seq, 10)
		if before == q.Get("before") {
			return nil, errors.New("history cursor did not advance")
		}
		q.Set("before", before)
	}
}
func jsonContains(value, filter any) bool {
	switch f := filter.(type) {
	case map[string]any:
		v, ok := value.(map[string]any)
		if !ok {
			return false
		}
		for k, x := range f {
			got, exists := v[k]
			if !exists || !jsonContains(got, x) {
				return false
			}
		}
		return true
	default:
		return equalJSON(value, filter)
	}
}

type encryptedFileHeader struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
}

func cryptoUpload(ctx context.Context, c Config, st *cryptoState, channel, name string, data []byte) (any, error) {
	if len(data) > e2ee.MaxFileBytes || len(name) == 0 || len(name) > 200 || strings.ContainsAny(name, "/\\\x00") {
		return nil, errors.New("invalid attachment name or size")
	}
	r, err := cryptoRoster(ctx, c, st, 0)
	if err != nil {
		return nil, err
	}
	recipients, err := pinnedRecipients(ctx, c, st, r, channel)
	if err != nil {
		return nil, err
	}
	header := encryptedFileHeader{Name: name, MIME: http.DetectContentType(data)}
	h, _ := json.Marshal(header)
	plain := append(append(h, '\n'), data...)
	e := e2ee.Envelope{Kind: "file", WorkspaceID: st.WorkspaceID, ChannelID: channel, SenderID: st.AgentID, Key: core.ID("file_")}
	if st.Protocol == 2 {
		err = sealMLS(ctx, c, st, &e, r, plain)
	} else {
		err = e.Seal(st.Identity, r, recipients, plain)
	}
	if err != nil {
		return nil, err
	}
	if st.Protocol == 2 {
		if err = privateJSON(c.CryptoPath, st); err != nil {
			return nil, err
		}
	}
	b, _ := json.Marshal(e)
	res, err := requestPlainContext(ctx, c, "POST", "/uploads?"+url.Values{"channel_id": {channel}, "name": {"encrypted.age"}}.Encode(), bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var at core.Attachment
	if err = json.NewDecoder(res.Body).Decode(&at); err != nil {
		return nil, err
	}
	if at.ID != "blob_"+strings.TrimPrefix(e.MessageID(), "msg_") {
		return nil, errors.New("server changed encrypted attachment identity")
	}
	at.Name, at.MIME, at.Bytes = name, header.MIME, int64(len(data))
	return at, nil
}
func openCryptoFile(ctx context.Context, c Config, st *cryptoState, id, channel string, data []byte) (core.Attachment, []byte, error) {
	var e e2ee.Envelope
	if len(data) > e2ee.MaxFileEnvelopeBytes || json.Unmarshal(data, &e) != nil {
		return core.Attachment{}, nil, errors.New("invalid encrypted attachment")
	}
	if e.Kind != "file" || e.WorkspaceID != st.WorkspaceID || (channel != "" && e.ChannelID != channel) || "blob_"+strings.TrimPrefix(e.MessageID(), "msg_") != id {
		return core.Attachment{}, nil, errors.New("encrypted attachment context was modified")
	}
	r, err := cryptoRoster(ctx, c, st, e.Epoch)
	if err != nil {
		return core.Attachment{}, nil, err
	}
	if err = e.Verify(r); err != nil {
		return core.Attachment{}, nil, err
	}
	if _, ok := r.Device(st.AgentID); !ok {
		if e.Version != 2 {
			return core.Attachment{}, nil, errBeforeJoin
		}
		if _, imported, err := st.archiveGet(c, e); err != nil {
			return core.Attachment{}, nil, err
		} else if !imported {
			return core.Attachment{}, nil, errBeforeJoin
		}
	}
	var plain []byte
	if e.Version == 2 {
		plain, err = openMLSPayload(ctx, c, st, e)
	} else {
		plain, err = e2ee.Decrypt(st.Identity, e.Ciphertext, e2ee.MaxFileBytes+1024)
	}
	if err != nil {
		return core.Attachment{}, nil, err
	}
	line, content, ok := bytes.Cut(plain, []byte{'\n'})
	var header encryptedFileHeader
	if !ok || len(line) > 1024 || len(content) > e2ee.MaxFileBytes || json.Unmarshal(line, &header) != nil || header.Name == "" || strings.ContainsAny(header.Name, "/\\\x00") {
		return core.Attachment{}, nil, errors.New("invalid decrypted attachment")
	}
	return core.Attachment{ID: id, Name: header.Name, MIME: header.MIME, Bytes: int64(len(content))}, content, nil
}
func cryptoDownload(ctx context.Context, c Config, st *cryptoState, id, channel string) (core.Attachment, []byte, error) {
	if !strings.HasPrefix(id, "blob_") || strings.ContainsAny(id, "/\\?#") {
		return core.Attachment{}, nil, errors.New("invalid attachment ID")
	}
	res, err := requestPlainContext(ctx, c, "GET", "/blobs/"+id, nil)
	if err != nil {
		return core.Attachment{}, nil, err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, e2ee.MaxFileEnvelopeBytes+1))
	if err != nil {
		return core.Attachment{}, nil, err
	}
	return openCryptoFile(ctx, c, st, id, channel, data)
}
func encryptedRequest(ctx context.Context, c Config, method, path string, body io.Reader) (*http.Response, error) {
	u, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	if method == "POST" && u.Path == "/uploads" {
		data, err := io.ReadAll(io.LimitReader(body, e2ee.MaxFileBytes+1))
		if err != nil {
			return nil, err
		}
		v, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
			if err := ensureCrypto(ctx, c, st); err != nil {
				return nil, err
			}
			return cryptoUpload(ctx, c, st, u.Query().Get("channel_id"), u.Query().Get("name"), data)
		})
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(v)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b))}, nil
	}
	return requestPlainContext(ctx, c, method, path, body)
}
func encryptionManage(ctx context.Context, c Config, requestID, fingerprint, revoke string) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		if requestID == "" && revoke == "" {
			v, err := rawCallContext(ctx, c, "GET", "/e2ee/requests", nil)
			if err != nil {
				return nil, err
			}
			var requests []map[string]any
			if err = decodeValue(v, &requests); err != nil {
				return nil, err
			}
			for _, req := range requests {
				var d e2ee.Device
				if err = decodeValue(req["device"], &d); err != nil {
					return nil, err
				}
				req["fingerprint"] = d.Fingerprint()
			}
			return requests, nil
		}
		local, _ := st.device(st.AgentID)
		if !bytes.Equal(local.SigningKey, st.Root) {
			return nil, errors.New("use the workspace creator's encryption identity")
		}
		r, err := cryptoRoster(ctx, c, st, 0)
		if err != nil {
			return nil, err
		}
		next := r
		next.Members = append([]e2ee.Device(nil), r.Members...)
		next.Epoch++
		next.Previous = r.Hash()
		if requestID != "" {
			if len(fingerprint) != 64 {
				return nil, errors.New("supply the joining device's full fingerprint verified through your existing conversation")
			}
			v, err := rawCallContext(ctx, c, "GET", "/e2ee/requests", nil)
			if err != nil {
				return nil, err
			}
			var requests []struct {
				ID      string      `json:"id"`
				AgentID string      `json:"agent_id"`
				Device  e2ee.Device `json:"device"`
				Status  string      `json:"status"`
			}
			if err = decodeValue(v, &requests); err != nil {
				return nil, err
			}
			found := false
			for _, req := range requests {
				if req.ID == requestID && req.Status == "pending" {
					if req.Device.Fingerprint() != fingerprint {
						return nil, errors.New("joining device fingerprint mismatch")
					}
					req.Device.AgentID = req.AgentID
					next.Members = append(next.Members, req.Device)
					found = true
					break
				}
			}
			if !found {
				return nil, errors.New("pending encrypted request not found")
			}
		} else {
			if revoke == st.AgentID {
				return nil, errors.New("creator cannot revoke their own encryption identity")
			}
			next.Members = nil
			for _, d := range r.Members {
				if d.AgentID != revoke {
					next.Members = append(next.Members, d)
				}
			}
			if len(next.Members) == len(r.Members) {
				return nil, errors.New("encrypted member not found")
			}
		}
		if err = next.Sign(st.Identity); err != nil {
			return nil, err
		}
		if st.Protocol == 2 {
			return changeMLSRoster(ctx, c, st, r, next, requestID, revoke)
		}
		out, err := rawCallContext(ctx, c, "POST", "/e2ee/roster", map[string]any{"roster": next, "request_id": requestID})
		if err != nil {
			return nil, err
		}
		st.Epoch, st.RosterHash = next.Epoch, next.Hash()
		return out, nil
	})
}
func localToolResult(v any, err error) (*mcp.CallToolResult, error) {
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
}
func encryptedTool(ctx context.Context, c Config, name string, args map[string]any) (*mcp.CallToolResult, error) {
	str := func(k string) string { v, _ := args[k].(string); return v }
	switch name {
	case "encryption_rejections":
		v, e := encryptionRejections(ctx, c)
		return localToolResult(v, e)
	case "encryption_rotate":
		v, e := rotateMLS(ctx, c)
		return localToolResult(v, e)
	case "encryption_history_backup":
		v, e := backupHistory(ctx, c, str("out"))
		return localToolResult(v, e)
	case "encryption_history_restore":
		v, e := restoreHistory(ctx, c, str("file"), str("key_file"))
		return localToolResult(v, e)
	case "message_send":
		v, e := callContext(ctx, c, "POST", "/messages", args)
		return localToolResult(v, e)
	case "messages_search":
		q := url.Values{}
		for k, v := range args {
			if k == "connection" {
				continue
			}
			if k == "query" {
				k = "q"
			}
			if k == "metadata" {
				b, _ := json.Marshal(v)
				q.Set(k, string(b))
			} else {
				q.Set(k, fmt.Sprint(v))
			}
		}
		v, e := callContext(ctx, c, "GET", "/messages?"+q.Encode(), nil)
		return localToolResult(v, e)
	case "attachment_upload":
		data, e := base64.StdEncoding.DecodeString(str("data"))
		if e != nil {
			return localToolResult(nil, e)
		}
		v, e := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
			if e := ensureCrypto(ctx, c, st); e != nil {
				return nil, e
			}
			return cryptoUpload(ctx, c, st, str("channel_id"), str("name"), data)
		})
		return localToolResult(v, e)
	case "attachment_download":
		v, e := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
			if e := ensureCrypto(ctx, c, st); e != nil {
				return nil, e
			}
			at, data, e := cryptoDownload(ctx, c, st, str("attachment_id"), "")
			if e != nil {
				return nil, e
			}
			return map[string]any{"attachment": at, "data": base64.StdEncoding.EncodeToString(data)}, nil
		})
		return localToolResult(v, e)
	case "encryption_requests":
		v, e := encryptionManage(ctx, c, "", "", "")
		return localToolResult(v, e)
	case "encryption_approve":
		v, e := encryptionManage(ctx, c, str("request_id"), str("fingerprint"), "")
		return localToolResult(v, e)
	case "encryption_deny":
		id := str("request_id")
		if !strings.HasPrefix(id, "jr_") || strings.ContainsAny(id, "/\\?#") {
			return localToolResult(nil, errors.New("invalid request ID"))
		}
		v, e := callContext(ctx, c, "POST", "/e2ee/requests/"+id+"/deny", map[string]any{})
		return localToolResult(v, e)
	case "encryption_revoke":
		v, e := encryptionManage(ctx, c, "", "", str("agent_id"))
		return localToolResult(v, e)
	case "invite_create":
		v, e := callContext(ctx, c, "POST", "/invites", args)
		return localToolResult(v, e)
	case "data_export":
		out := str("out")
		if out == "" {
			out = c.CryptoPath + ".export.zip"
		}
		e := cryptoExport(ctx, c, out)
		return localToolResult(map[string]string{"path": out}, e)
	case "events_wait":
		session, err := remote(c)
		if err != nil {
			return nil, err
		}
		defer session.Close()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			return result, err
		}
		var events []inboxEvent
		if len(result.Content) != 1 {
			return localToolResult(nil, errors.New("invalid encrypted event response"))
		}
		content, ok := result.Content[0].(*mcp.TextContent)
		if !ok || json.Unmarshal([]byte(content.Text), &events) != nil {
			return localToolResult(nil, errors.New("invalid encrypted event response"))
		}
		out, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
			if err := ensureCrypto(ctx, c, st); err != nil {
				return nil, err
			}
			out := []inboxEvent{}
			for _, event := range events {
				if event.Kind == "message" {
					if event.ChannelID != event.Payload.ChannelID {
						return nil, errors.New("event routing was modified")
					}
					if err := decryptMessage(ctx, c, st, &event.Payload); errors.Is(err, errBeforeJoin) {
						continue
					} else if err != nil {
						return nil, err
					}
					if event.Payload.EncryptionError != "" {
						continue
					}
				}
				out = append(out, event)
			}
			return out, nil
		})
		return localToolResult(out, err)
	case "a2a_enable", "a2a_tasks", "a2a_task_update":
		return localToolResult(nil, errors.New("A2A content is unavailable in encrypted workspaces; use encrypted messages"))
	}
	session, e := remote(c)
	if e != nil {
		return nil, e
	}
	defer session.Close()
	return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
}

func cryptoExport(ctx context.Context, c Config, out string) error {
	_, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		res, err := requestPlainContext(ctx, c, "GET", "/export", nil)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		temp, err := os.CreateTemp("", "tincan-encrypted-export-*")
		if err != nil {
			return nil, err
		}
		defer os.Remove(temp.Name())
		defer temp.Close()
		n, err := io.Copy(temp, res.Body)
		if err != nil {
			return nil, err
		}
		archive, err := zip.NewReader(temp, n)
		if err != nil {
			return nil, err
		}
		output, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		complete := false
		defer func() {
			output.Close()
			if !complete {
				os.Remove(out)
			}
		}()
		z := zip.NewWriter(output)
		defer z.Close()
		locked, rejected := 0, 0
		for _, entry := range archive.File {
			if entry.Name == "EXPORT_ERROR.txt" {
				return nil, errors.New("server export was incomplete")
			}
			if strings.Contains(entry.Name, "..") || strings.HasPrefix(entry.Name, "/") || strings.Contains(entry.Name, "\\") {
				return nil, errors.New("invalid export entry")
			}
			src, err := entry.Open()
			if err != nil {
				return nil, err
			}
			if entry.Name == "messages.jsonl" {
				dest, err := z.Create(entry.Name)
				if err != nil {
					src.Close()
					return nil, err
				}
				dec := json.NewDecoder(src)
				enc := json.NewEncoder(dest)
				for {
					var m core.Message
					err = dec.Decode(&m)
					if err == io.EOF {
						break
					}
					if err != nil {
						src.Close()
						return nil, err
					}
					if err = decryptMessage(ctx, c, st, &m); errors.Is(err, errBeforeJoin) {
						locked++
					} else if err != nil {
						src.Close()
						return nil, err
					}
					if m.EncryptionError != "" {
						rejected++
					}
					if err = enc.Encode(m); err != nil {
						src.Close()
						return nil, err
					}
				}
				src.Close()
				continue
			}
			if strings.HasPrefix(entry.Name, "attachments/") {
				parts := strings.Split(entry.Name, "/")
				if len(parts) != 3 {
					src.Close()
					return nil, errors.New("invalid attachment export path")
				}
				data, err := io.ReadAll(io.LimitReader(src, e2ee.MaxFileEnvelopeBytes+1))
				src.Close()
				if err != nil {
					return nil, err
				}
				at, plain, err := openCryptoFile(ctx, c, st, parts[1], "", data)
				name := entry.Name
				if errors.Is(err, errBeforeJoin) {
					locked++
					plain = data
				} else if errors.Is(err, e2ee.ErrMLSApplicationRejected) {
					rejected++
					plain = data
				} else if err != nil {
					return nil, err
				} else {
					name = "attachments/" + at.ID + "/" + at.Name
				}
				dest, err := z.Create(name)
				if err != nil {
					return nil, err
				}
				if _, err = dest.Write(plain); err != nil {
					return nil, err
				}
				continue
			}
			dest, err := z.Create(entry.Name)
			if err != nil {
				src.Close()
				return nil, err
			}
			_, err = io.Copy(dest, src)
			src.Close()
			if err != nil {
				return nil, err
			}
		}
		manifest, err := z.Create("encryption.json")
		if err != nil {
			return nil, err
		}
		if err = json.NewEncoder(manifest).Encode(map[string]any{"mode": "e2ee", "decrypted_locally": true, "unavailable_historical_entries": locked, "rejected_entries": rejected, "note": "Entries predating this device and rejected attachments remain ciphertext. Rejected messages are marked. This export contains plaintext; store it privately."}); err != nil {
			return nil, err
		}
		if err = z.Close(); err != nil {
			return nil, err
		}
		if err = output.Sync(); err != nil {
			return nil, err
		}
		if err = output.Close(); err != nil {
			return nil, err
		}
		complete = true
		return nil, nil
	})
	return err
}
func localCryptoTools() []*mcp.Tool {
	str := func(desc string) any { return map[string]any{"type": "string", "description": desc} }
	makeTool := func(name, desc string, props map[string]any, required ...string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: desc, InputSchema: map[string]any{"type": "object", "properties": props, "required": required}}
	}
	return []*mcp.Tool{
		makeTool("encryption_rejections", "Inspect locally quarantined encrypted messages. These messages were rejected and cannot trigger workers; their signed sender IDs remain available for review and revocation.", map[string]any{}),
		makeTool("encryption_rotate", "Refresh the MLS workspace's group keys. The local creator performs the update; no key material enters this tool.", map[string]any{}),
		makeTool("encryption_history_backup", "Save an encrypted local history backup without credentials or live MLS state. Keep its history key separately in private backup storage.", map[string]any{"out": str("Private output file path; existing files are never overwritten")}, "out"),
		makeTool("encryption_history_restore", "Import an explicitly selected encrypted history backup after joining the same workspace. This restores history only; it never rolls back live encryption state.", map[string]any{"file": str("History backup path"), "key_file": str("Owner-only recovery key file path; never supply the key itself")}, "file", "key_file"),
		makeTool("encryption_requests", "List pending encryption devices. Verify the fingerprint through the existing conversation with the joining device.", map[string]any{}),
		makeTool("encryption_approve", "Approve one device only after the account owner authorizes its verified fingerprint. The local creator signs membership; private keys never enter tool arguments.", map[string]any{"request_id": str("Pending request ID"), "fingerprint": str("Full 64-character fingerprint obtained from the joining device through a trusted conversation")}, "request_id", "fingerprint"),
		makeTool("encryption_deny", "Decline one pending encryption device after the owner decides not to admit it.", map[string]any{"request_id": str("Pending request ID")}, "request_id"),
		makeTool("encryption_revoke", "Remove an encrypted member from future messages. Requires owner authorization. Previously received content cannot be recalled.", map[string]any{"agent_id": str("Agent to revoke")}, "agent_id"),
		makeTool("attachment_download", "Download and decrypt an attachment locally. Returns its original name and base64 content.", map[string]any{"attachment_id": str("Attachment ID")}, "attachment_id"),
	}
}
func isLocalCryptoTool(name string) bool {
	for _, t := range localCryptoTools() {
		if t.Name == name {
			return true
		}
	}
	return false
}
func decryptInboxEvent(ctx context.Context, c Config, event *inboxEvent) (bool, error) {
	if c.CryptoPath == "" || event.Kind != "message" {
		return true, nil
	}
	if event.ChannelID != event.Payload.ChannelID || event.Seq <= 0 {
		return false, errors.New("encrypted event routing was modified")
	}
	v, err := withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		if err := decryptMessage(ctx, c, st, &event.Payload); errors.Is(err, errBeforeJoin) {
			return false, nil
		} else if err != nil {
			return nil, err
		}
		if event.Payload.EncryptionError != "" {
			return false, nil
		}
		// A valid ciphertext replayed with a new transport sequence cannot wake a
		// worker twice. The same sequence remains retryable until acknowledged.
		path := c.CryptoPath + ".received/" + event.Payload.ID + ".json"
		var seq int64
		if b, err := os.ReadFile(path); err == nil {
			if json.Unmarshal(b, &seq) != nil {
				return nil, errors.New("corrupt encrypted replay state")
			}
			if seq != event.Seq {
				return false, nil
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		if err := privateJSON(path, event.Seq); err != nil {
			return nil, err
		}
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return v.(bool), nil
}

// Probe the mode before a keyless local client sends content. This also catches
// imported credentials and a config whose private-key reference was lost.
func requirePlainClient(ctx context.Context, c Config) error {
	if c.Token == "" {
		return nil
	}
	var me struct {
		Agent core.Agent `json:"agent"`
	}
	v, err := rawCallContext(ctx, c, "GET", "/me", nil)
	if err != nil {
		return err
	}
	if err = decodeValue(v, &me); err != nil {
		return err
	}
	if me.Agent.EncryptionMode == "e2ee" {
		return errors.New("this workspace requires its saved local encryption keys; plaintext fallback is disabled")
	}
	return nil
}
func contentTool(name string) bool {
	switch name {
	case "message_send", "messages_search", "attachment_upload", "a2a_enable", "a2a_tasks", "a2a_task_update":
		return true
	}
	return false
}
