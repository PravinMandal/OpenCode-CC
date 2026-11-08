package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// PROBE 1: A stream chunk that carries a complete `message` object (some
// OpenAI-compatible gateways emit the final completion as a choice.message in
// SSE) is silently ignored by StreamConverter.
func TestProbeStreamMessageChunk(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "chat-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools([]string{"Bash", "Read"})
	conv.SetDeclaredSchemas([]AnthropicTool{stallBashTool, stallReadTool})

	finish := "tool_calls"
	chunk := &OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Message: &OpenAIMessage{
			Role:    "assistant",
			Content: "Doing this:",
			ToolCalls: []OpenAIToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
			}},
		},
		FinishReason: &finish,
	}}}
	if err := conv.HandleChunk(chunk); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	_ = conv.Flush()
	got := out.String()
	t.Logf("stop=%q tooluse=%v output:\n%s", conv.StopReason(), strings.Contains(got, "tool_use"), got)
}

// PROBE 2: truncated (finish=length) tool arguments are emitted as an
// input_json_delta even though they are not valid JSON.
func TestProbeStreamTruncatedToolArgs(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "chat-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools([]string{"Bash"})
	conv.SetDeclaredSchemas([]AnthropicTool{stallBashTool})

	finish := "length"
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			Index: 0, ID: "call_t", Type: "function",
			Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"echo`},
		}}},
		FinishReason: &finish,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	_ = conv.Flush()
	got := out.String()
	t.Logf("stop=%q output:\n%s", conv.StopReason(), got)
	// extract partial_json
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "partial_json") {
			var ev struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev)
			var probe any
			t.Logf("partial=%q validJSON=%v", ev.Delta.PartialJSON, json.Unmarshal([]byte(ev.Delta.PartialJSON), &probe) == nil)
		}
	}
}

// PROBE 3: AggregateOpenAIStream merges parallel tool calls that lack an
// `index` field (the normal non-streaming message shape).
func TestProbeAggregateParallelMessageToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[` +
			`{"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"pwd\"}"}},` +
			`{"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/x\"}"}}` +
			`]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
		``,
	}, "\n")
	out, err := AggregateOpenAIStream(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool calls: %+v", out.Choices[0].Message.ToolCalls)
}

// PROBE 4: an empty-argument tool call emits no input_json_delta.
func TestProbeStreamEmptyArgs(t *testing.T) {
	var out bytes.Buffer
	conv, err := NewStreamConverter(&out, "chat-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	conv.RestrictTools([]string{"Todo"})
	finish := "tool_calls"
	if err := conv.HandleChunk(&OpenAIStreamChunk{Choices: []OpenAIChoice{{
		Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
			Index: 0, ID: "call_e", Type: "function",
			Function: OpenAIFunctionCall{Name: "Todo", Arguments: ""},
		}}},
		FinishReason: &finish,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := conv.Finalize("end_turn"); err != nil {
		t.Fatal(err)
	}
	_ = conv.Flush()
	t.Logf("output:\n%s", out.String())
}

// PROBE 5: empty tool_result content serialises WITHOUT a content field.
func TestProbeEmptyToolResult(t *testing.T) {
	in := &AnthropicRequest{
		Model:     "deepseek-v4-flash",
		MaxTokens: 128,
		Messages: []AnthropicMessage{
			{Role: "assistant", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_use", ID: "t1", Name: "Bash", Input: jsonRawMessage(`{"command":"true"}`)},
			}}},
			{Role: "user", Content: AnthropicMessageContent{Blocks: []AnthropicContent{
				{Type: "tool_result", ToolUseID: "t1", Content: &AnthropicMessageContent{IsStr: true, Text: ""}},
			}}},
		},
	}
	out := ConvertRequest(in, func(s string) string { return s })
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("body: %s", raw)
}
