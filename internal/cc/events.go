package cc

import (
	"encoding/json"
	"regexp"
	"strings"
)

// CCEvent is one line of the upstream NDJSON event stream from
// POST /alpha/generate. Fields are shared across event types; Type selects.
type CCEvent struct {
	Type         string          `json:"type"`
	Text         string          `json:"text"`
	Delta        string          `json:"delta"`
	ToolCallID   string          `json:"toolCallId"`
	ToolName     string          `json:"toolName"`
	Input        json.RawMessage `json:"input"`
	FinishReason string          `json:"finishReason"`
	Usage        *CCUsage        `json:"usage"`
	TotalUsage   *CCUsage        `json:"totalUsage"`
	Error        *CCEventError   `json:"error"`
	Message      string          `json:"message"`
	Code         string          `json:"code"`
}

type CCEventError struct {
	Message     string `json:"message"`
	Code        string `json:"code"`
	StatusCode  *int   `json:"statusCode"`
	IsRetryable *bool  `json:"isRetryable"`
}

type CCUsage struct {
	InputTokens       int64                `json:"inputTokens"`
	OutputTokens      int64                `json:"outputTokens"`
	CachedInputTokens int64                `json:"cachedInputTokens"`
	InputTokenDetails *CCInputTokenDetails `json:"inputTokenDetails"`
}

type CCInputTokenDetails struct {
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	NoCacheTokens    int64 `json:"noCacheTokens"`
}

// TextOf mirrors event.text || event.delta || ”.
func (e *CCEvent) TextOf() string {
	if e.Text != "" {
		return e.Text
	}
	return e.Delta
}

// ArgsOf mirrors typeof input === 'string' ? input : JSON.stringify(input||{}).
func (e *CCEvent) ArgsOf() string {
	raw := e.Input
	if len(raw) == 0 {
		return "{}"
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	return string(raw)
}

// NormalizeUsage zeroes input + cached when outputTokens == 0
// (anti false billing — port of normalizeUsage).
func NormalizeUsage(u *CCUsage) {
	if u == nil {
		return
	}
	if u.OutputTokens == 0 {
		u.InputTokens = 0
		u.CachedInputTokens = 0
	}
}

// AnthropicInputTokens returns the Anthropic-semantic input_tokens, which
// counts only the NON-cached portion. CC's inputTokens is the total
// (noCache + cacheRead === inputTokens, verified on live traffic).
// Preference: explicit noCacheTokens override → inputTokenDetails.noCacheTokens
// → subtraction fallback for older upstreams. Port of anthropicInputTokens.
func AnthropicInputTokens(u *CCUsage, noCacheOverride int64) int64 {
	if u == nil {
		u = &CCUsage{}
	}
	if noCacheOverride >= 0 {
		return noCacheOverride
	}
	if u.InputTokenDetails != nil && u.InputTokenDetails.NoCacheTokens >= 0 {
		return u.InputTokenDetails.NoCacheTokens
	}
	cacheRead := u.CachedInputTokens
	if u.InputTokenDetails != nil && u.InputTokenDetails.CacheReadTokens > 0 {
		cacheRead = u.InputTokenDetails.CacheReadTokens
	}
	cacheWrite := int64(0)
	if u.InputTokenDetails != nil {
		cacheWrite = u.InputTokenDetails.CacheWriteTokens
	}
	n := u.InputTokens - cacheRead - cacheWrite
	if n < 0 {
		return 0
	}
	return n
}

var networkFinishRe = regexp.MustCompile(`^(?:network|connection|upstream)[-_\s]?error$`)

// MapFinishReason normalizes upstream finishReason to the proxy's internal
// vocabulary (port of mapFinishReason, aligned with the CLI's
// normalizeStopReason2 / isNetworkFailureFinish):
//
//	tool_use | tool-calls | tool_calls → tool_calls
//	length | max_tokens | max_output_tokens | model_context_window_exceeded → length
//	network/connection/upstream error family → upstream_error
//	pause_turn → passthrough; unknown values → passthrough (never fold to stop)
func MapFinishReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	if r == "" {
		return "stop"
	}
	switch r {
	case "tool-calls", "tool_calls", "tool_use":
		return "tool_calls"
	case "length", "max_tokens", "max_output_tokens", "model_context_window_exceeded":
		return "length"
	}
	if networkFinishRe.MatchString(r) {
		return "upstream_error"
	}
	return r
}

// IncompleteDetail explains why an upstream stream did not finish normally;
// "" means it did. Two cases, both retryable 502s in the CLI:
// no finish event at all, or provider-reported connection failure.
func IncompleteDetail(sawFinish bool, finishReason string) string {
	if !sawFinish {
		return "no finish event"
	}
	if finishReason == "upstream_error" {
		return "provider reported an upstream connection failure"
	}
	return ""
}

// ToOpenAIFinishReason folds pause_turn to length (OpenAI has no such enum;
// "length" at least conveys "output incomplete", "stop" would lie).
func ToOpenAIFinishReason(finishReason string) string {
	if finishReason == "pause_turn" {
		return "length"
	}
	return finishReason
}
