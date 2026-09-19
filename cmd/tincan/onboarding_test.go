package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise the real CLI entry point in a fresh process, including fatal errors.
func TestCLIConnectProcess(t *testing.T) {
	raw := os.Getenv("TINCAN_TEST_CONNECT_PROCESS")
	if raw == "" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"tincan"}, args...)
	main()
	os.Exit(0)
}

func TestCLIJoinSavesCredentialBeforeVerificationAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "muse.json")
	t.Setenv("TINCAN_CONFIG", path)
	t.Setenv("TINCAN_TOKEN", "")
	const credential = "fixture-private-muse-token"
	var joins atomic.Int32
	var failVerification atomic.Bool
	failVerification.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/join":
			if joins.Add(1) != 1 {
				http.Error(w, "invite already consumed", http.StatusGone)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"token": credential, "room_name": "Muse test room"})
		case "/api/v1/me":
			data, err := os.ReadFile(path)
			var saved Config
			if err != nil || json.Unmarshal(data, &saved) != nil || saved.Token != credential {
				t.Error("verification ran before the credential was saved")
			}
			if r.Header.Get("Authorization") != "Bearer "+credential {
				t.Error("verification did not authenticate as the saved agent")
			}
			if failVerification.Load() {
				http.Error(w, "temporary verification failure", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"room_name": "Muse test room"})
		default:
			t.Errorf("unexpected connection request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("TINCAN_SERVER", server.URL)
	run := func(args ...string) ([]byte, error) {
		t.Helper()
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestCLIConnectProcess$")
		cmd.Env = append(os.Environ(), "TINCAN_TEST_CONNECT_PROCESS="+string(encoded))
		output, err := cmd.CombinedOutput()
		if strings.Contains(string(output), credential) {
			t.Fatal("CLI printed the private credential")
		}
		return output, err
	}
	if _, err := run("connect", "--invite", server.URL+"/join#one-use", "--name", "Muse"); err == nil {
		t.Fatal("expected the post-join verification failure")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("failed verification did not leave a private saved connection")
	}
	failVerification.Store(false)
	if output, err := run("me"); err != nil || !strings.Contains(string(output), "Muse test room") {
		t.Fatal("a fresh command could not load and verify the saved connection")
	}
	if _, err := run("connect"); err != nil {
		t.Fatal("reconnecting failed to reuse the saved credential")
	}
	if joins.Load() != 1 {
		t.Fatal("recovery attempted to redeem the invite again")
	}
}

func TestNamedRuntimeIdentityPersistence(t *testing.T) {
	old := cliIdentity
	t.Cleanup(func() { cliIdentity = old })
	root := t.TempDir()
	t.Setenv("TINCAN_CONFIG", filepath.Join(root, "config.json"))
	t.Setenv("TINCAN_TOKEN", "unrelated-global-credential")
	t.Setenv("TINCAN_SERVER", "")
	cliIdentity = "first-runtime"
	if load().Token != "" {
		t.Fatal("new runtime inherited global identity")
	}
	first := Config{Server: "https://tincan.example", Token: "first-private-credential"}
	if err := save(first); err != nil {
		t.Fatal(err)
	}
	if got := load(); !reflect.DeepEqual(got, first) {
		t.Fatal("runtime failed to resume")
	}
	info, err := os.Stat(configPath())
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential file not private", err)
	}
	cliIdentity = "second-runtime"
	if load().Token != "" {
		t.Fatal("second runtime reused first identity")
	}
	if err := save(Config{Server: first.Server, Token: "second-private-credential"}); err != nil {
		t.Fatal(err)
	}
	cliIdentity = "first-runtime"
	if got := load(); !reflect.DeepEqual(got, first) {
		t.Fatal("second runtime overwrote first identity")
	}
	cliIdentity = ""
	if load().Token != "unrelated-global-credential" {
		t.Fatal("legacy global configuration changed")
	}
}
