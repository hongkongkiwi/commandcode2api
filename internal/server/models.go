package server

import (
	"context"
	"sync"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
)

// Models serves GET /v1/models: dynamic provider catalog with a hardcoded
// fallback and a refresh interval (port of fetchModels / MODELS).
type Models struct {
	Client       *cc.Client
	RefreshEvery time.Duration

	mu      sync.Mutex
	dyn     []map[string]string
	fetched time.Time
}

// HardcodedModels mirrors the MODELS constant.
var HardcodedModels = []map[string]string{
	{"id": "claude-sonnet-4-6", "name": "Claude Sonnet 4.6"},
	{"id": "claude-opus-4-8", "name": "Claude Opus 4.8"},
	{"id": "claude-opus-4-7", "name": "Claude Opus 4.7"},
	{"id": "claude-haiku-4-5-20251001", "name": "Claude Haiku 4.5"},
	{"id": "gpt-5.5", "name": "GPT-5.5"},
	{"id": "gpt-5.4", "name": "GPT-5.4"},
	{"id": "gpt-5.4-mini", "name": "GPT-5.4 Mini"},
	{"id": "gpt-5.3-codex", "name": "GPT-5.3 Codex"},
	{"id": "deepseek/deepseek-v4-pro", "name": "DeepSeek V4 Pro"},
	{"id": "deepseek/deepseek-v4-flash", "name": "DeepSeek V4 Flash"},
	{"id": "moonshotai/Kimi-K2.6", "name": "Kimi K2.6"},
	{"id": "moonshotai/Kimi-K2.5", "name": "Kimi K2.5"},
	{"id": "zai-org/GLM-5.1", "name": "GLM 5.1"},
	{"id": "zai-org/GLM-5", "name": "GLM 5"},
	{"id": "MiniMaxAI/MiniMax-M3", "name": "MiniMax M3"},
	{"id": "MiniMaxAI/MiniMax-M2.7", "name": "MiniMax M2.7"},
	{"id": "MiniMaxAI/MiniMax-M2.5", "name": "MiniMax M2.5"},
	{"id": "Qwen/Qwen3.6-Max-Preview", "name": "Qwen 3.6 Max Preview"},
	{"id": "Qwen/Qwen3.6-Plus", "name": "Qwen 3.6 Plus"},
	{"id": "Qwen/Qwen3.7-Max", "name": "Qwen 3.7 Max"},
	{"id": "stepfun/Step-3.7-Flash", "name": "Step 3.7 Flash"},
	{"id": "stepfun/Step-3.5-Flash", "name": "Step 3.5 Flash"},
	{"id": "xiaomi/mimo-v2.5-pro", "name": "MiMo V2.5 Pro"},
	{"id": "xiaomi/mimo-v2.5", "name": "MiMo V2.5"},
	{"id": "google/gemini-3.5-flash", "name": "Gemini 3.5 Flash"},
	{"id": "google/gemini-3.1-flash-lite", "name": "Gemini 3.1 Flash Lite"},
}

// List returns the current model catalog (dynamic when fresh, hardcoded
// fallback otherwise).
func (m *Models) List(ctx context.Context, apiKey string, useProvider bool) []map[string]string {
	m.mu.Lock()
	if m.dyn != nil && time.Since(m.fetched) < m.RefreshEvery {
		d := m.dyn
		m.mu.Unlock()
		return d
	}
	m.mu.Unlock()

	if apiKey != "" && useProvider {
		if models, ok := m.Client.FetchModels(ctx, apiKey); ok {
			m.mu.Lock()
			m.dyn = models
			m.fetched = time.Now()
			m.mu.Unlock()
			return models
		}
	}
	return HardcodedModels
}
