package native

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func hasClientSystemExtra(raw []byte) bool {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	for _, m := range body.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "client-system-marker") {
			return true
		}
	}
	return false
}

func summarizeGateBody(raw string) string {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal([]byte(raw), &body) != nil {
		return "unparseable"
	}
	texts := make([]string, 0, len(body.Messages)+1)
	for _, m := range body.Messages {
		content := fmt.Sprint(m.Content)
		if len(content) > 40 {
			content = content[:40]
		}
		texts = append(texts, m.Role+":"+content)
	}
	return fmt.Sprintf("%d msgs [%s]", len(body.Messages), strings.Join(texts, ", "))
}

// TestGateClassifierDecisionTable runs the real gate.mjs classifier so the
// table cannot drift from the sidecar's decision logic.
func TestGateClassifierDecisionTable(t *testing.T) {
	bun := testBun(t)
	script := `
import { classifyGate, compactMessages, shapeRepairable } from "./gate.mjs";
const rows = [
  [classifyGate({ probePassed: false }), "quota"],
  [classifyGate({ probePassed: true, shapeRepairPassed: true }), "shape"],
  [classifyGate({ probePassed: true, conversationRebuildPassed: true }), "conversation"],
  [classifyGate({ probePassed: true }), "conversation"],
  [String(shapeRepairable({ systemExtra: 1, toolsExtras: 0 })), "true"],
  [String(shapeRepairable({ systemExtra: 0, toolsExtras: 0 })), "false"],
  [String(shapeRepairable(null)), "false"],
];
// compactMessages: newest content survives, oldest is truncated, message
// count and tool_call_id pairing are preserved, system content is untouched.
const messages = [
  { role: "system", content: "S".repeat(10000) },
  { role: "user", content: "A".repeat(5000) },
  { role: "assistant", content: null, tool_calls: [{ id: "call_1" }] },
  { role: "tool", tool_call_id: "call_1", content: "B".repeat(5000) },
  { role: "user", content: "newest".repeat(100) },
];
const pruned = compactMessages(messages, 2000, "[cut]");
rows.push([String(pruned > 0), "true"]);
rows.push([String(messages.length), "5"]);
rows.push([messages[0].content.length === 10000 ? "system-intact" : "system-cut", "system-intact"]);
rows.push([messages[4].content.startsWith("newest") ? "newest-intact" : "newest-cut", "newest-intact"]);
rows.push([messages[1].content.endsWith("[cut]") || messages[1].content === "[cut]" ? "older-cut" : "older-intact", "older-cut"]);
console.log(JSON.stringify(rows));
`
	out, err := exec.Command(bun, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("gate classifier: %v\n%s", err, out)
	}
	var rows [][2]string
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("decode classifier output: %v\n%s", err, out)
	}
	for _, row := range rows {
		if row[0] != row[1] {
			t.Errorf("classifier row = %q, want %q", row[0], row[1])
		}
	}
}

// A shape gate must be repaired in place and retried on the same key. It must
// never consume an egress rotation or a key cycle.
func TestNativeTransportShapeGateRepairsWithoutRotation(t *testing.T) {
	bun := testBun(t)
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		raw, _ := io.ReadAll(r.Body)
		if hasClientSystemExtra(raw) {
			writeFreeLimit429(w)
			return
		}
		writeSSEStop(w)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	writeIdentity(t, dir, upstream.URL, []string{"shell", "read"})
	sc := startSidecar(t, bun, dir)

	status, headers, body := sidecarRequestFull(t, sc.port, "primary-model", nil, nil, map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "client-system-marker"},
			{"role": "user", "content": "do the task"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s, want repaired 200", status, body)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Fatalf("upstream attempts=%d, want 3 (gated request + probe + canonical retry)", got)
	}
	if reset := headers.Get("x-oc-reset"); reset != "" {
		t.Fatalf("x-oc-reset=%q, shape gate must not rotate egress", reset)
	}
}

// A conversation gate must rebuild the dialogue (dropping the poisoned
// prefix) and retry once, without resetting the phone's egress.
func TestNativeTransportConversationGateRebuildsDialogue(t *testing.T) {
	bun := testBun(t)
	var hits int32
	var mu sync.Mutex
	var lastBody []byte
	var seen []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = raw
		seen = append(seen, string(raw))
		mu.Unlock()
		if strings.Contains(string(raw), "old-poison-marker") {
			writeFreeLimit429(w)
			return
		}
		writeSSEStop(w)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	writeIdentity(t, dir, upstream.URL, []string{"shell", "read"})
	sc := startSidecar(t, bun, dir)

	messages := []map[string]any{
		{"role": "user", "content": "old-poison-marker start"},
		{"role": "assistant", "content": "ack"},
	}
	for i := 0; i < 60; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": fmt.Sprintf("turn %d", i)},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("reply %d", i)},
		)
	}
	status, headers, body := sidecarRequestFull(t, sc.port, "primary-model", nil, nil, map[string]any{
		"messages": messages,
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s, want rebuilt 200", status, body)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		mu.Lock()
		summaries := make([]string, len(seen))
		for i, s := range seen {
			summaries[i] = summarizeGateBody(s)
		}
		mu.Unlock()
		t.Fatalf("upstream attempts=%d, want 3 (request + probe + rebuilt request): %v", got, summaries)
	}
	mu.Lock()
	rebuilt := string(lastBody)
	mu.Unlock()
	if strings.Contains(rebuilt, "old-poison-marker") {
		t.Fatalf("rebuilt request still carries the poisoned prefix: %s", rebuilt)
	}
	if !strings.Contains(rebuilt, "opencode-cc: older tool output pruned") {
		// The compaction note is the rebuild's continuity marker; a rebuild
		// that silently drops history without telling the model is a bug.
		if !strings.Contains(rebuilt, "transcript was compacted") {
			t.Fatalf("rebuilt request has no compaction note: %s", rebuilt)
		}
	}
	if reset := headers.Get("x-oc-reset"); reset != "" {
		t.Fatalf("x-oc-reset=%q, conversation gate must not rotate egress", reset)
	}
}

// When the canonical probe also fails, the answer is real quota exhaustion:
// the response must keep the existing quota contract and carry no gate
// header, so Go's key cycling stays enabled.
func TestNativeTransportQuotaProbeFailureKeepsQuotaContract(t *testing.T) {
	bun := testBun(t)
	var hits int32
	var sawProbe atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"content":"ping"`) {
			sawProbe.Store(true)
		}
		writeFreeLimit429(w)
	}))
	t.Cleanup(upstream.Close)

	dir := t.TempDir()
	writeIdentity(t, dir, upstream.URL, []string{"shell", "read"})
	sc := startSidecar(t, bun, dir)

	status, headers, body := sidecarRequestFull(t, sc.port, "primary-model", nil, nil, map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if status != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s, want 429", status, body)
	}
	if gate := headers.Get("x-oc-gate"); gate != "" {
		t.Fatalf("x-oc-gate=%q, quota must not be reported as a shape/conversation gate", gate)
	}
	if reset := headers.Get("x-oc-reset"); reset == "" {
		t.Fatal("x-oc-reset missing: quota contract must be preserved")
	}
	if !sawProbe.Load() {
		t.Fatal("canonical probe never ran; quota was assumed, not classified")
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Fatalf("upstream attempts=%d, want request + probe at minimum", got)
	}
}
