package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// legacyAggregateFunctionIndex keys fragmented legacy function_call deltas.
// Upstream tool_calls indexes are always >= 0, so -1 never collides; keeping
// it stable across chunks merges the fragments into one call.
const legacyAggregateFunctionIndex = -1

// AggregateOpenAIStream consumes an OpenAI Chat Completions SSE response and
// returns the equivalent non-streaming response. Zen's free lane requires
// stream:true upstream, while ordinary SDKs still legitimately ask for a
// single JSON response.
func AggregateOpenAIStream(r io.Reader) (*OpenAIResponse, error) {
	var (
		id, model, finish  string
		content, reasoning strings.Builder
		usage              OpenAIUsage
		tools              = map[int]*OpenAIToolCall{}
		toolOrder          []int
		syntheticIndex     = -2
		sawChunk           bool
	)
	appendTool := func(call OpenAIToolCall) {
		index := call.Index
		if index < 0 && index != legacyAggregateFunctionIndex {
			index = syntheticIndex
			syntheticIndex--
		}
		// Non-streaming message.tool_calls normally omit index, so every
		// entry defaults to 0. A different non-empty id/name under an index
		// already claimed by another call is a second complete call, not a
		// fragment; park it under a fresh synthetic key instead of merging
		// its arguments into corrupt JSON.
		if current, ok := tools[index]; ok && call.ID != "" && current.ID != "" &&
			call.ID != current.ID && call.Function.Name != "" {
			index = syntheticIndex
			syntheticIndex--
		}
		current, ok := tools[index]
		if !ok {
			current = &OpenAIToolCall{Index: index, Type: "function"}
			tools[index] = current
			toolOrder = append(toolOrder, index)
		}
		if call.ID != "" {
			current.ID = call.ID
		}
		if call.Type != "" {
			current.Type = call.Type
		}
		if call.Function.Name != "" {
			current.Function.Name = call.Function.Name
		}
		current.Function.Arguments += call.Function.Arguments
	}
	err := ScanOpenAIStream(r, func(chunk *OpenAIStreamChunk) error {
		if chunk == nil {
			return nil
		}
		sawChunk = true
		if chunk.ID != "" && id == "" {
			id = chunk.ID
		}
		if chunk.Model != "" && model == "" {
			model = chunk.Model
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finish = *choice.FinishReason
			}
			if choice.Message != nil {
				content.WriteString(messageContentString(choice.Message))
				reasoning.WriteString(choice.Message.ReasoningContent)
				for _, call := range choice.Message.ToolCalls {
					appendTool(call)
				}
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			for _, call := range choice.Delta.ToolCalls {
				appendTool(call)
			}
			if choice.Delta.FunctionCall != nil {
				appendTool(OpenAIToolCall{
					Index:    legacyAggregateFunctionIndex,
					Type:     "function",
					Function: *choice.Delta.FunctionCall,
				})
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if !sawChunk {
		return nil, fmt.Errorf("upstream stream contained no OpenAI chunks")
	}
	if finish == "" {
		finish = "stop"
	}
	if id == "" {
		id = "chatcmpl-aggregated"
	}
	if model == "" {
		model = "unknown"
	}
	out := &OpenAIResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []OpenAIChoice{{
			Index: 0,
			Message: &OpenAIMessage{
				Role:             "assistant",
				Content:          content.String(),
				ReasoningContent: reasoning.String(),
			},
			FinishReason: &finish,
		}},
		Usage: usage,
	}
	if len(toolOrder) > 0 {
		calls := make([]OpenAIToolCall, 0, len(toolOrder))
		for i, index := range toolOrder {
			call := *tools[index]
			// Synthetic keys keep complete message calls apart while
			// aggregating; the final message exposes clean sequential
			// indexes instead of internal negative ones.
			call.Index = i
			calls = append(calls, call)
		}
		out.Choices[0].Message.ToolCalls = calls
		if finish == "stop" {
			out.Choices[0].FinishReason = stringPtr("tool_calls")
		}
	}
	return out, nil
}

func stringPtr(value string) *string { return &value }

// AggregateResponsesSSE extracts the final Responses object from a Zen SSE
// response. The upstream includes the complete output in its
// response.completed/response.incomplete event, so this avoids reimplementing
// every Responses event variant and preserves tool/reasoning items exactly.
func AggregateResponsesSSE(raw []byte) ([]byte, error) {
	var final []byte
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		switch event.Type {
		case "response.completed", "response.incomplete", "response.failed":
			if len(event.Response) > 0 && string(event.Response) != "null" {
				final = append([]byte(nil), event.Response...)
			}
		}
	}
	if len(final) == 0 {
		return nil, errors.New("upstream Responses stream contained no terminal response event")
	}
	return final, nil
}
