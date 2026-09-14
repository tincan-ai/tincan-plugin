package e2ee

import (
	"bytes"
	"encoding/json"
	"testing"
)

func fixture(t testing.TB) (Identity, Identity, Roster) {
	t.Helper()
	a, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	da, _ := a.Device("ag_a")
	db, _ := b.Device("ag_b")
	r := Roster{WorkspaceID: "ws_test", Epoch: 1, Members: []Device{da, db}}
	if err = r.Sign(*a); err != nil {
		t.Fatal(err)
	}
	return *a, *b, r
}
func TestAgeEnvelopeAuthenticationAndIsolation(t *testing.T) {
	a, b, r := fixture(t)
	root, _ := a.Device("ag_a")
	if err := r.Verify(root.SigningKey, "ws_test"); err != nil {
		t.Fatal(err)
	}
	e := Envelope{WorkspaceID: r.WorkspaceID, ChannelID: "ch_test", SenderID: "ag_a", Key: "request-1", Mentions: []string{"ag_b"}}
	plain := []byte(`{"text":"secret pineapple","metadata":{"private":"hidden"}}`)
	if err := e.Seal(a, r, r.Members, plain); err != nil {
		t.Fatal(err)
	}
	if err := e.Verify(r); err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(e)
	if bytes.Contains(wire, []byte("pineapple")) {
		t.Fatal("plaintext in envelope")
	}
	for _, identity := range []Identity{a, b} {
		got, err := Decrypt(identity, e.Ciphertext, MaxPlaintextBytes)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("round trip: %v", err)
		}
	}
	outsider, _ := NewIdentity()
	if _, err := Decrypt(*outsider, e.Ciphertext, MaxPlaintextBytes); err == nil {
		t.Fatal("outsider decrypted")
	}
	changes := map[string]func(*Envelope){
		"ciphertext": func(x *Envelope) {
			x.Ciphertext = append([]byte(nil), x.Ciphertext...)
			x.Ciphertext[len(x.Ciphertext)-1] ^= 1
		},
		"sender": func(x *Envelope) { x.SenderID = "ag_b" }, "channel": func(x *Envelope) { x.ChannelID = "ch_other" },
		"workspace": func(x *Envelope) { x.WorkspaceID = "ws_other" }, "key": func(x *Envelope) { x.Key = "replay" },
		"mention": func(x *Envelope) { x.Mentions = []string{"ag_a"} }, "attachment": func(x *Envelope) { x.AttachmentIDs = []string{"blob_forged"} },
		"reply": func(x *Envelope) { id := "msg_fake"; x.ReplyTo = &id }, "epoch": func(x *Envelope) { x.Epoch++ },
		"roster": func(x *Envelope) { x.RosterHash = "fake" }, "kind": func(x *Envelope) { x.Kind = "file" }, "version": func(x *Envelope) { x.Version++ },
		"signature": func(x *Envelope) { x.Signature = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			tampered := e
			change(&tampered)
			if tampered.Verify(r) == nil {
				t.Fatal("accepted tampering")
			}
		})
	}
	if _, err := Decrypt(a, e.Ciphertext, 2); err == nil {
		t.Fatal("decryption limit ignored")
	}
}
func TestMembershipSignaturesAndRevocationRecipients(t *testing.T) {
	a, b, r := fixture(t)
	root, _ := a.Device("ag_a")
	next := r
	next.Epoch++
	next.Previous = r.Hash()
	next.Members = append([]Device(nil), r.Members[:1]...)
	if err := next.Sign(a); err != nil {
		t.Fatal(err)
	}
	if err := next.Verify(root.SigningKey, r.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	cipher, err := Encrypt([]byte("future"), next.Members)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Decrypt(b, cipher, 100); err == nil {
		t.Fatal("removed member decrypted future content")
	}
	if _, err = Decrypt(a, cipher, 100); err != nil {
		t.Fatal(err)
	}
	tampered := next
	tampered.Members = append(tampered.Members, r.Members[1])
	if tampered.Verify(root.SigningKey, r.WorkspaceID) == nil {
		t.Fatal("server-added member accepted")
	}
	if err = next.Sign(b); err != nil {
		t.Fatal(err)
	}
	if next.Verify(root.SigningKey, r.WorkspaceID) == nil {
		t.Fatal("non-owner signed membership")
	}
	if r.Verify(root.SigningKey, "ws_other") == nil {
		t.Fatal("cross-workspace membership accepted")
	}
	duplicate := r
	duplicate.Members = []Device{r.Members[0], r.Members[0]}
	duplicate.Sign(a)
	if duplicate.Verify(root.SigningKey, r.WorkspaceID) == nil {
		t.Fatal("duplicate members accepted")
	}
}
func TestJoinProofBindsInvitationAndDevice(t *testing.T) {
	a, _, _ := fixture(t)
	d, _ := a.Device("")
	proof, err := JoinProof(a, "invite-a", d)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyJoin("invite-a", d, proof) {
		t.Fatal("valid proof rejected")
	}
	if VerifyJoin("invite-b", d, proof) {
		t.Fatal("cross-invite proof accepted")
	}
	b, _ := NewIdentity()
	other, _ := b.Device("")
	if VerifyJoin("invite-a", other, proof) {
		t.Fatal("substituted device accepted")
	}
	if VerifyJoin("invite-a", d, nil) {
		t.Fatal("missing proof accepted")
	}
}
func FuzzEncryptedEnvelope(f *testing.F) {
	a, _, r := fixture(f)
	e := Envelope{WorkspaceID: r.WorkspaceID, ChannelID: "ch_test", SenderID: "ag_a", Key: "key"}
	if err := e.Seal(a, r, r.Members, []byte("secret")); err != nil {
		f.Fatal(err)
	}
	valid, _ := json.Marshal(e)
	f.Add(valid)
	f.Add([]byte(`{"version":1,"signature":""}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxEnvelopeBytes {
			return
		}
		var parsed Envelope
		if json.Unmarshal(data, &parsed) != nil {
			return
		}
		if parsed.Verify(r) == nil {
			plain, err := Decrypt(a, parsed.Ciphertext, MaxPlaintextBytes)
			if err != nil || !bytes.Equal(plain, []byte("secret")) {
				t.Fatal("unauthenticated plaintext accepted")
			}
		}
	})
}
