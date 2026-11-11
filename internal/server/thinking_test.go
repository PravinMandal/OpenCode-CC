package server

import (
	"strings"
	"testing"

	"github.com/PravinMandal/OpenCode-CC/internal/config"
	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
)

func TestApplyOutputConfigEffortReachesReasoningEffortMapping(t *testing.T) {
	// The effort-2025-11-24 channel: Claude Code sends thinking.type=adaptive
	// with the level in output_config.effort. A reasoning_effort mapping must
	// forward it instead of early-returning because there is no budget.
	cfg := config.Default()
	cfg.ThinkingBudgetMappings = []config.ThinkingBudgetMapping{
		{Match: "deepseek-v4.1-flash-free", Field: "reasoning_effort"},
	}
	levels := proxy.DefaultReasoningLevels

	req := &proxy.OpenAIRequest{}
	areq := &proxy.AnthropicRequest{
		Thinking:     &proxy.AnthropicThinking{Type: "adaptive"},
		OutputConfig: &proxy.AnthropicOutputConfig{Effort: "max"},
	}
	applyThinkingBudgetMapping(req, areq, "deepseek-v4.1-flash-free", cfg)
	applyOutputConfigEffort(req, areq, "deepseek-v4.1-flash-free", cfg, levels)

	if req.ReasoningEffort != "max" {
		t.Fatalf("reasoning_effort = %q, want %q", req.ReasoningEffort, "max")
	}
}

func TestApplyOutputConfigEffortFallsBackToThinkingEffort(t *testing.T) {
	cfg := config.Default()
	cfg.ThinkingBudgetMappings = []config.ThinkingBudgetMapping{
		{Match: "deepseek", Field: "reasoning_effort"},
	}
	areq := &proxy.AnthropicRequest{
		Thinking: &proxy.AnthropicThinking{Type: "adaptive", Effort: "low"},
	}
	req := &proxy.OpenAIRequest{}
	applyOutputConfigEffort(req, areq, "deepseek-v4.1-flash-free", cfg, proxy.DefaultReasoningLevels)
	if req.ReasoningEffort != "low" {
		t.Fatalf("reasoning_effort = %q, want %q", req.ReasoningEffort, "low")
	}
}

func TestApplyThinkingBudgetMappingEnabledBudgetPinsEffort(t *testing.T) {
	cfg := config.Default()
	cfg.ThinkingBudgetMappings = []config.ThinkingBudgetMapping{
		{Match: "deepseek", Field: "reasoning_effort"},
	}
	req := &proxy.OpenAIRequest{}
	areq := &proxy.AnthropicRequest{
		Thinking:     &proxy.AnthropicThinking{Type: "enabled", BudgetTokens: 8192},
		OutputConfig: &proxy.AnthropicOutputConfig{Effort: "low"},
	}
	applyThinkingBudgetMapping(req, areq, "deepseek-v4.1-flash-free", cfg)
	applyOutputConfigEffort(req, areq, "deepseek-v4.1-flash-free", cfg, proxy.DefaultReasoningLevels)
	if req.ReasoningEffort != "high" {
		t.Fatalf("reasoning_effort = %q, want the explicit budget's %q", req.ReasoningEffort, "high")
	}
}

func TestApplyOutputConfigEffortDoesNotOverrideThinkingChannel(t *testing.T) {
	// A thinking-object mapping already chose its channel; adding
	// reasoning_effort on top would be a second, conflicting instruction.
	cfg := config.Default()
	cfg.ThinkingBudgetMappings = []config.ThinkingBudgetMapping{
		{Match: "glm", Field: "thinking"},
	}
	req := &proxy.OpenAIRequest{}
	areq := &proxy.AnthropicRequest{
		Thinking:     &proxy.AnthropicThinking{Type: "adaptive"},
		OutputConfig: &proxy.AnthropicOutputConfig{Effort: "max"},
	}
	applyThinkingBudgetMapping(req, areq, "glm-5.3-flash-free", cfg)
	applyOutputConfigEffort(req, areq, "glm-5.3-flash-free", cfg, proxy.DefaultReasoningLevels)
	if req.ReasoningEffort != "" {
		t.Fatalf("reasoning_effort = %q, want none (thinking mapping owns the channel)", req.ReasoningEffort)
	}
}

func TestEstimateTokensCountsToolResultsAndSchemas(t *testing.T) {
	toolResult := strings.Repeat("x", 40_000)
	areq := &proxy.AnthropicRequest{
		System: proxy.AnthropicSystem{Blocks: []proxy.AnthropicContent{{Type: "text", Text: strings.Repeat("s", 4_000)}}},
		Messages: []proxy.AnthropicMessage{{
			Role: "user",
			Content: proxy.AnthropicMessageContent{Blocks: []proxy.AnthropicContent{{
				Type:    "tool_result",
				Content: &proxy.AnthropicMessageContent{Text: toolResult, IsStr: true},
			}}},
		}},
		Tools: []proxy.AnthropicTool{{Name: "shell", Description: strings.Repeat("d", 1_000), InputSchema: []byte(`{"type":"object"}`)}},
	}
	got := estimateTokens(areq)
	// (40000 + 4000 + ~1020 tool) / 4 ~= 11.2K. The old estimator answered 1001.
	if got < 10_000 {
		t.Fatalf("estimateTokens = %d, want >= 10000 for a 40K tool_result", got)
	}
}

func TestApplyThinkingBudgetMappingNormalizesAdaptiveForGLM(t *testing.T) {

	tests := []struct {
		name       string
		thinking   *proxy.AnthropicThinking
		wantType   string
		wantBudget int
	}{
		{name: "default", wantType: "enabled"},
		{name: "adaptive", thinking: &proxy.AnthropicThinking{Type: "adaptive", BudgetTokens: 1024}, wantType: "auto"},
		{name: "adaptive case insensitive", thinking: &proxy.AnthropicThinking{Type: "Adaptive"}, wantType: "auto"},
		{name: "enabled", thinking: &proxy.AnthropicThinking{Type: "enabled", BudgetTokens: 4096}, wantType: "enabled", wantBudget: 4096},
		{name: "disabled", thinking: &proxy.AnthropicThinking{Type: "disabled", BudgetTokens: 4096}, wantType: "disabled"},
		{name: "auto", thinking: &proxy.AnthropicThinking{Type: "auto", BudgetTokens: 4096}, wantType: "auto"},
		{name: "custom value preserved", thinking: &proxy.AnthropicThinking{Type: "provider-specific"}, wantType: "provider-specific"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &proxy.OpenAIRequest{}
			areq := &proxy.AnthropicRequest{Thinking: tt.thinking}
			applyThinkingBudgetMapping(req, areq, "glm-5.2", config.Default())

			if req.Thinking == nil || req.Thinking.Type != tt.wantType {
				t.Fatalf("thinking = %+v, want type %q", req.Thinking, tt.wantType)
			}
			if tt.wantBudget == 0 {
				if req.Thinking.BudgetTokens != nil {
					t.Fatalf("budget_tokens = %d, want omitted", *req.Thinking.BudgetTokens)
				}
			} else if req.Thinking.BudgetTokens == nil || *req.Thinking.BudgetTokens != tt.wantBudget {
				t.Fatalf("budget_tokens = %v, want %d", req.Thinking.BudgetTokens, tt.wantBudget)
			}
		})
	}
}
