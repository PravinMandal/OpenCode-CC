package proxy

import (
	"encoding/json"
	"testing"
)

// TestAnthropicHistoryRestoresTranslatedCall proves that replayed Anthropic
// history is rewritten from the client-facing call (exec_command + client
// arguments) back to the canonical call the model originally made. Without
// this the model sees a tool in its own history that is absent from the
// upstream tool list and stalls or repeats the translated shell form.
func TestAnthropicHistoryRestoresTranslatedCall(t *testing.T) {
	const callID = "call_restore_history_1"
	RememberTranslatedCall(callID, "read", `{"file_path":"notes.txt"}`, "exec_command", `{"cmd":"cat notes.txt"}`)

	body, err := ConvertAnthropicToResponsesBody(&AnthropicRequest{
		Model:     "deepseek-v4.1-flash-free",
		MaxTokens: 64,
		Messages: []AnthropicMessage{
			{Role: "user", Content: AnthropicMessageContent{Text: "read it", IsStr: true}},
			{Role: "assistant", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_use", ID: callID, Name: "exec_command", Input: jsonRawMessage(`{"cmd":"cat notes.txt"}`)},
			}}},
			{Role: "user", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_result", ToolUseID: callID, Content: &AnthropicMessageContent{Text: "hello", IsStr: true}},
			}}},
		},
	}, "deepseek-v4.1-flash-free", BridgeOptions{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	var req struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var call map[string]any
	for _, item := range req.Input {
		if item["type"] == "function_call" {
			call = item
			break
		}
	}
	if call == nil {
		t.Fatalf("no function_call in %#v", req.Input)
	}
	if call["name"] != "read" {
		t.Errorf("history name not restored: %v", call["name"])
	}
	if call["arguments"] != `{"file_path":"notes.txt"}` {
		t.Errorf("history arguments not restored: %v", call["arguments"])
	}
}

// TestRestoreChatHistory proves the chat lane restores translated tool calls
// while preserving unrelated request fields.
func TestRestoreChatHistory(t *testing.T) {
	const callID = "call_chat_history_1"
	RememberTranslatedCall(callID, "shell", `{"command":"pwd"}`, "bash", `{"command":"pwd"}`)
	// The client may add fields its own schema requires when it replays the
	// call; the recorded fields are still present, so the canonical call is
	// restored.
	if name, args, ok := RestoreTranslatedCall(callID, "bash", `{"command":"pwd","description":"Print working directory"}`); !ok || name != "shell" || args != `{"command":"pwd"}` {
		t.Fatalf("cache lookup failed: name=%q args=%q ok=%v", name, args, ok)
	}
	if _, _, ok := RestoreTranslatedCall(callID, "bash", `{"command":"rm -rf /"}`); ok {
		t.Fatalf("a replayed call with different arguments must not restore")
	}
	if _, _, ok := RestoreTranslatedCall(callID, "Bash", `{"command":"pwd"}`); ok {
		t.Fatalf("a replayed call with a different name must not restore")
	}

	body := []byte(`{"model":"glm-5.3-flash-free","temperature":0.2,"messages":[` +
		`{"role":"user","content":"where am I?"},` +
		`{"role":"assistant","tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"bash","arguments":"{\"command\":\"pwd\",\"description\":\"Print working directory\"}"}}]},` +
		`{"role":"tool","tool_call_id":"` + callID + `","content":"/home/pravin"}]}`)

	restored := RestoreChatHistory(body)
	var root map[string]json.RawMessage
	if err := json.Unmarshal(restored, &root); err != nil {
		t.Fatalf("decode restored: %v", err)
	}
	if string(root["temperature"]) != "0.2" {
		t.Errorf("unrelated field changed: %s", root["temperature"])
	}
	var messages []map[string]any
	if err := json.Unmarshal(root["messages"], &messages); err != nil {
		t.Fatalf("decode messages: %v", err)
	}
	calls, _ := messages[1]["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls: %#v", messages[1]["tool_calls"])
	}
	fn, _ := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Errorf("chat history name not restored: %v", fn["name"])
	}
	if fn["arguments"] != `{"command":"pwd"}` {
		t.Errorf("chat history arguments not restored: %v", fn["arguments"])
	}
}

// TestRestoreOpenAIHistoryRewritesChatLane covers the Anthropic /v1/messages
// bridge: its upstream body is built from typed messages, not a replayed JSON
// blob, so it needs the struct-level restore. Before this the chat lane
// recorded the canonical call but never put it back, and the model saw its
// previous turn invoke a client tool ("Bash") that is not on the wire.
func TestRestoreOpenAIHistoryRewritesChatLane(t *testing.T) {
	const known = "call_chat_restore_known"
	const unknown = "call_chat_restore_unknown"
	RememberTranslatedCall(known, "shell", `{"command":"ls -la"}`, "Bash", `{"command":"ls -la"}`)

	req := &OpenAIRequest{Messages: []OpenAIMessage{
		{Role: "assistant", ToolCalls: []OpenAIToolCall{
			{ID: known, Type: "function", Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"ls -la"}`}},
			{ID: unknown, Type: "function", Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`}},
		}},
	}}
	RestoreOpenAIHistory(req)

	got := req.Messages[0].ToolCalls
	if got[0].Function.Name != "shell" || got[0].Function.Arguments != `{"command":"ls -la"}` {
		t.Fatalf("known call = %q %q, want canonical shell", got[0].Function.Name, got[0].Function.Arguments)
	}
	if got[1].Function.Name != "Bash" {
		t.Fatalf("unknown call was rewritten to %q; only remembered calls may change", got[1].Function.Name)
	}

	var nilReq *OpenAIRequest
	RestoreOpenAIHistory(nilReq) // must not panic
}

// Client-controlled history can contain null messages, null calls and a null
// function object. RestoreChatHistory must leave the body intact instead of
// panicking the proxy.
func TestRestoreChatHistoryToleratesMalformedEntries(t *testing.T) {
	const callID = "call_malformed_history"
	RememberTranslatedCall(callID, "read", `{"path":"x"}`, "Read", `{"path":"x"}`)
	body := []byte(`{"model":"m","messages":[` +
		`null,` +
		`{"role":"assistant","tool_calls":[null,` +
		`{"id":"` + callID + `","function":null},` +
		`{"id":"` + callID + `","function":"not-an-object"},` +
		`{"id":"` + callID + `","function":{}}]}]}`)
	got := RestoreChatHistory(body)
	if !json.Valid(got) {
		t.Fatalf("restored body is not JSON: %s", got)
	}
	// The empty function object is valid JSON but carries neither the client
	// name nor the arguments the proxy handed out, so it cannot prove it is
	// the recorded call and must stay untouched. Malformed entries also stay
	// untouched; the point here is that none of them panic the proxy.
	var root struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(root.Messages) != 2 {
		t.Fatalf("unexpected messages: %s", got)
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(root.Messages[1]["tool_calls"], &calls); err != nil {
		t.Fatalf("decode tool_calls: %v", err)
	}
	if len(calls) != 4 {
		t.Fatalf("unexpected calls: %s", got)
	}
	var fn map[string]any
	if err := json.Unmarshal(calls[3]["function"], &fn); err != nil {
		t.Fatalf("decode function: %v", err)
	}
	if len(fn) != 0 {
		t.Fatalf("an empty replayed call was rewritten: %+v", fn)
	}
}
