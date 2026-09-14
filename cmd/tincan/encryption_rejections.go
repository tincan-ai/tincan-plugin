package main

import (
	"context"
	"encoding/json"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"sort"
)

type rejectedMLSMessage struct {
	MessageID string `json:"message_id"`
	SenderID  string `json:"sender_id"`
	Epoch     int64  `json:"epoch"`
	Seq       int64  `json:"seq"`
	Reason    string `json:"reason"`
}

// Match the exact signed capsule, not only its payload identity. A different
// capsule for the same payload must undergo independent authentication.
func rejectedEnvelopeID(e e2ee.Envelope) string {
	e.Ciphertext = nil
	b, _ := json.Marshal(e)
	return e2ee.PayloadHash(b)
}

func encryptionRejections(ctx context.Context, c Config) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		// Inspection works offline, even if a later membership entry is corrupt.
		out := make([]rejectedMLSMessage, 0, len(st.Rejected))
		for _, rejected := range st.Rejected {
			out = append(out, rejected)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
		return out, nil
	})
}
