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

	mu           sync.Mutex
	dyn          []map[string]string
	fetched      time.Time
	providerGone bool // /provider/v1/models returned 404 — stop retrying
}

// HardcodedModels mirrors the 1.72.4 CLI's built-in catalog (extracted from
// the npm package source; the /provider/v1/models endpoint was removed in
// 1.7x, so this list is the steady-state catalog).
var HardcodedModels = []map[string]string{
	{"id": "deepseek/deepseek-v4-pro", "name": "DeepSeek V4 Pro"},
	{"id": "deepseek/deepseek-v4-flash", "name": "DeepSeek V4 Flash"},
	{"id": "deepseek/deepseek-v4-flash-fast", "name": "DeepSeek V4 Flash Fast"},
	{"id": "deepseek/deepseek-v4.1-flash", "name": "DeepSeek V4.1 Flash"},
	{"id": "MiniMaxAI/MiniMax-M2.5", "name": "MiniMax M2.5"},
	{"id": "MiniMaxAI/MiniMax-M2.7", "name": "MiniMax M2.7"},
	{"id": "MiniMaxAI/MiniMax-M3", "name": "MiniMax M3"},
	{"id": "MiniMaxAI/MiniMax-M3-Free", "name": "MiniMax M3 Free"},
	{"id": "moonshotai/Kimi-K2.5", "name": "Kimi K2.5"},
	{"id": "moonshotai/Kimi-K2.6", "name": "Kimi K2.6"},
	{"id": "moonshotai/Kimi-K2.7-Code", "name": "Kimi K2.7 Code"},
	{"id": "moonshotai/Kimi-K2.7-Code-Highspeed", "name": "Kimi K2.7 Code Highspeed"},
	{"id": "moonshotai/Kimi-K3", "name": "Kimi K3"},
	{"id": "zai-org/GLM-5", "name": "GLM 5"},
	{"id": "zai-org/GLM-5.1", "name": "GLM 5.1"},
	{"id": "zai-org/GLM-5.2", "name": "GLM 5.2"},
	{"id": "zai-org/GLM-5.2-Fast", "name": "GLM 5.2 Fast"},
	{"id": "zai-org/GLM-5.3", "name": "GLM 5.3"},
	{"id": "Qwen/Qwen3.6-Max-Preview", "name": "Qwen 3.6 Max Preview"},
	{"id": "Qwen/Qwen3.6-Plus", "name": "Qwen 3.6 Plus"},
	{"id": "Qwen/Qwen3.7-Max", "name": "Qwen 3.7 Max"},
	{"id": "Qwen/Qwen3.7-Plus", "name": "Qwen 3.7 Plus"},
	{"id": "Qwen/Qwen3.7-Flash", "name": "Qwen 3.7 Flash"},
	{"id": "Qwen/Qwen3.8-Max", "name": "Qwen 3.8 Max"},
	{"id": "Qwen/Qwen3.8-Flash", "name": "Qwen 3.8 Flash"},
	{"id": "stepfun/Step-3.7-Flash", "name": "Step 3.7 Flash"},
	{"id": "stepfun/Step-3.5-Flash", "name": "Step 3.5 Flash"},
	{"id": "stepfun/Step-5-Preview", "name": "Step 5 Preview"},
	{"id": "xiaomi/mimo-v2.5", "name": "MiMo V2.5"},
	{"id": "xiaomi/mimo-v2.5-pro", "name": "MiMo V2.5 Pro"},
	{"id": "xiaomi/mimo-v2.6-flash", "name": "MiMo V2.6 Flash"},
	{"id": "xiaomi/mimo-v2.6-pro", "name": "MiMo V2.6 Pro"},
	{"id": "google/gemini-3.5-flash", "name": "Gemini 3.5 Flash"},
	{"id": "google/gemini-3.8-flash", "name": "Gemini 3.8 Flash"},
	{"id": "xai/grok-4.5", "name": "Grok 4.5"},
	{"id": "xai/grok-4.7", "name": "Grok 4.7"},
	{"id": "openai/gpt-5.6", "name": "GPT-5.6"},
	{"id": "openai/gpt-5.6-luna", "name": "GPT-5.6 Luna"},
	{"id": "tencent/Hy3", "name": "Hunyuan H3"},
	{"id": "tencent/hy4-preview", "name": "Hunyuan H4 Preview"},
	{"id": "nvidia/nemotron-3-ultra-550b-a55b", "name": "Nemotron 3 Ultra"},
	{"id": "thinkingmachines/inkling", "name": "Inkling"},
	{"id": "meta/muse-spark-1.3", "name": "Muse Spark 1.3"},
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

	if apiKey != "" && useProvider && !m.Client.ModelsGone() {
		models, ok := m.Client.FetchModels(ctx, apiKey)
		m.mu.Lock()
		if ok {
			m.dyn = models
		}
		m.fetched = time.Now() // success or not: retry only after the interval
		m.mu.Unlock()
		if ok {
			return models
		}
	}
	return HardcodedModels
}
