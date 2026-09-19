package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"os"
	"strconv"
	"strings"
	"time"
)

type pendingMLSRoster struct {
	Roster    e2ee.Roster `json:"roster"`
	RequestID string      `json:"request_id"`
}
type archiveEntry struct {
	Key     []byte `json:"key"`
	Payload []byte `json:"payload,omitempty"`
}

func (st *cryptoState) device(id string) (e2ee.Device, error) {
	d, err := st.Identity.Device(id)
	d.Protocol, d.KeyPackage = st.Protocol, st.KeyPackage
	return d, err
}
func historyKey(c Config) ([]byte, error) {
	path := c.CryptoPath + ".history.key"
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local history key is missing or insecure; restore the separate history backup")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) != 32 {
		return nil, errors.New("invalid local history key")
	}
	return b, nil
}
func archiveID(e e2ee.Envelope) string { return e.MessageID() + ":" + e.PayloadHash }
func (st *cryptoState) archiveGet(c Config, e e2ee.Envelope) (archiveEntry, bool, error) {
	var out archiveEntry
	data, ok := st.Archive[archiveID(e)]
	if !ok {
		return out, false, nil
	}
	key, err := historyKey(c)
	if err != nil {
		return out, false, err
	}
	plain, err := e2ee.OpenLocal(key, data, []byte(st.WorkspaceID+"/"+archiveID(e)))
	if err != nil {
		return out, false, errors.New("local history archive authentication failed")
	}
	err = json.Unmarshal(plain, &out)
	return out, true, err
}
func (st *cryptoState) archivePut(c Config, e e2ee.Envelope, entry archiveEntry) error {
	key, err := historyKey(c)
	if err != nil {
		return err
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	sealed, err := e2ee.SealLocal(key, data, []byte(st.WorkspaceID+"/"+archiveID(e)))
	if err != nil {
		return err
	}
	if st.Archive == nil {
		st.Archive = map[string][]byte{}
	}
	st.Archive[archiveID(e)] = sealed
	if e.Kind == "message" && len(entry.Payload) > 0 {
		var body encryptedBody
		if json.Unmarshal(entry.Payload, &body) == nil {
			if st.SearchIndex == nil {
				st.SearchIndex = map[string][]byte{}
			}
			st.SearchIndex[archiveID(e)] = historyIndex(key, body.Text)
		}
	}
	return nil
}
func (st *cryptoState) mlsCall(ctx context.Context, op string, data, aad, member []byte, private bool) (e2ee.MLSResponse, error) {
	state, group := st.MLS, st.WorkspaceID
	if st.RoomID != "" {
		group = st.WorkspaceID + "/room/" + st.RoomID
	}
	if private {
		state, group = st.PrivateMLS, st.WorkspaceID+"/private/"+st.AgentID
	}
	return e2ee.MLS(ctx, e2ee.MLSRequest{Op: op, State: state, SigningKey: st.Identity.SigningKey, Group: group, Data: data, AAD: aad, Member: member})
}
func checkMLSMembers(out e2ee.MLSResponse, r e2ee.Roster) error {
	if out.Epoch+1 != uint64(r.Epoch) || len(out.Members) != len(r.Members) {
		return errors.New("MLS group does not match signed membership")
	}
	for _, d := range r.Members {
		found := false
		for _, k := range out.Members {
			if bytes.Equal(k, d.SigningKey) {
				found = true
				break
			}
		}
		if !found {
			return errors.New("MLS device identity differs from verified membership")
		}
	}
	return nil
}
func ensureMLS(ctx context.Context, c Config, st *cryptoState, remoteEpoch int64) error {
	if _, err := historyKey(c); err != nil {
		return err
	}
	if remoteEpoch < st.Epoch {
		return errors.New("encryption membership rollback detected")
	}
	if st.PendingRoster == nil && remoteEpoch == 0 {
		if !bytes.Equal(st.Identity.SigningKey[32:], st.Root) {
			return errors.New("creator has not initialized MLS")
		}
		out, err := st.mlsCall(ctx, "create", nil, nil, nil, false)
		if err != nil {
			return err
		}
		st.MLS = out.State
		d, err := st.device(st.AgentID)
		if err != nil {
			return err
		}
		r := e2ee.Roster{RoomID: st.RoomID, Protocol: 2, WorkspaceID: st.WorkspaceID, Epoch: 1, Members: []e2ee.Device{d}, PlainChannels: st.InitialPlainChannels}
		if st.RoomID != "" {
			r.PlainChannels = nil
		}
		if st.RoomID == "" {
			if err = loadInitialRoomPolicy(ctx, c, &r); err != nil {
				return err
			}
		}
		if err = r.Sign(st.Identity); err != nil {
			return err
		}
		st.PendingRoster = &pendingMLSRoster{Roster: r}
		if err = privateJSON(c.CryptoPath, st); err != nil {
			return err
		}
	}
	if st.PendingRoster != nil {
		_, err := rawCallContext(ctx, c, "POST", "/e2ee/roster", st.PendingRoster)
		if err != nil {
			if st.PendingRoster.Roster.Epoch > 1 && (strings.Contains(err.Error(), `"code":"request_unavailable"`) || strings.Contains(err.Error(), `"code":"invalid_roster"`) || strings.Contains(err.Error(), `"code":"encryption_plan_required"`) || strings.Contains(err.Error(), `"code":"invalid_room_policy"`) || strings.Contains(err.Error(), `"code":"room_owner_required"`)) {
				// A definitive rejection cannot have published this commit. Keep
				// the current receive ratchet while discarding only the proposal.
				out, discardErr := st.mlsCall(ctx, "discard", nil, nil, nil, false)
				if discardErr != nil {
					return discardErr
				}
				st.MLS, st.PendingRoster = out.State, nil
				if saveErr := privateJSON(c.CryptoPath, st); saveErr != nil {
					return saveErr
				}
			}
			return err
		} // An uncertain outcome retains the exact staged commit.
	}
	if err := syncMLS(ctx, c, st); err != nil {
		return err
	}
	r, err := cryptoRoster(ctx, c, st, 0)
	if err != nil {
		return err
	}
	if st.MLSEpoch != r.Epoch || st.ActiveRoster.Hash() != r.Hash() {
		return errors.New("encryption synchronization incomplete; retry before sending")
	}
	own, ok := r.Device(st.AgentID)
	local, _ := st.device(st.AgentID)
	if !ok || own.Fingerprint() != local.Fingerprint() {
		return errors.New("this device is not in verified MLS membership")
	}
	if bytes.Equal(local.SigningKey, st.Root) && (st.MessagesSinceUpdate >= 100 || !st.LastMLSUpdate.IsZero() && time.Since(st.LastMLSUpdate) >= 24*time.Hour) {
		next := r
		next.Epoch++
		next.Previous = r.Hash()
		_, err = changeMLSRoster(ctx, c, st, r, next, "", "")
		return err
	}
	return nil
}
func syncMLS(ctx context.Context, c Config, st *cryptoState) error {
	for {
		v, err := rawCallContext(ctx, c, "GET", "/e2ee/sync?after="+strconv.FormatInt(st.SyncSeq, 10), nil)
		if err != nil {
			return err
		}
		var rows []struct {
			Seq     int64           `json:"seq"`
			Kind    string          `json:"kind"`
			Payload json.RawMessage `json:"payload"`
		}
		if err = decodeValue(v, &rows); err != nil {
			return err
		}
		for _, row := range rows {
			if row.Seq <= st.SyncSeq {
				return errors.New("encryption journal cursor did not advance")
			}
			switch row.Kind {
			case "roster":
				var r e2ee.Roster
				if err = json.Unmarshal(row.Payload, &r); err != nil {
					return err
				}
				if r.RoomID != st.RoomID {
					return errors.New("encryption journal room mismatch")
				}
				if err = r.Verify(st.Root, st.WorkspaceID); err != nil {
					return err
				}
				// Even before admission, verify the creator's complete public chain.
				if r.Epoch != st.ActiveRoster.Epoch+1 || (r.Epoch > 1 && r.Previous != st.ActiveRoster.Hash()) {
					return errors.New("encryption membership journal fork or gap")
				}
				_, member := r.Device(st.AgentID)
				if member {
					var out e2ee.MLSResponse
					if st.PendingRoster != nil && st.PendingRoster.Roster.Hash() == r.Hash() {
						op := "finalize"
						if r.Epoch == 1 {
							op = "info"
						}
						out, err = st.mlsCall(ctx, op, nil, nil, nil, false)
					} else if st.MLSEpoch == 0 {
						if len(r.Welcome) == 0 {
							return errors.New("MLS welcome missing; obtain a new device invitation")
						}
						out, err = st.mlsCall(ctx, "join", r.Welcome, nil, nil, false)
					} else {
						out, err = st.mlsCall(ctx, "commit", r.Commit, nil, nil, false)
						if err == nil && !bytes.Equal(out.Sender, st.Root) {
							return errors.New("MLS membership transition was not created by the owner")
						}
					}
					if err != nil {
						return err
					}
					if err = checkMLSMembers(out, r); err != nil {
						return err
					}
					st.MLS, st.MLSEpoch = out.State, r.Epoch
					st.MessagesSinceUpdate, st.LastMLSUpdate = 0, time.Now()
					if st.PendingRoster != nil && st.PendingRoster.Roster.Hash() == r.Hash() {
						st.PendingRoster = nil
					}
				} else if st.MLSEpoch > 0 {
					return errors.New("this encrypted device has been removed; request a new invitation")
				}
				st.ActiveRoster = r
			case "opaque":
				var payload struct {
					Capsule  []byte `json:"capsule"`
					Identity string `json:"identity"`
				}
				if err = json.Unmarshal(row.Payload, &payload); err != nil {
					return err
				}
				if _, member := st.ActiveRoster.Device(st.AgentID); member && !st.Consumed[payload.Identity] {
					out, e := st.mlsCall(ctx, "decrypt", payload.Capsule, nil, nil, false)
					if e != nil && !errors.Is(e, e2ee.ErrMLSApplicationRejected) {
						return e
					}
					if e == nil {
						st.MLS = out.State
					}
				}
			case "envelope":
				var e e2ee.Envelope
				if err = json.Unmarshal(row.Payload, &e); err != nil {
					return err
				}
				if err = e.Verify(st.ActiveRoster); err != nil {
					return err
				}
				if _, member := st.ActiveRoster.Device(st.AgentID); member {
					if err = consumeMLSKey(ctx, c, st, e); err != nil {
						if !errors.Is(err, e2ee.ErrMLSApplicationRejected) {
							return err
						}
						if st.Rejected == nil {
							st.Rejected = map[string]rejectedMLSMessage{}
						}
						st.Rejected[rejectedEnvelopeID(e)] = rejectedMLSMessage{MessageID: e.MessageID(), SenderID: e.SenderID, Epoch: e.Epoch, Seq: row.Seq, Reason: e2ee.ErrMLSApplicationRejected.Error()}
					}
				}
			default:
				return errors.New("unknown encryption journal entry")
			}
			st.SyncSeq = row.Seq
			// A single replace commits consumed keys, encrypted archive, and replay cursor.
			if err = privateJSON(c.CryptoPath, st); err != nil {
				return err
			}
		}
		if len(rows) < 100 {
			return nil
		}
	}
}
func consumeMLSKey(ctx context.Context, c Config, st *cryptoState, e e2ee.Envelope) error {
	if st.Consumed[archiveID(e)] {
		return nil
	}
	if e.SenderID == st.AgentID {
		return errors.New("this runtime's encryption state is stale or cloned; rejoin as a new verified device and import history instead of restoring an old session snapshot")
	}
	private := strings.HasPrefix(e.ChannelID, "ch_private_")
	if private && !strings.HasPrefix(e.ChannelID, "ch_private_"+st.AgentID+"_") {
		return errors.New("private history belongs to another runtime")
	}
	out, err := st.mlsCall(ctx, "decrypt", e.Capsule, nil, nil, private)
	if err != nil {
		return err
	}
	sender, ok := st.ActiveRoster.Device(e.SenderID)
	if !ok || !bytes.Equal(out.Sender, sender.SigningKey) || !bytes.Equal(out.AAD, e.MLSContext()) {
		return e2ee.ErrMLSApplicationRejected
	}
	key := out.Data
	if e.RoomID != "" && !e.RoomGroup {
		if !st.ActiveRoster.RoomHas(e.RoomID, st.AgentID) {
			st.MLS = out.State
			if st.Consumed == nil {
				st.Consumed = map[string]bool{}
			}
			st.Consumed[archiveID(e)] = true
			return nil
		}
		key, err = e2ee.Decrypt(st.Identity, out.Data, 32)
		if err != nil {
			return e2ee.ErrMLSApplicationRejected
		}
	}
	if len(key) != 32 {
		return e2ee.ErrMLSApplicationRejected
	}
	if err = st.archivePut(c, e, archiveEntry{Key: key}); err != nil {
		return err
	}
	if st.Consumed == nil {
		st.Consumed = map[string]bool{}
	}
	st.Consumed[archiveID(e)] = true
	if private {
		st.PrivateMLS = out.State
	} else {
		st.MLS = out.State
		st.MessagesSinceUpdate++
	}
	return nil
}
func sealMLS(ctx context.Context, c Config, st *cryptoState, e *e2ee.Envelope, r e2ee.Roster, plain []byte) error {
	if r.Protocol != 2 || r.Hash() != st.ActiveRoster.Hash() {
		return errors.New("synchronize MLS membership before sending")
	}
	if e.Kind == "" {
		e.Kind = "message"
	}
	limit := e2ee.MaxPlaintextBytes
	if e.Kind == "file" {
		limit = e2ee.MaxFileBytes + 1024
	}
	if len(plain) > limit {
		return errors.New("encrypted payload too large")
	}
	e.Version, e.Epoch, e.RosterHash = 2, r.Epoch, r.Hash()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	var err error
	// Payload AEAD binds its immutable identity; the MLS AAD additionally binds
	// all routing and the ciphertext hash, preventing key-capsule substitution.
	e.Ciphertext, err = e2ee.SealLocal(key, plain, []byte(e.MessageID()))
	if err != nil {
		return err
	}
	e.PayloadHash = e2ee.PayloadHash(e.Ciphertext)
	private := strings.HasPrefix(e.ChannelID, "ch_private_")
	if private && !strings.HasPrefix(e.ChannelID, "ch_private_"+st.AgentID+"_") {
		return errors.New("private history belongs to another runtime")
	}
	if private && len(st.PrivateMLS) == 0 {
		out, err := st.mlsCall(ctx, "create", nil, nil, nil, true)
		if err != nil {
			return err
		}
		st.PrivateMLS = out.State
	}
	capsuleKey := key
	if room := r.ChannelRoom(e.ChannelID); room != "" {
		if !r.RoomHas(room, st.AgentID) {
			return errors.New("this device is not a member of the encrypted room")
		}
		e.RoomID = room
		if st.RoomID != "" {
			e.RoomGroup = true
		} else {
			capsuleKey, err = e2ee.Encrypt(key, r.RoomDevices(room))
			if err != nil {
				return err
			}
		}
	}
	out, err := st.mlsCall(ctx, "encrypt", capsuleKey, e.MLSContext(), nil, private)
	if err != nil {
		return err
	}
	e.Capsule = out.Data
	if err = e.SignMLS(st.Identity); err != nil {
		return err
	}
	entry := archiveEntry{Key: key}
	if e.Kind == "message" {
		entry.Payload = plain
	}
	if err = st.archivePut(c, *e, entry); err != nil {
		return err
	}
	if st.Consumed == nil {
		st.Consumed = map[string]bool{}
	}
	st.Consumed[archiveID(*e)] = true
	if private {
		st.PrivateMLS = out.State
	} else {
		st.MLS = out.State
		st.MessagesSinceUpdate++
	}
	return nil
}
func openMLSPayload(ctx context.Context, c Config, st *cryptoState, e e2ee.Envelope) ([]byte, error) {
	if _, rejected := st.Rejected[rejectedEnvelopeID(e)]; rejected {
		return nil, e2ee.ErrMLSApplicationRejected
	}
	if len(e.Ciphertext) == 0 || e2ee.PayloadHash(e.Ciphertext) != e.PayloadHash {
		return nil, errors.New("encrypted payload hash mismatch")
	}
	entry, ok, err := st.archiveGet(c, e)
	if err != nil {
		return nil, err
	}
	if !ok {
		if err = syncMLS(ctx, c, st); err != nil {
			return nil, err
		}
		entry, ok, err = st.archiveGet(c, e)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errors.New("message key unavailable; synchronize this device or restore its history archive")
		}
	}
	plain, err := e2ee.OpenLocal(entry.Key, e.Ciphertext, []byte(e.MessageID()))
	if err != nil {
		return nil, err
	}
	if e.Kind == "message" && len(entry.Payload) == 0 {
		entry.Payload = plain
		if err = st.archivePut(c, e, entry); err != nil {
			return nil, err
		}
	}
	return plain, nil
}
func changeMLSRoster(ctx context.Context, c Config, st *cryptoState, old, next e2ee.Roster, requestID, revoke string) (any, error) {
	var data, member []byte
	op := "update"
	if requestID != "" {
		op = "add"
		for _, d := range next.Members {
			if _, ok := old.Device(d.AgentID); !ok {
				data, member = d.KeyPackage, d.SigningKey
			}
		}
	} else if revoke != "" {
		op = "remove"
		d, ok := old.Device(revoke)
		if !ok {
			return nil, errors.New("device missing")
		}
		member = d.SigningKey
	}
	out, err := st.mlsCall(ctx, op, data, nil, member, false)
	if err != nil {
		return nil, err
	}
	next.Commit, next.Welcome = out.Data, out.Welcome
	if err = next.Sign(st.Identity); err != nil {
		return nil, err
	}
	st.MLS = out.State
	st.PendingRoster = &pendingMLSRoster{Roster: next, RequestID: requestID}
	if requestID != "" {
		if st.CommittedAdmissions == nil {
			st.CommittedAdmissions = map[string]bool{}
		}
		for _, device := range next.Members {
			if _, existed := old.Device(device.AgentID); !existed {
				st.CommittedAdmissions[device.Fingerprint()] = true
			}
		}
	}
	if err = privateJSON(c.CryptoPath, st); err != nil {
		return nil, err
	}
	if err = ensureMLS(ctx, c, st, old.Epoch); err != nil {
		return nil, fmt.Errorf("membership update pending: %w", err)
	}
	return map[string]bool{"ok": true}, nil
}
