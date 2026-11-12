package server

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
)

// Free-tier marker-tool filtering for the OpenAI Chat Completions lane.
//
// The proxy injects two marker tools ("shell", "read") into every Zen free-lane
// request (see proxy.PrepareFreeChatBody) because that lane gates on the exact
// tool shape the official OpenCode client sends. Those tools are NOT declared by
// the downstream client, and the model occasionally calls one spontaneously.
// The /v1/messages lane already resolves such names to the client's declared
// equivalent (proxy.ResolveDeclaredToolName, wired through
// StreamConverter.RestrictTools) so the call survives as a real tool call
// instead of stalling the agent loop.
//
// This lane used to be a raw byte relay, so a phantom "shell" call reached
// OpenCode verbatim: the client has no such tool and errors with "tool not
// found", ending the turn. filterChatToolCalls applies the same name
// resolution to the streaming wire format so both lanes behave identically.

// chatCallKey identifies one streamed tool call. OpenAI numbers tool calls by
// index within a choice, and a call's fragments (name first, then arguments)
// all carry the same pair, so this keys the per-call verdict across chunks.
type chatCallKey struct {
	choice int
	call   int
}

// chatToolCallFilter resolves upstream tool-call names against the client's
// declared tools and remembers the verdict per call for the life of the
// stream, so argument-only continuation fragments inherit the decision made
// when the name arrived.
type chatToolCallFilter struct {
	declared []string
	// schemas maps a declared tool name to its input schema, so a client
	// that already uses the canonical OpenCode argument shape is not
	// translated (native OpenCode declares read/write/edit with `path`).
	schemas map[string]json.RawMessage

	// verdicts records keep/drop per call key.
	verdicts map[chatCallKey]bool
	// keptAny tracks, per choice, whether at least one call survived, so a
	// trailing finish_reason:"tool_calls" can be corrected to "stop" when
	// every call was dropped.
	keptAny map[int]bool
	// pending buffers argument fragments for calls whose canonical shape
	// differs from the client tool. The consolidated call is emitted only
	// once the full JSON is available, so argument keys/defaults can be
	// translated (DSH's bash requires description, read requires file_path,
	// websearch requires a queries array).
	pending map[chatCallKey]*chatPendingCall

	droppedNames []string
	renamedFrom  []string
}

// chatPendingCall accumulates one streamed tool call until its arguments are
// complete enough to translate.
type chatPendingCall struct {
	id       string
	name     string
	resolved string
	args     strings.Builder
	emitted  bool
	// first is the fragment that introduced the call. Keeping it lets the
	// consolidated call preserve wire fields such as `index`, which SDK
	// accumulators key on, without inventing them.
	first map[string]json.RawMessage
}

// chatArgsNeedTranslation reports whether a client tool's argument schema
// differs from the canonical tool's, so the call must be buffered and
// translated rather than relayed fragment by fragment. Invalid (still
// streaming) JSON for a translated tool is buffered; a pure name rename with
// already-compatible arguments passes through untouched.
func chatArgsNeedTranslation(name, args string, schema json.RawMessage) bool {
	if proxy.SchemaUsesCanonicalArgs(name, schema) {
		return false
	}
	var obj map[string]any
	valid := json.Unmarshal([]byte(args), &obj) == nil && obj != nil
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "shell", "exec_command", "shell_command", "run_command":
		if !valid {
			return true
		}
		for _, key := range []string{"command", "cmd", "workdir", "timeout", "background", "run_in_background"} {
			if _, ok := obj[key]; ok {
				return true
			}
		}
		return false
	case "read", "write":
		if !valid {
			return true
		}
		_, hasPath := obj["path"]
		return hasPath
	case "edit":
		if !valid {
			return true
		}
		for _, key := range []string{"path", "oldString", "newString", "replaceAll"} {
			if _, ok := obj[key]; ok {
				return true
			}
		}
		return false
	case "webfetch":
		// WebFetch requires `prompt`; the canonical tool has only `url`.
		// TranslateToolArguments fills the default, so the call must be
		// buffered even when no canonical-only key is present.
		return true
	case "websearch":
		if !valid {
			return true
		}
		_, hasQuery := obj["query"]
		return hasQuery
	case "grep":
		if !valid {
			return true
		}
		for _, key := range []string{"include", "limit", "literal", "caseSensitive"} {
			if _, ok := obj[key]; ok {
				return true
			}
		}
		return false
	case "glob":
		if !valid {
			return true
		}
		for _, key := range []string{"hidden", "limit"} {
			if _, ok := obj[key]; ok {
				return true
			}
		}
		return false
	case "skill":
		if !valid {
			return true
		}
		_, hasID := obj["id"]
		return hasID
	case "subagent":
		if !valid {
			return true
		}
		for _, key := range []string{"agent", "background", "sessionID"} {
			if _, ok := obj[key]; ok {
				return true
			}
		}
		return false
	case "question":
		if !valid {
			return true
		}
		return containsJSONKey(obj, "multiple")
	}
	return false
}

// containsJSONKey reports whether a decoded JSON value carries key anywhere in
// its object/array tree. AskUserQuestion nests the canonical `multiple` flag
// inside each question object, not at the top level.
func containsJSONKey(v any, key string) bool {
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t[key]; ok {
			return true
		}
		for _, sub := range t {
			if containsJSONKey(sub, key) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if containsJSONKey(sub, key) {
				return true
			}
		}
	}
	return false
}

// newChatToolCallFilter builds a filter for the client-declared tool names.
func newChatToolCallFilter(declared []string) *chatToolCallFilter {
	return newChatToolCallFilterWithSchemas(declared, nil)
}

// newChatToolCallFilterWithSchemas additionally records each declared tool's
// input schema so canonical-schema clients pass through untranslated.
func newChatToolCallFilterWithSchemas(declared []string, schemas map[string]json.RawMessage) *chatToolCallFilter {
	clean := make([]string, 0, len(declared))
	for _, d := range declared {
		if d = strings.TrimSpace(d); d != "" {
			clean = append(clean, d)
		}
	}
	return &chatToolCallFilter{
		declared: clean,
		schemas:  schemas,
		verdicts: map[chatCallKey]bool{},
		keptAny:  map[int]bool{},
		pending:  map[chatCallKey]*chatPendingCall{},
	}
}

// summary renders a stable, log-safe description of what the filter changed.
func (f *chatToolCallFilter) summary() string {
	if f == nil || (len(f.droppedNames) == 0 && len(f.renamedFrom) == 0) {
		return ""
	}
	parts := make([]string, 0, 2)
	if len(f.renamedFrom) > 0 {
		parts = append(parts, "renamed="+strings.Join(dedupe(f.renamedFrom), ","))
	}
	if len(f.droppedNames) > 0 {
		parts = append(parts, "dropped="+strings.Join(dedupe(f.droppedNames), ","))
	}
	return strings.Join(parts, " ")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// verdictFor decides a call's fate. An unlabelled fragment (no name yet) is
// passed through unchanged: it will be judged when its name arrives, and
// inventing a decision here would either leak or swallow a legitimate call.
// buffer reports that the name has no declared equivalent yet but a
// shell/search fallback might express it once the arguments are complete.
func (f *chatToolCallFilter) verdictFor(key chatCallKey, name string) (keep bool, buffer bool, decided bool) {
	if v, ok := f.verdicts[key]; ok {
		return v, false, true
	}
	if strings.TrimSpace(name) == "" {
		return true, false, false
	}
	if len(f.declared) == 0 {
		// Client declared no tools: every call is phantom.
		f.verdicts[key] = false
		f.droppedNames = append(f.droppedNames, name)
		return false, false, true
	}
	resolved, ok := proxy.ResolveDeclaredToolName(name, f.declared)
	if !ok {
		// No declared tool carries this name, but the client's shell can
		// still run an equivalent read/write/edit/grep/glob/webfetch/
		// websearch. The fallback needs the complete arguments, so defer
		// the decision instead of dropping the call here.
		return false, true, true
	}
	f.verdicts[key] = true
	if resolved != name {
		f.renamedFrom = append(f.renamedFrom, name+"->"+resolved)
	}
	return true, false, true
}

// finishPending translates a buffered call and appends it to kept. A call
// buffered because its name had no declared equivalent is resolved through
// the shell/search fallbacks now that its arguments are complete; when no
// fallback exists it is dropped and recorded.
func (f *chatToolCallFilter) finishPending(key chatCallKey, p *chatPendingCall, kept *[]map[string]json.RawMessage, choice int) {
	if p.name == "" {
		// A call whose stream never carried a name is not executable. Emitting
		// it would hand the client a nameless tool call; dropping it lets the
		// finish_reason correction below close the turn cleanly.
		p.emitted = true
		f.verdicts[key] = false
		return
	}
	args := p.args.String()
	name := p.resolved
	translated := args
	if name == "" {
		resolved, outArgs, ok := proxy.ResolveDeclaredCallWithSchemas(p.name, args, f.declared, f.schemas)
		if !ok {
			p.emitted = true
			f.verdicts[key] = false
			f.droppedNames = append(f.droppedNames, p.name)
			return
		}
		name, translated = resolved, outArgs
		if name != p.name {
			f.renamedFrom = append(f.renamedFrom, p.name+"->"+name)
		}
	} else {
		translated = proxy.TranslateToolArgumentsForSchema(p.name, name, args, f.schemas[name])
	}
	if name != p.name || translated != args {
		proxy.RememberTranslatedCall(p.id, p.name, args, name, translated)
	}
	*kept = append(*kept, toolCallFromParts(p.first, p.id, name, translated))
	f.keptAny[choice] = true
	p.emitted = true
}

// cloneCallDelta copies one raw tool-call fragment so a buffered call can keep
// the wire fields (notably `index`) of the fragment that introduced it.
func cloneCallDelta(call map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(call))
	for k, v := range call {
		out[k] = v
	}
	return out
}

// filterChatToolCalls rewrites one SSE "data:" line of an OpenAI Chat
// Completions stream. Lines that are not tool-bearing JSON are returned
// byte-identical, so the common path stays a pure relay and unknown upstream
// fields survive untouched.
func (f *chatToolCallFilter) filterChatToolCalls(line []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return line, false
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(trimmed, &root) != nil {
		return line, false
	}
	rawChoices, ok := root["choices"]
	if !ok {
		return line, false
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(rawChoices, &choices) != nil {
		return line, false
	}

	changed := false
	for ci := range choices {
		choice := choices[ci]
		idx := ci
		if raw, ok := choice["index"]; ok {
			var n int
			if json.Unmarshal(raw, &n) == nil {
				idx = n
			}
		}

		rawDelta, hasDelta := choice["delta"]
		if hasDelta {
			var delta map[string]json.RawMessage
			if json.Unmarshal(rawDelta, &delta) == nil {
				if rawCalls, hasCalls := delta["tool_calls"]; hasCalls {
					var calls []map[string]json.RawMessage
					if json.Unmarshal(rawCalls, &calls) == nil && len(calls) > 0 {
						kept := make([]map[string]json.RawMessage, 0, len(calls))
						callsChanged := false
						for ti, call := range calls {
							callIdx := ti
							if raw, ok := call["index"]; ok {
								var n int
								if json.Unmarshal(raw, &n) == nil {
									callIdx = n
								}
							}
							key := chatCallKey{choice: idx, call: callIdx}

							// A call being buffered for argument translation:
							// append this fragment and emit the consolidated
							// call as soon as its JSON is complete.
							if p := f.pending[key]; p != nil {
								// The id can arrive in a later fragment than
								// the one that introduced the call. Keep it
								// for the consolidated call and the history
								// record.
								if p.id == "" {
									if id := toolCallID(call); id != "" {
										p.id = id
									}
								}
								// The name is a streamed string like the
								// arguments: a split name must be rebuilt
								// before a shell fallback can resolve it.
								// The helper ignores a repeated complete name.
								if p.resolved == "" {
									if n := toolCallName(call); n != "" {
										p.name = proxy.AppendToolNameFragment(p.name, n, f.declared)
									}
								}
								p.args.WriteString(toolCallArguments(call))
								callsChanged = true
								if !p.emitted && json.Valid([]byte(p.args.String())) {
									f.finishPending(key, p, &kept, idx)
								}
								continue
							}

							name := toolCallName(call)
							keep, buffer, decided := f.verdictFor(key, name)
							if !decided {
								// No name has arrived yet. Buffer instead of
								// relaying: if the call turns out to be
								// undeclared, a relayed fragment would already
								// be on the client's wire with no name to
								// resolve. A later fragment supplies the name.
								p := &chatPendingCall{
									id:    toolCallID(call),
									first: cloneCallDelta(call),
								}
								p.args.WriteString(toolCallArguments(call))
								f.pending[key] = p
								callsChanged = true
								continue
							}
							if buffer {
								// No declared name yet; a shell/search
								// fallback may express it once the arguments
								// are complete.
								p := &chatPendingCall{
									id:    toolCallID(call),
									name:  name,
									first: cloneCallDelta(call),
								}
								p.args.WriteString(toolCallArguments(call))
								f.pending[key] = p
								callsChanged = true
								if json.Valid([]byte(p.args.String())) {
									f.finishPending(key, p, &kept, idx)
								}
								continue
							}
							if !keep {
								callsChanged = true
								continue
							}
							resolved, _ := proxy.ResolveDeclaredToolName(name, f.declared)
							if resolved == "" {
								resolved = name
							}
							args := toolCallArguments(call)
							// Buffer a renamed call whose arguments are still
							// streaming even when its keys need no translation:
							// the remembered canonical args must be the complete
							// object, and only the first fragment carries the
							// upstream name.
							if chatArgsNeedTranslation(name, args, f.schemas[resolved]) ||
								(resolved != name && !json.Valid([]byte(args))) {
								p := &chatPendingCall{
									id:       toolCallID(call),
									name:     name,
									resolved: resolved,
									first:    cloneCallDelta(call),
								}
								p.args.WriteString(toolCallArguments(call))
								f.pending[key] = p
								callsChanged = true
								if json.Valid([]byte(p.args.String())) {
									f.finishPending(key, p, &kept, idx)
								}
								continue
							}
							f.keptAny[idx] = true
							if resolved != name {
								proxy.RememberTranslatedCall(toolCallID(call), name, toolCallArguments(call), resolved, toolCallArguments(call))
								if setToolCallName(call, resolved) {
									callsChanged = true
								}
							}
							kept = append(kept, call)
						}
						if callsChanged {
							// Renames mutate the call maps in place, so the
							// array must be re-encoded even when no call was
							// dropped (len(kept) == len(calls)).
							if len(kept) == 0 {
								delete(delta, "tool_calls")
							} else if encoded, err := json.Marshal(kept); err == nil {
								delta["tool_calls"] = encoded
							}
							if len(delta) == 0 {
								delete(choice, "delta")
							} else if encoded, err := json.Marshal(delta); err == nil {
								choice["delta"] = encoded
							}
							changed = true
						}
					}
				}
			}
		}

		// Flush any buffered call whose arguments never reached complete JSON
		// before the choice finished. Dropping it here produces an empty
		// finish_reason:"stop" turn: the model sees its call vanish and the
		// agent loop stalls. Emitting the best-effort call lets the client
		// return a schema error that the model can correct on the next turn.
		if rawFinish, ok := choice["finish_reason"]; ok && string(bytes.TrimSpace(rawFinish)) != "null" {
			var finish string
			if json.Unmarshal(rawFinish, &finish) == nil && finish != "" {
				keys := make([]chatCallKey, 0, len(f.pending))
				for key, p := range f.pending {
					if key.choice == idx && !p.emitted {
						keys = append(keys, key)
					}
				}
				sort.Slice(keys, func(i, j int) bool { return keys[i].call < keys[j].call })
				if len(keys) > 0 {
					flush := make([]map[string]json.RawMessage, 0, len(keys))
					for _, key := range keys {
						p := f.pending[key]
						args := p.args.String()
						if p.resolved == "" {
							// Buffered for a shell/search fallback, but the
							// arguments never became valid JSON. There is no
							// safe call to emit.
							p.emitted = true
							f.verdicts[key] = false
							f.droppedNames = append(f.droppedNames, p.name)
							continue
						}
						if json.Valid([]byte(args)) {
							translated := proxy.TranslateToolArgumentsForSchema(p.name, p.resolved, args, f.schemas[p.resolved])
							if p.resolved != p.name || translated != args {
								proxy.RememberTranslatedCall(p.id, p.name, args, p.resolved, translated)
							}
							args = translated
						}
						flush = append(flush, toolCallFromParts(p.first, p.id, p.resolved, args))
						p.emitted = true
						f.keptAny[idx] = true
					}
					if len(flush) > 0 {
						var delta map[string]json.RawMessage
						if rawDelta, hasDelta := choice["delta"]; hasDelta {
							_ = json.Unmarshal(rawDelta, &delta)
						}
						if delta == nil {
							delta = map[string]json.RawMessage{}
						}
						var existing []map[string]json.RawMessage
						if rawCalls, ok := delta["tool_calls"]; ok {
							_ = json.Unmarshal(rawCalls, &existing)
						}
						merged := append(existing, flush...)
						if encoded, err := json.Marshal(merged); err == nil {
							delta["tool_calls"] = encoded
						}
						if encoded, err := json.Marshal(delta); err == nil {
							choice["delta"] = encoded
						}
						if finish == "stop" {
							choice["finish_reason"] = json.RawMessage(`"tool_calls"`)
						}
						changed = true
					}
				}
			}
		}

		// A choice that lost every call must not advertise tool_calls as the
		// finish reason: OpenAI SDKs treat that as "there are calls to run" and
		// then wait forever for tool results that will never arrive.
		if rawFinish, ok := choice["finish_reason"]; ok && !f.keptAny[idx] {
			var finish string
			if json.Unmarshal(rawFinish, &finish) == nil && finish == "tool_calls" {
				choice["finish_reason"] = json.RawMessage(`"stop"`)
				changed = true
			}
		}
	}
	if !changed {
		return line, false
	}
	if encodedChoices, err := json.Marshal(choices); err == nil {
		root["choices"] = encodedChoices
	}
	out, err := json.Marshal(root)
	if err != nil {
		return line, false
	}
	return out, true
}

// toolCallName extracts function.name from a raw tool-call delta.
func toolCallName(call map[string]json.RawMessage) string {
	rawFn, ok := call["function"]
	if !ok {
		return ""
	}
	var fn map[string]json.RawMessage
	if json.Unmarshal(rawFn, &fn) != nil {
		return ""
	}
	rawName, ok := fn["name"]
	if !ok {
		return ""
	}
	var name string
	if json.Unmarshal(rawName, &name) != nil {
		return ""
	}
	return name
}

// toolCallArguments extracts the streamed function.arguments fragment.
func toolCallArguments(call map[string]json.RawMessage) string {
	rawFn, ok := call["function"]
	if !ok {
		return ""
	}
	var fn map[string]json.RawMessage
	if json.Unmarshal(rawFn, &fn) != nil {
		return ""
	}
	rawArgs, ok := fn["arguments"]
	if !ok {
		return ""
	}
	var args string
	if json.Unmarshal(rawArgs, &args) != nil {
		return ""
	}
	return args
}

// toolCallID extracts the streamed call id (empty when not yet present).
func toolCallID(call map[string]json.RawMessage) string {
	raw, ok := call["id"]
	if !ok {
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return ""
	}
	return id
}

// toolCallFromParts builds the consolidated delta for a translated call.
func toolCallFromParts(base map[string]json.RawMessage, id, name, args string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(base)+3)
	for k, v := range base {
		out[k] = v
	}
	if id != "" {
		out["id"], _ = json.Marshal(id)
	}
	out["type"] = json.RawMessage(`"function"`)
	fn, err := json.Marshal(map[string]any{
		"name":      name,
		"arguments": args,
	})
	if err != nil {
		return out
	}
	out["function"] = fn
	return out
}

// setToolCallName rewrites function.name in a raw tool-call delta.
func setToolCallName(call map[string]json.RawMessage, name string) bool {
	rawFn, ok := call["function"]
	if !ok {
		return false
	}
	var fn map[string]json.RawMessage
	if json.Unmarshal(rawFn, &fn) != nil {
		return false
	}
	encoded, err := json.Marshal(name)
	if err != nil {
		return false
	}
	if fn["name"] == nil {
		// Never introduce a name where the upstream sent none: an argument-only
		// continuation must stay argument-only.
		return false
	}
	fn["name"] = encoded
	callFn, err := json.Marshal(fn)
	if err != nil {
		return false
	}
	call["function"] = callFn
	return true
}

// filterUndeclaredOpenAIToolsRaw rewrites canonical tool names in a complete
// (non-streaming) Chat Completions response to the names the client declared.
// The streaming lane resolves every delta; without this a free-lane response
// carries "shell" while a client that declared "Bash" dispatches only its own
// names and silently never runs the call. Only the affected values are
// rewritten; the rest of the envelope is carried through as raw JSON.
func filterUndeclaredOpenAIToolsRaw(raw, reqBody []byte) []byte {
	// An empty declared list is meaningful: the sidecar always sends the
	// canonical tools, so a client that declared none must not receive a
	// phantom tool call it cannot dispatch.
	declared := declaredOpenAIToolNames(reqBody)
	schemas := declaredOpenAIToolSchemas(reqBody)

	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil {
		return raw
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(top["choices"], &choices) != nil {
		return raw
	}
	changed := false
	for i := range choices {
		rawMessage, ok := choices[i]["message"]
		if !ok {
			continue
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(rawMessage, &message) != nil {
			continue
		}
		calls, callsChanged, allDropped := rewriteRawToolCalls(message["tool_calls"], declared, schemas)
		if !callsChanged {
			continue
		}
		changed = true
		if allDropped {
			delete(message, "tool_calls")
			if reason, ok := choices[i]["finish_reason"]; ok {
				var fr string
				if json.Unmarshal(reason, &fr) == nil && fr == "tool_calls" {
					choices[i]["finish_reason"] = json.RawMessage(`"stop"`)
				}
			}
		} else {
			encoded, err := json.Marshal(calls)
			if err != nil {
				continue
			}
			message["tool_calls"] = encoded
		}
		encodedMessage, err := json.Marshal(message)
		if err != nil {
			continue
		}
		choices[i]["message"] = encodedMessage
	}
	if !changed {
		return raw
	}
	encodedChoices, err := json.Marshal(choices)
	if err != nil {
		return raw
	}
	top["choices"] = encodedChoices
	out, err := json.Marshal(top)
	if err != nil {
		return raw
	}
	return out
}

// rewriteRawToolCalls resolves every call in a raw tool_calls array. It
// returns the kept calls, whether anything changed, and whether every call
// was dropped as undeclared.
func rewriteRawToolCalls(rawCalls json.RawMessage, declared []string, schemas map[string]json.RawMessage) ([]map[string]json.RawMessage, bool, bool) {
	if len(rawCalls) == 0 {
		return nil, false, false
	}
	var calls []map[string]json.RawMessage
	if json.Unmarshal(rawCalls, &calls) != nil || len(calls) == 0 {
		return nil, false, false
	}
	kept := calls[:0]
	changed := false
	for _, call := range calls {
		var fn map[string]json.RawMessage
		if json.Unmarshal(call["function"], &fn) != nil {
			// A complete response should always carry a function object;
			// drop the malformed call rather than forwarding a name the
			// client cannot dispatch.
			changed = true
			continue
		}
		var name, args, id string
		_ = json.Unmarshal(fn["name"], &name)
		_ = json.Unmarshal(fn["arguments"], &args)
		_ = json.Unmarshal(call["id"], &id)
		resolved, translated, ok := proxy.ResolveDeclaredCallWithSchemas(name, args, declared, schemas)
		if !ok {
			changed = true
			continue
		}
		if resolved != name || translated != args {
			proxy.RememberTranslatedCall(id, name, args, resolved, translated)
			encodedName, nameErr := json.Marshal(resolved)
			encodedArgs, argsErr := json.Marshal(translated)
			if nameErr != nil || argsErr != nil {
				kept = append(kept, call)
				continue
			}
			fn["name"] = encodedName
			fn["arguments"] = encodedArgs
			encodedFn, err := json.Marshal(fn)
			if err != nil {
				kept = append(kept, call)
				continue
			}
			call["function"] = encodedFn
			changed = true
		}
		kept = append(kept, call)
	}
	return kept, changed, len(kept) == 0
}

// declaredOpenAIToolNames extracts the function names a client declared on a
// Chat Completions request.
func declaredOpenAIToolNames(body []byte) []string {
	var payload struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
			Name string `json:"name"` // Responses-style shape
		} `json:"tools"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	out := make([]string, 0, len(payload.Tools))
	for _, t := range payload.Tools {
		if t.Function.Name != "" {
			out = append(out, t.Function.Name)
			continue
		}
		if t.Name != "" {
			out = append(out, t.Name)
		}
	}
	return out
}

// declaredOpenAIToolSchemas extracts each declared function's input schema,
// so argument translation can be skipped for canonical-schema clients.
func declaredOpenAIToolSchemas(body []byte) map[string]json.RawMessage {
	var payload struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
			Name       string          `json:"name"` // Responses-style shape
			Parameters json.RawMessage `json:"parameters"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(payload.Tools))
	for _, t := range payload.Tools {
		name, schema := t.Function.Name, t.Function.Parameters
		if name == "" {
			name, schema = t.Name, t.Parameters
		}
		if name != "" && len(schema) > 0 {
			out[name] = schema
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
