// Port of the reference test/stream-end.test.mjs (issue #38 contracts):
// upstream streams that don't finish normally must be reported honestly —
// never faked into a completed answer.
package contract_test

import (
	"encoding/json"
	"testing"

	"github.com/hongkongkiwi/commandcode2api/test/testutil"
)

func anthropicBody(stream bool) map[string]any {
	return map[string]any{
		"model": "m", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"stream":   stream,
	}
}

func responsesBody(stream bool) map[string]any {
	return map[string]any{"model": "m", "input": "hi", "stream": stream}
}

// ── ① truncation finishReasons must be reported as truncated ──────────

func TestMaxOutputTokensReportsLength(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("max_output_tokens")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d body=%s", resp.StatusCode, raw)
	}
	j := testutil.JSON(t, raw)
	got := j["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if got != "length" {
		t.Fatalf("finish_reason = %v, want length (max_output_tokens means truncated)", got)
	}
}

func TestContextWindowExceededReportsMaxTokens(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("model_context_window_exceeded")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	got := testutil.JSON(t, raw)["stop_reason"]
	if got != "max_tokens" {
		t.Fatalf("stop_reason = %v, want max_tokens (reporting end_turn would lie)", got)
	}
}

func TestMaxOutputTokensResponsesIncomplete(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("max_output_tokens")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/responses", responsesBody(false), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	j := testutil.JSON(t, raw)
	if j["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", j["status"])
	}
	details, _ := json.Marshal(j["incomplete_details"])
	if string(details) != `{"reason":"max_output_tokens"}` {
		t.Fatalf("incomplete_details = %s", details)
	}
}

// ── ② pause_turn must not be swallowed ─────────────────────────────────

func TestPauseTurnAnthropicPassthrough(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("pause_turn")...)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	got := testutil.JSON(t, raw)["stop_reason"]
	if got != "pause_turn" {
		t.Fatalf("stop_reason = %v, want pause_turn passthrough", got)
	}
}

func TestPauseTurnChatBecomesLength(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("pause_turn")...)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	got := testutil.JSON(t, raw)["choices"].([]any)[0].(map[string]any)["finish_reason"]
	if got != "length" {
		t.Fatalf("finish_reason = %v, want length (never stop — that lies about completion)", got)
	}
}

func TestPauseTurnResponsesIncomplete(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("pause_turn")...)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/responses", responsesBody(false), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	j := testutil.JSON(t, raw)
	if j["status"] != "incomplete" {
		t.Fatalf("status = %v, want incomplete", j["status"])
	}
	details, _ := json.Marshal(j["incomplete_details"])
	if string(details) != `{"reason":"pause_turn"}` {
		t.Fatalf("incomplete_details = %s", details)
	}
}

// ── ③ provider-reported connection failure → retryable 502 ────────────

func TestNetworkErrorChatRetryable502(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("network-error")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 (CLI treats this family as retryable)", resp.StatusCode)
	}
	j := testutil.JSON(t, raw)
	if j["error"].(map[string]any)["type"] != "upstream_error" {
		t.Fatalf("error.type = %v", j["error"])
	}
	if j["retry_after"] != float64(10) {
		t.Fatalf("retry_after = %v", j["retry_after"])
	}
}

func TestConnectionErrorAnthropicRetryable502(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("connection_error")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if testutil.JSON(t, raw)["error"].(map[string]any)["type"] != "upstream_error" {
		t.Fatal("error.type must be upstream_error")
	}
}

// ── ④ no finish event at all ────────────────────────────────────────────

func TestNoFinishChat502(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.NoFinish...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 (not 200 with a silently short answer)", resp.StatusCode)
	}
	j := testutil.JSON(t, raw)
	if j["error"].(map[string]any)["type"] != "upstream_error" {
		t.Fatal("error.type must be upstream_error")
	}
	testutil.AssertContains(t, string(raw), "no finish event")
}

func TestNoFinishAnthropic502(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.NoFinish...)
	p := testutil.Setup(t, mock)

	resp, _, err := p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestNoFinishResponses502(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.NoFinish...)
	p := testutil.Setup(t, mock)

	resp, _, err := p.Post("/v1/responses", responsesBody(false), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

// ── ⑤ streaming paths must not fake termination either ─────────────────

func TestNoFinishAnthropicStreamErrorEvent(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.NoFinish...)
	p := testutil.Setup(t, mock)

	body := anthropicBody(true)
	_, raw, err := p.Post("/v1/messages", body, testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertContains(t, string(raw), "event: error")
	testutil.AssertNotContains(t, string(raw), "event: message_stop")
}

func TestNoFinishChatStreamError(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.NoFinish...)
	p := testutil.Setup(t, mock)

	body := testutil.CHAT()
	body["stream"] = true
	_, raw, err := p.Post("/v1/chat/completions", body, testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertContains(t, string(raw), "upstream_error")
	testutil.AssertNotContains(t, string(raw), "[DONE]")
}

// ── ⑥ normal finishes must be unaffected (regression) ──────────────────

func TestNormalFinishAllProtocols(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("chat: status=%d err=%v body=%s", resp.StatusCode, err, raw)
	}
	if got := testutil.JSON(t, raw)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != "stop" {
		t.Fatalf("chat finish_reason = %v", got)
	}

	resp, raw, err = p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("messages: status=%d err=%v body=%s", resp.StatusCode, err, raw)
	}
	if got := testutil.JSON(t, raw)["stop_reason"]; got != "end_turn" {
		t.Fatalf("messages stop_reason = %v", got)
	}

	resp, raw, err = p.Post("/v1/responses", responsesBody(false), testutil.Bearer("user_test"))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("responses: status=%d err=%v body=%s", resp.StatusCode, err, raw)
	}
	if got := testutil.JSON(t, raw)["status"]; got != "completed" {
		t.Fatalf("responses status = %v", got)
	}
}

func TestToolCallsFinishMapsBothProtocols(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("tool-calls")...)
	p := testutil.Setup(t, mock)

	_, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.JSON(t, raw)["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != "tool_calls" {
		t.Fatalf("chat finish_reason = %v, want tool_calls", got)
	}

	_, raw, err = p.Post("/v1/messages", anthropicBody(false), testutil.XKey("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := testutil.JSON(t, raw)["stop_reason"]; got != "tool_use" {
		t.Fatalf("messages stop_reason = %v, want tool_use", got)
	}
}
