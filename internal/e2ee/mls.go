package e2ee

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// MLS is a private pipe interface to pinned OpenMLS. State and signing material
// are never passed on a command line, in environment variables, or over HTTP.
type MLSRequest struct {
	Op         string          `json:"op"`
	State      json.RawMessage `json:"state,omitempty"`
	SigningKey []byte          `json:"signing_key"`
	Group      string          `json:"group"`
	Data       []byte          `json:"data,omitempty"`
	AAD        []byte          `json:"aad,omitempty"`
	Member     []byte          `json:"member,omitempty"`
}
type MLSResponse struct {
	State   json.RawMessage `json:"state"`
	Data    []byte          `json:"data"`
	Welcome []byte          `json:"welcome"`
	Sender  []byte          `json:"sender"`
	AAD     []byte          `json:"aad"`
	Epoch   uint64          `json:"epoch"`
	Members [][]byte        `json:"members"`
}

var helperOnce sync.Once
var helperRuntime wazero.Runtime
var helperModule wazero.CompiledModule
var helperError error

func mlsModulePath() (string, error) {
	if p := os.Getenv("TINCAN_MLS_MODULE"); p != "" {
		if !filepath.IsAbs(p) {
			return "", errors.New("TINCAN_MLS_MODULE must be absolute")
		}
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	for _, dir := range []string{filepath.Dir(exe), filepath.Dir(filepath.Dir(exe))} {
		p := filepath.Join(dir, "tincan-mls.wasm")
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, nil
		}
	}
	// Only source-tree tests may invoke the build tool. Installed clients never
	// download or compile code and require the module shipped in their release.
	if strings.HasSuffix(exe, ".test") || strings.HasSuffix(exe, ".test.exe") {
		dir, _ := os.Getwd()
		for dir != filepath.Dir(dir) {
			script := filepath.Join(dir, "scripts", "build-mls.py")
			if _, err := os.Stat(script); err == nil {
				p := filepath.Join(dir, ".tools", "mls", "wasm32-wasip1", "release", "tincan-mls.wasm")
				if _, err := os.Stat(p); err != nil {
					cmd := exec.Command("python3", script)
					cmd.Dir = dir
					if err := cmd.Run(); err != nil {
						return "", fmt.Errorf("build MLS test module: %w", err)
					}
				}
				_ = os.Setenv("TINCAN_MLS_MODULE", p)
				return p, nil
			}
			dir = filepath.Dir(dir)
		}
	}
	return "", errors.New("MLS module missing; install a complete Tincan release")
}
func initMLS() error {
	helperOnce.Do(func() {
		p, err := mlsModulePath()
		if err != nil {
			helperError = err
			return
		}
		module, err := os.ReadFile(p)
		if err != nil {
			helperError = errors.New("MLS module unavailable")
			return
		}
		ctx := context.Background()
		helperRuntime = wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).WithCloseOnContextDone(true))
		if _, err = wasi_snapshot_preview1.Instantiate(ctx, helperRuntime); err != nil {
			helperError = err
			return
		}
		helperModule, helperError = helperRuntime.CompileModule(ctx, module)
	})
	return helperError
}

type boundedMLSOutput struct{ bytes.Buffer }

// Only the adapter's explicit application rejection is safe to quarantine.
// Missing state, traps, cancellation, and module/IO failures must stay fatal.
var ErrMLSApplicationRejected = errors.New("invalid MLS application message")

func (w *boundedMLSOutput) Write(p []byte) (int, error) {
	if w.Len()+len(p) > 64*1024*1024 {
		return 0, errors.New("MLS output exceeds limit")
	}
	return w.Buffer.Write(p)
}
func MLS(ctx context.Context, in MLSRequest) (MLSResponse, error) {
	var out MLSResponse
	if err := initMLS(); err != nil {
		return out, err
	}
	data, err := json.Marshal(in)
	if err != nil {
		return out, err
	}
	defer clear(data)
	if len(data) > 64*1024*1024 {
		return out, errors.New("MLS state exceeds limit")
	}
	var output boundedMLSOutput
	defer func() { clear(output.Bytes()) }()
	// A fresh sandbox per operation: private pipes, cryptographic randomness and
	// clocks only. No filesystem mounts, networking, environment or shared memory.
	config := wazero.NewModuleConfig().WithName("").WithArgs("tincan-mls").
		WithStdin(bytes.NewReader(data)).WithStdout(&output).WithStderr(io.Discard).
		WithRandSource(rand.Reader).WithSysWalltime().WithSysNanotime()
	module, err := helperRuntime.InstantiateModule(ctx, helperModule, config)
	if module != nil {
		defer module.Close(context.Background())
		if memory := module.Memory(); memory != nil {
			if b, ok := memory.Read(0, memory.Size()); ok {
				clear(b)
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		var exit *sys.ExitError
		if in.Op == "decrypt" && errors.As(err, &exit) && exit.ExitCode() == 2 {
			return out, ErrMLSApplicationRejected
		}
		return out, fmt.Errorf("MLS %s rejected; synchronize or rejoin this device", in.Op)
	}
	if json.Unmarshal(output.Bytes(), &out) != nil {
		return out, errors.New("invalid MLS module response")
	}
	return out, nil
}

// Payload keys travel only inside MLS. The archive key is separate from live
// MLS state; archives intentionally preserve history and do not claim FS.
func SealLocal(key, data, context []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("invalid encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, data, context), nil
}
func OpenLocal(key, data, context []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, errors.New("invalid encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < aead.NonceSize()+aead.Overhead() {
		return nil, errors.New("invalid encrypted payload")
	}
	return aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], context)
}
func PayloadHash(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }
func (e Envelope) MLSContext() []byte {
	e.Ciphertext, e.Capsule, e.Signature = nil, nil, nil
	b, _ := json.Marshal(e)
	return b
}
func (e *Envelope) SignMLS(i Identity) error {
	copy := *e
	copy.Ciphertext, copy.Signature = nil, nil
	sig, err := sign(i.SigningKey, "tincan-message-v2", copy)
	e.Signature = sig
	return err
}
func (e Envelope) verifyMLS(r Roster, d Device, ok bool, sig []byte) error {
	limit := MaxPlaintextBytes + 64
	if e.Kind == "file" {
		limit = MaxFileBytes + 2048
	} else if e.Kind != "message" {
		return errors.New("unknown encrypted payload kind")
	}
	if !ok || r.Protocol != 2 || e.WorkspaceID != r.WorkspaceID || e.Epoch != r.Epoch || e.RosterHash != r.Hash() || e.ChannelID == "" || len(e.Key) < 1 || len(e.Key) > 128 || len(e.Capsule) < 32 || len(e.Capsule) > 128*1024 || len(e.PayloadHash) != 64 || len(e.Ciphertext) > limit {
		return errors.New("invalid MLS envelope context")
	}
	if len(e.Ciphertext) > 0 && PayloadHash(e.Ciphertext) != e.PayloadHash {
		return errors.New("encrypted payload hash mismatch")
	}
	e.Ciphertext = nil
	if !verify(d.SigningKey, "tincan-message-v2", e, sig) {
		return errors.New("invalid MLS envelope signature")
	}
	return nil
}
