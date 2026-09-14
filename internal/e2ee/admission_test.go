package e2ee

import (
	"bytes"
	"testing"
)

func TestAdmissionProofBindsDeviceAndContext(t *testing.T) {
	identity, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	device, err := identity.Device("")
	if err != nil {
		t.Fatal(err)
	}
	secret, root := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	proof := &AdmissionProof{InviteHash: "invite", WorkspaceID: "workspace", MAC: AdmissionMAC(secret, root, "workspace", "invite", device)}
	if !VerifyAdmission(secret, root, "workspace", "invite", device, proof) {
		t.Fatal("valid proof rejected")
	}
	other, _ := NewIdentity()
	otherDevice, _ := other.Device("")
	for _, tc := range []struct {
		name              string
		secret, root      []byte
		workspace, invite string
		device            Device
	}{
		{"wrong secret", bytes.Repeat([]byte{3}, 32), root, "workspace", "invite", device},
		{"wrong root", secret, bytes.Repeat([]byte{3}, 32), "workspace", "invite", device},
		{"wrong workspace", secret, root, "other", "invite", device},
		{"wrong invite", secret, root, "workspace", "other", device},
		{"substituted device", secret, root, "workspace", "invite", otherDevice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if VerifyAdmission(tc.secret, tc.root, tc.workspace, tc.invite, tc.device, proof) {
				t.Fatal("forgery accepted")
			}
		})
	}
	device.KeyPackage = []byte("substituted MLS key package")
	if VerifyAdmission(secret, root, "workspace", "invite", device, proof) {
		t.Fatal("key package substitution accepted")
	}
}
