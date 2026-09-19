package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Fixed-size keyed trigram Bloom filters accelerate repeated substring search
// without writing plaintext terms. A possible match is always verified against
// decrypted text; short queries and unindexed messages use the normal scan.
func historyIndexPositions(key []byte, token string) [3]uint16 {
	h := hmac.New(sha256.New, key)
	h.Write([]byte("tincan-history-search-v2\x00" + token))
	sum := h.Sum(nil)
	return [3]uint16{binary.BigEndian.Uint16(sum[:2]) % 2048, binary.BigEndian.Uint16(sum[2:4]) % 2048, binary.BigEndian.Uint16(sum[4:6]) % 2048}
}
func historyIndex(key []byte, text string) []byte {
	filter := make([]byte, 256)
	text = strings.ToLower(text)
	for i := 0; i+3 <= len(text); i++ {
		for _, p := range historyIndexPositions(key, text[i:i+3]) {
			filter[p/8] |= 1 << (p % 8)
		}
	}
	return filter
}
func historyIndexMayMatch(filter, key []byte, query string) bool {
	if len(filter) != 256 {
		return true
	}
	query = strings.ToLower(query)
	for i := 0; i+3 <= len(query); i++ {
		for _, p := range historyIndexPositions(key, query[i:i+3]) {
			if filter[p/8]&(1<<(p%8)) == 0 {
				return false
			}
		}
	}
	return true
}

// History backups intentionally exclude live MLS state, credentials, pending
// commits, and signing keys. The archive key must be kept separately by the user.
type historyBackup struct {
	RoomID      string            `json:"room_id,omitempty"`
	Version     int               `json:"version"`
	WorkspaceID string            `json:"workspace_id"`
	Root        []byte            `json:"root"`
	Archive     map[string][]byte `json:"archive"`
}

func backupHistory(ctx context.Context, c Config, path string) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if st.Protocol != 2 || st.WorkspaceID == "" {
			return nil, errors.New("history backup requires an initialized MLS workspace")
		}
		if _, err := historyKey(c); err != nil {
			return nil, err
		}
		if path == "" {
			return nil, errors.New("choose a private history backup path")
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, err
		}
		complete := false
		defer func() {
			f.Close()
			if !complete {
				os.Remove(path)
			}
		}()
		if err = json.NewEncoder(f).Encode(historyBackup{RoomID: st.RoomID, Version: 2, WorkspaceID: st.WorkspaceID, Root: st.Root, Archive: st.Archive}); err != nil {
			return nil, err
		}
		if err = f.Sync(); err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		complete = true
		return map[string]any{"path": path, "entries": len(st.Archive), "history_key_path": c.CryptoPath + ".history.key", "next": "Keep the history key in separate private backup storage. This archive contains no live session state. After reinstalling, join as a new verified device and import this archive using the saved key file."}, nil
	})
}
func restoreHistory(ctx context.Context, c Config, path, keyPath string) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if st.Protocol != 2 || st.WorkspaceID == "" {
			return nil, errors.New("join the encrypted workspace with a new verified device before importing history")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		data, err := io.ReadAll(io.LimitReader(f, 128*1024*1024+1))
		if err != nil || len(data) > 128*1024*1024 {
			return nil, errors.New("history backup exceeds limit")
		}
		var backup historyBackup
		if json.Unmarshal(data, &backup) != nil || backup.Version != 2 || backup.RoomID != st.RoomID || backup.WorkspaceID != st.WorkspaceID || !bytes.Equal(backup.Root, st.Root) {
			return nil, errors.New("history backup belongs to another workspace or protocol")
		}
		if keyPath == "" {
			keyPath = c.CryptoPath + ".history.key"
		}
		info, err := os.Lstat(keyPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("history recovery key must be an owner-only regular file")
		}
		oldKey, err := os.ReadFile(keyPath)
		if err != nil || len(oldKey) != 32 {
			return nil, errors.New("invalid history recovery key")
		}
		defer clear(oldKey)
		newKey, err := historyKey(c)
		if err != nil {
			return nil, err
		}
		defer clear(newKey)
		imported := map[string][]byte{}
		indexes := map[string][]byte{}
		for id, sealed := range backup.Archive {
			parts := strings.Split(id, ":")
			if len(parts) != 2 || !strings.HasPrefix(parts[0], "msg_") || len(parts[0]) != 36 || len(parts[1]) != 64 {
				return nil, errors.New("invalid history entry identity")
			}
			if _, err = hex.DecodeString(parts[0][4:] + parts[1]); err != nil {
				return nil, errors.New("invalid history entry identity")
			}
			aad := []byte(st.WorkspaceID + "/" + id)
			plain, err := e2ee.OpenLocal(oldKey, sealed, aad)
			if err != nil {
				return nil, errors.New("history backup authentication failed; check its recovery key")
			}
			var entry archiveEntry
			if json.Unmarshal(plain, &entry) != nil || len(entry.Key) != 32 || len(entry.Payload) > e2ee.MaxPlaintextBytes {
				return nil, errors.New("invalid archived content")
			}
			if _, exists := st.Archive[id]; exists {
				continue
			}
			imported[id], err = e2ee.SealLocal(newKey, plain, aad)
			if len(entry.Payload) > 0 {
				var body encryptedBody
				if json.Unmarshal(entry.Payload, &body) == nil {
					indexes[id] = historyIndex(newKey, body.Text)
				}
			}
			clear(plain)
			if err != nil {
				return nil, err
			}
		}
		if st.Archive == nil {
			st.Archive = map[string][]byte{}
		}
		for id, sealed := range imported {
			st.Archive[id] = sealed
		}
		if st.SearchIndex == nil {
			st.SearchIndex = map[string][]byte{}
		}
		for id, index := range indexes {
			st.SearchIndex[id] = index
		}
		return map[string]any{"restored_entries": len(imported), "session_state_restored": false}, nil
	})
}
func rotateMLS(ctx context.Context, c Config) (any, error) {
	return withCrypto(ctx, c, func(st *cryptoState) (any, error) {
		if err := ensureCrypto(ctx, c, st); err != nil {
			return nil, err
		}
		if st.Protocol != 2 || !bytes.Equal(st.Identity.SigningKey[32:], st.Root) {
			return nil, errors.New("use the MLS workspace creator to rotate the group")
		}
		old := st.ActiveRoster
		next := old
		next.Epoch++
		next.Previous = old.Hash()
		return changeMLSRoster(ctx, c, st, old, next, "", "")
	})
}
