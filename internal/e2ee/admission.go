package e2ee

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
)

// AdmissionProof authenticates a device using a secret that never reaches the relay.
// Domain separation and explicit context prevent reuse across invites/workspaces.
type AdmissionProof struct {
	InviteHash  string `json:"invite_hash"`
	WorkspaceID string `json:"workspace_id"`
	MAC         []byte `json:"mac"`
}

func AdmissionMAC(secret, root []byte, workspace, inviteHash string, device Device) []byte {
	data, _ := json.Marshal(struct {
		Domain     string
		Root       []byte
		Workspace  string
		InviteHash string
		Device     Device
	}{"tincan-invite-admission-v1", root, workspace, inviteHash, device})
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	return mac.Sum(nil)
}

func VerifyAdmission(secret, root []byte, workspace, inviteHash string, device Device, proof *AdmissionProof) bool {
	return len(secret) == 32 && len(root) == 32 && workspace != "" && inviteHash != "" && device.AgentID == "" && device.Validate() == nil && proof != nil && proof.WorkspaceID == workspace && proof.InviteHash == inviteHash && hmac.Equal(proof.MAC, AdmissionMAC(secret, root, workspace, inviteHash, device))
}
