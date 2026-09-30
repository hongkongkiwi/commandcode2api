package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/ir"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
	"github.com/hongkongkiwi/commandcode2api/internal/pool"
	"github.com/hongkongkiwi/commandcode2api/internal/store"
	"github.com/hongkongkiwi/commandcode2api/internal/translate"
)

// Deps bundles everything handlers need.
type Deps struct {
	Client *cc.Client
	States *cc.KeyStates
	Models *Models
	Cfg    *config.Config
	// Pool infrastructure — nil in pure pass-through deployments.
	Pool  *pool.Pool
	Store *store.Store
}

// SSEHeaders are written once real content (or an explicit start) arrives.
func sseHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// beginSSE writes the 200 SSE headers and returns a flusher.
func beginSSE(w http.ResponseWriter) http.Flusher {
	sseHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	if f != nil {
		f.Flush()
	}
	return f
}

func flushBatch(w http.ResponseWriter, f http.Flusher, buf translate.SSEBuf) {
	if len(buf) == 0 {
		return
	}
	_, _ = w.Write(buf)
	if f != nil {
		f.Flush()
	}
}

// parseIR decodes an OpenAI-Chat-shaped body into the IR.
func parseIR(raw json.RawMessage) (*ir.ChatRequest, *RequestError) {
	var req ir.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, &RequestError{Status: 400, Body: errPayload("Invalid JSON body", "invalid_request_error")}
	}
	if len(req.Messages) == 0 {
		return nil, &RequestError{Status: 400, Body: errPayload("messages array is required", "invalid_request_error")}
	}
	return &req, nil
}

// forwardOnce performs ensureInitialized + Generate for an IR request and
// returns the upstream response (status < 300) or a rendered error.
//
// In pool mode (poolDecision in ctx) the Authorization header is substituted
// with a pooled key, with pre-content failover to the next key on
// rate-limit/exhaustion/invalid classifications. Failover is safe here
// because no response bytes have been written yet.
func (d *Deps) forwardOnce(ctx context.Context, req *ir.ChatRequest, apiKey string, headers http.Header) (*http.Response, *RequestError) {
	if model := req.Model; model != "" {
		if dec := decisionFrom(ctx); dec != nil && dec.resolved.Gateway != nil {
			if !modelWhitelisted(dec.resolved.Gateway.ModelWhitelist, model) {
				return nil, &RequestError{Status: 403, Body: errPayload("Model not allowed for this gateway key", "permission_error")}
			}
		}
	}

	var poolPool *pool.Pool
	var decision *poolDecision
	if decision = decisionFrom(ctx); decision != nil {
		poolPool = d.Pool
	}

	excluded := map[int64]bool{}
	for attempt := 0; attempt < 3; attempt++ {
		upstreamKey := apiKey
		var keyID int64
		if poolPool != nil {
			key := poolPool.Select(poolAffinityKey(decision, headers), excluded)
			if key == nil {
				return nil, &RequestError{Status: 503, Body: errPayload("No available key in the pool (all cooling/exhausted/invalid)", "server_busy")}
			}
			upstreamKey = key.PlainKey
			keyID = key.ID
		}

		if decision != nil && decision.keyIDHolder != nil {
			decision.keyIDHolder.Store(keyID)
		}
		resp, rerr := d.generateOnce(ctx, req, upstreamKey, headers)
		if rerr == nil {
			if poolPool != nil {
				poolPool.ReportSuccess(keyID)
			}
			return resp, nil
		}
		if rerr == errClientGone {
			return nil, nil
		}
		// Fail over only on classifiable upstream failures with retries left.
		class := pool.Classify(rerr.UpstreamStatus, rerr.UpstreamCode)
		if poolPool != nil && class != "none" && attempt < 2 {
			switch class {
			case "invalid":
				poolPool.ReportInvalid(keyID)
			case "exhausted":
				poolPool.ReportExhausted(keyID)
			case "rate_limit":
				poolPool.ReportRateLimit(keyID, rerr.RetryAfter)
			}
			excluded[keyID] = true
			continue
		}
		return nil, rerr
	}
	return nil, &RequestError{Status: 503, Body: errPayload("No available key in the pool", "server_busy")}
}

func poolAffinityKey(decision *poolDecision, headers http.Header) string {
	if decision == nil || decision.resolved.Gateway == nil {
		return ""
	}
	return pool.StickySessionKey(headerMap(headers), "gw:"+itoa(int(decision.resolved.Gateway.ID)))
}

// errClientGone signals the downstream client vanished (nothing to send).
var errClientGone = &RequestError{}

// generateOnce is one raw attempt: init + envelope + POST + status check.
func (d *Deps) generateOnce(ctx context.Context, req *ir.ChatRequest, apiKey string, headers http.Header) (*http.Response, *RequestError) {
	promptCacheKey := req.PromptCacheKey
	sessionID := cc.SessionIDFromHeaders(headerMap(headers), apiKey, promptCacheKey, d.States.SessionID)

	d.Client.EnsureInitialized(ctx, apiKey)

	env := cc.BuildEnvelope(req, sessionID, cc.BuildOpts{
		Device:                 d.Client.Device,
		CLIMode:                d.Cfg.CLIMode,
		EmptySystemPlaceholder: d.Cfg.EmptySystemPlaceholder,
	})

	resp, err := d.Client.Generate(ctx, env, apiKey, sessionID, headers.Get("X-Cmd-Zdr") == "1")
	if err != nil {
		if ctx.Err() != nil {
			return nil, errClientGone // client went away; nothing to send
		}
		logx.Error("Upstream error", map[string]any{"message": err.Error()})
		return nil, &RequestError{Status: 502, Body: errPayload("Upstream error: "+err.Error(), "proxy_error")}
	}
	if resp.StatusCode >= 300 {
		bodyBytes := readAllLimited(resp.Body, 1<<20)
		resp.Body.Close()
		mapped := cc.MapCcError(resp.StatusCode, string(bodyBytes))
		logx.Error("CC API error", map[string]any{
			"status": resp.StatusCode, "code": mapped.Code,
			"body": logx.SummarizeUpstreamError(string(bodyBytes), 500),
		})
		return nil, &RequestError{
			Status:         mapped.Status,
			Body:           errBodyToPayload(mapped),
			UpstreamStatus: mapped.ReportedStatus,
			UpstreamCode:   mapped.Code,
			RetryAfter:     mapped.Body.RetryAfter,
		}
	}
	return resp, nil
}

func errBodyToPayload(m *cc.MappedError) map[string]any {
	payload := errPayloadCode(m.Body.Error.Message, m.Body.Error.Type, m.Code)
	if m.Body.RetryAfter > 0 {
		payload["retry_after"] = m.Body.RetryAfter
	}
	return payload
}

func readAllLimited(r interface{ Read([]byte) (int, error) }, n int64) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 32*1024)
	var total int64
	for total < n {
		read, err := r.Read(tmp)
		if read > 0 {
			remaining := n - total
			if int64(read) > remaining {
				read = int(remaining)
			}
			buf = append(buf, tmp[:read]...)
			total += int64(read)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func headerMap(h http.Header) map[string]string {
	m := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			m[strings.ToLower(k)] = v[0]
		}
	}
	return m
}

// HandleChatCompletions ports handleChatCompletions.
func (d *Deps) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	raw, rerr := readBody(r, config.MaxBodyBytes())
	if rerr != nil {
		writeErrJSON(w, rerr.Status, rerr.Body)
		return
	}
	apiKey, authed := d.downstreamKey(r)
	if !authed {
		writeErrJSON(w, 401, errPayload("Missing API key. Send in Authorization: Bearer <key> or x-api-key header", "auth_error"))
		return
	}

	req, rerr := parseIR(raw)
	if rerr != nil {
		writeErrJSON(w, rerr.Status, rerr.Body)
		return
	}

	stream := req.Stream
	model := req.Model
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}
	completionID := "chatcmpl-" + cc.UUID12()
	created := cc.NowUnix()
	start := time.Now()

	resp, ferr := d.forwardOnce(r.Context(), req, apiKey, r.Header)
	if ferr != nil || resp == nil {
		if ferr != nil {
			writeErrJSON(w, ferr.Status, ferr.Body)
		}
		return
	}
	defer resp.Body.Close()

	if stream {
		d.streamChat(w, r, resp, model, completionID, created, start)
	} else {
		d.nonStreamChat(w, r, resp, model, completionID, created, start)
	}
}

// usageContext carries pool attribution for usage recording.
func usageFrom(ctx context.Context) *pool.Resolved {
	if dec := decisionFrom(ctx); dec != nil {
		return dec.resolved
	}
	return nil
}

// downstreamKey resolves the effective credential for the handler-level
// auth check. Pool-mode requests were already authenticated by PoolGate —
// their upstream key is substituted in forwardOnce, so no user_ key is
// required here.
func (d *Deps) downstreamKey(r *http.Request) (apiKey string, ok bool) {
	if dec := decisionFrom(r.Context()); dec != nil && dec.resolved.Gateway != nil {
		return "", true
	}
	key := ExtractAPIKey(r.Header)
	return key, key != ""
}

func (d *Deps) streamChat(w http.ResponseWriter, r *http.Request, resp *http.Response, model, completionID string, created int64, start time.Time) {
	tr := translate.NewChatTranslator(model, completionID, created)
	var started bool
	var flusher http.Flusher
	var lastEvent string

	err := translate.PumpLines(r.Context(), resp.Body, time.Duration(config.StreamIdleMS())*time.Millisecond, func(line string) {
		tr.ParseLine(line)
		buf := tr.Take()
		if len(buf) > 0 {
			if !started {
				flusher = beginSSE(w)
				started = true
			}
			flushBatch(w, flusher, buf)
		}
		if tr.LastCcEvent != "" {
			lastEvent = tr.LastCcEvent
		}
		if started && len(tr.Take()) == 0 {
			// Silent read window: SSE comment keepalive (only after start).
		}
	})
	// keepalive between reads is handled by writing a comment when a pump
	// read produced no output; approximated by the started-only guard above.

	if err != nil {
		switch {
		case r.Context().Err() != nil:
			logDisconnect("/v1/chat/completions", model, completionID, time.Since(start).Milliseconds(), lastEvent)
			return
		case err == translate.ErrIdleTimeout:
			logx.Warn("Stream idle timeout", map[string]any{
				"path": "/v1/chat/completions", "model": model, "streaming": true,
				"timeoutMs": config.StreamIdleMS(), "elapsedMs": time.Since(start).Milliseconds(),
				"id": completionID, "lastCcEvent": lastEvent,
			})
			msg := timeoutMessage()
			if !started {
				writeErrJSON(w, 429, map[string]any{
					"error":       map[string]any{"message": msg, "type": "rate_limit_error", "input_tokens": 0},
					"retry_after": 5,
				})
				return
			}
			var tail translate.SSEBuf
			tail.Data(map[string]any{
				"error":       map[string]any{"message": msg, "type": "rate_limit_error"},
				"retry_after": 5,
			})
			_, _ = w.Write(tail) // clean end (FIN), never a reset
			return
		default:
			logx.Error("Stream error", map[string]any{"message": err.Error()})
			if !started {
				writeErrJSON(w, 502, map[string]any{
					"error":       map[string]any{"message": "Upstream error: " + err.Error(), "type": "proxy_error", "input_tokens": 0},
					"retry_after": 10,
				})
				return
			}
			var tail translate.SSEBuf
			tail.Data(map[string]any{"error": map[string]any{"message": err.Error(), "type": "proxy_error"}})
			_, _ = w.Write(tail)
			return
		}
	}

	if r.Context().Err() != nil {
		return
	}
	resetTimeouts()
	defer func() {
		d.recordUsage(usageFrom(r.Context()), upstreamKeyID(r.Context()), model, "/v1/chat/completions",
			200, tr.InputTokens, tr.OutputTokens, tr.CachedInputTokens, time.Since(start).Milliseconds())
	}()

	// Terminal precedence (port of the reference's order): upstream error →
	// incomplete (no finish / connection failure) → zero-output → success.
	if tr.UpstreamError != nil {
		if !started {
			writeErrJSON(w, tr.UpstreamError.Status, errBodyToPayload(tr.UpstreamError))
			return
		}
		var tail translate.SSEBuf
		tail.Data(errBodyToPayload(tr.UpstreamError))
		_, _ = w.Write(tail)
		return
	}
	if detail := tr.IncompleteDetail(); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/chat/completions", "reason": detail})
		mapped := cc.IncompleteUpstreamError(detail)
		if !started {
			writeErrJSON(w, mapped.Status, errBodyToPayload(mapped))
			return
		}
		var tail translate.SSEBuf
		tail.Data(errBodyToPayload(mapped))
		_, _ = w.Write(tail)
		return
	}
	if tr.OutputTokens == 0 {
		if !started {
			writeErrJSON(w, 429, map[string]any{
				"error":       map[string]any{"message": "Empty response from upstream (zero output tokens)", "type": "rate_limit_error"},
				"retry_after": 10,
			})
			return
		}
		var tail translate.SSEBuf
		tail.Data(map[string]any{
			"error":       map[string]any{"message": "Empty response from upstream (zero output tokens)", "type": "rate_limit_error"},
			"retry_after": 10,
		})
		_, _ = w.Write(tail)
		return
	}

	if !started {
		flusher = beginSSE(w)
	}
	var tail translate.SSEBuf
	tail.Raw(tr.DoneEvent())
	flushBatch(w, flusher, tail)
}

func (d *Deps) nonStreamChat(w http.ResponseWriter, r *http.Request, resp *http.Response, model, completionID string, created int64, start time.Time) {
	agg := translate.NewChatAggregate()
	err := translate.PumpLines(r.Context(), resp.Body, time.Duration(config.NonStreamIdleMS())*time.Millisecond, agg.ProcessLine)
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			return
		case err == translate.ErrIdleTimeout:
			logx.Warn("Stream idle timeout", map[string]any{
				"path": "/v1/chat/completions", "model": model, "streaming": false,
				"timeoutMs": config.NonStreamIdleMS(), "elapsedMs": time.Since(start).Milliseconds(),
				"id": completionID,
			})
			writeErrJSON(w, 429, map[string]any{
				"error":       map[string]any{"message": timeoutMessage(), "type": "rate_limit_error", "input_tokens": 0},
				"retry_after": 5,
			})
			return
		default:
			writeErrJSON(w, 502, map[string]any{
				"error":       map[string]any{"message": "Upstream error: " + err.Error(), "type": "proxy_error", "input_tokens": 0},
				"retry_after": 10,
			})
			return
		}
	}
	resetTimeouts()
	pTokens, cTokens, cachedTokens := int64(0), int64(0), int64(0)
	if agg.Usage != nil {
		cc.NormalizeUsage(agg.Usage)
		pTokens, cTokens, cachedTokens = agg.Usage.InputTokens, agg.Usage.OutputTokens, agg.Usage.CachedInputTokens
	}
	defer func() {
		d.recordUsage(usageFrom(r.Context()), upstreamKeyID(r.Context()), model, "/v1/chat/completions",
			200, pTokens, cTokens, cachedTokens, time.Since(start).Milliseconds())
	}()

	if agg.UpstreamError != nil {
		writeErrJSON(w, agg.UpstreamError.Status, errBodyToPayload(agg.UpstreamError))
		return
	}
	if detail := cc.IncompleteDetail(agg.SawFinish, agg.FinishReason); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/chat/completions", "reason": detail})
		mapped := cc.IncompleteUpstreamError(detail)
		writeErrJSON(w, mapped.Status, errBodyToPayload(mapped))
		return
	}
	outTokens := int64(0)
	if agg.Usage != nil {
		outTokens = agg.Usage.OutputTokens
	}
	if outTokens == 0 {
		writeErrJSON(w, 429, map[string]any{
			"error":       map[string]any{"message": "Empty response from upstream (zero output tokens)", "type": "rate_limit_error"},
			"retry_after": 10,
		})
		return
	}

	content := agg.FullText
	msg := map[string]any{"role": "assistant", "content": nil}
	if content != "" {
		msg["content"] = content
	}
	if len(agg.ToolCalls) > 0 {
		msg["tool_calls"] = agg.ToolCalls
	}
	if agg.ReasoningContent != "" {
		msg["reasoning_content"] = agg.ReasoningContent
	}
	finish := cc.ToOpenAIFinishReason(agg.FinishReason)

	var usage *cc.CCUsage
	if agg.Usage == nil {
		usage = &cc.CCUsage{}
	} else {
		usage = agg.Usage
	}
	writeErrJSON(w, 200, map[string]any{
		"id": completionID, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": msg, "finish_reason": finish,
		}},
		"usage": translate.OpenAIUsage(usage),
	})
}
