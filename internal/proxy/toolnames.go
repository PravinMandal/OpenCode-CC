package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
)

// Upstream function-name resolution.
//
// Zen presents tools to the model namespaced the OpenCode way ("default.Bash",
// "default.shell"), and the sidecar advertises the canonical OpenCode tool set
// ("shell", "read", "grep", "skill", ...) ahead of the client's own tools. A
// Claude Code client declares the same capabilities under different names and
// different argument schemas (Read/file_path, Skill/skill, Bash/command), so a
// call can arrive with a name the client never declared.
//
// Dropping such calls silently turns them into text-only end_turn responses,
// which stalls agent loops: the model announces what it is about to do, the
// turn ends, and the user has to type "continue". This file resolves those
// names to the client's declared tool and translates the argument keys to the
// client's schema.

// canonicalToolAliases maps a canonical (OpenCode) tool name to client tool
// names that serve the same purpose, most preferred first.
var canonicalToolAliases = map[string][]string{
	"shell":     {"Bash", "bash", "Shell", "shell", "run_command", "exec_command", "shell_command"},
	"read":      {"Read", "read", "read_file", "ReadFile", "view_file"},
	"write":     {"Write", "write", "write_file", "WriteFile", "create_file"},
	"edit":      {"Edit", "edit", "edit_file", "EditFile", "str_replace"},
	"glob":      {"Glob", "glob", "list_files", "find_files"},
	"grep":      {"Grep", "grep", "search_files", "ripgrep"},
	"webfetch":  {"WebFetch", "webfetch", "fetch"},
	"websearch": {"WebSearch", "websearch", "search", "web_search"},
	"skill":     {"Skill", "skill", "load_skill"},
	"subagent":  {"Agent", "Task", "subagent", "task", "delegate"},
	"question":  {"AskUserQuestion", "question", "ask_user"},
	"execute":   {"Execute", "execute", "run_code", "code_interpreter"},
	"experimental_image_generate": {
		"ImageGenerate", "generate_image", "image_generate", "experimental_image_generate",
	},
}

// ResolveDeclaredToolName maps an upstream function-call name to a name
// declared by the downstream client. It resolves, in order:
//
//  1. exact match against declared,
//  2. case-insensitive match (returns the declared canonical name),
//  3. OpenCode namespace stripping ("default.Bash" -> "Bash", last dot
//     segment, then 1-2 again),
//  4. canonical aliases (shell->Bash, read->Read, skill->Skill, ...) on both
//     the raw and the namespace-stripped name.
//
// ok is false when nothing matches; callers should drop the call but log it
// (never silently — invisible drops are how agent-loop stalls hide).
// A nil/empty declared list resolves nothing.
func ResolveDeclaredToolName(name string, declared []string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || len(declared) == 0 {
		return "", false
	}
	if d, ok := exactOrFold(name, declared); ok {
		return d, true
	}
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		short := name[i+1:]
		if d, ok := exactOrFold(short, declared); ok {
			return d, true
		}
		if d, ok := aliasTarget(short, declared); ok {
			return d, true
		}
	}
	return aliasTarget(name, declared)
}

// AppendToolNameFragment joins one streamed function-name fragment to the name
// accumulated so far. OpenAI streams the name as a string that may be split
// (`re` + `ad`) or repeated in full by a non-conforming provider. Names stream
// before arguments, so callers stop appending once arguments arrive. Prefer
// the longer form only when it is a real declared tool; otherwise keep the
// name that already resolves so a repeated `shell` cannot become `shellshell`.
func AppendToolNameFragment(current, fragment string, declared []string) string {
	if fragment == "" {
		return current
	}
	if current == "" || fragment == current {
		return fragment
	}
	candidate := current + fragment
	_, currentOK := ResolveDeclaredToolName(current, declared)
	_, candidateOK := ResolveDeclaredToolName(candidate, declared)
	switch {
	case candidateOK:
		return candidate
	case currentOK:
		return current
	case strings.HasPrefix(fragment, current):
		return fragment
	default:
		return candidate
	}
}

func exactOrFold(name string, declared []string) (string, bool) {
	for _, d := range declared {
		if d == name {
			return d, true
		}
	}
	for _, d := range declared {
		if strings.EqualFold(d, name) {
			return d, true
		}
	}
	// A client may declare a namespaced tool ("mcp.read"). The sidecar counts
	// its short name as runnable, so resolution must do the same or the call is
	// dropped after the sidecar deliberately kept the canonical tool.
	for _, d := range declared {
		if strings.EqualFold(declaredShortName(d), name) {
			return d, true
		}
	}
	return "", false
}

// declaredShortName returns the last dotted segment of a declared tool name.
func declaredShortName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// aliasTarget resolves one canonical tool name to the best-matching declared
// tool, exact before case-insensitive.
func aliasTarget(name string, declared []string) (string, bool) {
	candidates, ok := canonicalToolAliases[canonicalKey(name)]
	if !ok {
		return "", false
	}
	for _, want := range candidates {
		for _, d := range declared {
			if d == want {
				return d, true
			}
		}
	}
	for _, want := range candidates {
		for _, d := range declared {
			if strings.EqualFold(d, want) {
				return d, true
			}
		}
	}
	for _, want := range candidates {
		for _, d := range declared {
			if strings.EqualFold(declaredShortName(d), want) {
				return d, true
			}
		}
	}
	return "", false
}

// canonicalKey normalises an upstream tool name to the canonical alias key:
// lowercase, with any OpenCode namespace ("default.") removed.
func canonicalKey(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	name = strings.ToLower(name)
	switch name {
	case "exec_command", "shell_command", "run_command":
		// Codex and other clients name their shell tool differently. Treat
		// the direct client name as the canonical shell so argument
		// translation (command->cmd, numeric limits) still runs when the
		// model calls the client tool by name instead of "shell".
		return "shell"
	}
	return name
}

// argumentRenames lists the canonical argument key -> client argument key
// differences. Tools that share a schema keep their keys and need no entry.
var argumentRenames = map[string]map[string]string{
	"read":  {"path": "file_path"},
	"write": {"path": "file_path"},
	"edit": {
		"path":       "file_path",
		"oldString":  "old_string",
		"newString":  "new_string",
		"replaceAll": "replace_all",
	},
	"shell": {"background": "run_in_background"},
	"skill": {"id": "skill"},
	"subagent": {
		"agent":      "subagent_type",
		"background": "run_in_background",
	},
	"grep":      {"include": "glob", "limit": "head_limit"},
	"question":  {"multiple": "multiSelect"},
	"websearch": {"query": "queries"},
}

// argumentDrops lists canonical-only fields a client tool does not accept.
// Claude Code validates tool input with additionalProperties:false, so an
// extra key turns a valid call into an input-validation error.
var argumentDrops = map[string][]string{
	"glob":     {"hidden", "limit"},
	"grep":     {"literal", "caseSensitive"},
	"webfetch": {"format", "timeout"},
	"subagent": {"sessionID"},
}

// argumentDefaults fills a required client field the canonical tool does not
// have. WebFetch requires `prompt`; the canonical tool only carries `url`.
var argumentDefaults = map[string]map[string]any{
	"webfetch": {"prompt": "Summarize the page content relevant to the current task."},
}

// TranslateToolArguments rewrites a canonical (OpenCode) tool call's argument
// object into the schema of the client tool it resolved to. It never invents
// data: unknown keys are preserved, unrelated tools pass through untouched,
// and unparseable JSON is returned unchanged so the existing truncation
// handling still applies.
func TranslateToolArguments(upstreamName, clientName, args string) string {
	if strings.TrimSpace(args) == "" {
		return args
	}
	key := canonicalKey(upstreamName)
	client := strings.ToLower(strings.TrimSpace(clientName))
	codexShell := client == "exec_command" || client == "shell_command"
	bashShell := client == "bash"
	renames, hasRenames := argumentRenames[key]
	_, hasDrops := argumentDrops[key]
	_, hasDefaults := argumentDefaults[key]
	if !hasRenames && !hasDrops && !hasDefaults && !codexShell && !bashShell {
		return args
	}
	// Claude's Bash has no workdir field; fold it into the command instead of
	// dropping it, so `cd` semantics survive the hop. Codex exec_command does
	// take workdir, so it keeps the field.
	workdir := ""
	if key == "shell" {
		workdir = extractString(args, "workdir")
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return args
	}
	if !codexShell {
		for from, to := range renames {
			if v, exists := obj[from]; exists {
				if _, clash := obj[to]; !clash {
					obj[to] = v
				}
				delete(obj, from)
			}
		}
	}
	// DSH's websearch takes a `queries` array, while the canonical tool takes a
	// single `query` string. Wrap it so the client schema validates.
	if key == "websearch" {
		if q, ok := obj["queries"].(string); ok {
			obj["queries"] = []string{q}
		}
		if q, ok := obj["query"].(string); ok {
			if _, exists := obj["queries"]; !exists {
				obj["queries"] = []string{q}
			}
			delete(obj, "query")
		}
	}
	if key == "shell" {
		switch {
		case codexShell:
			// Codex's exec_command names the command `cmd` and has no
			// background/timeout fields; it takes yield/max-output instead.
			if v, exists := obj["command"]; exists {
				if _, clash := obj["cmd"]; !clash {
					obj["cmd"] = v
				}
				delete(obj, "command")
			}
			delete(obj, "run_in_background")
			delete(obj, "background")
			delete(obj, "timeout")
			if _, exists := obj["yield_time_ms"]; !exists {
				obj["yield_time_ms"] = 10000
			}
			if _, exists := obj["max_output_chars"]; !exists {
				obj["max_output_chars"] = 20000
			}
			// Models often emit numeric limits as strings. Codex validates
			// them as u64, so a quoted "10000" fails the call.
			coerceJSONInt(obj, "yield_time_ms")
			coerceJSONInt(obj, "max_output_chars")
		case bashShell:
			delete(obj, "workdir")
			if workdir != "" {
				if cmd, isStr := obj["command"].(string); isStr {
					obj["command"] = "cd " + shellQuote(workdir) + " && " + cmd
				}
			}
			// Claude Code and DSH both require a description on Bash.
			if _, exists := obj["description"]; !exists {
				obj["description"] = "Run a shell command"
			}
		default:
			delete(obj, "workdir")
			if workdir != "" {
				if cmd, isStr := obj["command"].(string); isStr {
					obj["command"] = "cd " + shellQuote(workdir) + " && " + cmd
				}
			}
		}
	}
	for _, drop := range argumentDrops[key] {
		delete(obj, drop)
	}
	for k, v := range argumentDefaults[key] {
		if _, exists := obj[k]; !exists {
			obj[k] = v
		}
	}
	// AskUserQuestion nests the multi-select flag inside each question,
	// while the canonical tool calls it `multiple`.
	if key == "question" {
		if questions, ok := obj["questions"].([]any); ok {
			for _, item := range questions {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if v, exists := m["multiple"]; exists {
					if _, clash := m["multiSelect"]; !clash {
						m["multiSelect"] = v
					}
					delete(m, "multiple")
				}
			}
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return args
	}
	return string(out)
}

// canonicalOnlyArgs lists argument keys that exist only in the canonical
// OpenCode tool schema. A client whose schema contains one of these (and none
// of the translated counterparts) already speaks the canonical schema, so
// rewriting its arguments would corrupt a call it can execute as-is. This is
// how the native OpenCode client is distinguished from DSH/Claude, which
// reuse names like "read" but take file_path.
var canonicalOnlyArgs = map[string][]string{
	"shell":     {"workdir"},
	"read":      {"path"},
	"write":     {"path"},
	"edit":      {"oldString", "newString", "replaceAll"},
	"grep":      {"include", "literal", "caseSensitive"},
	"glob":      {"hidden"},
	"webfetch":  {"format"},
	"websearch": {"query"},
	"skill":     {"id"},
	"subagent":  {"agent", "sessionID"},
	"question":  {"multiple"},
}

// translatedArgs are the client-schema counterparts of canonicalOnlyArgs.
var translatedArgs = map[string][]string{
	"shell":     {"run_in_background", "cmd"},
	"read":      {"file_path"},
	"write":     {"file_path"},
	"edit":      {"old_string", "new_string", "replace_all"},
	"grep":      {"glob", "head_limit"},
	"webfetch":  {"prompt"},
	"websearch": {"queries"},
	"skill":     {"skill"},
	"subagent":  {"subagent_type", "run_in_background"},
	"question":  {"multiSelect"},
}

// SchemaUsesCanonicalArgs reports whether a client tool's JSON schema already
// declares the canonical OpenCode argument shape for the tool. Callers use it
// to skip argument translation for a client that runs canonical tools.
func SchemaUsesCanonicalArgs(upstreamName string, schema json.RawMessage) bool {
	key := canonicalKey(upstreamName)
	canonical := canonicalOnlyArgs[key]
	if len(canonical) == 0 || len(schema) == 0 {
		return false
	}
	props := schemaPropertyNames(schema)
	if props == nil {
		return false
	}
	hasCanonical := false
	for _, name := range canonical {
		if props[name] {
			hasCanonical = true
			break
		}
	}
	if !hasCanonical {
		return false
	}
	for _, name := range translatedArgs[key] {
		if props[name] {
			return false
		}
	}
	return true
}

// schemaPropertyNames collects property names from a JSON schema, including
// nested object/item schemas (AskUserQuestion nests `multiple` inside
// questions.items).
func schemaPropertyNames(schema json.RawMessage) map[string]bool {
	var root any
	if json.Unmarshal(schema, &root) != nil {
		return nil
	}
	out := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		if props, ok := m["properties"].(map[string]any); ok {
			for name, sub := range props {
				out[name] = true
				walk(sub)
			}
		}
		if items, ok := m["items"]; ok {
			walk(items)
		}
	}
	walk(root)
	if len(out) == 0 {
		return nil
	}
	return out
}

// TranslateToolArgumentsForSchema is TranslateToolArguments with the client's
// declared input schema. A client that already uses the canonical argument
// shape (the native OpenCode tool set) passes through untouched.
func TranslateToolArgumentsForSchema(upstreamName, clientName, args string, schema json.RawMessage) string {
	if SchemaUsesCanonicalArgs(upstreamName, schema) {
		return args
	}
	return TranslateToolArguments(upstreamName, clientName, args)
}

// coerceJSONInt turns a quoted integer into a JSON number. Codex tool schemas
// declare these fields as u64 and reject strings.
func coerceJSONInt(obj map[string]any, key string) {
	s, ok := obj[key].(string)
	if !ok {
		return
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return
	}
	obj[key] = n
}

// extractString returns obj[key] when it is a string, else "".
func extractString(args, key string) string {
	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return ""
	}
	s, _ := obj[key].(string)
	return s
}

// ResolveDeclaredCall resolves an upstream call to a declared client tool and
// translates its arguments. Name resolution runs first, then the structured
// search fallback (grep/glob/websearch -> shell), then the file/network shell
// fallback (read/write/webfetch -> shell). ok is false when the client has no
// tool that can express the call.
func ResolveDeclaredCall(name, args string, declared []string) (string, string, bool) {
	return ResolveDeclaredCallWithSchemas(name, args, declared, nil)
}

// ResolveDeclaredCallWithSchemas is ResolveDeclaredCall with the client tool
// schemas, so a client that declares the canonical argument shape is not
// rewritten (see TranslateToolArgumentsForSchema).
func ResolveDeclaredCallWithSchemas(name, args string, declared []string, schemas map[string]json.RawMessage) (string, string, bool) {
	if resolved, ok := ResolveDeclaredToolName(name, declared); ok {
		return resolved, TranslateToolArgumentsForSchema(name, resolved, args, schemas[resolved]), true
	}
	if fbName, fbArgs, ok := SearchFallback(name, args, declared); ok {
		return fbName, fbArgs, true
	}
	if fbName, fbArgs, ok := ShellFallback(name, args, declared); ok {
		return fbName, fbArgs, true
	}
	return "", "", false
}

// SearchFallback rewrites a canonical grep/glob/websearch call into an
// equivalent shell command when the client does not declare a structured
// search tool. Claude Code does not always declare Grep/Glob/WebSearch (they
// may be deferred behind ToolSearch), but it always declares Bash, so search
// stays available instead of being dropped with only a notice.
func SearchFallback(upstreamName, args string, declared []string) (string, string, bool) {
	key := canonicalKey(upstreamName)
	if key != "grep" && key != "glob" && key != "websearch" {
		return "", "", false
	}
	shellName, family, ok := shellTool(declared)
	if !ok {
		return "", "", false
	}
	cmd, ok := searchCommand(key, args)
	if !ok {
		return "", "", false
	}
	payload, err := shellPayload(family, cmd)
	if err != nil {
		return "", "", false
	}
	return shellName, payload, true
}

// ShellFallback rewrites a canonical file/network call into an equivalent
// shell command when the client declares a shell but not the structured tool.
// Codex declares exec_command instead of read/write/edit/webfetch, so without
// this a canonical file call is dropped and the agent loop stalls with a
// text-only turn.
func ShellFallback(upstreamName, args string, declared []string) (string, string, bool) {
	key := canonicalKey(upstreamName)
	if key != "read" && key != "write" && key != "edit" && key != "webfetch" {
		return "", "", false
	}
	shellName, family, ok := shellTool(declared)
	if !ok {
		return "", "", false
	}
	cmd, ok := fileCommand(key, args)
	if !ok {
		return "", "", false
	}
	payload, err := shellPayload(family, cmd)
	if err != nil {
		return "", "", false
	}
	return shellName, payload, true
}

// shellTool picks the client's shell tool, preferring Codex's exec_command
// (whose argument schema differs), then Bash, then a plain shell. The names
// must stay in lockstep with TranslateToolArguments and the sidecar's
// SHELL_FALLBACK_TOOLS, or a fallback emits a name the client cannot run.
func shellTool(declared []string) (name, family string, ok bool) {
	if n, found := exactOrFold("exec_command", declared); found {
		return n, "exec", true
	}
	if n, found := exactOrFold("shell_command", declared); found {
		return n, "exec", true
	}
	if n, found := exactOrFold("Bash", declared); found {
		return n, "bash", true
	}
	if n, found := exactOrFold("shell", declared); found {
		return n, "plain", true
	}
	if n, found := exactOrFold("run_command", declared); found {
		return n, "plain", true
	}
	return "", "", false
}

func shellPayload(family, cmd string) (string, error) {
	payload := map[string]any{"command": cmd}
	switch family {
	case "exec":
		payload = map[string]any{
			"cmd":              cmd,
			"yield_time_ms":    10000,
			"max_output_chars": 20000,
		}
	case "bash":
		payload["description"] = "Run a shell command"
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// searchCommand renders a canonical grep/glob/websearch call as a shell command.
func searchCommand(key, args string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", false
	}
	if key == "websearch" {
		query, _ := obj["query"].(string)
		if strings.TrimSpace(query) == "" {
			return "", false
		}
		// DuckDuckGo's HTML endpoint is the no-key fallback. The grep extracts
		// result anchors; a challenge page simply yields no matches, which the
		// model can see and retry differently.
		u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
		return "curl -fsSL --max-time 30 -A " + shellQuote("Mozilla/5.0") + " " +
			shellQuote(u) + " | grep -oE 'result__a[^>]*>[^<]+' | head -n 8", true
	}
	pattern, _ := obj["pattern"].(string)
	if pattern == "" {
		return "", false
	}
	path, _ := obj["path"].(string)
	if path == "" {
		path = "."
	}
	var parts []string
	if key == "grep" {
		parts = []string{"rg", "--line-number", "--no-heading", "--color", "never"}
		if literal, _ := obj["literal"].(bool); literal {
			parts = append(parts, "-F")
		}
		if sensitive, ok := obj["caseSensitive"].(bool); ok && !sensitive {
			parts = append(parts, "-i")
		}
		if include, _ := obj["include"].(string); include != "" {
			parts = append(parts, "--glob", shellQuote(include))
		}
		parts = append(parts, "--", shellQuote(pattern), shellQuote(path))
	} else {
		parts = []string{"rg", "--files"}
		if hidden, _ := obj["hidden"].(bool); hidden {
			parts = append(parts, "--hidden")
		}
		parts = append(parts, "--glob", shellQuote(pattern), shellQuote(path))
	}
	cmd := strings.Join(parts, " ")
	if raw, ok := obj["limit"].(json.Number); ok {
		cmd += " | head -n " + raw.String()
	} else if key == "glob" {
		cmd += " | head -n 100"
	}
	return cmd, true
}

// fileCommand renders a canonical read/write/webfetch call as a shell command.
func fileCommand(key, args string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", false
	}
	switch key {
	case "read":
		path, _ := obj["path"].(string)
		if path == "" {
			return "", false
		}
		// The canonical read tool takes a 1-based line offset and a line limit
		// and answers with numbered lines. Ignoring them (plain cat) handed the
		// model the entire file on every paged read and made paging impossible.
		start := intArg(obj, "offset")
		if start < 1 {
			start = 1
		}
		count := intArg(obj, "limit")
		if count <= 0 || count > 2000 {
			count = 2000
		}
		end := start + count
		quoted := shellQuote(path)
		return "if [ -d " + quoted + " ]; then ls -1A -- " + quoted +
			" | sed -n '" + strconv.Itoa(start) + "," + strconv.Itoa(end-1) + "p'; " +
			"else awk 'NR>=" + strconv.Itoa(start) + " && NR<" + strconv.Itoa(end) +
			" {printf \"%d: %s\\n\", NR, $0}' < " + quoted + "; fi", true
	case "write":
		path, _ := obj["path"].(string)
		content, _ := obj["content"].(string)
		if path == "" {
			return "", false
		}
		dir := path
		if i := strings.LastIndexByte(dir, '/'); i > 0 {
			dir = dir[:i]
		} else {
			dir = "."
		}
		// Base64 writes the exact bytes. A heredoc appends a trailing newline
		// the canonical write tool did not ask for, which corrupts files whose
		// last line is significant (configs, lockfiles, snapshots).
		encoded := base64.StdEncoding.EncodeToString([]byte(content))
		return "mkdir -p -- " + shellQuote(dir) + " && printf %s " + shellQuote(encoded) +
			" | base64 -d > " + shellQuote(path), true
	case "edit":
		path, _ := obj["path"].(string)
		if path == "" {
			path, _ = obj["file_path"].(string)
		}
		oldText, _ := obj["oldString"].(string)
		if oldText == "" {
			oldText, _ = obj["old_string"].(string)
		}
		newText, _ := obj["newString"].(string)
		if newText == "" {
			newText, _ = obj["new_string"].(string)
		}
		replaceAll, _ := obj["replaceAll"].(bool)
		if !replaceAll {
			replaceAll, _ = obj["replace_all"].(bool)
		}
		if path == "" || oldText == "" {
			return "", false
		}
		// Pass the replacement as base64 JSON so arbitrary file contents and
		// newlines survive shell quoting. The canonical edit contract is strict:
		// without replaceAll, oldString must match exactly once. The previous
		// script replaced the first match silently even when several existed,
		// which edits the wrong occurrence and hides the ambiguity from the
		// model (and from the user).
		payload, err := json.Marshal(map[string]any{
			"path": path,
			"old":  oldText,
			"new":  newText,
			"all":  replaceAll,
		})
		if err != nil {
			return "", false
		}
		// read_bytes/write_bytes keeps CRLF and non-UTF-8 bytes intact;
		// read_text/write_text rewrote every line ending and corrupted files
		// that were not UTF-8.
		script := "import base64,json,pathlib; d=json.loads(base64.b64decode(\"" +
			base64.StdEncoding.EncodeToString(payload) +
			"\")); p=pathlib.Path(d[\"path\"]); s=p.read_bytes(); " +
			"old=d[\"old\"].encode(); new=d[\"new\"].encode(); n=s.count(old); " +
			"assert n>0, \"old_string not found\"; " +
			"assert d[\"all\"] or n==1, \"old_string must match exactly once (%d found); set replaceAll\" % n; " +
			"p.write_bytes(s.replace(old,new,-1 if d[\"all\"] else 1))"
		return "python3 -c " + shellQuote(script), true
	case "webfetch":
		u, _ := obj["url"].(string)
		if u == "" {
			return "", false
		}
		return "curl -fsSL --max-time 30 -- " + shellQuote(u), true
	}
	return "", false
}

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// intArg reads an integer tool argument. The decoder uses UseNumber, so the
// value is a json.Number; float64 is accepted for callers that did not.
func intArg(obj map[string]any, key string) int {
	switch v := obj[key].(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case float64:
		return int(v)
	}
	return 0
}
