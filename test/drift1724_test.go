// Regression tests for the 1.72.4 wire-dialect drift fixes (verified against
// the npm package source on 2026-09-30).
package contract_test

import (
	"testing"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/test/testutil"
)

// 1.72.4 reads rawFinishReason ?? finishReason — raw wins when present.
func TestRawFinishReasonPrecedence(t *testing.T) {
	mock := testutil.NewMockCC(t,
		`{"type":"text-start"}`,
		`{"type":"text-delta","text":"partial"}`,
		`{"type":"finish","finishReason":"length","rawFinishReason":"max_output_tokens","totalUsage":{"inputTokens":9,"outputTokens":3}}`,
	)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	got := testutil.JSON(t, raw)["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if got != "length" {
		t.Fatalf("finish_reason = %v (raw max_output_tokens must normalize to length)", got)
	}

	// And on the Anthropic surface it must be max_tokens, not end_turn.
	_, raw, err = p.Post("/v1/messages",
		map[string]any{"model": "m", "max_tokens": 100, "messages": []map[string]any{{"role": "user", "content": "hi"}}},
		testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.JSON(t, raw)["stop_reason"]; got != "max_tokens" {
		t.Fatalf("stop_reason = %v, want max_tokens", got)
	}
}

// New 1.72.4 stream events must not produce "Unknown CC event type" warnings.
func TestCacheWriteTokensEventSilent(t *testing.T) {
	ndjson := []string{
		`{"type":"text-start"}`,
		`{"type":"cache-write-tokens","tokens":42}`,
		`{"type":"text-delta","text":"ok"}`,
		`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":9,"outputTokens":3,"inputTokenDetails":{"cacheReadTokens":5,"cacheWriteTokens":42}}}`,
	}
	mock := testutil.NewMockCC(t, ndjson...)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if testutil.JSON(t, raw) == nil {
		t.Fatal("unparseable response")
	}
	// 1.72.4 usage shape: cache tokens under inputTokenDetails must surface
	// as prompt_tokens_details.cached_tokens.
	j := testutil.JSON(t, raw)
	details := j["usage"].(map[string]any)["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(5) {
		t.Fatalf("cached_tokens = %v, want 5 (inputTokenDetails.cacheReadTokens)", details["cached_tokens"])
	}
}

// EffectiveCachedTokens covers both wire shapes.
func TestEffectiveCachedTokens(t *testing.T) {
	legacy := &cc.CCUsage{InputTokens: 100, CachedInputTokens: 80}
	if got := legacy.EffectiveCachedTokens(); got != 80 {
		t.Fatalf("legacy shape: %d", got)
	}
	modern := &cc.CCUsage{InputTokens: 100, InputTokenDetails: &cc.CCInputTokenDetails{CacheReadTokens: 80}}
	if got := modern.EffectiveCachedTokens(); got != 80 {
		t.Fatalf("1.72.4 shape: %d", got)
	}
	// Anthropic input_tokens subtraction must work on the modern shape.
	if got := cc.AnthropicInputTokens(modern, -1); got != 20 {
		t.Fatalf("anthropic input = %d, want 20 (100-80)", got)
	}
}
