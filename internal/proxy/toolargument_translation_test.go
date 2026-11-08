package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveDeclaredToolNameCanonicalAliases(t *testing.T) {
	tests := []struct {
		upstream string
		declared []string
		want     string
	}{
		{"skill", []string{"Skill"}, "Skill"},
		{"skill", []string{"Bash", "Read", "Skill"}, "Skill"},
		{"grep", []string{"Grep"}, "Grep"},
		{"glob", []string{"Glob"}, "Glob"},
		{"webfetch", []string{"WebFetch"}, "WebFetch"},
		{"websearch", []string{"WebSearch"}, "WebSearch"},
		{"subagent", []string{"Agent"}, "Agent"},
		{"subagent", []string{"Task"}, "Task"},
		{"question", []string{"AskUserQuestion"}, "AskUserQuestion"},
		{"edit", []string{"Edit"}, "Edit"},
		{"write", []string{"Write"}, "Write"},
		{"read", []string{"Read"}, "Read"},
		{"shell", []string{"Bash"}, "Bash"},
		{"shell", []string{"exec_command"}, "exec_command"},
		{"shell", []string{"exec_command", "write_stdin"}, "exec_command"},
		// Namespaced shape still resolves.
		{"default.skill", []string{"Skill"}, "Skill"},
		// Undeclared capability stays undeclared rather than guessing.
		{"execute", []string{"Skill", "Bash"}, ""},
	}
	for _, tc := range tests {
		got, ok := ResolveDeclaredToolName(tc.upstream, tc.declared)
		if tc.want == "" {
			if ok {
				t.Fatalf("%q resolved to %q, want no match", tc.upstream, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Fatalf("%q -> %q (ok=%v), want %q", tc.upstream, got, ok, tc.want)
		}
	}
}

// A client that declares the canonical OpenCode schema (the native OpenCode
// tool set) already speaks path/oldString/workdir. Renaming those keys would
// corrupt a call it can execute as-is.
func TestResolveDeclaredCallWithSchemasCanonicalPassthrough(t *testing.T) {
	declared := []string{"read", "write", "edit", "shell"}
	schemas := map[string]json.RawMessage{
		"read":  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		"write": json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}}}`),
		"edit":  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"oldString":{"type":"string"},"newString":{"type":"string"},"replaceAll":{"type":"boolean"}}}`),
		"shell": json.RawMessage(`{"type":"object","properties":{"command":{"type":"string"},"workdir":{"type":"string"}}}`),
	}
	tests := []struct{ name, args string }{
		{"read", `{"path":"/tmp/f.txt"}`},
		{"write", `{"path":"/tmp/f.txt","content":"x"}`},
		{"edit", `{"path":"/tmp/f.txt","oldString":"a","newString":"b","replaceAll":true}`},
		{"shell", `{"command":"pwd","workdir":"/tmp/x"}`},
	}
	for _, tc := range tests {
		gotName, gotArgs, ok := ResolveDeclaredCallWithSchemas(tc.name, tc.args, declared, schemas)
		if !ok || gotName != tc.name {
			t.Fatalf("%s resolved to %q (ok=%v)", tc.name, gotName, ok)
		}
		if gotArgs != tc.args {
			t.Fatalf("%s args were rewritten: %s -> %s", tc.name, tc.args, gotArgs)
		}
	}
}

// The same names with a client schema that uses file_path/old_string must
// still be translated for DSH-style clients.
func TestResolveDeclaredCallWithSchemasTranslatesClientShape(t *testing.T) {
	declared := []string{"read", "edit"}
	schemas := map[string]json.RawMessage{
		"read": json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
		"edit": json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}}}`),
	}
	name, args, ok := ResolveDeclaredCallWithSchemas("read", `{"path":"/tmp/f.txt"}`, declared, schemas)
	if !ok || name != "read" {
		t.Fatalf("read resolved to %q (ok=%v)", name, ok)
	}
	if args != `{"file_path":"/tmp/f.txt"}` {
		t.Fatalf("read args = %s, want file_path translation", args)
	}
	_, args, ok = ResolveDeclaredCallWithSchemas("edit",
		`{"path":"/tmp/f.txt","oldString":"a","newString":"b","replaceAll":true}`, declared, schemas)
	if !ok {
		t.Fatal("edit did not resolve")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		t.Fatalf("edit args are not JSON: %v (%s)", err, args)
	}
	if obj["file_path"] != "/tmp/f.txt" || obj["old_string"] != "a" || obj["new_string"] != "b" || obj["replace_all"] != true {
		t.Fatalf("edit args = %s, want snake_case translation", args)
	}
	if _, leaked := obj["path"]; leaked {
		t.Fatalf("canonical path leaked: %s", args)
	}
}

// SchemaUsesCanonicalArgs must see nested properties (AskUserQuestion nests
// `multiple` inside questions.items) and must not treat a schema that also
// declares the translated key as canonical.
func TestSchemaUsesCanonicalArgs(t *testing.T) {
	question := json.RawMessage(`{"type":"object","properties":{"questions":{"type":"array","items":{"type":"object","properties":{"question":{"type":"string"},"multiple":{"type":"boolean"}}}}}}`)
	if !SchemaUsesCanonicalArgs("question", question) {
		t.Fatal("nested canonical property was not detected")
	}
	mixed := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"file_path":{"type":"string"}}}`)
	if SchemaUsesCanonicalArgs("read", mixed) {
		t.Fatal("a schema with the translated key must not be treated as canonical")
	}
	if SchemaUsesCanonicalArgs("read", nil) {
		t.Fatal("a missing schema must not be treated as canonical")
	}
}

func TestTranslateToolArguments(t *testing.T) {
	tests := []struct {
		name     string
		upstream string
		client   string
		args     string
		want     string
	}{
		{
			name:     "read path to file_path",
			upstream: "read",
			client:   "Read",
			args:     `{"path":"/tmp/a","offset":2,"limit":10}`,
			want:     `{"file_path":"/tmp/a","limit":10,"offset":2}`,
		},
		{
			name:     "edit snake cases every renamed field",
			upstream: "edit",
			client:   "Edit",
			args:     `{"path":"/x","oldString":"a","newString":"b","replaceAll":true}`,
			want:     `{"file_path":"/x","new_string":"b","old_string":"a","replace_all":true}`,
		},
		{
			name:     "skill id to skill",
			upstream: "skill",
			client:   "Skill",
			args:     `{"id":"omarchy"}`,
			want:     `{"skill":"omarchy"}`,
		},
		{
			name:     "grep include to glob",
			upstream: "grep",
			client:   "Grep",
			args:     `{"pattern":"foo","include":"*.go"}`,
			want:     `{"glob":"*.go","pattern":"foo"}`,
		},
		{
			name:     "grep drops canonical-only fields and maps limit",
			upstream: "grep",
			client:   "Grep",
			args:     `{"pattern":"foo","literal":true,"caseSensitive":false,"limit":5}`,
			want:     `{"head_limit":5,"pattern":"foo"}`,
		},
		{
			name:     "question nested multiple becomes multiSelect",
			upstream: "question",
			client:   "AskUserQuestion",
			args:     `{"questions":[{"question":"Pick","header":"H","options":[],"multiple":true}]}`,
			want:     `{"questions":[{"header":"H","multiSelect":true,"options":[],"question":"Pick"}]}`,
		},
		{
			name:     "webfetch gains the required prompt",
			upstream: "webfetch",
			client:   "WebFetch",
			args:     `{"url":"https://example.com","format":"markdown"}`,
			want:     `{"url":"https://example.com","prompt":"Summarize the page content relevant to the current task."}`,
		},
		{
			name:     "shell background to run_in_background",
			upstream: "shell",
			client:   "Bash",
			args:     `{"command":"sleep 1","background":true}`,
			want:     `{"command":"sleep 1","run_in_background":true,"description":"Run a shell command"}`,
		},
		{
			name:     "shell workdir folds into the command",
			upstream: "shell",
			client:   "Bash",
			args:     `{"command":"pwd","workdir":"/tmp/x"}`,
			want:     `{"command":"cd '/tmp/x' && pwd","description":"Run a shell command"}`,
		},
		{
			name:     "codex exec_command gets cmd and limits",
			upstream: "shell",
			client:   "exec_command",
			args:     `{"command":"pwd","workdir":"/tmp/x"}`,
			want:     `{"cmd":"pwd","workdir":"/tmp/x","yield_time_ms":10000,"max_output_chars":20000}`,
		},
		{
			name:     "direct codex exec_command maps command and numeric strings",
			upstream: "exec_command",
			client:   "exec_command",
			args:     `{"command":"pwd","yield_time_ms":"10000","max_output_chars":"20000"}`,
			want:     `{"cmd":"pwd","yield_time_ms":10000,"max_output_chars":20000}`,
		},
		{
			name:     "unrelated tool passes through byte for byte",
			upstream: "Bash",
			client:   "Bash",
			args:     `{"command":"pwd"}`,
			want:     `{"command":"pwd"}`,
		},
		{
			name:     "invalid json passes through unchanged",
			upstream: "read",
			client:   "Read",
			args:     `{"path":`,
			want:     `{"path":`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := TranslateToolArguments(tc.upstream, tc.client, tc.args)
			var gotObj, wantObj map[string]any
			if json.Unmarshal([]byte(got), &gotObj) == nil && json.Unmarshal([]byte(tc.want), &wantObj) == nil {
				if !jsonEqual(gotObj, wantObj) {
					t.Fatalf("args = %s, want %s", got, tc.want)
				}
				return
			}
			if got != tc.want {
				t.Fatalf("args = %s, want %s", got, tc.want)
			}
		})
	}
}

func jsonEqual(a, b map[string]any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func TestSearchFallbackToBash(t *testing.T) {
	declared := []string{"Read", "Bash", "Write"}
	tests := []struct {
		name     string
		upstream string
		args     string
		wantCmd  string
	}{
		{
			name:     "grep without Grep becomes rg",
			upstream: "grep",
			args:     `{"pattern":"ZEBRA","path":"/tmp/x"}`,
			wantCmd:  `rg --line-number --no-heading --color never -- 'ZEBRA' '/tmp/x'`,
		},
		{
			name:     "grep literal and case-insensitive",
			upstream: "grep",
			args:     `{"pattern":"a.b","literal":true,"caseSensitive":false}`,
			wantCmd:  `rg --line-number --no-heading --color never -F -i -- 'a.b' '.'`,
		},
		{
			name:     "glob without Glob becomes rg --files",
			upstream: "glob",
			args:     `{"pattern":"*.go","path":"src"}`,
			wantCmd:  `rg --files --glob '*.go' 'src' | head -n 100`,
		},
		{
			name:     "websearch without WebSearch becomes DuckDuckGo curl",
			upstream: "websearch",
			args:     `{"query":"OpenCode Zen"}`,
			wantCmd:  `curl -fsSL --max-time 30 -A 'Mozilla/5.0' 'https://html.duckduckgo.com/html/?q=OpenCode+Zen' | grep -oE 'result__a[^>]*>[^<]+' | head -n 8`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name, args, ok := SearchFallback(tc.upstream, tc.args, declared)
			if !ok || name != "Bash" {
				t.Fatalf("fallback = %q, %v, want Bash, true", name, ok)
			}
			var payload struct {
				Command string `json:"command"`
			}
			if err := json.Unmarshal([]byte(args), &payload); err != nil {
				t.Fatalf("payload: %v (%s)", err, args)
			}
			if payload.Command != tc.wantCmd {
				t.Fatalf("command = %q, want %q", payload.Command, tc.wantCmd)
			}
		})
	}
}

// Codex declares exec_command but no Edit tool. An edit call must become a
// shell command that performs the replacement, not a dropped call that ends
// the turn.
func TestShellFallbackEditToExecCommand(t *testing.T) {
	args := `{"path":"/tmp/a.txt","oldString":"alpha","newString":"beta","replaceAll":true}`
	name, payload, ok := ShellFallback("edit", args, []string{"exec_command", "write_stdin"})
	if !ok || name != "exec_command" {
		t.Fatalf("fallback = %q, %v, want exec_command, true", name, ok)
	}
	var out struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		t.Fatalf("payload: %v (%s)", err, payload)
	}
	if !strings.Contains(out.Cmd, "python3 -c") || !strings.Contains(out.Cmd, "old_string not found") {
		t.Fatalf("command = %q, want a python3 replacement command", out.Cmd)
	}
	const prefix = `b64decode("`
	start := strings.Index(out.Cmd, prefix)
	end := strings.Index(out.Cmd, `"))`)
	if start < 0 || end <= start {
		t.Fatalf("command has no base64 payload: %q", out.Cmd)
	}
	raw, err := base64.StdEncoding.DecodeString(out.Cmd[start+len(prefix) : end])
	if err != nil {
		t.Fatalf("base64 payload: %v", err)
	}
	var decoded struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
		All  bool   `json:"all"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoded payload: %v (%s)", err, raw)
	}
	if decoded.Path != "/tmp/a.txt" || decoded.Old != "alpha" || decoded.New != "beta" || !decoded.All {
		t.Fatalf("decoded payload = %#v", decoded)
	}
}

// A client without Grep/Glob must still be able to search: the call becomes a
// Bash rg invocation instead of a visible drop.
func TestStreamConverterFallsBackToBashForUndeclaredGrep(t *testing.T) {
	finish := "tool_calls"
	_, raw := runChatStream(t, []AnthropicTool{stallBashTool}, []OpenAIStreamChunk{
		{Choices: []OpenAIChoice{{Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			Index: 0, ID: "call_grep",
			Function: OpenAIFunctionCall{Name: "grep", Arguments: `{"pattern":"ZEBRA","path":"/tmp/x"}`},
		}}}}}},
		{Choices: []OpenAIChoice{{FinishReason: &finish}}},
	})
	events := collectAnthropicEvents(t, bytes.NewBufferString(raw))
	tools := auditToolUses(events)
	if len(tools) != 1 {
		t.Fatalf("tool_use blocks = %+v, want 1", tools)
	}
	if tools[0].Name != "Bash" {
		t.Fatalf("name = %q, want Bash", tools[0].Name)
	}
	if !strings.Contains(tools[0].Input, `rg --line-number`) {
		t.Fatalf("input = %s, want an rg command", tools[0].Input)
	}
}

// The model sees canonical `skill` with {"id":...}; Claude Code declares
// `Skill` with {"skill":...}. Both the name and the schema must be rewritten,
// or the call reaches the client invalid and the turn stalls.
func TestStreamConverterTranslatesCanonicalSkillCall(t *testing.T) {
	finish := "tool_calls"
	skillTool := AnthropicTool{
		Name:        "Skill",
		InputSchema: jsonRawMessage(`{"type":"object","properties":{"skill":{"type":"string"}},"required":["skill"]}`),
	}
	conv, raw := runChatStream(t, []AnthropicTool{skillTool}, []OpenAIStreamChunk{
		{Choices: []OpenAIChoice{{Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			Index: 0, ID: "call_skill",
			Function: OpenAIFunctionCall{Name: "skill", Arguments: `{"id":"omarchy"}`},
		}}}}}},
		{Choices: []OpenAIChoice{{FinishReason: &finish}}},
	})
	events := collectAnthropicEvents(t, bytes.NewBufferString(raw))
	tools := auditToolUses(events)
	if len(tools) != 1 {
		t.Fatalf("tool_use blocks = %+v, want 1", tools)
	}
	if tools[0].Name != "Skill" {
		t.Fatalf("name = %q, want Skill", tools[0].Name)
	}
	if tools[0].Input != `{"skill":"omarchy"}` {
		t.Fatalf("input = %s, want {\"skill\":\"omarchy\"}", tools[0].Input)
	}
	if got := conv.StopReason(); got != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", got)
	}
}

// The old failure shape: every call dropped, no text. Finalize used to emit a
// thinking-only end_turn and Claude Code would inject a synthetic continuation
// forever. The turn must now carry a visible explanation.
func TestStreamConverterDroppedOnlyTurnIsVisible(t *testing.T) {
	finish := "tool_calls"
	conv, raw := runChatStream(t, []AnthropicTool{stallBashTool}, []OpenAIStreamChunk{
		{Choices: []OpenAIChoice{{Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			Index: 0, ID: "call_skill",
			Function: OpenAIFunctionCall{Name: "skill", Arguments: `{"id":"omarchy"}`},
		}}}}}},
		{Choices: []OpenAIChoice{{FinishReason: &finish}}},
	})
	events := collectAnthropicEvents(t, bytes.NewBufferString(raw))
	if len(auditToolUses(events)) != 0 {
		t.Fatalf("undeclared call should not be emitted")
	}
	text := auditText(events)
	if !strings.Contains(text, "[opencode-cc] Dropped tool call(s) skill") {
		t.Fatalf("missing drop notice, text = %q", text)
	}
	if !strings.Contains(text, "Declared: Bash") {
		t.Fatalf("drop notice should list declared tools, text = %q", text)
	}
	if got := conv.StopReason(); got != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", got)
	}
}
