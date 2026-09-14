package core

import "time"

// JoinReceipt is a scoped capability, never an agent bearer credential.
// Only its hash is stored. VerificationPhrase is a comparison aid, not a secret.
type JoinReceipt struct {
	Automatic          bool      `json:"automatic_admission,omitempty"`
	Fingerprint        string    `json:"fingerprint,omitempty"`
	RequestID          string    `json:"request_id"`
	Receipt            string    `json:"receipt,omitempty"`
	Status             string    `json:"status"`
	VerificationPhrase string    `json:"verification_phrase"`
	ExpiresAt          time.Time `json:"expires_at"`
}
