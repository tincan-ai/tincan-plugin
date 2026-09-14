package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"net/url"
	"strings"
	"time"
)

func admissionInviteHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

var errManualAdmission = errors.New("this request requires manual fingerprint verification")

type savedAdmissionInvite struct {
	Secret      []byte    `json:"secret"`
	ExpiresAt   time.Time `json:"expires_at"`
	RequestID   string    `json:"request_id,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// Fragment material is decoded locally and is never forwarded to the HTTP API.
func admissionFragment(raw string) (string, []byte, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", nil, "", errors.New("invalid invitation")
	}
	base, extra, ok := strings.Cut(u.Fragment, ".auto.")
	if !ok {
		return raw, nil, "", nil
	}
	parts := strings.Split(extra, ".")
	if len(parts) != 2 || !strings.Contains(base, ".e2ee.") {
		return "", nil, "", errors.New("invalid automatic invitation")
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[0])
	workspace, we := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || we != nil || len(secret) != 32 || len(workspace) == 0 || len(workspace) > 128 {
		return "", nil, "", errors.New("invalid automatic invitation")
	}
	u.Fragment = base
	return u.String(), secret, string(workspace), nil
}

func prepareAdmissionJoin(ctx context.Context, c Config, raw string) error {
	_, secret, workspace, err := admissionFragment(raw)
	if err != nil || len(secret) == 0 {
		return err
	}
	defer clear(secret)
	clean, _, err := cryptoJoinAddress(raw)
	if err != nil {
		return err
	}
	_, token, err := connectAddress(clean, c.Server)
	if err != nil {
		return err
	}
	_, err = withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		d, err := st.device("")
		if err != nil {
			return nil, err
		}
		hash := admissionInviteHash(token)
		st.JoinAdmission = &e2ee.AdmissionProof{InviteHash: hash, WorkspaceID: workspace, MAC: e2ee.AdmissionMAC(secret, st.Root, workspace, hash, d)}
		return nil, nil
	})
	return err
}

func issueCryptoInvite(st *cryptoState, raw string, expires time.Time) (string, error) {
	pinned := cryptoInvite(raw, st.Root)
	own, err := st.device(st.AgentID)
	if err != nil {
		return "", err
	}
	// Old identities and non-creators retain independently verified invitations.
	if !st.AutomaticInvites || st.WorkspaceID == "" || !bytes.Equal(own.SigningKey, st.Root) {
		return pinned, nil
	}
	clean, _, err := cryptoJoinAddress(pinned)
	if err != nil {
		return "", err
	}
	_, token, err := connectAddress(clean, st.Server)
	if err != nil {
		return "", err
	}
	now := time.Now()
	if expires.IsZero() || expires.After(now.Add(24*time.Hour)) {
		expires = now.Add(24 * time.Hour)
	}
	if !expires.After(now) {
		return "", errors.New("invitation has expired")
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return "", err
	}
	if st.AdmissionInvites == nil {
		st.AdmissionInvites = map[string]savedAdmissionInvite{}
	}
	for hash, invite := range st.AdmissionInvites {
		if !invite.ExpiresAt.After(now) {
			delete(st.AdmissionInvites, hash)
		}
	}
	st.AdmissionInvites[admissionInviteHash(token)] = savedAdmissionInvite{Secret: secret, ExpiresAt: expires}
	return pinned + ".auto." + base64.RawURLEncoding.EncodeToString(secret) + "." + base64.RawURLEncoding.EncodeToString([]byte(st.WorkspaceID)), nil
}

// Reserve durably before signing or sending a commit. Uncertain attempts can
// retry only the same request and device, even if the relay lies about status.
func reserveAdmission(c Config, st *cryptoState, requestID string, device e2ee.Device, proof *e2ee.AdmissionProof) error {
	if requestID == "" || !st.AutomaticInvites || proof == nil || st.CommittedAdmissions[device.Fingerprint()] {
		return errManualAdmission
	}
	if prior := st.AdmittedDevices[device.Fingerprint()]; prior != "" && prior != requestID {
		return errManualAdmission
	}
	invite, ok := st.AdmissionInvites[proof.InviteHash]
	if !ok || !invite.ExpiresAt.After(time.Now()) || !e2ee.VerifyAdmission(invite.Secret, st.Root, st.WorkspaceID, proof.InviteHash, device, proof) {
		return errManualAdmission
	}
	if invite.RequestID != "" && (invite.RequestID != requestID || invite.Fingerprint != device.Fingerprint()) {
		return errManualAdmission
	}
	invite.RequestID, invite.Fingerprint = requestID, device.Fingerprint()
	st.AdmissionInvites[proof.InviteHash] = invite
	return privateJSON(c.CryptoPath, st)
}

func encryptionAdmissionPolicy(ctx context.Context, c Config, automatic *bool) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		own, err := st.device(st.AgentID)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(own.SigningKey, st.Root) {
			return nil, errors.New("use the workspace creator's encryption identity")
		}
		if automatic != nil {
			if *automatic && st.Protocol != 2 {
				return nil, errors.New("automatic admission requires an MLS workspace")
			}
			st.AutomaticInvites = *automatic
			// Disabling invalidates outstanding automatic capabilities locally.
			if !*automatic {
				st.AdmissionInvites = nil
			}
		}
		return map[string]any{"automatic_invites": st.AutomaticInvites, "requires_fingerprint": !st.AutomaticInvites}, nil
	})
}
