package proxy

import (
	"bytes"
	"encoding/json"
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
	"shell":     {"Bash", "bash", "Shell", "shell", "run_command"},
	"read":      {"Read", "read", "read_file", "ReadFile", "view_file"},
	"write":     {"Write", "write", "write_file", "WriteFile", "create_file"},
	"edit":      {"Edit", "edit", "edit_file", "EditFile", "str_replace"},
	"glob":      {"Glob", "glob", "list_files", "find_files"},
	"grep":      {"Grep", "grep", "search_files", "ripgrep"},
	"webfetch":  {"WebFetch", "webfetch", "fetch"},
	"websearch": {"WebSearch", "websearch", "search"},
	"skill":     {"Skill", "skill", "load_skill"},
	"subagent":  {"Agent", "Task", "subagent", "task", "delegate"},
	"question":  {"AskUserQuestion", "question", "ask_user"},
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
	return "", false
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
	return "", false
}

// canonicalKey normalises an upstream tool name to the canonical alias key:
// lowercase, with any OpenCode namespace ("default.") removed.
func canonicalKey(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i+1 < len(name) {
		name = name[i+1:]
	}
	return strings.ToLower(name)
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
	"grep":     {"include": "glob", "limit": "head_limit"},
	"question": {"multiple": "multiSelect"},
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
	renames, hasRenames := argumentRenames[key]
	_, hasDrops := argumentDrops[key]
	_, hasDefaults := argumentDefaults[key]
	if !hasRenames && !hasDrops && !hasDefaults {
		return args
	}
	// Claude's Bash has no workdir field; fold it into the command instead of
	// dropping it, so `cd` semantics survive the hop.
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
	for from, to := range renames {
		if v, exists := obj[from]; exists {
			if _, clash := obj[to]; !clash {
				obj[to] = v
			}
			delete(obj, from)
		}
	}
	if key == "shell" {
		delete(obj, "workdir")
		if workdir != "" {
			if cmd, isStr := obj["command"].(string); isStr {
				obj["command"] = "cd " + shellQuote(workdir) + " && " + cmd
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

// SearchFallback rewrites a canonical grep/glob call into an equivalent Bash
// command when the client does not declare a structured search tool. Claude
// Code does not always declare Grep/Glob (they may be deferred behind
// ToolSearch), but it always declares Bash, so search stays available instead
// of being dropped with only a notice.
func SearchFallback(upstreamName, args string, declared []string) (string, string, bool) {
	key := canonicalKey(upstreamName)
	if key != "grep" && key != "glob" {
		return "", "", false
	}
	bash, ok := exactOrFold("Bash", declared)
	if !ok {
		return "", "", false
	}
	cmd, ok := searchCommand(key, args)
	if !ok {
		return "", "", false
	}
	payload, err := json.Marshal(map[string]string{"command": cmd})
	if err != nil {
		return "", "", false
	}
	return bash, string(payload), true
}

// searchCommand renders a canonical grep/glob call as a ripgrep shell command.
func searchCommand(key, args string) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader([]byte(args)))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", false
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

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
