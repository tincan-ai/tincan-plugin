// Package e2ee implements Tincan's versioned, signed application envelopes.
// Payload encryption is the age v1 format; this is not MLS and provides no
// forward secrecy or post-compromise security for retained device identities.
package e2ee

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"

	"filippo.io/age"
)

const Version = 1
const MaxMembers = 256
const MaxEnvelopeBytes = 256 * 1024
const MaxPlaintextBytes = 96 * 1024
const MaxFileBytes = 10 * 1024 * 1024
const MaxFileEnvelopeBytes = 16 * 1024 * 1024

type Identity struct {
	Age        string `json:"age"`
	SigningKey []byte `json:"signing_key"`
}
type Device struct {
	AgentID    string `json:"agent_id"`
	Recipient  string `json:"recipient"`
	SigningKey []byte `json:"signing_key"`
	Protocol   int    `json:"protocol,omitempty"`
	KeyPackage []byte `json:"key_package,omitempty"`
}
type Roster struct {
	RoomID        string              `json:"room_id,omitempty"`
	RoomMembers   map[string][]string `json:"room_members,omitempty"`
	RoomChannels  map[string]string   `json:"room_channels,omitempty"`
	PlainChannels []string            `json:"plain_channels,omitempty"`
	Version       int                 `json:"version"`
	WorkspaceID   string              `json:"workspace_id"`
	Epoch         int64               `json:"epoch"`
	Previous      string              `json:"previous"`
	Members       []Device            `json:"members"`
	Signature     []byte              `json:"signature"`
	Protocol      int                 `json:"protocol,omitempty"`
	Commit        []byte              `json:"commit,omitempty"`
	Welcome       []byte              `json:"welcome,omitempty"`
}
type Envelope struct {
	RoomGroup     bool     `json:"room_group,omitempty"`
	RoomID        string   `json:"room_id,omitempty"`
	Kind          string   `json:"kind"`
	Version       int      `json:"version"`
	WorkspaceID   string   `json:"workspace_id"`
	ChannelID     string   `json:"channel_id"`
	SenderID      string   `json:"sender_id"`
	Key           string   `json:"key"`
	Mentions      []string `json:"mentions"`
	ReplyTo       *string  `json:"reply_to"`
	AttachmentIDs []string `json:"attachment_ids"`
	Epoch         int64    `json:"epoch"`
	RosterHash    string   `json:"roster_hash"`
	Ciphertext    []byte   `json:"ciphertext"`
	Signature     []byte   `json:"signature"`
	Capsule       []byte   `json:"capsule,omitempty"`
	PayloadHash   string   `json:"payload_hash,omitempty"`
}

func NewIdentity() (*Identity, error) {
	a, err := age.GenerateX25519Identity()
	if err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{Age: a.String(), SigningKey: key}, nil
}
func (i Identity) Device(id string) (Device, error) {
	a, err := age.ParseX25519Identity(i.Age)
	if err != nil || len(i.SigningKey) != ed25519.PrivateKeySize {
		return Device{}, errors.New("invalid encryption identity")
	}
	key := ed25519.PrivateKey(i.SigningKey)
	// Reject corrupt or mismatched public halves of persisted private keys.
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return Device{}, errors.New("invalid signing identity")
	}
	return Device{AgentID: id, Recipient: a.Recipient().String(), SigningKey: append([]byte(nil), key.Public().(ed25519.PublicKey)...)}, nil
}
func (d Device) Validate() error {
	if d.Protocol != 0 && d.Protocol != 2 || d.Protocol == 2 && (len(d.KeyPackage) < 64 || len(d.KeyPackage) > 8192) {
		return errors.New("invalid encryption protocol or key package")
	}
	if len(d.SigningKey) != ed25519.PublicKeySize {
		return errors.New("invalid signing public key")
	}
	_, err := age.ParseX25519Recipient(d.Recipient)
	return err
}
func (d Device) Fingerprint() string {
	d.AgentID = ""
	d.KeyPackage = nil
	return hash(d)
}
func hash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (r Roster) Hash() string { return hash(r) }
func (r Roster) Device(id string) (Device, bool) {
	for _, d := range r.Members {
		if d.AgentID == id {
			return d, true
		}
	}
	return Device{}, false
}
func sign(key []byte, domain string, v any) ([]byte, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("missing signing key")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(key, append([]byte(domain+"\x00"), b...)), nil
}
func verify(key []byte, domain string, v any, signature []byte) bool {
	if len(key) != ed25519.PublicKeySize {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && ed25519.Verify(key, append([]byte(domain+"\x00"), b...), signature)
}
func (r *Roster) Sign(i Identity) error {
	sort.Slice(r.Members, func(a, b int) bool { return r.Members[a].AgentID < r.Members[b].AgentID })
	r.Version = Version
	r.Signature = nil
	s, err := sign(i.SigningKey, "tincan-roster-v1", *r)
	r.Signature = s
	return err
}
func (r Roster) Verify(root []byte, workspace string) error {
	sig := r.Signature
	r.Signature = nil
	if r.Version != Version || r.WorkspaceID != workspace || workspace == "" || r.Epoch < 1 || len(r.Members) == 0 || len(r.Members) > MaxMembers || !verify(root, "tincan-roster-v1", r, sig) {
		return errors.New("untrusted encryption membership")
	}
	seenKeys := map[string]bool{}
	if r.Protocol != 0 && r.Protocol != 2 || len(r.Commit) > 256*1024 || len(r.Welcome) > 512*1024 {
		return errors.New("invalid encryption protocol")
	}
	for n, d := range r.Members {
		if d.Protocol != r.Protocol || d.AgentID == "" || d.Validate() != nil || (n > 0 && r.Members[n-1].AgentID >= d.AgentID) || seenKeys[d.Fingerprint()] {
			return errors.New("invalid encryption membership")
		}
		seenKeys[d.Fingerprint()] = true
	}
	return nil
}
func Encrypt(data []byte, devices []Device) ([]byte, error) {
	if len(devices) == 0 || len(devices) > MaxMembers {
		return nil, errors.New("invalid recipient count")
	}
	recipients := make([]age.Recipient, 0, len(devices))
	for _, d := range devices {
		r, e := age.ParseX25519Recipient(d.Recipient)
		if e != nil {
			return nil, e
		}
		recipients = append(recipients, r)
	}
	var out bytes.Buffer
	w, e := age.Encrypt(&out, recipients...)
	if e != nil {
		return nil, e
	}
	if _, e = w.Write(data); e != nil {
		return nil, e
	}
	if e = w.Close(); e != nil {
		return nil, e
	}
	return out.Bytes(), nil
}
func Decrypt(i Identity, data []byte, limit int64) ([]byte, error) {
	a, e := age.ParseX25519Identity(i.Age)
	if e != nil {
		return nil, errors.New("missing encryption identity")
	}
	r, e := age.Decrypt(bytes.NewReader(data), a)
	if e != nil {
		return nil, errors.New("message cannot be decrypted by this device")
	}
	b, e := io.ReadAll(io.LimitReader(r, limit+1))
	if e != nil || int64(len(b)) > limit {
		return nil, errors.New("invalid encrypted payload")
	}
	return b, nil
}
func (e *Envelope) Seal(i Identity, r Roster, recipients []Device, data []byte) error {
	if room := r.ChannelRoom(e.ChannelID); room != "" {
		e.RoomID = room
		recipients = r.RoomDevices(room)
	}

	limit := MaxPlaintextBytes
	if e.Kind == "file" {
		limit = MaxFileBytes + 1024
	} else {
		e.Kind = "message"
	}
	if len(data) > limit {
		return errors.New("encrypted payload is too large")
	}
	e.Version = Version
	e.Epoch = r.Epoch
	e.RosterHash = r.Hash()
	e.Signature = nil
	var err error
	e.Ciphertext, err = Encrypt(data, recipients)
	if err != nil {
		return err
	}
	e.Signature, err = sign(i.SigningKey, "tincan-message-v1", *e)
	return err
}
func (e Envelope) Verify(r Roster) error {
	if e.RoomGroup != (r.RoomID != "") || r.RoomID != "" && (e.RoomID != r.RoomID || r.ChannelRoom(e.ChannelID) != r.RoomID) {
		return errors.New("encryption group scope mismatch")
	}

	if room := r.ChannelRoom(e.ChannelID); room != "" && (e.RoomID != room || !r.RoomHas(room, e.SenderID)) {
		return errors.New("encrypted room membership mismatch")
	}
	if e.RoomID != "" && r.ChannelRoom(e.ChannelID) != e.RoomID {
		return errors.New("untrusted encrypted room binding")
	}

	d, ok := r.Device(e.SenderID)
	sig := e.Signature
	e.Signature = nil
	if e.Version == 2 {
		return e.verifyMLS(r, d, ok, sig)
	}
	limit := MaxEnvelopeBytes
	if e.Kind == "file" {
		limit = MaxFileBytes + 128*1024
	} else if e.Kind != "message" {
		return errors.New("unknown encrypted payload kind")
	}
	if r.Protocol != 0 || !ok || e.Version != Version || e.WorkspaceID != r.WorkspaceID || e.Epoch != r.Epoch || e.RosterHash != r.Hash() || e.ChannelID == "" || len(e.Key) < 1 || len(e.Key) > 128 || len(e.Ciphertext) == 0 || len(e.Ciphertext) > limit || !verify(d.SigningKey, "tincan-message-v1", e, sig) {
		return errors.New("invalid encrypted message signature or context")
	}
	return nil
}
func (e Envelope) MessageID() string {
	h := sha256.Sum256([]byte(e.WorkspaceID + "\x00" + e.SenderID + "\x00" + e.Key))
	return "msg_" + hex.EncodeToString(h[:16])
}

// A join proves possession of the signing key and binds it to this invitation.
func JoinProof(i Identity, invite string, d Device) ([]byte, error) {
	return sign(i.SigningKey, "tincan-join-v1", struct {
		Invite string
		Device Device
	}{invite, d})
}
func VerifyJoin(invite string, d Device, proof []byte) bool {
	return d.Validate() == nil && d.AgentID == "" && verify(d.SigningKey, "tincan-join-v1", struct {
		Invite string
		Device Device
	}{invite, d}, proof)
}

// New encrypted channel identifiers pin their room independently of relay labels.
func RoomForChannel(channel string) string {
	if !strings.HasPrefix(channel, "ch_room_") {
		return ""
	}
	tail := strings.TrimPrefix(channel, "ch_room_")
	i := strings.LastIndex(tail, "_")
	if i < 1 {
		return ""
	}
	return tail[:i]
}
func (r Roster) ChannelRoom(channel string) string {
	if room := RoomForChannel(channel); room != "" {
		return room
	}
	return r.RoomChannels[channel]
}
func (r Roster) RoomDevices(room string) []Device {
	if r.RoomID == room {
		return r.Members
	}
	var devices []Device
	for _, id := range r.RoomMembers[room] {
		if d, ok := r.Device(id); ok {
			devices = append(devices, d)
		}
	}
	return devices
}
func (r Roster) RoomHas(room, id string) bool {
	if r.RoomID == room {
		_, ok := r.Device(id)
		return ok
	}
	for _, member := range r.RoomMembers[room] {
		if member == id {
			return true
		}
	}
	return false
}
