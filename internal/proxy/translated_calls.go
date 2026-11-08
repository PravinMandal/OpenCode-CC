package proxy

import (
	"encoding/json"
	"sync"
	"time"
)

// translatedCallRecord remembers what the model actually asked for before a
// canonical tool call was rewritten into a client-declared tool. Codex and
// other Responses clients replay their own history on the next turn; without
// this map the model sees its previous write/edit/read call as exec_command
// and can get stuck repeating the translated shell form.
type translatedCallRecord struct {
	name string
	args string
	// clientName/clientArgs are the call exactly as it was handed to the
	// client. Restore only applies when the replayed call still matches them,
	// so a client cannot replay another session's call id with different
	// arguments and have the proxy inject that session's canonical call (and
	// its possibly sensitive arguments) into its own upstream history.
	clientName string
	clientArgs string
	at         time.Time
}

var translatedCallCache = struct {
	sync.Mutex
	m map[string]translatedCallRecord
}{m: map[string]translatedCallRecord{}}

const (
	translatedCallTTL   = 12 * time.Hour
	translatedCallLimit = 2048
)

// RememberTranslatedCall records the canonical upstream call for a
// client-facing call id, together with the name and arguments the client was
// given. name must be the model-facing (canonical) name; clientName/clientArgs
// are what the downstream client received and will replay.
func RememberTranslatedCall(callID, name, args, clientName, clientArgs string) {
	if callID == "" || name == "" {
		return
	}
	translatedCallCache.Lock()
	defer translatedCallCache.Unlock()
	now := time.Now()
	if len(translatedCallCache.m) >= translatedCallLimit {
		for k, v := range translatedCallCache.m {
			if now.Sub(v.at) > translatedCallTTL {
				delete(translatedCallCache.m, k)
			}
		}
		for len(translatedCallCache.m) >= translatedCallLimit {
			var oldestKey string
			var oldest time.Time
			for k, v := range translatedCallCache.m {
				if oldestKey == "" || v.at.Before(oldest) {
					oldestKey, oldest = k, v.at
				}
			}
			delete(translatedCallCache.m, oldestKey)
		}
	}
	translatedCallCache.m[callID] = translatedCallRecord{
		name:       name,
		args:       args,
		clientName: clientName,
		clientArgs: clientArgs,
		at:         now,
	}
}

// RestoreTranslatedCall returns the canonical name and arguments recorded for a
// call id, if the id is still within the TTL and the replayed call matches the
// name and arguments the proxy handed to a client. args is the exact JSON the
// model originally produced, so argument-key translation for the client is not
// replayed into the upstream history.
func RestoreTranslatedCall(callID, clientName, clientArgs string) (name string, args string, ok bool) {
	if callID == "" {
		return "", "", false
	}
	translatedCallCache.Lock()
	defer translatedCallCache.Unlock()
	rec, found := translatedCallCache.m[callID]
	if !found {
		return "", "", false
	}
	if time.Since(rec.at) > translatedCallTTL {
		delete(translatedCallCache.m, callID)
		return "", "", false
	}
	if rec.clientName != "" && rec.clientName != clientName {
		return "", "", false
	}
	if rec.clientArgs != "" && !callArgsContain(rec.clientArgs, clientArgs) {
		return "", "", false
	}
	return rec.name, rec.args, true
}

// callArgsContain reports whether replayed still carries every field the proxy
// handed the client. Clients normalise what they replay: Bash adds a
// `description`, some SDKs reorder keys or drop nulls. The canonical call is
// only restored when the fields the client was given are unchanged, so a
// forged call id cannot pull another call's arguments into this history.
func callArgsContain(recorded, replayed string) bool {
	if recorded == replayed {
		return true
	}
	var rec, rep map[string]any
	if json.Unmarshal([]byte(recorded), &rec) != nil || json.Unmarshal([]byte(replayed), &rep) != nil {
		return false
	}
	if rec == nil || rep == nil {
		return false
	}
	for k, want := range rec {
		got, ok := rep[k]
		if !ok || !sameJSONValue(want, got) {
			return false
		}
	}
	return true
}

// sameJSONValue compares two decoded JSON values structurally.
func sameJSONValue(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

// RestoreOpenAIHistory rewrites tool calls in an already-decoded Chat
// Completions request back to the canonical call the model made. It is the
// struct-level twin of RestoreChatHistory, for lanes that build the upstream
// body from typed messages (the Anthropic /v1/messages bridge): the client
// replays the tool name it was given ("Bash") and the argument keys it
// executed, while the upstream tool set is canonical ("shell"), so without
// this the model sees its own previous turn call a tool it cannot see.
func RestoreOpenAIHistory(req *OpenAIRequest) {
	if req == nil {
		return
	}
	for i := range req.Messages {
		calls := req.Messages[i].ToolCalls
		for j := range calls {
			name, args, ok := RestoreTranslatedCall(calls[j].ID, calls[j].Function.Name, calls[j].Function.Arguments)
			if !ok {
				continue
			}
			calls[j].Function.Name = name
			calls[j].Function.Arguments = args
		}
	}
}

// RestoreChatHistory rewrites tool calls in a Chat Completions request body
// from the client-facing name and arguments back to the canonical call the
// model actually made. OpenAI-compatible clients (DSH) replay their own
// history, which contains the translated tool name (bash) and client argument
// keys even though the upstream tool set is canonical (shell); leaving it
// untranslated makes the model's own prior turns reference a tool it cannot
// see. Unknown fields and byte order outside tool_calls are preserved.
func RestoreChatHistory(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body
	}
	rawMessages, ok := root["messages"]
	if !ok {
		return body
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(rawMessages, &messages) != nil {
		return body
	}
	changed := false
	for _, message := range messages {
		if message == nil {
			continue
		}
		rawCalls, ok := message["tool_calls"]
		if !ok {
			continue
		}
		var calls []map[string]json.RawMessage
		if json.Unmarshal(rawCalls, &calls) != nil {
			continue
		}
		callsChanged := false
		for _, call := range calls {
			if call == nil {
				continue
			}
			var id string
			if raw, ok := call["id"]; ok {
				_ = json.Unmarshal(raw, &id)
			}
			var fn map[string]json.RawMessage
			rawFn, ok := call["function"]
			// json.Unmarshal("null", &fn) succeeds and leaves fn nil; writing
			// to that map panics. Client-controlled history must never take the
			// proxy down.
			if !ok || json.Unmarshal(rawFn, &fn) != nil || fn == nil {
				continue
			}
			var clientName, clientArgs string
			_ = json.Unmarshal(fn["name"], &clientName)
			_ = json.Unmarshal(fn["arguments"], &clientArgs)
			name, args, ok := RestoreTranslatedCall(id, clientName, clientArgs)
			if !ok {
				continue
			}
			fn["name"], _ = json.Marshal(name)
			fn["arguments"], _ = json.Marshal(args)
			encoded, err := json.Marshal(fn)
			if err != nil {
				continue
			}
			call["function"] = encoded
			callsChanged = true
		}
		if !callsChanged {
			continue
		}
		encoded, err := json.Marshal(calls)
		if err != nil {
			continue
		}
		message["tool_calls"] = encoded
		changed = true
	}
	if !changed {
		return body
	}
	encodedMessages, err := json.Marshal(messages)
	if err != nil {
		return body
	}
	root["messages"] = encodedMessages
	encoded, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return encoded
}
