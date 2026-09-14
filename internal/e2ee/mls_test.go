package e2ee

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMLSApplicationRejectionIsDistinctFromStateAndRuntimeFailure(t *testing.T) {
	a, b := mlsPair(t)
	bad := []byte{0, 1, 2}
	req := MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: bad}
	if _, err := MLS(context.Background(), req); !errors.Is(err, ErrMLSApplicationRejected) {
		t.Fatalf("invalid application was not classified: %v", err)
	}
	req.State = nil
	if _, err := MLS(context.Background(), req); err == nil || errors.Is(err, ErrMLSApplicationRejected) {
		t.Fatalf("missing state must be fatal: %v", err)
	}
	req.State, req.Op = b.state, "commit"
	if _, err := MLS(context.Background(), req); err == nil || errors.Is(err, ErrMLSApplicationRejected) {
		t.Fatalf("bad commit must be fatal: %v", err)
	}
	req.Op = "decrypt"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := MLS(ctx, req); !errors.Is(err, context.Canceled) || errors.Is(err, ErrMLSApplicationRejected) {
		t.Fatalf("cancellation must remain retryable: %v", err)
	}
	good := a.run(t, "encrypt", []byte("valid after rejection"), nil, nil)
	if got := b.run(t, "decrypt", good.Data, nil, nil); string(got.Data) != "valid after rejection" {
		t.Fatal("rejection damaged group")
	}
}

func TestMLSExcessiveGenerationIsQuarantinable(t *testing.T) {
	a, b := mlsPair(t)
	first := a.run(t, "encrypt", []byte("first generation"), nil, nil)
	var distant MLSResponse
	// Exercise the actual sender ratchet, rather than fabricating its storage.
	for generation := 1; generation <= 4097; generation++ {
		distant = a.run(t, "encrypt", []byte("distant generation"), nil, nil)
	}
	if _, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: distant.Data}); !errors.Is(err, ErrMLSApplicationRejected) {
		t.Fatalf("excessive distance was not quarantinable: %v", err)
	}
	if got := b.run(t, "decrypt", first.Data, nil, nil); string(got.Data) != "first generation" {
		t.Fatal("rejection lost an otherwise valid generation")
	}
}

type mlsPeer struct {
	identity Identity
	state    json.RawMessage
	group    string
}

func (p *mlsPeer) run(t testing.TB, op string, data, aad, member []byte) MLSResponse {
	t.Helper()
	out, err := MLS(context.Background(), MLSRequest{Op: op, State: p.state, SigningKey: p.identity.SigningKey, Group: p.group, Data: data, AAD: aad, Member: member})
	if err != nil {
		t.Fatal(err)
	}
	p.state = out.State
	return out
}
func mlsPair(t testing.TB) (*mlsPeer, *mlsPeer) {
	t.Helper()
	a, _ := NewIdentity()
	b, _ := NewIdentity()
	alice := &mlsPeer{identity: *a, group: "test-group"}
	bob := &mlsPeer{identity: *b, group: "test-group"}
	kp := bob.run(t, "key_package", nil, nil, nil)
	alice.run(t, "create", nil, nil, nil)
	added := alice.run(t, "add", kp.Data, nil, b.SigningKey[32:])
	alice.run(t, "finalize", nil, nil, nil)
	bob.run(t, "join", added.Welcome, nil, nil)
	return alice, bob
}
func TestMLSForwardSecrecyAcrossMessagesEpochsAndRestart(t *testing.T) {
	a, b := mlsPair(t)
	before := append(json.RawMessage(nil), b.state...)
	first := a.run(t, "encrypt", []byte("message-key-canary"), []byte("bound context"), nil)
	received := b.run(t, "decrypt", first.Data, nil, nil)
	if !bytes.Equal(received.Data, []byte("message-key-canary")) || !bytes.Equal(received.AAD, []byte("bound context")) || !bytes.Equal(received.Sender, a.identity.SigningKey[32:]) {
		t.Fatal("MLS context/identity round trip")
	}
	// Prove the captured ciphertext was decryptable before consumption.
	old := &mlsPeer{identity: b.identity, state: before, group: b.group}
	old.run(t, "decrypt", first.Data, nil, nil)
	// All current transport state plus the long-lived signing key is insufficient
	// to open the consumed ciphertext. No archive is present in this fixture.
	if _, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: first.Data}); err == nil {
		t.Fatal("current keys decrypted consumed message")
	}
	// Each call instantiates a fresh WASM sandbox from serialized state.
	second := a.run(t, "encrypt", []byte("next key"), nil, nil)
	if got := b.run(t, "decrypt", second.Data, nil, nil); !bytes.Equal(got.Data, []byte("next key")) {
		t.Fatal("restart lost ratchet")
	}
	update := a.run(t, "update", nil, nil, nil)
	a.run(t, "finalize", nil, nil, nil)
	b.run(t, "commit", update.Data, nil, nil)
	for _, peer := range []*mlsPeer{a, b} {
		if _, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: peer.state, SigningKey: peer.identity.SigningKey, Group: peer.group, Data: first.Data}); err == nil {
			t.Fatal("old epoch retained")
		}
	}
	third := b.run(t, "encrypt", []byte("new epoch key"), nil, nil)
	if got := a.run(t, "decrypt", third.Data, nil, nil); !bytes.Equal(got.Data, []byte("new epoch key")) {
		t.Fatal("epoch update diverged")
	}
}
func TestMLSOutOfOrderTamperingAndRevocation(t *testing.T) {
	a, b := mlsPair(t)
	first := a.run(t, "encrypt", []byte("one"), nil, nil)
	second := a.run(t, "encrypt", []byte("two"), nil, nil)
	corrupt := append([]byte(nil), second.Data...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: corrupt}); err == nil {
		t.Fatal("tampering accepted")
	}
	if got := b.run(t, "decrypt", second.Data, nil, nil); string(got.Data) != "two" {
		t.Fatal("out of order")
	}
	if got := b.run(t, "decrypt", first.Data, nil, nil); string(got.Data) != "one" {
		t.Fatal("skipped key lost")
	}
	removed := a.run(t, "remove", nil, nil, b.identity.SigningKey[32:])
	_ = removed
	a.run(t, "finalize", nil, nil, nil)
	future := a.run(t, "encrypt", []byte("excluded"), nil, nil)
	if _, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: future.Data}); err == nil {
		t.Fatal("removed member decrypted")
	}
}
func TestMLSAdmissionRejectsSubstitutedKeyPackage(t *testing.T) {
	a, b := mlsPair(t)
	stranger, _ := NewIdentity()
	c := &mlsPeer{identity: *stranger, group: a.group}
	kp := c.run(t, "key_package", nil, nil, nil)
	if _, err := MLS(context.Background(), MLSRequest{Op: "add", State: a.state, SigningKey: a.identity.SigningKey, Group: a.group, Data: kp.Data, Member: b.identity.SigningKey[32:]}); err == nil {
		t.Fatal("key package substitution accepted")
	}
	// Failed verification must not alter durable state or consume the session.
	msg := b.run(t, "encrypt", []byte("still works"), nil, nil)
	a.run(t, "decrypt", msg.Data, nil, nil)
}
func TestMLSContextAndArchiveAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	encrypted, err := SealLocal(key, []byte("history"), []byte("workspace/message"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenLocal(key, encrypted, []byte("different/message")); err == nil {
		t.Fatal("archive substitution")
	}
	if _, err = OpenLocal(bytes.Repeat([]byte{8}, 32), encrypted, []byte("workspace/message")); err == nil {
		t.Fatal("wrong history key")
	}
}

func FuzzMLSApplicationMessage(f *testing.F) {
	a, b := mlsPair(f)
	message := a.run(f, "encrypt", []byte("fuzz-key"), []byte("fuzz-context"), nil)
	f.Add(message.Data)
	f.Add([]byte{})
	f.Add([]byte{0, 1, 2, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		out, err := MLS(context.Background(), MLSRequest{Op: "decrypt", State: b.state, SigningKey: b.identity.SigningKey, Group: b.group, Data: data})
		if err == nil && (!bytes.Equal(out.Data, []byte("fuzz-key")) || !bytes.Equal(out.AAD, []byte("fuzz-context")) || !bytes.Equal(out.Sender, a.identity.SigningKey[32:])) {
			t.Fatal("MLS accepted unauthenticated application content")
		}
	})
}
