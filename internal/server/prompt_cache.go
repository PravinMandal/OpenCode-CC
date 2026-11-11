package server

import (
	"github.com/PravinMandal/OpenCode-CC/internal/config"
	"github.com/PravinMandal/OpenCode-CC/internal/proxy"
)

func promptCacheOptionsFromConfig(cfg *config.Config) proxy.PromptCacheOptions {
	if cfg == nil {
		return proxy.PromptCacheOptions{}
	}
	return proxy.PromptCacheOptions{
		Enabled:               cfg.PromptCacheEnabled,
		KeyPrefix:             cfg.PromptCacheKeyPrefix,
		AnthropicCacheControl: cfg.PromptCacheAnthropicControl,
		NormalizePrompts:      cfg.PromptCacheNormalize,
	}
}
