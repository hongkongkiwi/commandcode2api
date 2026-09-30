// Port of the reference test/connection-lifecycle.test.mjs:
// error-event statusCode propagation, silent-event list, and stream-idle
// timeout termination semantics (end, never reset).
package contract_test

import (
	"testing"

	"github.com/hongkongkiwi/commandcode2api/test/testutil"
)

// ── ① error events carry statusCode that must be honored ───────────────

func TestErrorEventStatusCode429(t *testing.T) {
	mock := testutil.NewMockCC(t,
		`{"type":"text-start"}`,
		`{"type":"text-delta","text":"partial"}`,
		`{"type":"error","error":{"message":"providers are currently at capacity","statusCode":429}}`,
	)
	p := testutil.Setup(t, mock)

	resp, raw, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Fatalf("status = %d, want 429 (upstream statusCode must not collapse to 502)\n%s", resp.StatusCode, raw)
	}
	j := testutil.JSON(t, raw)
	if j["error"].(map[string]any)["type"] != "rate_limit_error" {
		t.Fatalf("error.type = %v", j["error"])
	}
	if j["retry_after"] != float64(30) {
		t.Fatalf("retry_after = %v, want 30 (SDK needs backoff hint)", j["retry_after"])
	}
}

func TestErrorEventStatusCode503(t *testing.T) {
	mock := testutil.NewMockCC(t,
		`{"type":"text-start"}`,
		`{"type":"error","error":{"message":"service unavailable","statusCode":503}}`,
	)
	p := testutil.Setup(t, mock)

	resp, _, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 503 {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestErrorEventNoStatusCodeFallsTo502(t *testing.T) {
	mock := testutil.NewMockCC(t,
		`{"type":"text-start"}`,
		`{"type":"error","error":{"message":"something broke"}}`,
	)
	p := testutil.Setup(t, mock)

	resp, _, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d, want 502 (preserve original behavior)", resp.StatusCode)
	}
}

func TestMessageNNNPrefixBeatsStatusCode(t *testing.T) {
	mock := testutil.NewMockCC(t,
		`{"type":"text-start"}`,
		`{"type":"error","error":{"message":"<400> bad request","statusCode":503}}`,
	)
	p := testutil.Setup(t, mock)

	resp, _, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("status = %d, want 400 (the <NNN> prefix is the primary source)", resp.StatusCode)
	}
}

// ── ② stream idle timeout: clean end(), full delivery of prior content ─

func TestStreamIdleTimeoutCleanEnd(t *testing.T) {
	t.Setenv("CC_STREAM_IDLE_MS", "300")

	stall := testutil.NewStallingUpstream(t)
	p := testutil.SetupAgainst(t, stall)

	body := testutil.CHAT()
	body["stream"] = true
	_, raw, err := p.Post("/v1/chat/completions", body, testutil.Bearer("user_test"))
	if err != nil {
		t.Fatal(err)
	}
	// Content already produced must survive the timeout (a destroy would lose
	// the buffer), and the error must land in the stream so SDKs retry.
	testutil.AssertContains(t, string(raw), "partial-content", "rate_limit_error")
}

// ── ③ pre-requests + generate headers sanity ────────────────────────────

func TestInitPreRequestsAndGenerateHeaders(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	p := testutil.Setup(t, mock)

	if _, _, err := p.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_abc")); err != nil {
		t.Fatal(err)
	}

	// Two pre-requests fire on first use of the key (fingerprint + lifecycle).
	seenPaths := map[string]bool{}
	for i := 0; i < 3; i++ {
		req := mock.NextRequest(t)
		seenPaths[req.Path] = true
		if req.Path == "/alpha/generate" {
			if got := req.Headers.Get("x-command-code-version"); got != "1.53.1" {
				t.Fatalf("x-command-code-version = %q, want pinned 1.53.1", got)
			}
			if got := req.Headers.Get("x-cli-environment"); got != "production" {
				t.Fatalf("x-cli-environment = %q", got)
			}
			if got := req.Headers.Get("User-Agent"); got != "cli" {
				t.Fatalf("User-Agent = %q, want cli", got)
			}
			if got := req.Headers.Get("x-project-slug"); got != "c-users-dev-projects-app" {
				t.Fatalf("x-project-slug = %q", got)
			}
			if got := req.Headers.Get("Authorization"); got != "Bearer user_abc" {
				t.Fatalf("Authorization = %q", got)
			}
			if tp := req.Headers.Get("traceparent"); len(tp) != 55 || tp[:3] != "00-" {
				t.Fatalf("traceparent = %q", tp)
			}
			// envelope: memory/taste/skills null, params.tools always present
			env, err := testutil.JSONBytes(req.Body)
			if err != nil {
				t.Fatalf("envelope not JSON: %v", err)
			}
			if v, ok := env["memory"]; !ok || v != nil {
				t.Fatalf("memory must be null, got %v (ok=%v)", v, ok)
			}
			if v, ok := env["skills"]; !ok || v != nil {
				t.Fatalf("skills must be null (not empty string), got %v", v)
			}
			params := env["params"].(map[string]any)
			if _, ok := params["tools"]; !ok {
				t.Fatal("params.tools must always be present (empty array ok)")
			}
			if _, ok := params["stream"]; !ok || params["stream"] != true {
				t.Fatal("params.stream must always be true")
			}
			// emptySystemPlaceholder: no system in request → space placeholder
			sys, ok := params["system"].([]any)
			if !ok || len(sys) == 0 {
				t.Fatal("params.system placeholder missing (issue #17)")
			}
			first := sys[0].(map[string]any)
			if first["text"] != " " {
				t.Fatalf("placeholder text = %v, want single space", first["text"])
			}
		}
	}
	if !seenPaths["/alpha/fingerprint/record"] || !seenPaths["/alpha/lifecycle-events"] || !seenPaths["/alpha/generate"] {
		t.Fatalf("expected fingerprint+generate+lifecycle, saw %v", seenPaths)
	}
}

func TestSessionHeaderOverride(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	p := testutil.Setup(t, mock)

	hdrs := testutil.Bearer("user_abc")
	hdrs["X-Session-Id"] = "my-fixed-session-123"
	if _, _, err := p.Post("/v1/chat/completions", testutil.CHAT(), hdrs); err != nil {
		t.Fatal(err)
	}
	// Skip pre-requests; only /alpha/generate carries x-session-id.
	var req testutil.RecordedRequest
	for i := 0; i < 3; i++ {
		req = mock.NextRequest(t)
		if req.Path == "/alpha/generate" {
			break
		}
	}
	if got := req.Headers.Get("x-session-id"); got != "my-fixed-session-123" {
		t.Fatalf("x-session-id = %q, want client override", got)
	}
}
