package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// auditChunk marshals one Chat Completions chunk into an SSE data line.
func auditChunk(t *testing.T, c OpenAIStreamChunk) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal chunk: %v", err)
	}
	return "data: " + string(b) + "\n\n"
}

type auditToolUse struct {
	Name  string
	Input string
}

// auditToolUses collects the tool_use blocks (with their concatenated
// input_json_delta payload) from an Anthropic SSE stream.
func auditToolUses(events []anthropicStreamEvent) []auditToolUse {
	byIndex := map[int]*auditToolUse{}
	var order []int
	for _, e := range events {
		switch e.Type {
		case "content_block_start":
			if typ, _ := e.ContentBlock["type"].(string); typ == "tool_use" {
				name, _ := e.ContentBlock["name"].(string)
				byIndex[e.Index] = &auditToolUse{Name: name}
				order = append(order, e.Index)
			}
		case "content_block_delta":
			if typ, _ := e.Delta["type"].(string); typ == "input_json_delta" {
				if u := byIndex[e.Index]; u != nil {
					if s, ok := e.Delta["partial_json"].(string); ok {
						u.Input += s
					}
				}
			}
		}
	}
	out := make([]auditToolUse, 0, len(order))
	for _, index := range order {
		out = append(out, *byIndex[index])
	}
	return out
}

func auditText(events []anthropicStreamEvent) string {
	var sb strings.Builder
	for _, e := range events {
		if e.Type != "content_block_delta" {
			continue
		}
		if typ, _ := e.Delta["type"].(string); typ == "text_delta" {
			if s, ok := e.Delta["text"].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String()
}

func auditDeref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Some OpenAI-compatible gateways put the final completion in the SSE
// choice.message field instead of choice.delta. StreamConverter used to
// ignore choice.Message entirely, losing the tool call and ending the turn.
func TestStreamConverterHandlesMessageShapedChunks(t *testing.T) {
	tests := []struct {
		name      string
		message   *OpenAIMessage
		wantText  string
		wantTools []auditToolUse
	}{
		{
			name: "text and tool call",
			message: &OpenAIMessage{
				Role:    "assistant",
				Content: "Doing this:",
				ToolCalls: []OpenAIToolCall{{
					ID:       "call_a",
					Type:     "function",
					Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
				}},
			},
			wantText:  "Doing this:",
			wantTools: []auditToolUse{{Name: "Bash", Input: `{"command":"pwd"}`}},
		},
		{
			name: "parallel tool calls",
			message: &OpenAIMessage{
				Role: "assistant",
				ToolCalls: []OpenAIToolCall{
					{
						ID:       "call_a",
						Type:     "function",
						Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
					},
					{
						ID:       "call_b",
						Type:     "function",
						Function: OpenAIFunctionCall{Name: "Read", Arguments: `{"file_path":"/x"}`},
					},
				},
			},
			wantTools: []auditToolUse{
				{Name: "Bash", Input: `{"command":"pwd"}`},
				{Name: "Read", Input: `{"file_path":"/x"}`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			finish := "tool_calls"
			conv, raw := runChatStream(t, []AnthropicTool{stallBashTool, stallReadTool}, []OpenAIStreamChunk{{
				Choices: []OpenAIChoice{{
					Message:      tc.message,
					FinishReason: &finish,
				}},
			}})
			events := collectAnthropicEvents(t, bytes.NewBufferString(raw))

			if got := auditText(events); got != tc.wantText {
				t.Fatalf("text = %q, want %q", got, tc.wantText)
			}
			gotTools := auditToolUses(events)
			if len(gotTools) != len(tc.wantTools) {
				t.Fatalf("tool_use blocks = %+v, want %+v", gotTools, tc.wantTools)
			}
			for i := range tc.wantTools {
				if gotTools[i].Name != tc.wantTools[i].Name || gotTools[i].Input != tc.wantTools[i].Input {
					t.Fatalf("tool_use[%d] = %+v, want %+v", i, gotTools[i], tc.wantTools[i])
				}
			}
			if conv.StopReason() != "tool_use" {
				t.Fatalf("stop_reason = %q, want tool_use", conv.StopReason())
			}
		})
	}
}

type auditAggregatedCall struct {
	ID   string
	Name string
	Args string
}

// A non-streaming message carries complete tool calls, normally without an
// index. AggregateOpenAIStream keyed every call by Index (default 0), merging
// parallel calls into one corrupt invocation. Legacy fragmented function_call
// deltas were likewise split into one call per fragment.
func TestAggregateOpenAIStreamPreservesToolCalls(t *testing.T) {
	tests := []struct {
		name       string
		chunks     []OpenAIStreamChunk
		want       []auditAggregatedCall
		wantFinish string
	}{
		{
			name: "parallel message calls without index",
			chunks: []OpenAIStreamChunk{{
				Choices: []OpenAIChoice{{
					Message: &OpenAIMessage{
						Role: "assistant",
						ToolCalls: []OpenAIToolCall{
							{
								ID:       "call_a",
								Type:     "function",
								Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
							},
							{
								ID:       "call_b",
								Type:     "function",
								Function: OpenAIFunctionCall{Name: "Read", Arguments: `{"file_path":"/x"}`},
							},
						},
					},
					FinishReason: strPtr("tool_calls"),
				}},
			}},
			want: []auditAggregatedCall{
				{ID: "call_a", Name: "Bash", Args: `{"command":"pwd"}`},
				{ID: "call_b", Name: "Read", Args: `{"file_path":"/x"}`},
			},
			wantFinish: "tool_calls",
		},
		{
			name: "legacy function_call fragments",
			chunks: []OpenAIStreamChunk{
				{Choices: []OpenAIChoice{{Delta: OpenAIDelta{
					FunctionCall: &OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":`},
				}}}},
				{Choices: []OpenAIChoice{{Delta: OpenAIDelta{
					FunctionCall: &OpenAIFunctionCall{Arguments: `"pwd"}`},
				}}}},
				{Choices: []OpenAIChoice{{FinishReason: strPtr("function_call")}}},
			},
			want:       []auditAggregatedCall{{Name: "Bash", Args: `{"command":"pwd"}`}},
			wantFinish: "function_call",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var stream strings.Builder
			for _, chunk := range tc.chunks {
				stream.WriteString(auditChunk(t, chunk))
			}
			stream.WriteString("data: [DONE]\n\n")

			out, err := AggregateOpenAIStream(strings.NewReader(stream.String()))
			if err != nil {
				t.Fatalf("AggregateOpenAIStream: %v", err)
			}
			if len(out.Choices) != 1 || out.Choices[0].Message == nil {
				t.Fatalf("choices = %+v", out.Choices)
			}
			calls := out.Choices[0].Message.ToolCalls
			if len(calls) != len(tc.want) {
				t.Fatalf("tool calls = %+v, want %+v", calls, tc.want)
			}
			for i, want := range tc.want {
				if calls[i].ID != want.ID || calls[i].Function.Name != want.Name ||
					calls[i].Function.Arguments != want.Args {
					t.Fatalf("tool call[%d] = %+v, want %+v", i, calls[i], want)
				}
			}
			if got := auditDeref(out.Choices[0].FinishReason); got != tc.wantFinish {
				t.Fatalf("finish_reason = %q, want %q", got, tc.wantFinish)
			}
		})
	}
}

// Anthropic input_json_delta fragments must concatenate to valid JSON. A
// length-truncated call used to surface as a tool_use with broken JSON; it
// must be dropped so the turn reports max_tokens, mirroring the non-streaming
// converter. Corrupt args on a non-truncated turn are sanitised to {}.
func TestStreamToolArgumentsAreAlwaysValidJSON(t *testing.T) {
	tests := []struct {
		name      string
		finish    string
		args      string
		wantTools int
		wantInput string
		wantStop  string
	}{
		{
			name:      "length truncated args dropped",
			finish:    "length",
			args:      `{"command":"echo`,
			wantTools: 0,
			wantStop:  "max_tokens",
		},
		{
			name:      "length complete args kept",
			finish:    "length",
			args:      `{"command":"pwd"}`,
			wantTools: 1,
			wantInput: `{"command":"pwd"}`,
			wantStop:  "max_tokens",
		},
		{
			name:      "corrupt args sanitised",
			finish:    "tool_calls",
			args:      `{oops`,
			wantTools: 1,
			wantInput: `{}`,
			wantStop:  "tool_use",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			finish := tc.finish
			conv, raw := runChatStream(t, []AnthropicTool{stallBashTool}, []OpenAIStreamChunk{{
				Choices: []OpenAIChoice{{
					Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
						Index: 0, ID: "call_t", Type: "function",
						Function: OpenAIFunctionCall{Name: "Bash", Arguments: tc.args},
					}}},
					FinishReason: &finish,
				}},
			}})
			events := collectAnthropicEvents(t, bytes.NewBufferString(raw))

			tools := auditToolUses(events)
			if len(tools) != tc.wantTools {
				t.Fatalf("tool_use blocks = %+v, want %d", tools, tc.wantTools)
			}
			if tc.wantTools > 0 {
				if !json.Valid([]byte(tools[0].Input)) {
					t.Fatalf("input_json_delta is not valid JSON: %q", tools[0].Input)
				}
				if tools[0].Input != tc.wantInput {
					t.Fatalf("tool input = %q, want %q", tools[0].Input, tc.wantInput)
				}
			}
			if conv.StopReason() != tc.wantStop {
				t.Fatalf("stop_reason = %q, want %q", conv.StopReason(), tc.wantStop)
			}
		})
	}
}

// The Responses bridge has the same truncation contract: an incomplete
// upstream response whose function_call arguments were cut off must not
// surface as executable tool_use with invalid JSON.
func TestResponsesIncompleteTruncatedToolCallSurfacesMaxTokens(t *testing.T) {
	t.Run("stream", func(t *testing.T) {
		conv, events := runBridgeStream(t, []AnthropicTool{stallBashTool}, [][2]string{
			{"response.output_item.added", `{"item_id":"fc_1","item":{"type":"function_call","call_id":"call_t","name":"Bash"}}`},
			{"response.function_call_arguments.delta", `{"item_id":"fc_1","delta":"{\"command\":\"echo"}`},
			{"response.output_item.done", `{"item_id":"fc_1","item":{"type":"function_call","call_id":"call_t","name":"Bash","arguments":"{\"command\":\"echo"}}`},
			{"response.incomplete", `{"response":{"usage":{"input_tokens":10,"output_tokens":2}}}`},
		})
		if tools := auditToolUses(events); len(tools) != 0 {
			t.Fatalf("truncated call surfaced as tool_use: %+v", tools)
		}
		if got := stopOf(events); got != "max_tokens" {
			t.Fatalf("stop_reason = %q, want max_tokens", got)
		}
		if len(conv.DroppedCalls()) != 1 || conv.DroppedCalls()[0].Reason != "truncated-args" {
			t.Fatalf("dropped calls = %+v, want one truncated-args", conv.DroppedCalls())
		}
	})

	t.Run("nonstream", func(t *testing.T) {
		raw := []byte(`{"id":"resp_1","object":"response","status":"incomplete","output":[{"id":"fc_1","type":"function_call","status":"incomplete","call_id":"call_t","name":"Bash","arguments":"{\"command\":\"echo"}],"usage":{"input_tokens":10,"output_tokens":2}}`)
		out, err := ConvertResponsesToAnthropicResponse(raw, "m")
		if err != nil {
			t.Fatalf("ConvertResponsesToAnthropicResponse: %v", err)
		}
		for _, block := range out.Content {
			if block.Type == "tool_use" {
				t.Fatalf("truncated call surfaced as tool_use: %+v", block)
			}
		}
		if out.StopReason == nil || *out.StopReason != "max_tokens" {
			t.Fatalf("stop_reason = %v, want max_tokens", out.StopReason)
		}
	})
}

// If a tool_use block is emitted, the stop reason must be tool_use: clients
// use it to decide whether to execute the call. Unknown/content_filter
// upstream finishes used to fall through to end_turn, stranding an executable
// call. length keeps max_tokens, matching the non-streaming converter.
func TestToolUseStopReasonInvariant(t *testing.T) {
	tests := []struct {
		reason   string
		wantStop string
	}{
		{"stop", "tool_use"},
		{"tool_calls", "tool_use"},
		{"function_call", "tool_use"},
		{"content_filter", "tool_use"},
		{"future_reason", "tool_use"},
		{"length", "max_tokens"},
	}

	for _, tc := range tests {
		t.Run("stream/"+tc.reason, func(t *testing.T) {
			finish := tc.reason
			conv, _ := runChatStream(t, []AnthropicTool{stallBashTool}, []OpenAIStreamChunk{{
				Choices: []OpenAIChoice{{
					Delta: OpenAIDelta{ToolCalls: []OpenAIToolCall{{
						Index: 0, ID: "call_1", Type: "function",
						Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
					}}},
					FinishReason: &finish,
				}},
			}})
			if conv.StopReason() != tc.wantStop {
				t.Fatalf("stop_reason = %q, want %q", conv.StopReason(), tc.wantStop)
			}
		})

		t.Run("nonstream/"+tc.reason, func(t *testing.T) {
			finish := tc.reason
			out := ConvertResponse(&OpenAIResponse{
				ID: "chatcmpl-stop",
				Choices: []OpenAIChoice{{
					Message: &OpenAIMessage{
						Role: "assistant",
						ToolCalls: []OpenAIToolCall{{
							ID: "call_1", Type: "function",
							Function: OpenAIFunctionCall{Name: "Bash", Arguments: `{"command":"pwd"}`},
						}},
					},
					FinishReason: &finish,
				}},
			}, "chat-test")
			if out.StopReason == nil || *out.StopReason != tc.wantStop {
				t.Fatalf("stop_reason = %v, want %q", out.StopReason, tc.wantStop)
			}
		})
	}
}
