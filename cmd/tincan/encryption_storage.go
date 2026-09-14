package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func privateTempPrefix(path string) string {
	// Scope to the target within its directory; another identity's active write
	// must never be removed by this identity's recovery.
	h := sha256.Sum256([]byte(filepath.Base(path)))
	return ".e2ee-" + hex.EncodeToString(h[:]) + "-"
}

// Call only while holding the target identity's process lock. All production
// privateJSON calls are serialized by withCrypto or newCrypto.
func cleanupPrivateTemps(path string, identity *e2ee.Identity) error {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := privateTempPrefix(path)
	removed := false
	for _, entry := range entries {
		name := entry.Name()
		owned := strings.HasPrefix(name, prefix)
		legacy := strings.TrimPrefix(name, ".e2ee-")
		if !owned && (identity == nil || legacy == name || legacy == "" || strings.Trim(legacy, "0123456789") != "") {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			if owned {
				return errors.New("insecure encryption temporary file")
			}
			continue
		}
		if !owned {
			f, err := os.Open(p)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			opened, statErr := f.Stat()
			if statErr != nil || !os.SameFile(info, opened) {
				f.Close()
				return errors.New("encryption temporary file changed during recovery")
			}
			owned = legacyTempIdentity(f, identity.Age)
			f.Close()
		}
		if owned {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return err
			}
			removed = true
		}
	}
	if removed {
		return syncPrivateDirectory(path)
	}
	return nil
}

// Old state serialization starts with {"identity":{"age":"...". Read only
// that bounded prefix so a truncated crash write can still be attributed,
// without loading private signing keys or an arbitrarily large archive.
func legacyTempIdentity(r io.Reader, age string) bool {
	if age == "" {
		return false
	}
	d := json.NewDecoder(io.LimitReader(r, 1024))
	for _, want := range []any{json.Delim('{'), "identity", json.Delim('{'), "age"} {
		got, err := d.Token()
		if err != nil || got != want {
			return false
		}
	}
	var got string
	return d.Decode(&got) == nil && got == age
}
