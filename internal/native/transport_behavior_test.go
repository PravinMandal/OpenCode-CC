package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// These tests run the real Bun sidecar against a fake upstream. The Go unit
// tests only grep the script text; that passed while the kill-switch note was
// missing and while a shell-only client still kept tools Go could not execute.
// Skip when Bun is unavailable so the Go suite stays runnable everywhere.
func testBun(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("OPENCODE_CC_BUN"); p != "" {
		return p
	}
	if p, err := exec.LookPath("bun"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".local", "lib", "opencode-native", "bun")
	if _, err := os.Stat(p); err == nil {
		return p
	}
	t.Skip("bun not installed")
	return ""
}

type sidecar struct {
	cmd  *exec.Cmd
	port int
	out  *bytes.Buffer
}

func startSidecar(t *testing.T, bun, identityDir string, extraEnv ...string) *sidecar {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	noCapture := filepath.Join(t.TempDir(), "no-capture.mjs")
	captureStub := `const path = process.env.BRIDGE_IDENTITY;
const identity = JSON.parse(await Bun.file(path).text());
identity.capturedAt = new Date().toISOString();
try {
  const auth = JSON.parse(await Bun.file(process.env.BRIDGE_AUTH_JSON).text());
  identity.headers ??= {};
  identity.headers["x-opencode-org-id"] = "org-" + auth.opencode.key;
} catch {}
await Bun.write(path, JSON.stringify(identity));
`
	if err := os.WriteFile(noCapture, []byte(captureStub), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + binDir,
		"HOME=" + os.Getenv("HOME"),
		"BRIDGE_IDENTITY_DIR=" + identityDir,
		"BRIDGE_IDENTITY=" + filepath.Join(identityDir, "identity.json"),
		"BRIDGE_TOKEN=test-token",
		"BRIDGE_PORT=0",
		"BRIDGE_HOST=127.0.0.1",
		"BRIDGE_CAPTURE=" + noCapture,
		"BRIDGE_CREDENTIAL_DB=" + filepath.Join(identityDir, "no-credential.db"),
		"BRIDGE_AUTH_JSON=" + filepath.Join(identityDir, "no-auth.json"),
		"BRIDGE_CAPTURE_TIMEOUT_MS=1000",
		"BRIDGE_SESSION_STORE=" + filepath.Join(identityDir, "sessions.json"),
		"BRIDGE_TRACE_DIR=" + filepath.Join(identityDir, "trace"),
		"BRIDGE_RESET_SCRIPT=/bin/true",
		"BRIDGE_RESET_COOLDOWN_MS=999999999",
		"BRIDGE_RESET_STATE=" + filepath.Join(identityDir, "reset-state.json"),
		"BRIDGE_RESET_LOCK=" + filepath.Join(identityDir, "reset.lock"),
	}
	env = append(env, extraEnv...)

	cmd := exec.CommandContext(ctx, bun, "transport.mjs")
	cmd.Dir = "."
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sidecar: %v", err)
	}
	s := &sidecar{cmd: cmd, out: &stderr}

	ports := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var ev struct {
				Event string `json:"event"`
				Port  int    `json:"port"`
			}
			if json.Unmarshal(scanner.Bytes(), &ev) == nil && ev.Event == "listening" && ev.Port > 0 {
				select {
				case ports <- ev.Port:
				default:
				}
			}
		}
	}()

	select {
	case s.port = <-ports:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("sidecar did not announce a port; stderr=%s", stderr.String())
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return s
}

type upstreamCapture struct {
	mu      sync.Mutex
	last    []byte
	headers http.Header
	ch      chan []byte
}

func newFakeUpstream(t *testing.T) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	c := &upstreamCapture{ch: make(chan []byte, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastSessionMu.Lock()
		lastSessionValue = r.Header.Get("x-opencode-session")
		lastSessionMu.Unlock()
		c.mu.Lock()
		c.last = body
		c.headers = r.Header.Clone()
		c.mu.Unlock()
		select {
		case c.ch <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(srv.Close)
	return srv, c
}

func tool(name string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":       name,
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		},
	}
}

func tools(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, tool(name))
	}
	return out
}

func writeIdentity(t *testing.T, dir, endpoint string, names []string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ident := map[string]any{
		"version":    "test-version",
		"capturedAt": time.Now().UTC().Format(time.RFC3339),
		"model":      "primary-model",
		"endpoint":   endpoint,
		"system":     "- Model ID: primary-model\n<env>\n  Working directory: /capture\n</env>\nToday's date: old",
		"tools":      tools(names...),
		"headers":    map[string]string{},
	}
	raw, _ := json.Marshal(ident)
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func sidecarRequestWithHeaders(t *testing.T, port int, model string, clientTools []map[string]any, headers map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    clientTools,
	})
	req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-bridge-token", "test-token")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func sidecarRequest(t *testing.T, port int, model string, clientTools []map[string]any) {
	t.Helper()
	status, body := sidecarRequestWithHeaders(t, port, model, clientTools, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
}

func upstreamBody(t *testing.T, c *upstreamCapture) map[string]any {
	t.Helper()
	var raw []byte
	select {
	case raw = <-c.ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("upstream request not observed; stderr=%s", "")
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	return body
}

func upstreamToolNames(body map[string]any) []string {
	raw, _ := body["tools"].([]any)
	names := make([]string, 0, len(raw))
	for _, item := range raw {
		m, _ := item.(map[string]any)
		fn, _ := m["function"].(map[string]any)
		name, _ := fn["name"].(string)
		names = append(names, name)
	}
	return names
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func hasSystemNote(body map[string]any) bool {
	raw, _ := body["messages"].([]any)
	for _, item := range raw {
		m, _ := item.(map[string]any)
		text, _ := m["content"].(string)
		if strings.Contains(text, "[opencode-cc]") {
			return true
		}
	}
	return false
}

func firstSystem(body map[string]any) string {
	raw, _ := body["messages"].([]any)
	if len(raw) == 0 {
		return ""
	}
	m, _ := raw[0].(map[string]any)
	text, _ := m["content"].(string)
	return text
}

// The free-tier gate fingerprints the complete stock tool set. Sending fewer
// than all canonical tools makes the request look like an unknown client and
// returns FreeUsageLimitError while quota remains. Keep every entry even when
// the client cannot run it, and tell the model which ones are unavailable.
func TestNativeTransportKeepsCanonicalToolsForFingerprint(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	canonical := []string{"shell", "read", "write", "edit", "glob", "grep", "webfetch",
		"websearch", "skill", "subagent", "question", "execute", "experimental_image_generate"}
	writeIdentity(t, dir, srv.URL, canonical)
	s := startSidecar(t, bun, dir)

	client := []string{"Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch",
		"WebSearch", "Skill", "Agent", "AskUserQuestion"}
	sidecarRequest(t, s.port, "primary-model", tools(client...))
	body := upstreamBody(t, cap)
	names := upstreamToolNames(body)
	if len(names) != len(canonical) {
		t.Fatalf("tools = %v, want all %d canonical tools", names, len(canonical))
	}
	for _, want := range canonical {
		if !containsString(names, want) {
			t.Fatalf("canonical tool %q missing from %v", want, names)
		}
	}
	if !hasSystemNote(body) {
		t.Fatalf("missing unavailable-tools note: %#v", body["messages"])
	}
}

// BRIDGE_KEEP_DEAD_TOOLS=0 keeps the old pruned shape available for debugging
// a future gate change without a rebuild.
func TestNativeTransportDeadToolPruningEscapeHatch(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	canonical := []string{"shell", "read", "write", "edit", "glob", "grep", "webfetch",
		"websearch", "skill", "subagent", "question", "execute", "experimental_image_generate"}
	writeIdentity(t, dir, srv.URL, canonical)
	s := startSidecar(t, bun, dir, "BRIDGE_KEEP_DEAD_TOOLS=0")

	sidecarRequest(t, s.port, "primary-model", tools("Bash", "Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch", "Skill", "Agent", "AskUserQuestion"))
	body := upstreamBody(t, cap)
	names := upstreamToolNames(body)
	for _, dead := range []string{"execute", "experimental_image_generate"} {
		if containsString(names, dead) {
			t.Fatalf("dead tool %q reached upstream in prune mode: %v", dead, names)
		}
	}
}

func TestNativeTransportKillSwitchKeepsNote(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	canonical := []string{"shell", "read", "execute", "experimental_image_generate"}
	writeIdentity(t, dir, srv.URL, canonical)
	s := startSidecar(t, bun, dir, "BRIDGE_KEEP_DEAD_TOOLS=1")

	sidecarRequest(t, s.port, "primary-model", tools("Bash"))
	body := upstreamBody(t, cap)
	if names := upstreamToolNames(body); len(names) != 4 {
		t.Fatalf("kill switch tools = %v, want all 4", names)
	}
	if !hasSystemNote(body) {
		t.Fatalf("kill switch dropped the unavailable-tools note")
	}
}

func TestNativeTransportShellFallbackAliases(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	canonical := []string{"shell", "read", "write", "edit", "glob", "grep", "webfetch",
		"websearch", "skill", "subagent", "question", "execute", "experimental_image_generate"}
	writeIdentity(t, dir, srv.URL, canonical)
	s := startSidecar(t, bun, dir, "BRIDGE_KEEP_DEAD_TOOLS=0")

	// Codex-style client: only exec_command. The shell fallbacks are runnable
	// through it; skill/subagent/question/execute/image are not.
	sidecarRequest(t, s.port, "primary-model", tools("exec_command"))
	body := upstreamBody(t, cap)
	got := upstreamToolNames(body)
	want := map[string]bool{"shell": true, "read": true, "write": true, "edit": true,
		"glob": true, "grep": true, "webfetch": true, "websearch": true}
	if len(got) != len(want) {
		t.Fatalf("tools = %v, want %d shell-fallback tools", got, len(want))
	}
	for _, name := range got {
		if !want[name] {
			t.Fatalf("unexpected tool %q in %v", name, got)
		}
	}
}

func TestNativeTransportEmptyToolListIsOmitted(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	// Escape-hatch shape only: the default always sends the canonical set
	// because the free-tier fingerprint rejects a missing tool.
	s := startSidecar(t, bun, dir, "BRIDGE_KEEP_DEAD_TOOLS=0")

	sidecarRequest(t, s.port, "primary-model", tools("mcp__x__read"))
	body := upstreamBody(t, cap)
	if _, ok := body["tools"]; ok {
		t.Fatalf("empty tools array was sent: %#v", body["tools"])
	}
}

func TestNativeTransportTruncatedModelIdentity(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	if err := os.WriteFile(filepath.Join(dir, "identity-m-free.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := startSidecar(t, bun, dir)

	sidecarRequest(t, s.port, "m-free", tools("Bash"))
	body := upstreamBody(t, cap)
	if system := firstSystem(body); !strings.Contains(system, "Model ID: m-free") {
		t.Fatalf("truncated identity silently served the primary prompt: %q", system)
	}
}

func TestNativeTransportSessionSurvivesRestart(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})

	first := startSidecar(t, bun, dir)
	sidecarRequest(t, first.port, "primary-model", tools("Bash"))
	_ = upstreamBody(t, cap)
	var store []byte
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for {
		store, err = os.ReadFile(filepath.Join(dir, "sessions.json"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session store: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var saved struct {
		Sessions map[string]struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(store, &saved); err != nil {
		t.Fatalf("decode store: %v", err)
	}
	var before string
	for _, rec := range saved.Sessions {
		before = rec.ID
	}
	if before == "" {
		t.Fatalf("no session id persisted: %s", store)
	}
	_ = first.cmd.Process.Signal(syscall.SIGTERM)
	_ = first.cmd.Wait()

	second := startSidecar(t, bun, dir)
	sidecarRequest(t, second.port, "primary-model", tools("Bash"))
	_ = upstreamBody(t, cap)
	if after := lastUpstreamSession(cap); after != before {
		t.Fatalf("session id rotated across restart: %q -> %q", before, after)
	}
}

// A new conversation can arrive after the daily free window closes. Its fresh
// id has no lease, while an established id still answers 200. The bridge must
// reuse the accepted id before it rotates the egress: the earlier code reset
// the phone and retried the same unleased id, which always failed.
func TestNativeTransportFallsBackToLeasedSession(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()

	var mu sync.Mutex
	leased := ""
	seen := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("x-opencode-session")
		mu.Lock()
		seen = append(seen, sid)
		if leased == "" {
			leased = sid
		}
		accept := sid == leased
		mu.Unlock()
		if !accept {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	t.Cleanup(srv.Close)

	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir)

	// First conversation establishes the lease.
	sidecarRequest(t, s.port, "primary-model", tools("Bash"))

	// Second conversation has a different client session header, so the bridge
	// mints a new upstream id. The fake upstream rejects it exactly as Zen does.
	status, body := sidecarRequestWithHeaders(t, s.port, "primary-model", tools("Bash"),
		map[string]string{"x-opencode-session": "ses_secondconversation000000000000"})
	if status != http.StatusOK {
		t.Fatalf("fallback request status %d: %s", status, body)
	}

	mu.Lock()
	gotLeased := leased
	gotSeen := append([]string(nil), seen...)
	mu.Unlock()
	if len(gotSeen) != 3 {
		t.Fatalf("upstream attempts = %v, want initial 200, rejected new id, fallback 200", gotSeen)
	}
	if gotSeen[0] != gotLeased || gotSeen[2] != gotLeased {
		t.Fatalf("fallback did not reuse the leased id: leased=%q attempts=%v", gotLeased, gotSeen)
	}
	if gotSeen[1] == gotLeased {
		t.Fatalf("second conversation reused the leased id without a fallback: %v", gotSeen)
	}

	// The fallback must be remembered, or every later turn of that conversation
	// mints another unleased id and repeats the rejection.
	var store []byte
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for {
		store, err = os.ReadFile(filepath.Join(dir, "sessions.json"))
		if err == nil && strings.Contains(string(store), `"lastGood":"`+gotLeased+`"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("store did not persist the leased fallback: %v %s", err, store)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// lastUpstreamSession is filled by the fake upstream handler through the
// x-opencode-session header. Keeping it in the capture avoids a second
// channel.
var lastSessionMu sync.Mutex
var lastSessionValue string

func lastUpstreamSession(c *upstreamCapture) string {
	lastSessionMu.Lock()
	defer lastSessionMu.Unlock()
	return lastSessionValue
}

func lastUpstreamHeader(c *upstreamCapture, name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.headers.Get(name)
}

func TestNativeTransportModelTraversalRejected(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir)

	sidecarRequest(t, s.port, "foo/x/../../evil-free", tools("Bash"))
	body := upstreamBody(t, cap)
	if system := firstSystem(body); !strings.Contains(system, "Model ID: primary-model") {
		t.Fatalf("traversal model name was used as an identity: %q", system)
	}
	for _, name := range []string{"evil-free.json", "identity-x.json"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(dir), name)); err == nil {
			t.Fatalf("traversal wrote %s outside the identity dir", name)
		}
	}
}

func writeFreeLimit429(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	fmt.Fprint(w, `{"error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`)
}

func writeSSEStop(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	chunk := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
}

func sidecarRequestRaw(t *testing.T, port int, model string, clientTools []map[string]any,
	headers map[string]string) (int, http.Header, string, error) {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    clientTools,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-bridge-token", "test-token")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), string(raw), nil
}

func sidecarRequestFull(t *testing.T, port int, model string, clientTools []map[string]any,
	headers map[string]string, extra map[string]any) (int, http.Header, string) {
	t.Helper()
	payload := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools":    clientTools,
	}
	for key, value := range extra {
		payload[key] = value
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", port), bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-bridge-token", "test-token")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Clone(), string(raw)
}

func waitForStore(t *testing.T, path, contains string) []byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []byte
	var err error
	for {
		last, err = os.ReadFile(path)
		if err == nil && strings.Contains(string(last), contains) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("store %s did not contain %q: %v %s", path, contains, err, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func countFileLines(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(raw)), "\n"))
}

// The lease pool must survive one rejected fallback id and try a later one.
// The old code remembered exactly one `lastGood`; when it died, every scope
// held it and the reset retried the same dead id forever.
func TestNativeTransportPoolSkipsRejectedFallback(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	const idA = "ses_0123456789abAAAAAAAAAAAAAA"
	const idB = "ses_0123456789abBBBBBBBBBBBBBB"
	now := time.Now().UnixMilli()
	store := map[string]any{
		"version":    2,
		"savedAt":    now,
		"lastGood":   idB,
		"lastGoodAt": now,
		"pool": []map[string]any{
			{"id": idA, "goodAt": now - 2000},
			{"id": idB, "goodAt": now - 1000},
		},
		"sessions": map[string]any{},
	}
	raw, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("x-opencode-session")
		mu.Lock()
		seen = append(seen, sid)
		mu.Unlock()
		if sid != idB {
			writeFreeLimit429(w)
			return
		}
		writeSSEStop(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir)

	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"),
		map[string]string{"x-opencode-session": "ses_0123456789abCCCCCCCCCCCCCC"}, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("upstream attempts = %v, want fresh id, rejected pool id A, accepted pool id B", got)
	}
	if got[1] != idA || got[2] != idB {
		t.Fatalf("fallback did not walk the pool: %v (A=%s B=%s)", got, idA, idB)
	}
	if got[0] == idA || got[0] == idB {
		t.Fatalf("new scope reused a pooled id without a fallback: %v", got)
	}
}

// A generic FreeUsageLimitError does not prove that every stored lease is
// exhausted. Bound lease fallbacks so egress recovery is not delayed by a burst
// of 16 identical 429s.
func TestNativeTransportBoundsLeaseFallbacksBeforeReset(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	now := time.Now().UnixMilli()
	ids := []string{
		"ses_0123456789abA0000000000000",
		"ses_0123456789abA1111111111111",
		"ses_0123456789abA2222222222222",
		"ses_0123456789abA3333333333333",
		"ses_0123456789abA4444444444444",
		"ses_0123456789abA5555555555555",
	}
	pool := make([]map[string]any, 0, len(ids))
	for i, id := range ids {
		pool = append(pool, map[string]any{"id": id, "goodAt": now - int64(len(ids)-i)*1000})
	}
	store := map[string]any{
		"version": 2, "savedAt": now,
		"lastGood": ids[len(ids)-1], "lastGoodAt": now,
		"pool": pool, "sessions": map[string]any{},
	}
	raw, _ := json.Marshal(store)
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("x-opencode-session")
		mu.Lock()
		seen = append(seen, sid)
		attempt := len(seen)
		mu.Unlock()
		if attempt <= 3 {
			writeFreeLimit429(w)
			return
		}
		writeSSEStop(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	resetScript := filepath.Join(dir, "reset.sh")
	if err := os.WriteFile(resetScript, []byte("#!/bin/sh\nprintf 'ok via phone\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	s := startSidecar(t, bun, dir,
		"BRIDGE_NODE=/bin/sh",
		"BRIDGE_RESET_SCRIPT="+resetScript)

	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"),
		map[string]string{"x-opencode-session": "ses_0123456789abCCCCCCCCCCCCCC"}, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 4 {
		t.Fatalf("upstream attempts = %v, want initial + 2 lease fallbacks + post-reset retry", got)
	}
	if got[1] != ids[0] || got[2] != ids[1] {
		t.Fatalf("lease candidates = %v, want only the two oldest before reset", got[1:3])
	}
}

// A reset can legitimately run for 600s, but the request budget is 270s. The
// A pool key can belong to a different account than the one whose request
// shape was captured. Zen answers 403 Workspace access denied while this
// account's org header is attached; the sidecar must learn the key as foreign,
// drop the org/workspace headers for it, and retry without them.
func TestNativeTransportForeignCredentialDropsWorkspaceIdentity(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	var mu sync.Mutex
	var orgs, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		orgs = append(orgs, r.Header.Get("x-opencode-org-id"))
		auths = append(auths, r.Header.Get("Authorization"))
		n := len(orgs)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"Workspace access denied"}}`)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	path := filepath.Join(dir, "identity.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var ident map[string]any
	if err := json.Unmarshal(raw, &ident); err != nil {
		t.Fatal(err)
	}
	ident["headers"] = map[string]string{
		"x-opencode-org-id":       "org-primary",
		"x-opencode-workspace-id": "wrk-primary",
	}
	raw, _ = json.Marshal(ident)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s := startSidecar(t, bun, dir, "BRIDGE_MIN_START_GAP_MS=0")
	status, _, body, err := sidecarRequestRaw(t, s.port, "primary-model", tools("Bash"),
		map[string]string{"x-bridge-credential": "foreign-key"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200 after dropping the foreign org", status, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(orgs) != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (reject then retry)", len(orgs))
	}
	if orgs[0] != "org-primary" || orgs[1] != "" {
		t.Fatalf("org headers = %q, want [org-primary \"\"]", orgs)
	}
	for _, a := range auths {
		if a != "Bearer foreign-key" {
			t.Fatalf("authorization = %q, want the pooled key on every attempt", a)
		}
	}
}

// reset wait must be bounded by what is left of the budget, or a hung reset
// pins every inflight slot and then turns the expired request into a 502.
func TestNativeTransportResetWaitBoundedByDeadline(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "slow-reset.sh")
	// startSidecar clears PATH; use an absolute binary so this really sleeps.
	if err := os.WriteFile(script, []byte("#!/bin/sh\n/bin/sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeFreeLimit429(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir,
		"BRIDGE_NODE=/bin/sh",
		"BRIDGE_RESET_SCRIPT="+script,
		"BRIDGE_RESET_TIMEOUT_MS=5000",
		"BRIDGE_HEADER_TIMEOUT_MS=300",
		"BRIDGE_TRANSPORT_RETRY_BUDGET_MS=1000",
		"BRIDGE_MIN_START_GAP_MS=0",
	)

	started := time.Now()
	const requests = 4
	type result struct {
		status int
		reset  string
		body   string
		err    error
	}
	results := make(chan result, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, headers, body, err := sidecarRequestRaw(t, s.port, "primary-model", tools("Bash"),
				map[string]string{"x-opencode-session": fmt.Sprintf("ses_0123456789abZ%013d", i)})
			results <- result{status: status, reset: headers.Get("x-oc-reset"), body: body, err: err}
		}(i)
	}
	wg.Wait()
	close(results)
	elapsed := time.Since(started)
	for res := range results {
		if res.err != nil {
			t.Fatalf("request: %v", res.err)
		}
		if res.status != http.StatusTooManyRequests || res.reset != "failed" {
			t.Fatalf("status=%d x-oc-reset=%q body=%s, want 429/failed", res.status, res.reset, res.body)
		}
	}
	if elapsed > 3*time.Second {
		t.Fatalf("reset wait took %s; the request budget was 1.3s and the reset sleeps 5s", elapsed)
	}
	// A fast pass means the reset never ran (e.g. the script failed instantly).
	// The bounded wait should be close to the 1.3s budget.
	if elapsed < 900*time.Millisecond {
		t.Fatalf("reset wait returned in %s; the reset script did not run", elapsed)
	}
	if got := hits.Load(); got != requests {
		t.Fatalf("upstream attempts = %d, want %d", got, requests)
	}
}

// A header timeout is not a socket reset. Retrying it tripled the stall and let
// eight requests pin every MAX_INFLIGHT slot for about twelve minutes.
func TestNativeTransportHeaderTimeoutNotRetried(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	var count int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&count, 1)
		time.Sleep(1 * time.Second)
		writeSSEStop(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir,
		"BRIDGE_HEADER_TIMEOUT_MS=150",
		"BRIDGE_TRANSPORT_RETRY_BUDGET_MS=500")

	started := time.Now()
	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil, nil)
	elapsed := time.Since(started)
	if status != http.StatusBadGateway {
		t.Fatalf("status %d: %s", status, body)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("header timeout took %s; it must not be retried", elapsed)
	}
	if got := atomic.LoadInt32(&count); got != 1 {
		t.Fatalf("upstream attempts = %d, want 1", got)
	}
}

// Rotating an expired key must keep the account's lease lane and captured
// headers. Identity belongs to the account, not to an individual key.
func TestNativeTransportCredentialRotationReusesAccountLane(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	writeAuth := func(key string) {
		t.Helper()
		raw := []byte(fmt.Sprintf(`{"opencode":{"key":%q}}`, key))
		if err := os.WriteFile(auth, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("key-one")

	srv, cap := newFakeUpstream(t)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	env := []string{
		"BRIDGE_AUTH_JSON=" + auth,
		"BRIDGE_CREDENTIAL_DB=" + filepath.Join(dir, "missing.db"),
	}
	sessionHeader := "ses_0123456789abDDDDDDDDDDDDDD"

	sidecar := startSidecar(t, bun, dir, env...)
	status, _, body := sidecarRequestFull(t, sidecar.port, "primary-model", tools("Bash"),
		map[string]string{"x-opencode-session": sessionHeader}, nil)
	if status != http.StatusOK {
		t.Fatalf("first request status %d: %s", status, body)
	}
	id1 := lastUpstreamSession(cap)
	if id1 == "" {
		t.Fatal("no first session id")
	}
	if got := lastUpstreamHeader(cap, "x-opencode-org-id"); got != "org-key-one" {
		t.Fatalf("first request org = %q, want account-one capture", got)
	}

	// Rotate the active key while the bridge remains up. Same account: the
	// captured identity and its lease lane must survive the key change.
	writeAuth("key-two")
	status, _, body = sidecarRequestFull(t, sidecar.port, "primary-model", tools("Bash"),
		map[string]string{"x-opencode-session": sessionHeader}, nil)
	if status != http.StatusOK {
		t.Fatalf("second request status %d: %s", status, body)
	}
	id2 := lastUpstreamSession(cap)
	if id2 == "" {
		t.Fatal("no second session id")
	}
	if id1 != id2 {
		t.Fatalf("same-account key rotation started a new lane: %q -> %q", id1, id2)
	}
}

// BRIDGE_SESSION_ID must accept only the stock shape and reject the all-hex
// spoof shape Zen rejects. The loose regex accepted the spoof, so a documented
// seed could pin an id that can never obtain a lease.
func TestNativeTransportSessionOverrideShape(t *testing.T) {
	// Stock shape: ses_ + 12 hex + 14 base62 characters.
	const valid = "ses_0123456789abABCDEFGHIJKLMN"
	const allHex = "ses_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	cases := []struct {
		name       string
		override   string
		wantPinned bool
	}{
		{"valid", valid, true},
		{"all-hex", allHex, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bun := testBun(t)
			srv, cap := newFakeUpstream(t)
			dir := t.TempDir()
			writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
			s := startSidecar(t, bun, dir, "BRIDGE_SESSION_ID="+tc.override)
			sidecarRequest(t, s.port, "primary-model", tools("Bash"))
			_ = upstreamBody(t, cap)
			got := lastUpstreamSession(cap)
			if tc.wantPinned {
				if got != tc.override {
					t.Fatalf("override = %q, want %q", got, tc.override)
				}
				return
			}
			if got == tc.override {
				t.Fatalf("all-hex override was sent upstream: %q", got)
			}
			if !strings.HasPrefix(got, "ses_") || len(got) != 30 {
				t.Fatalf("rejected override did not fall back to a stock-shaped id: %q", got)
			}
		})
	}
}

// A shape that survives a successful reset and a fresh-id retry is not a quota
// incident. The next identical request must not cycle the radio again.
func TestNativeTransportShapeFailureIsScopedToCredential(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	writeAuth := func(key string) {
		t.Helper()
		raw := []byte(fmt.Sprintf(`{"opencode":{"key":%q}}`, key))
		if err := os.WriteFile(auth, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeAuth("key-one")
	resetCount := filepath.Join(dir, "reset-count")
	resetScript := filepath.Join(dir, "reset.sh")
	script := "#!/bin/sh\necho x >> " + resetCount + "\necho 'ok via test'\nexit 0\n"
	if err := os.WriteFile(resetScript, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	var upstreamCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)
		writeFreeLimit429(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir,
		"BRIDGE_NODE="+resetScript,
		"BRIDGE_AUTH_JSON="+auth,
		"BRIDGE_CREDENTIAL_DB="+filepath.Join(dir, "missing.db"))

	status, headers, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("first status %d: %s", status, body)
	}
	if got := headers.Get("x-oc-reset"); got != "shape" {
		t.Fatalf("first x-oc-reset = %q, want shape; body=%s", got, body)
	}
	if got := countFileLines(t, resetCount); got != 1 {
		t.Fatalf("reset invocations = %d, want 1", got)
	}

	status, headers, body = sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil, nil)
	if status != http.StatusTooManyRequests {
		t.Fatalf("second status %d: %s", status, body)
	}
	if got := headers.Get("x-oc-reset"); got != "shape" {
		t.Fatalf("second x-oc-reset = %q, want shape; body=%s", got, body)
	}
	if got := countFileLines(t, resetCount); got != 1 {
		t.Fatalf("reset invocations after identical shape = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 4 {
		t.Fatalf("upstream calls = %d, want 4 (3 ladder attempts + 1 shape probe)", got)
	}

	// A failed shape under one account must not suppress recovery for another
	// account using the same prompt/tool shape.
	writeAuth("key-two")
	status, headers, body = sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil, nil)
	if status != http.StatusTooManyRequests || headers.Get("x-oc-reset") != "shape" {
		t.Fatalf("rotated-account status=%d reset=%q body=%s", status, headers.Get("x-oc-reset"), body)
	}
	if got := countFileLines(t, resetCount); got != 2 {
		t.Fatalf("reset invocations after credential rotation = %d, want 2", got)
	}
	if got := atomic.LoadInt32(&upstreamCalls); got != 7 {
		t.Fatalf("upstream calls after credential rotation = %d, want 7", got)
	}
}

// An upstream tool_calls index of 1e9 used to build a sparse array and block
// the single-threaded sidecar for ~16s. Invalid indices must be dropped, not
// clamped to 0: clamping merged the fragment into call 0, producing the name
// `shellshell` and concatenated JSON arguments.
func TestNativeTransportAggregateRejectsBadToolIndex(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		first := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"echo a\"}"}}]}}]}`
		bad := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1000000000,"id":"call_2","type":"function","function":{"name":"shell","arguments":"{\"command\":\"echo b\"}"}}]}}]}`
		second := `{"id":"x","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
		fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, bad, second)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir)

	started := time.Now()
	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("aggregate took %s; a bad tool index must not stall the event loop", elapsed)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(parsed.Choices) != 1 || len(parsed.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %+v, want exactly the one valid call", parsed.Choices)
	}
	call := parsed.Choices[0].Message.ToolCalls[0].Function
	if call.Name != "shell" {
		t.Fatalf("tool name = %q, want %q (bad index must not merge into call 0)", call.Name, "shell")
	}
	if call.Arguments != `{"command":"echo a"}` {
		t.Fatalf("arguments = %q, want the call-0 arguments only", call.Arguments)
	}
}

// A tool_choice naming a pruned tool must not stay on the wire: the upstream
// tool list no longer contains it.
func TestNativeTransportDropsPrunedToolChoice(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read", "execute"})
	s := startSidecar(t, bun, dir, "BRIDGE_KEEP_DEAD_TOOLS=0")

	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil,
		map[string]any{
			"tool_choice": map[string]any{
				"type":     "function",
				"function": map[string]any{"name": "execute"},
			},
		})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	upstream := upstreamBody(t, cap)
	if _, ok := upstream["tool_choice"]; ok {
		t.Fatalf("pruned tool_choice was forwarded: %v", upstream["tool_choice"])
	}
}

// A 300-character model name must be rejected before it becomes a filename or
// an unbounded journal field.
func TestNativeTransportLongModelNameRejected(t *testing.T) {
	bun := testBun(t)
	srv, cap := newFakeUpstream(t)
	dir := t.TempDir()
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir)

	long := strings.Repeat("a", 300) + "-free"
	status, _, body := sidecarRequestFull(t, s.port, long, tools("Bash"), nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	_ = upstreamBody(t, cap)
	if _, err := os.Stat(filepath.Join(dir, "identity-"+long+".json")); err == nil {
		t.Fatalf("long model name was used as a capture filename")
	}
}

// Some routed models reject the stock prompt-cache field with a 400 before any
// generation. The sidecar must repair the shape once and replay, then remember
// the model so later turns never pay that failed round trip.
func TestNativeTransportRepairsRejectedPromptCacheField(t *testing.T) {
	bun := testBun(t)
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth.json")
	if err := os.WriteFile(auth, []byte(`{"opencode":{"key":"key-one"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var hits int32
	var firstHad, secondHad atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		has := strings.Contains(string(body), "prompt_cache_options")
		if n == 1 {
			firstHad.Store(has)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, "{\"error\":{\"message\":\"Unsupported parameter(s): `prompt_cache_options`\"}}")
			return
		}
		secondHad.Store(has)
		writeSSEStop(w)
	}))
	t.Cleanup(srv.Close)
	writeIdentity(t, dir, srv.URL, []string{"shell", "read"})
	s := startSidecar(t, bun, dir,
		"BRIDGE_AUTH_JSON="+auth,
		"BRIDGE_CREDENTIAL_DB="+filepath.Join(dir, "missing.db"))

	status, _, body := sidecarRequestFull(t, s.port, "primary-model", tools("Bash"), nil,
		map[string]any{"prompt_cache_retention": "24h"})
	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (fail, repaired retry)", got)
	}
	if !firstHad.Load() {
		t.Error("first request did not carry the stock prompt-cache field; test proves nothing")
	}
	if secondHad.Load() {
		t.Error("repaired retry still carried prompt_cache_options")
	}
}
