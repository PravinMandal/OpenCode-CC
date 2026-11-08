package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kiowx/opencode-cc/internal/config"
	"github.com/Kiowx/opencode-cc/internal/proxy"
)

// applyFilter runs one SSE data line through the filter and returns the
// resulting line (or the original when unchanged).
func applyFilter(t *testing.T, f *chatToolCallFilter, line string) string {
	t.Helper()
	out, changed := f.filterChatToolCalls([]byte(line))
	if !changed {
		return line
	}
	return string(out)
}

func TestFilterRenamesMarkerToolToDeclaredTool(t *testing.T) {
	// The free lane injects a phantom "shell" marker; the client declared
	// "Bash". The call must be renamed, not dropped.
	f := newChatToolCallFilter([]string{"Bash"})
	line := `{"id":"1","object":"chat.completion.chunk","model":"space-bunny-free","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":"{}"}}]},"finish_reason":null}]}`

	out := applyFilter(t, f, line)

	if strings.Contains(out, `"name":"shell"`) {
		t.Fatalf("phantom marker tool leaked to client: %s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("expected rename to declared Bash, got: %s", out)
	}
	if !strings.Contains(out, `"arguments":"{}"`) {
		t.Fatalf("arguments lost during rename: %s", out)
	}
	if got := f.summary(); !strings.Contains(got, "renamed=shell->Bash") {
		t.Fatalf("summary missing rename record: %q", got)
	}
}

func TestFilterRenamesNamespacedMarkerTool(t *testing.T) {
	// Zen presents OpenCode-namespaced names ("default.shell").
	f := newChatToolCallFilter([]string{"bash"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"default.shell","arguments":"{}"}}]}}]}`

	out := applyFilter(t, f, line)
	if strings.Contains(out, "default.shell") {
		t.Fatalf("namespaced phantom leaked: %s", out)
	}
	if !strings.Contains(out, `"name":"bash"`) {
		t.Fatalf("expected rename to declared bash, got: %s", out)
	}
}

func TestFilterDropsUnresolvableToolCall(t *testing.T) {
	// "frobnicate" is neither declared nor a marker alias: it must be dropped.
	f := newChatToolCallFilter([]string{"Bash"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"frobnicate","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

	out := applyFilter(t, f, line)
	if strings.Contains(out, "frobnicate") {
		t.Fatalf("unknown tool call leaked: %s", out)
	}
	// A choice that lost every call must not claim tool_calls, or the client
	// waits forever for tool results that will never arrive.
	if strings.Contains(out, `"tool_calls"`) && strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("dropped call still advertised as tool_calls: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason should fall back to stop: %s", out)
	}
	if got := f.summary(); !strings.Contains(got, "dropped=frobnicate") {
		t.Fatalf("summary missing drop record: %q", got)
	}
}

func TestFilterDropsEverythingWhenClientDeclaredNoTools(t *testing.T) {
	f := newChatToolCallFilter(nil)
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"shell","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

	out := applyFilter(t, f, line)
	if strings.Contains(out, `"name"`) {
		t.Fatalf("tool call leaked to a client with no tools: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason should be stop: %s", out)
	}
}

func TestFilterKeepsDeclaredToolAcrossArgumentFragments(t *testing.T) {
	// Name arrives in the first fragment, arguments stream in later fragments
	// keyed by the same index. Both must survive.
	f := newChatToolCallFilter([]string{"Bash"})
	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"Bash","arguments":""}}]}}]}`
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]}}]}`

	if out := applyFilter(t, f, first); !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("declared tool dropped in name fragment: %s", out)
	}
	// The fragment carries escaped JSON inside the arguments string, so match
	// the decoded content rather than a raw byte sequence.
	if out := applyFilter(t, f, second); !strings.Contains(out, `command`) {
		t.Fatalf("argument fragment dropped: %s", out)
	}
}

func TestFilterDropsArgumentFragmentsOfDroppedCall(t *testing.T) {
	// Once a call is judged phantom, its later argument fragments must be
	// suppressed too, or the client receives orphaned arguments.
	f := newChatToolCallFilter([]string{"Bash"})
	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"frobnicate","arguments":""}}]}}]}`
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":1}"}}]}}]}`

	if out := applyFilter(t, f, first); strings.Contains(out, "frobnicate") {
		t.Fatalf("phantom name leaked: %s", out)
	}
	if out := applyFilter(t, f, second); strings.Contains(out, `{\"x\":1}`) {
		t.Fatalf("orphan argument fragment leaked: %s", out)
	}
}

func TestFilterLeavesNonToolLinesByteIdentical(t *testing.T) {
	f := newChatToolCallFilter([]string{"Bash"})
	lines := []string{
		`{"id":"1","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}],"usage":null}`,
		`{"id":"1","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	for _, line := range lines {
		out, changed := f.filterChatToolCalls([]byte(line))
		if changed {
			t.Fatalf("line should be untouched but was rewritten:\n in: %s\nout: %s", line, out)
		}
		if string(out) != line {
			t.Fatalf("line mutated:\n in: %s\nout: %s", line, out)
		}
	}
}

func TestFilterPreservesUsageAndUnknownFields(t *testing.T) {
	f := newChatToolCallFilter([]string{"Bash"})
	line := `{"id":"1","object":"chat.completion.chunk","created":123,"system_fingerprint":"fp","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","type":"function","function":{"name":"frobnicate","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`

	out := applyFilter(t, f, line)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("filtered line is not valid JSON: %v\n%s", err, out)
	}
	if got["id"] != "1" || got["object"] != "chat.completion.chunk" {
		t.Fatalf("top-level fields lost: %s", out)
	}
	if _, ok := got["created"]; !ok {
		t.Fatalf("unknown field 'created' dropped: %s", out)
	}
	if _, ok := got["system_fingerprint"]; !ok {
		t.Fatalf("unknown field 'system_fingerprint' dropped: %s", out)
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok || usage["prompt_tokens"] != float64(9) {
		t.Fatalf("usage block lost: %s", out)
	}
}

func TestDeclaredOpenAIToolNamesReadsBothShapes(t *testing.T) {
	chatBody := []byte(`{"tools":[{"type":"function","function":{"name":"Bash"}},{"type":"function","function":{"name":"Read"}}]}`)
	got := declaredOpenAIToolNames(chatBody)
	if len(got) != 2 || got[0] != "Bash" || got[1] != "Read" {
		t.Fatalf("unexpected declared names: %#v", got)
	}
	responsesBody := []byte(`{"tools":[{"type":"function","name":"shell"},{"type":"function","name":"read"}]}`)
	got = declaredOpenAIToolNames(responsesBody)
	if len(got) != 2 || got[0] != "shell" || got[1] != "read" {
		t.Fatalf("unexpected declared names (responses shape): %#v", got)
	}
	if declaredOpenAIToolNames([]byte(`not json`)) != nil {
		t.Fatal("invalid JSON should yield no declared names")
	}
}

// TestRelayFiltersSplitLines drives the relay the way io.Copy does: arbitrary
// byte chunks that do not align with SSE line boundaries.
func TestRelayFiltersSplitLines(t *testing.T) {
	var dst bytes.Buffer
	relay := &openAIStreamRelay{
		dst:      &dst,
		flusher:  nopFlusher{},
		log:      &strings.Builder{},
		logLimit: 1 << 20,
		filter:   newChatToolCallFilter([]string{"Bash"}),
	}
	stream := "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"

	// Feed one byte at a time: the worst case for line buffering.
	for i := 0; i < len(stream); i++ {
		if _, err := relay.Write([]byte{stream[i]}); err != nil {
			t.Fatalf("write failed at %d: %v", i, err)
		}
	}
	if err := relay.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	out := dst.String()
	if strings.Contains(out, `"name":"shell"`) {
		t.Fatalf("phantom marker leaked through byte-wise relay: %s", out)
	}
	if !strings.Contains(out, `"name":"Bash"`) {
		t.Fatalf("expected renamed Bash in relay output: %s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("DONE sentinel lost: %s", out)
	}
	if !strings.Contains(out, "\n\n") {
		t.Fatalf("SSE event framing lost: %q", out)
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}

// TestRelayPreservesSSEFramingOnRewrite is the regression test for a rewrite
// bug: a filtered line must keep its "data:" prefix and its original single
// line terminator. Dropping the prefix makes the client ignore the chunk as a
// nameless event, and an extra newline injects a blank event into the stream.
func TestRelayPreservesSSEFramingOnRewrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"unix newline", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"},
		{"crlf newline", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"shell\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\r\n\r\ndata: [DONE]\r\n\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dst bytes.Buffer
			relay := &openAIStreamRelay{
				dst:      &dst,
				flusher:  nopFlusher{},
				log:      &strings.Builder{},
				logLimit: 1 << 20,
				filter:   newChatToolCallFilter([]string{"Bash"}),
			}
			for i := 0; i < len(tc.input); i++ {
				if _, err := relay.Write([]byte{tc.input[i]}); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			if err := relay.Flush(); err != nil {
				t.Fatalf("flush: %v", err)
			}

			out := dst.String()
			if !strings.Contains(out, `"name":"Bash"`) {
				t.Fatalf("rename did not survive: %q", out)
			}
			// Every emitted event must still be a data event.
			for _, line := range strings.Split(strings.TrimRight(out, "\r\n"), "\n") {
				line = strings.TrimRight(line, "\r")
				if line == "" {
					continue
				}
				if !strings.HasPrefix(line, "data:") {
					t.Fatalf("non-data line emitted, framing corrupted: %q (full: %q)", line, out)
				}
			}
			// Exactly one terminator per line: a rewritten line must not gain a
			// second newline (that would inject a blank event).
			if strings.Contains(out, "}\n\n\n") || strings.Contains(out, "}\r\n\r\n\r\n") {
				t.Fatalf("rewrite added an extra newline: %q", out)
			}
		})
	}
}

// A payload containing newlines (pretty-printed JSON) must not be split across
// events, and the [DONE] sentinel must pass through untouched.
func TestRelayKeepsMultilineJSONPayloadIntact(t *testing.T) {
	var dst bytes.Buffer
	relay := &openAIStreamRelay{
		dst:      &dst,
		flusher:  nopFlusher{},
		log:      &strings.Builder{},
		logLimit: 1 << 20,
		filter:   newChatToolCallFilter([]string{"Bash"}),
	}
	in := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\\nb\"}}]}\n\ndata: [DONE]\n\n"
	if _, err := relay.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := relay.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if got := dst.String(); got != in {
		t.Fatalf("non-tool line mutated:\n in: %q\nout: %q", in, got)
	}
}

func TestRelayPassesNonJSONLinesThrough(t *testing.T) {
	var dst bytes.Buffer
	relay := &openAIStreamRelay{
		dst:      &dst,
		flusher:  nopFlusher{},
		log:      &strings.Builder{},
		logLimit: 1 << 20,
		filter:   newChatToolCallFilter([]string{"Bash"}),
	}
	in := ": keep-alive comment\nevent: ping\ndata: [DONE]\n\n"
	if _, err := relay.Write([]byte(in)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dst.String(); got != in {
		t.Fatalf("non-JSON lines mutated:\n in: %q\nout: %q", in, got)
	}
}

// chatFilterE2EServer builds a Server whose only upstream replays the supplied
// SSE chunks verbatim, so the full handler path (not just the relay) can be
// exercised against a deliberately-phantom tool call.
func chatFilterE2EServer(t *testing.T, chunks ...string) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, chunk := range chunks {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := config.Default()
	cfg.UpstreamBase = ""
	cfg.ZenAPIKey = ""
	cfg.Upstreams = []config.Upstream{{BaseURL: upstream.URL, APIKey: "k", Enabled: true}}
	cfg.NativeAnthropic = false
	cfg.ModelMappings = []config.ModelMapping{{Match: "*", Target: "space-bunny-free"}}
	srv, _ := newTestServerWithCfg(t, cfg)
	return srv
}

const chatFilterE2EBody = `{"model":"client-model","stream":true,"max_tokens":64,` +
	`"tools":[{"type":"function","function":{"name":"Bash","parameters":{"type":"object"}}}],` +
	`"messages":[{"role":"user","content":"list files"}]}`

func chatFilterE2ECall(t *testing.T, srv *Server) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatFilterE2EBody))
	rec := httptest.NewRecorder()
	srv.OpenAIProxy().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// assertSSEFraming checks the wire shape a strict SSE parser requires: every
// non-blank line is a data line, and no rewrite introduced a blank event.
func assertSSEFraming(t *testing.T, raw string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			t.Fatalf("SSE framing corrupted, non-data line: %q\nfull: %q", line, raw)
		}
	}
	if strings.Contains(raw, "}\n\n\n") {
		t.Fatalf("rewrite injected a blank event: %q", raw)
	}
	if !strings.Contains(raw, "data: [DONE]") {
		t.Fatalf("DONE sentinel lost: %q", raw)
	}
}

// TestChatProxyFiltersPhantomToolCallEndToEnd drives the whole
// /v1/chat/completions streaming path against a fake upstream that deliberately
// emits the injected "shell" marker. The client declared only "Bash", so the
// wire output must carry a renamed, still-parseable call — the case that used
// to reach the client as a raw "tool not found" and kill the turn.
func TestChatProxyFiltersPhantomToolCallEndToEnd(t *testing.T) {
	srv := chatFilterE2EServer(t,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
	)

	raw := chatFilterE2ECall(t, srv)

	if strings.Contains(raw, `"name":"shell"`) {
		t.Fatalf("phantom marker reached the client: %s", raw)
	}
	if !strings.Contains(raw, `"name":"Bash"`) {
		t.Fatalf("call was not renamed to the declared tool: %s", raw)
	}
	if !strings.Contains(raw, `ls`) {
		t.Fatalf("argument fragment lost: %s", raw)
	}
	assertSSEFraming(t, raw)
}

// A phantom the client cannot resolve at all must be dropped, and the choice
// must not advertise tool_calls or the client waits forever for results.
func TestChatProxyDropsUnresolvableToolCallEndToEnd(t *testing.T) {
	srv := chatFilterE2EServer(t,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"frobnicate","arguments":"{}"}}]},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	)

	raw := chatFilterE2ECall(t, srv)

	if strings.Contains(raw, "frobnicate") {
		t.Fatalf("unresolvable tool call reached the client: %s", raw)
	}
	if strings.Contains(raw, `"finish_reason":"tool_calls"`) {
		t.Fatalf("dropped call still advertised as tool_calls: %s", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason should fall back to stop: %s", raw)
	}
	assertSSEFraming(t, raw)
}

// A stream with no tool calls at all must pass through byte-identical: the
// filter must stay invisible on the common path.
func TestChatProxyPlainStreamUntouchedEndToEnd(t *testing.T) {
	srv := chatFilterE2EServer(t,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
		`{"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	raw := chatFilterE2ECall(t, srv)

	if !strings.Contains(raw, `"content":"hi"`) {
		t.Fatalf("text content lost: %s", raw)
	}
	if !strings.Contains(raw, `"finish_reason":"stop"`) {
		t.Fatalf("finish_reason altered on a plain stream: %s", raw)
	}
	assertSSEFraming(t, raw)
}

// firstFilteredCall extracts the first consolidated tool call from a filtered
// SSE data line.
func firstFilteredCall(t *testing.T, line string) (name, args string) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(line), &root); err != nil {
		t.Fatalf("filtered line is not JSON: %v\n%s", err, line)
	}
	choices, _ := root["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in filtered line: %s", line)
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	calls, _ := delta["tool_calls"].([]any)
	if len(calls) == 0 {
		t.Fatalf("no tool_calls in filtered line: %s", line)
	}
	call, _ := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	name, _ = fn["name"].(string)
	args, _ = fn["arguments"].(string)
	return name, args
}

// DSH's bash requires a description and has no workdir field. A canonical
// shell call must be renamed and have both keys translated before the client
// sees it.
func TestFilterTranslatesShellArgumentsForDSHBash(t *testing.T) {
	f := newChatToolCallFilter([]string{"bash"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\",\"workdir\":\"/tmp/x\"}"}}]},"finish_reason":null}]}`

	out := applyFilter(t, f, line)
	name, args := firstFilteredCall(t, out)

	if name != "bash" {
		t.Fatalf("expected bash, got %q: %s", name, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatalf("translated args are not JSON: %v\n%s", err, args)
	}
	if obj["description"] == nil {
		t.Fatalf("description missing from DSH bash call: %s", args)
	}
	if _, ok := obj["workdir"]; ok {
		t.Fatalf("workdir leaked to DSH bash: %s", args)
	}
	cmd, _ := obj["command"].(string)
	if !strings.Contains(cmd, "cd '/tmp/x' && pwd") {
		t.Fatalf("workdir was not folded into command: %s", cmd)
	}
}

// DSH's read takes file_path, not the canonical path key.
func TestFilterTranslatesReadArgumentsForDSH(t *testing.T) {
	f := newChatToolCallFilter([]string{"read"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"/tmp/f.txt\"}"}}]},"finish_reason":null}]}`

	out := applyFilter(t, f, line)
	name, args := firstFilteredCall(t, out)

	if name != "read" {
		t.Fatalf("expected read, got %q: %s", name, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatalf("translated args are not JSON: %v\n%s", err, args)
	}
	if obj["file_path"] != "/tmp/f.txt" {
		t.Fatalf("file_path missing or wrong: %s", args)
	}
	if _, ok := obj["path"]; ok {
		t.Fatalf("path key leaked to DSH read: %s", args)
	}
}

// A client that declares the canonical argument schema (native OpenCode)
// already speaks path/oldString; translating it would corrupt the call.
func TestFilterPassesCanonicalArgsForCanonicalSchema(t *testing.T) {
	f := newChatToolCallFilterWithSchemas([]string{"read", "edit"}, map[string]json.RawMessage{
		"read": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		"edit": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"oldString":{"type":"string"},"newString":{"type":"string"},"replaceAll":{"type":"boolean"}}}`),
	})
	readLine := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"read","arguments":"{\"path\":\"/tmp/f.txt\"}"}}]},"finish_reason":null}]}`
	name, args := firstFilteredCall(t, applyFilter(t, f, readLine))
	if name != "read" || args != `{"path":"/tmp/f.txt"}` {
		t.Fatalf("canonical read was rewritten: %s %s", name, args)
	}
	editLine := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"edit","arguments":"{\"path\":\"/tmp/f.txt\",\"oldString\":\"a\",\"newString\":\"b\",\"replaceAll\":true}"}}]},"finish_reason":null}]}`
	name, args = firstFilteredCall(t, applyFilter(t, f, editLine))
	if name != "edit" || args != `{"path":"/tmp/f.txt","oldString":"a","newString":"b","replaceAll":true}` {
		t.Fatalf("canonical edit was rewritten: %s %s", name, args)
	}
}

// DSH's websearch takes a queries array, not a single query string.
func TestFilterTranslatesWebsearchArgumentsForDSH(t *testing.T) {
	f := newChatToolCallFilter([]string{"web_search"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"websearch","arguments":"{\"query\":\"golang json\"}"}}]},"finish_reason":null}]}`

	out := applyFilter(t, f, line)
	name, args := firstFilteredCall(t, out)

	if name != "web_search" {
		t.Fatalf("expected web_search, got %q: %s", name, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatalf("translated args are not JSON: %v\n%s", err, args)
	}
	queries, ok := obj["queries"].([]any)
	if !ok || len(queries) != 1 || queries[0] != "golang json" {
		t.Fatalf("queries array missing or wrong: %s", args)
	}
	if _, ok := obj["query"]; ok {
		t.Fatalf("query key leaked to DSH websearch: %s", args)
	}
}

// Arguments that arrive in several SSE fragments must be buffered and emitted
// as one translated call, not relayed fragment by fragment.
func TestFilterConsolidatesSplitArguments(t *testing.T) {
	f := newChatToolCallFilter([]string{"bash"})
	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":""}}]},"finish_reason":null}]}`
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":"}}]},"finish_reason":null}]}`
	third := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"pwd\"}"}}]},"finish_reason":null}]}`

	applyFilter(t, f, first)
	applyFilter(t, f, second)
	out := applyFilter(t, f, third)
	name, args := firstFilteredCall(t, out)

	if name != "bash" {
		t.Fatalf("expected bash, got %q: %s", name, out)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatalf("consolidated args are not JSON: %v\n%s", err, args)
	}
	if obj["description"] == nil || obj["command"] != "pwd" {
		t.Fatalf("split args not translated: %s", args)
	}
}

// If the stream ends before the buffered arguments form valid JSON, the call
// must still be emitted. A silent drop here becomes an empty stop turn and
// stalls the agent loop.
func TestFilterFlushesIncompleteCallAtFinish(t *testing.T) {
	f := newChatToolCallFilter([]string{"bash"})
	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"shell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	applyFilter(t, f, first)
	out := applyFilter(t, f, finish)

	name, _ := firstFilteredCall(t, out)
	if name != "bash" {
		t.Fatalf("incomplete buffered call was dropped: %s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish_reason should be tool_calls after flush: %s", out)
	}
}

// A call resolved from a single complete fragment must still be remembered for
// history restore; only the split-fragment path used to record it.
func TestFilterRemembersSingleFragmentTranslatedCall(t *testing.T) {
	const callID = "call_single_fragment_remember"
	f := newChatToolCallFilter([]string{"bash"})
	line := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","type":"function","function":{"name":"shell","arguments":"{\"command\":\"pwd\"}"}}]},"finish_reason":null}]}`
	name, _ := firstFilteredCall(t, applyFilter(t, f, line))
	if name != "bash" {
		t.Fatalf("resolved name = %q, want bash", name)
	}
	canonicalName, canonicalArgs, ok := proxy.RestoreTranslatedCall(callID, "bash", `{"command":"pwd","description":"Run a shell command"}`)
	if !ok || canonicalName != "shell" || canonicalArgs != `{"command":"pwd"}` {
		t.Fatalf("call not remembered: name=%q args=%q ok=%v", canonicalName, canonicalArgs, ok)
	}
}

// A canonical skill call is renamed and its `id` key translated. Its arguments
// arrive in fragments, so the canonical args must be remembered only once the
// full JSON is available, and the client must receive one consolidated call.
func TestFilterTranslatesAndRemembersSplitSkillCall(t *testing.T) {
	const callID = "call_skill_split"
	f := newChatToolCallFilter([]string{"Skill"})

	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","type":"function","function":{"name":"skill","arguments":"{\"id\":"}}]},"finish_reason":null}]}`
	out := applyFilter(t, f, first)
	if strings.Contains(out, callID) {
		t.Fatalf("incomplete call was emitted before its arguments were valid: %s", out)
	}

	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"load_skill\"}"}}]},"finish_reason":null}]}`
	name, args := firstFilteredCall(t, applyFilter(t, f, second))
	if name != "Skill" {
		t.Fatalf("resolved name = %q, want Skill", name)
	}
	if args != `{"skill":"load_skill"}` {
		t.Fatalf("translated args = %s, want the client schema shape", args)
	}

	canonicalName, canonicalArgs, ok := proxy.RestoreTranslatedCall(callID, "Skill", `{"skill":"load_skill"}`)
	if !ok || canonicalName != "skill" || canonicalArgs != `{"id":"load_skill"}` {
		t.Fatalf("remembered call = %q %q (ok=%v), want skill with full canonical args",
			canonicalName, canonicalArgs, ok)
	}
}

// A renamed call with no argument translation still buffers split arguments so
// history restore never records a truncated JSON object.
func TestFilterRemembersFullArgsForRenamedSplitCall(t *testing.T) {
	const callID = "call_renamed_split"
	f := newChatToolCallFilter([]string{"ImageGenerate"})

	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"` + callID + `","type":"function","function":{"name":"experimental_image_generate","arguments":"{\"prompt\":"}}]},"finish_reason":null}]}`
	if out := applyFilter(t, f, first); strings.Contains(out, callID) {
		t.Fatalf("incomplete call was emitted: %s", out)
	}
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a cat\"}"}}]},"finish_reason":null}]}`
	name, args := firstFilteredCall(t, applyFilter(t, f, second))
	if name != "ImageGenerate" || args != `{"prompt":"a cat"}` {
		t.Fatalf("emitted call = %q %s", name, args)
	}
	canonicalName, canonicalArgs, ok := proxy.RestoreTranslatedCall(callID, "ImageGenerate", `{"prompt":"a cat"}`)
	if !ok || canonicalName != "experimental_image_generate" || canonicalArgs != `{"prompt":"a cat"}` {
		t.Fatalf("remembered call = %q %q (ok=%v)", canonicalName, canonicalArgs, ok)
	}
}

// OpenAI streams a tool call's name as a string that may be split across
// chunks. The filter must rebuild it before resolving a shell fallback;
// resolving only the first fragment turned `read` into an unresolvable `re`
// and dropped a call the client could have run.
func TestFilterRebuildsSplitToolName(t *testing.T) {
	f := newChatToolCallFilter([]string{"Bash"})

	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_split","type":"function","function":{"name":"re","arguments":"{\"pa"}}]},"finish_reason":null}]}`
	if out := applyFilter(t, f, first); strings.Contains(out, "call_split") {
		t.Fatalf("split call was emitted before its name and args completed: %s", out)
	}
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"ad","arguments":"th\":\"/tmp/a.txt\"}"}}]},"finish_reason":null}]}`
	name, args := firstFilteredCall(t, applyFilter(t, f, second))
	if name != "Bash" {
		t.Fatalf("resolved name = %q, want Bash", name)
	}
	if !strings.Contains(args, "/tmp/a.txt") {
		t.Fatalf("fallback args lost the path: %s", args)
	}
}

// A fragment with no name must not reach the client before the filter knows
// whether the call survives. Relaying it left a nameless tool call on the wire
// even when the later name was undeclared and dropped.
func TestFilterBuffersNamelessFragment(t *testing.T) {
	f := newChatToolCallFilter([]string{"Bash"})

	first := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_noname","type":"function","function":{"arguments":"{}"}}]},"finish_reason":null}]}`
	if out := applyFilter(t, f, first); strings.Contains(out, "call_noname") || strings.Contains(out, `"function"`) {
		t.Fatalf("nameless fragment leaked to the client: %s", out)
	}
	second := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"frobnicate","arguments":""}}]},"finish_reason":null}]}`
	if out := applyFilter(t, f, second); strings.Contains(out, "frobnicate") {
		t.Fatalf("dropped call leaked: %s", out)
	}
	finish := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	out := applyFilter(t, f, finish)
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Fatalf("finish reason still advertises a dropped tool call: %s", out)
	}
}
