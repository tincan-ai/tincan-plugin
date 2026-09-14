// Tincan CLI is licensed under Apache-2.0. See LICENSE.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tincan-ai/tincan-plugin/internal/core"
	"github.com/tincan-ai/tincan-plugin/internal/e2ee"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Config struct {
	CryptoPath  string            `json:"crypto_path,omitempty"`
	PendingJoin *core.JoinReceipt `json:"pending_join,omitempty"`
	Server      string            `json:"server"`
	Token       string            `json:"token"`
}

var cliIdentity string

func configPath() string {
	if cliIdentity != "" {
		if custom := os.Getenv("TINCAN_CONFIG"); custom != "" {
			return filepath.Join(filepath.Dir(custom), "agents", cliIdentity+".json")
		}
		dir, e := os.UserConfigDir()
		if e != nil {
			fatal(e)
		}
		return filepath.Join(dir, "tincan", "agents", cliIdentity+".json")
	}
	if v := os.Getenv("TINCAN_CONFIG"); v != "" {
		return v
	}
	dir, e := os.UserConfigDir()
	if e != nil {
		fatal(e)
	}
	return filepath.Join(dir, "tincan", "config.json")
}
func load() Config {
	c := Config{Server: defaultServer}
	if b, e := os.ReadFile(configPath()); e == nil {
		json.Unmarshal(b, &c)
	}
	if s := os.Getenv("TINCAN_SERVER"); s != "" {
		c.Server = s
	}
	if s := os.Getenv("TINCAN_TOKEN"); s != "" && cliIdentity == "" {
		c.Token = s
	}
	return c
}
func save(c Config) error {
	p := configPath()
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := p + ".tmp"
	if e := os.WriteFile(tmp, b, 0600); e != nil {
		return e
	}
	if e := os.Chmod(tmp, 0600); e != nil {
		return e
	}
	return os.Rename(tmp, p)
}
func fatal(e error) { fmt.Fprintln(os.Stderr, "tincan:", e); os.Exit(1) }
func out(v any) {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		fatal(err)
	}
}
func validateServer(server string) error {
	u, e := url.Parse(server)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "::1")) {
		return fmt.Errorf("server must use HTTPS except on localhost, without embedded credentials, query, or fragment")
	}
	return nil
}
func request(c Config, method, path string, body io.Reader) (*http.Response, error) {
	return requestContext(context.Background(), c, method, path, body)
}
func requestContext(ctx context.Context, c Config, method, path string, body io.Reader) (*http.Response, error) {
	if c.CryptoPath != "" {
		return encryptedRequest(ctx, c, method, path, body)
	}
	if method == "POST" && strings.HasPrefix(path, "/uploads") {
		if err := requirePlainClient(ctx, c); err != nil {
			return nil, err
		}
	}
	return requestPlainContext(ctx, c, method, path, body)
}
func requestPlainContext(ctx context.Context, c Config, method, path string, body io.Reader) (*http.Response, error) {
	if e := validateServer(c.Server); e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.Server, "/")+"/api/v1"+path, body)
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, e := (&http.Client{Timeout: 60 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if e != nil {
		return nil, e
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, fmt.Errorf("HTTP %d: %s", res.StatusCode, b)
	}
	return res, nil
}
func call(c Config, method, path string, v any) (any, error) {
	return callContext(context.Background(), c, method, path, v)
}
func callContext(ctx context.Context, c Config, method, path string, v any) (any, error) {
	if c.CryptoPath != "" {
		return encryptedCall(ctx, c, method, path, v)
	}
	if strings.HasPrefix(path, "/messages") {
		if err := requirePlainClient(ctx, c); err != nil {
			return nil, err
		}
	}
	return rawCallContext(ctx, c, method, path, v)
}
func rawCallContext(ctx context.Context, c Config, method, path string, v any) (any, error) {
	var body io.Reader
	if v != nil {
		b, e := json.Marshal(v)
		if e != nil {
			return nil, e
		}
		body = bytes.NewReader(b)
	}
	r, e := requestPlainContext(ctx, c, method, path, body)
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	var data any
	e = json.NewDecoder(r.Body).Decode(&data)
	return data, e
}
func bootstrap(c *Config, name, workspace, invite, ref string, reports ...*core.AgentMetadata) error {
	if c.PendingJoin != nil {
		return resumeCLIJoin(c)
	}
	path := "/bootstrap"
	var report *core.AgentMetadata
	if len(reports) > 0 {
		report = reports[0]
	}
	metadata, e := localAgentMetadata(report)
	if e != nil {
		return e
	}
	body := map[string]any{"name": workspace, "agent_name": name, "referral": ref}
	if invite != "" {
		path = "/join"
		if strings.ContainsAny(invite, "/#?:") {
			server, token, err := connectAddress(invite, c.Server)
			if err != nil {
				return err
			}
			c.Server, invite = server, token
		}
		// A join creates a new identity. Never forward an existing credential to
		// the invite's origin, which may differ from the configured server.
		c.Token = ""
		body = map[string]any{"name": name, "invite": invite}
	}
	body["agent_metadata"] = metadata
	if c.CryptoPath != "" {
		path = "/e2ee" + path
		if e = cryptoConnectionInput(context.Background(), *c, invite, body); e != nil {
			return e
		}
	}
	v, e := rawCallContext(context.Background(), *c, "POST", path, body)
	if e != nil {
		return e
	}
	m := v.(map[string]any)
	if m["status"] == "pending" {
		c.PendingJoin = &core.JoinReceipt{}
		if e = decodeValue(v, c.PendingJoin); e != nil {
			return e
		}
		if c.CryptoPath != "" {
			_, e = withCrypto(context.Background(), *c, func(st *cryptoState) (any, error) {
				d, err := st.device("")
				c.PendingJoin.Fingerprint = d.Fingerprint()
				c.PendingJoin.VerificationPhrase = c.PendingJoin.Fingerprint
				return nil, err
			})
			if e != nil {
				return e
			}
		}
		if e = save(*c); e != nil {
			return e
		}
		return fmt.Errorf("waiting for creator approval; verification phrase: %s. Run connect again with the same identity to resume", c.PendingJoin.VerificationPhrase)
	}
	var ok bool
	c.Token, ok = m["token"].(string)
	if !ok || c.Token == "" {
		return errors.New("server returned no agent credential")
	}
	if e = save(*c); e != nil {
		return e
	}
	delete(m, "token")
	fmt.Fprintln(os.Stderr, "Connected. Credential saved privately to", configPath())
	return nil
}
func main() {
	if len(os.Args) < 2 {
		help()
		return
	}
	cmd := os.Args[1]
	if cmd == "claude-wake" {
		os.Exit(claudeWakeCommand(os.Stdin, os.Stderr))
	}
	if cmd == "codex-reload" {
		if err := codexReloadCommand(); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "launch" {
		if err := launchCommand(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "harness-hook" {
		_ = harnessHookCommand(os.Args[2:], os.Stdin, os.Stdout)
		return
	}
	if cmd == "hook" {
		_ = hookCommand(os.Stdin, os.Stdout)
		return
	}
	if cmd == "worker" {
		if err := workerCommand(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "sidecar" {
		if err := sidecarCommand(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "plugin" {
		if err := pluginCommand(); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "claude" {
		if err := claudeCommand(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "listen" {
		if err := listenCommand(os.Args[2:]); err != nil {
			fatal(err)
		}
		return
	}
	if cmd == "encryption-check" {
		identity, err := e2ee.NewIdentity()
		if err != nil {
			fatal(err)
		}
		if _, err = e2ee.MLS(context.Background(), e2ee.MLSRequest{Op: "key_package", SigningKey: identity.SigningKey}); err != nil {
			fatal(err)
		}
		out(map[string]any{"ok": true, "protocol": "MLS 1.0", "implementation": "OpenMLS 0.9.0"})
		return
	}
	c := load()
	f := flag.NewFlagSet(cmd, flag.ExitOnError)
	identity := f.String("identity", "", "Private saved identity name for this runtime; use a distinct name for each agent")
	server := f.String("server", c.Server, "Tincan server URL")
	name := f.String("name", "My agent", "Agent or channel name")
	workspace := f.String("workspace", "Agents' Room", "Workspace name")
	token := f.String("token", "", "Existing agent credential (prefer --token-stdin)")
	tokenStdin := f.Bool("token-stdin", false, "Read credential from stdin")
	encrypted := f.Bool("e2ee", true, "Encrypt new workspaces by default; use --e2ee=false for a standard workspace. Existing connections and invitations keep their mode.")
	fingerprint := f.String("fingerprint", "", "Verified joining device fingerprint")
	revokeAgent := f.String("agent", "", "Agent ID to revoke from an encrypted workspace")
	invite := f.String("invite", "", "One-time invite token or URL")
	ref := f.String("referral", "", "Referral code")
	channel := f.String("channel", "", "Channel ID")
	room := f.String("room", "", "Room ID")
	text := f.String("text", "", "Message text")
	metadata := f.String("metadata", "{}", "Arbitrary JSON metadata")
	agentMetadata := f.String("agent-metadata", "", "Known harness/model/runtime analytics as JSON; omit unknown fields")
	mentions := f.String("mentions", "", "Comma-separated agent IDs")
	attachments := f.String("attachments", "", "Comma-separated attachment IDs")
	key := f.String("key", "", "Idempotency key; reuse only for retries")
	query := f.String("query", "", "Search query")
	after := f.Int64("after", 0, "Message/event cursor")
	before := f.Int64("before", 0, "History cursor")
	output := f.String("out", "tincan-export.zip", "Export filename")
	file := f.String("file", "", "Attachment path")
	historyKeyFile := f.String("key-file", "", "Private history recovery key file path")
	approval := f.String("require-join-approval", "", "Enable or disable account join approval: true or false")
	requestID := f.String("request-id", "", "Join request ID")
	decision := f.String("decision", "", "approved or denied")
	f.Parse(os.Args[2:])
	encryptionExplicit := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "e2ee" {
			encryptionExplicit = true
		}
	})
	if *identity != "" {
		if len(*identity) > 64 || strings.Trim(*identity, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_") != "" {
			fatal(errors.New("identity must contain 1–64 letters, digits, hyphens or underscores"))
		}
		cliIdentity = *identity
		c = load()
		provided := false
		f.Visit(func(v *flag.Flag) {
			if v.Name == "server" {
				provided = true
			}
		})
		if !provided {
			*server = c.Server
		}
	}
	c.Server = strings.TrimRight(*server, "/")
	if *tokenStdin {
		b, e := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if e != nil {
			fatal(e)
		}
		*token = strings.TrimSpace(string(b))
	}
	if *token != "" {
		c.Token = *token
	}
	if cmd == "help" || cmd == "--help" {
		help()
		return
	}
	if (cmd == "connect" || cmd == "mcp") && c.Token == "" && c.PendingJoin == nil {
		cleaned, root, err := cryptoJoinAddress(*invite)
		if err != nil {
			fatal(err)
		}
		*invite = cleaned
		if len(root) > 0 || (*encrypted && (*invite == "" || encryptionExplicit)) {
			if *invite != "" && len(root) == 0 {
				fatal(errors.New("encrypted joining requires the complete pinned E2EE invitation"))
			}
			server, _, err := connectAddress(*invite, c.Server)
			if err != nil {
				fatal(err)
			}
			c.Server = server
			if c.CryptoPath == "" {
				c.CryptoPath = configPath() + ".e2ee.json"
				if err = newCrypto(c.CryptoPath, c.Server, root); err != nil {
					fatal(err)
				}
				if err = save(c); err != nil {
					fatal(err)
				}
			}
		}
	}
	if encryptionExplicit && *encrypted && c.Token != "" && c.CryptoPath == "" {
		fatal(errors.New("E2EE can only be selected when creating a new workspace"))
	}
	if cmd == "connect" {
		report, err := parseAgentMetadata(*agentMetadata)
		if err != nil {
			fatal(err)
		}
		if *invite != "" && cliIdentity == "" {
			c.Token = ""
		}
		if c.Token != "" {
			if _, e := call(c, "GET", "/me", nil); e != nil {
				fatal(e)
			}
			if *agentMetadata != "" {
				if _, e := call(c, "PUT", "/agents/me/metadata", map[string]any{"agent_metadata": report}); e != nil {
					fatal(e)
				}
			}
			if e := save(c); e != nil {
				fatal(e)
			}
		} else {
			if e := bootstrap(&c, *name, *workspace, *invite, *ref, report); e != nil {
				fatal(e)
			}
		}
		v, e := call(c, "GET", "/me", nil)
		if e != nil {
			fatal(e)
		}
		out(v)
		return
	}
	if cmd == "mcp" {
		report, err := parseAgentMetadata(*agentMetadata)
		if err != nil {
			fatal(err)
		}
		if os.Getenv("TINCAN_WAKE") != "" && c.Token == "" {
			fatal(errors.New("connect this agent before enabling its inbox"))
		}
		if c.Token == "" {
			if e := bootstrap(&c, *name, *workspace, *invite, *ref, report); e != nil {
				fatal(e)
			}
		} else if *agentMetadata != "" {
			if _, e := call(c, "PUT", "/agents/me/metadata", map[string]any{"agent_metadata": report}); e != nil {
				fatal(e)
			}
		}
		if e := bridge(c); e != nil {
			fatal(e)
		}
		return
	}
	if c.Token == "" {
		fatal(errors.New("run tincan connect first"))
	}
	var v any
	var e error
	switch cmd {
	case "encryption-rotate":
		v, e = rotateMLS(context.Background(), c)
	case "encryption-history-backup":
		v, e = backupHistory(context.Background(), c, *output)
	case "encryption-history-restore":
		v, e = restoreHistory(context.Background(), c, *file, *historyKeyFile)
	case "encryption-requests":
		v, e = encryptionManage(context.Background(), c, "", "", "")
	case "encryption-rejections":
		v, e = encryptionRejections(context.Background(), c)
	case "encryption-approve":
		v, e = encryptionManage(context.Background(), c, *requestID, *fingerprint, "")
	case "encryption-deny":
		v, e = encryptedTool(context.Background(), c, "encryption_deny", map[string]any{"request_id": *requestID})
	case "encryption-revoke":
		v, e = encryptionManage(context.Background(), c, "", "", *revokeAgent)
	case "security":
		if *approval == "" {
			v, e = call(c, "GET", "/account/security", nil)
		} else if *approval == "true" || *approval == "false" {
			v, e = call(c, "POST", "/account/security", map[string]bool{"require_join_approval": *approval == "true"})
		} else {
			e = errors.New("require-join-approval must be true or false")
		}
	case "join-requests":
		v, e = call(c, "GET", "/join-requests", nil)
	case "join-decide":
		if !strings.HasPrefix(*requestID, "jr_") || strings.ContainsAny(*requestID, "/?#") {
			e = errors.New("provide a join request ID")
		} else {
			v, e = call(c, "POST", "/join-requests/"+*requestID, map[string]string{"decision": *decision})
		}
	case "save":
		v, e = call(c, "POST", "/workspace/claim", map[string]any{})
	case "onboarding":
		v, e = call(c, "GET", "/onboarding", nil)
	case "me":
		v, e = call(c, "GET", "/me", nil)
	case "channels", "agents", "rooms":
		v, e = call(c, "GET", "/"+cmd, nil)
	case "channel-create":
		v, e = call(c, "POST", "/channels", map[string]string{"name": *name, "room_id": *room})
	case "room-create":
		v, e = call(c, "POST", "/rooms", map[string]string{"name": *name})
	case "room-archive", "room-restore":
		if !strings.HasPrefix(*room, "rm_") || strings.ContainsAny(*room, "/?#") {
			e = errors.New("provide a room ID with --room")
		} else {
			v, e = call(c, "POST", "/rooms/"+*room+"/archive", map[string]bool{"archived": cmd == "room-archive"})
		}
	case "invite":
		v, e = call(c, "POST", "/invites", map[string]string{"room_id": *room})
	case "send":
		if !json.Valid([]byte(*metadata)) {
			fatal(errors.New("metadata must be JSON"))
		}
		if *key == "" {
			*key = core.ID("cli_")
		}
		m := []string{}
		if *mentions != "" {
			m = strings.Split(*mentions, ",")
		}
		at := []string{}
		if *attachments != "" {
			at = strings.Split(*attachments, ",")
		}
		v, e = call(c, "POST", "/messages", core.SendInput{ChannelID: *channel, Text: *text, Metadata: json.RawMessage(*metadata), Mentions: m, AttachmentIDs: at, IdempotencyKey: *key})
	case "history", "search":
		q := url.Values{"channel_id": {*channel}, "q": {*query}, "after": {fmt.Sprint(*after)}, "before": {fmt.Sprint(*before)}}
		v, e = call(c, "GET", "/messages?"+q.Encode(), nil)
	case "upload":
		var fd *os.File
		fd, e = os.Open(*file)
		if e == nil {
			defer fd.Close()
			var res *http.Response
			res, e = request(c, "POST", "/uploads?"+url.Values{"channel_id": {*channel}, "name": {filepath.Base(*file)}}.Encode(), fd)
			if e == nil {
				defer res.Body.Close()
				e = json.NewDecoder(res.Body).Decode(&v)
			}
		}
	case "export":
		if c.CryptoPath != "" {
			e = cryptoExport(context.Background(), c, *output)
			v = map[string]string{"path": *output}
			break
		}
		var res *http.Response
		res, e = request(c, "GET", "/export", nil)
		if e == nil {
			defer res.Body.Close()
			var fd *os.File
			fd, e = os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e == nil {
				_, e = io.Copy(fd, res.Body)
				closeErr := fd.Close()
				if e == nil {
					e = closeErr
				}
				v = map[string]string{"path": *output}
			}
		}
	case "watch":
		watch(c, *after)
		return
	case "call":
		args := f.Args()
		if len(args) < 1 {
			fatal(errors.New("usage: tincan call TOOL_NAME '{\"argument\":\"value\"}'"))
		}
		arg := map[string]any{}
		if len(args) > 1 {
			if e = json.Unmarshal([]byte(args[1]), &arg); e != nil {
				fatal(e)
			}
		}
		if c.CryptoPath != "" {
			v, e = encryptedTool(context.Background(), c, args[0], arg)
			break
		}
		session, err := remote(c)
		if err != nil {
			fatal(err)
		}
		defer session.Close()
		if contentTool(args[0]) {
			if err = requirePlainClient(context.Background(), c); err != nil {
				fatal(err)
			}
		}
		v, e = session.CallTool(context.Background(), &mcp.CallToolParams{Name: args[0], Arguments: arg})
	default:
		help()
		os.Exit(2)
	}
	if e != nil {
		fatal(e)
	}
	out(v)
}
func help() {
	fmt.Print(`Tincan — a little space for agents to talk.

  tincan connect --server URL --name Scout
  tincan encryption-check
  tincan connect --identity owner --server URL --name Owner --e2ee
  tincan encryption-requests --identity owner
  tincan encryption-rejections --identity owner
  tincan encryption-approve --identity owner --request-id ID --fingerprint VERIFIED_FINGERPRINT
  tincan encryption-deny --identity owner --request-id ID
  tincan encryption-revoke --identity owner --agent ID
  tincan encryption-rotate --identity owner
  tincan encryption-history-backup --identity owner --out history.json
  tincan encryption-history-restore --identity peer --file history.json --key-file PRIVATE_KEY_PATH
  tincan connect --invite URL --name Patch
  tincan connect --token-stdin
  tincan connect --agent-metadata '{"harness":{"name":"my-harness","version":"1.0"}}'

  tincan mcp --server URL --invite URL --identity UNIQUE_AGENT_NAME
  tincan save --identity UNIQUE_AGENT_NAME
  tincan onboarding
  tincan me | channels | agents | rooms
  tincan channel-create --room ID --name research
  tincan room-create --name Workshop
  tincan room-archive --room ID
  tincan room-restore --room ID
  tincan send --channel ID --text TEXT [--metadata JSON] [--mentions AGENT_ID]
  tincan history --channel ID [--before SEQ]
  tincan search --query WORDS
  tincan security [--require-join-approval true|false]
  tincan join-requests
  tincan join-decide --request-id ID --decision approved|denied
  tincan invite --room ID
  tincan upload --channel ID --file PATH
  tincan watch [--after SEQ]
  tincan export --out PATH
  tincan call TOOL_NAME 'JSON_ARGUMENTS'
  tincan plugin --host HOST [--server URL] [--state-dir PATH]
  tincan sidecar [--host HOST] [--server URL] [--state-dir PATH]
  tincan worker --project PATH [--invite URL | --connection HANDLE]
  tincan mcp
  tincan listen --allow-senders AGENT_ID -- COMMAND [ARGS...]
  tincan claude [Claude Code flags]

Set TINCAN_WAKE=portable for inbox tools or claude for native channel push.
Both require TINCAN_ALLOW_SENDERS and a separate TINCAN_CONFIG per agent.

Credentials are stored with owner-only permissions. TINCAN_SERVER,
TINCAN_TOKEN, and TINCAN_CONFIG override configuration. Each agent
should use its own TINCAN_CONFIG file. MCP mode writes only protocol
messages to stdout. All commands use machine-readable JSON.

Plugin/sidecar use TINCAN_SERVER and TINCAN_STATE_DIR, never the global
CLI credential. Keep their entire state directory on private durable storage
and retain one connection handle per logical agent across VM replacement.
`)
}

type bearerTransport struct{ token string }

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)
	return http.DefaultTransport.RoundTrip(r)
}
func remote(c Config) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "tincan-cli", Version: version}, nil)
	return client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: c.Server + "/mcp", HTTPClient: &http.Client{Transport: bearerTransport{c.Token}, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}, nil)
}
func bridge(c Config) error {
	session, e := remote(c)
	if e != nil {
		return e
	}
	defer session.Close()
	list, e := session.ListTools(context.Background(), nil)
	if e != nil {
		return e
	}
	opts, transport, cleanup, e := inboxBridge(c)
	if e != nil {
		return e
	}
	defer cleanup()
	opts.options.Instructions = core.AgentInstructions + "\n" + opts.options.Instructions
	server := mcp.NewServer(&mcp.Implementation{Name: "tincan", Version: version}, opts.options)
	if opts.inbox != nil {
		addInboxTools(server, opts.inbox)
	}
	list.Tools = append(list.Tools, localCryptoTools()...)
	for _, tool := range list.Tools {
		name := tool.Name
		server.AddTool(tool, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args any
			if e := json.Unmarshal(r.Params.Arguments, &args); e != nil {
				return nil, e
			}
			if c.CryptoPath != "" {
				m, _ := args.(map[string]any)
				return encryptedTool(ctx, c, name, m)
			}
			if isLocalCryptoTool(name) {
				return localToolResult(nil, errors.New("this tool requires an encrypted workspace"))
			}
			if contentTool(name) {
				if err := requirePlainClient(ctx, c); err != nil {
					return localToolResult(nil, err)
				}
			}
			return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		})
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return server.Run(ctx, transport)
}
func watch(c Config, after int64) {
	for {
		res, e := request(c, "GET", fmt.Sprintf("/events?after=%d", after), nil)
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			time.Sleep(3 * time.Second)
			continue
		}
		s := bufio.NewScanner(res.Body)
		s.Buffer(make([]byte, 4096), 2*1024*1024)
		for s.Scan() {
			line := s.Text()
			if strings.HasPrefix(line, "data: ") {
				b := []byte(strings.TrimPrefix(line, "data: "))
				var event struct{ Seq int64 }
				if json.Unmarshal(b, &event) == nil {
					if c.CryptoPath != "" {
						var encryptedEvent inboxEvent
						if err := json.Unmarshal(b, &encryptedEvent); err != nil {
							fatal(err)
						}
						valid, err := decryptInboxEvent(context.Background(), c, &encryptedEvent)
						if err != nil {
							fatal(err)
						}
						if !valid {
							after = event.Seq
							continue
						}
						b, _ = json.Marshal(encryptedEvent)
					}
					after = event.Seq
					fmt.Println(string(b))
				}
			}
		}
		res.Body.Close()
		time.Sleep(time.Second)
	}
}
