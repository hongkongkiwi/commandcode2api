package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
	"github.com/hongkongkiwi/commandcode2api/internal/translate"
)

// HandleResponses ports handleResponses (OpenAI Responses API). Stateless:
// previous_response_id is rejected with 400 rather than silently degraded.
func (d *Deps) HandleResponses(w http.ResponseWriter, r *http.Request) {
	raw, rerr := readBody(r, config.MaxBodyBytes())
	if rerr != nil {
		writeResponsesError(w, rerr.Status, "invalid_request_error", firstString(rerr.Body), 0)
		return
	}

	var req translate.ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeResponsesError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}
	if req.PreviousResponseID != "" {
		writeResponsesError(w, 400, "invalid_request_error",
			"previous_response_id is not supported (this proxy is stateless); send the full input each turn", 0)
		return
	}

	chatReq, err := translate.ConvertResponsesToIR(&req)
	if err != nil || len(chatReq.Messages) == 0 {
		writeResponsesError(w, 400, "invalid_request_error", "input is required", 0)
		return
	}

	apiKey := ExtractAPIKey(r.Header)
	if apiKey == "" {
		writeResponsesError(w, 401, "authentication_error",
			"Missing API key. Send in Authorization: Bearer <key> or x-api-key header", 0)
		return
	}

	stream := chatReq.Stream
	model := chatReq.Model
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}
	responseID := cc.NewResponsesID("resp_")
	created := cc.NowUnix()
	start := time.Now()

	resp, ferr := d.forwardOnce(r.Context(), chatReq, apiKey, r.Header)
	if ferr != nil || resp == nil {
		if ferr != nil {
			m, _ := ferr.Body["error"].(map[string]any)
			typ, _ := m["type"].(string)
			msg, _ := m["message"].(string)
			ra, _ := ferr.Body["retry_after"].(int)
			writeResponsesError(w, ferr.Status, typ, msg, ra)
		}
		return
	}
	defer resp.Body.Close()

	if stream {
		d.streamResponses(w, r, resp, model, responseID, created, start)
	} else {
		d.nonStreamResponses(w, r, resp, req, model, responseID, created, start)
	}
}

func writeResponsesError(w http.ResponseWriter, status int, typ, message string, retryAfter int) {
	body := map[string]any{"error": map[string]any{"message": message, "type": typ, "code": nil, "param": nil}}
	if retryAfter > 0 {
		body["retry_after"] = retryAfter
	}
	writeErrJSON(w, status, body)
}

func (d *Deps) streamResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, model, responseID string, created int64, start time.Time) {
	tr := translate.NewResponsesTranslator(model, responseID, created)
	var started bool
	var flusher http.Flusher
	var lastEvent string

	err := translate.PumpLines(r.Context(), resp.Body, time.Duration(config.StreamIdleMS())*time.Millisecond, func(line string) {
		tr.ParseLine(line)
		buf := tr.Take()
		if len(buf) > 0 && !started {
			flusher = beginSSE(w)
			started = true
		}
		flushBatch(w, flusher, buf)
		if tr.LastCcEvent != "" {
			lastEvent = tr.LastCcEvent
		}
	})

	if err != nil {
		switch {
		case r.Context().Err() != nil:
			logDisconnect("/v1/responses", model, responseID, time.Since(start).Milliseconds(), lastEvent)
			return
		case err == translate.ErrIdleTimeout:
			logx.Warn("Stream idle timeout", map[string]any{
				"path": "/v1/responses", "model": model, "streaming": true,
				"timeoutMs": config.StreamIdleMS(), "elapsedMs": time.Since(start).Milliseconds(),
				"lastCcEvent": lastEvent,
			})
			msg := timeoutMessage()
			if !started {
				writeResponsesError(w, 429, "rate_limit_error", msg, 5)
				return
			}
			_, _ = w.Write(tr.ErrorEvent(msg))
			return
		default:
			logx.Error("Stream error", map[string]any{"message": err.Error(), "path": "/v1/responses"})
			if !started {
				writeResponsesError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
				return
			}
			_, _ = w.Write(tr.ErrorEvent(err.Error()))
			return
		}
	}

	if r.Context().Err() != nil {
		return
	}
	resetTimeouts()

	if tr.UpstreamError != nil {
		if !started {
			writeResponsesError(w, tr.UpstreamError.Status, tr.UpstreamError.Body.Error.Type,
				tr.UpstreamError.Body.Error.Message, tr.UpstreamError.Body.RetryAfter)
			return
		}
		tr.Fail(tr.UpstreamError.Body.Error.Message)
		flushBatch(w, flusher, tr.Take())
		return
	}
	if tr.OutputTokens == 0 && !tr.Started() {
		writeResponsesError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}
	if !started {
		flusher = beginSSE(w)
	}
	tr.Finish()
	flushBatch(w, flusher, tr.Take())
}

func (d *Deps) nonStreamResponses(w http.ResponseWriter, r *http.Request, resp *http.Response, req translate.ResponsesRequest, model, responseID string, created int64, start time.Time) {
	var fullText, thinkingText string
	var toolCalls []translate.ChatTool
	finishReason := "stop"
	sawFinish := false
	var usage *cc.CCUsage
	var upstreamError *cc.MappedError

	err := translate.PumpLines(r.Context(), resp.Body, time.Duration(config.NonStreamIdleMS())*time.Millisecond, func(line string) {
		line = trimSpace(line)
		if line == "" || line == "[DONE]" || hasPrefix(line, ":") {
			return
		}
		var event cc.CCEvent
		if json.Unmarshal([]byte(line), &event) != nil {
			return
		}
		switch event.Type {
		case "text-delta":
			fullText += event.TextOf()
		case "reasoning-delta":
			thinkingText += event.Text
		case "tool-call":
			id := event.ToolCallID
			if id == "" {
				id = "call_" + cc.NewUUID()[:8]
			}
			toolCalls = append(toolCalls, translate.ChatTool{
				ID: id, Type: "function",
				Function: translate.ChatToolFn{Name: event.ToolName, Arguments: event.ArgsOf()},
			})
		case "finish-step", "finish":
			sawFinish = true
			finishReason = cc.MapFinishReason(event.FinishReason)
			if event.TotalUsage != nil {
				usage = event.TotalUsage
			} else if event.Usage != nil {
				usage = event.Usage
			}
		case "error":
			upstreamError = cc.MapCcEventError(&event)
		case "text-start", "text-end", "start", "start-step",
			"reasoning-start", "reasoning-end",
			"provider-metadata", "tool-input-start", "tool-input-delta", "tool-input-end",
			"tool-error":
			// silent
		default:
			logx.Warn("Unknown CC event type", map[string]any{"type": event.Type})
		}
	})
	if err != nil {
		switch {
		case r.Context().Err() != nil:
			return
		case err == translate.ErrIdleTimeout:
			writeResponsesError(w, 429, "rate_limit_error", timeoutMessage(), 5)
			return
		default:
			writeResponsesError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
			return
		}
	}
	resetTimeouts()

	if upstreamError != nil {
		writeResponsesError(w, upstreamError.Status, upstreamError.Body.Error.Type,
			upstreamError.Body.Error.Message, upstreamError.Body.RetryAfter)
		return
	}
	if detail := cc.IncompleteDetail(sawFinish, finishReason); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/responses", "reason": detail})
		mapped := cc.IncompleteUpstreamError(detail)
		writeResponsesError(w, mapped.Status, mapped.Body.Error.Type, mapped.Body.Error.Message, mapped.Body.RetryAfter)
		return
	}
	if fullText == "" && thinkingText == "" && len(toolCalls) == 0 {
		writeResponsesError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}

	echo := map[string]any{
		"instructions":      jsonOrNull(req.Instructions),
		"max_output_tokens": intOrNull(req.MaxOutputTokens),
		"temperature":       req.Temperature,
		"top_p":             req.TopP,
		"reasoning":         nilIfEmptyReasoning(req),
		"tool_choice":       toolChoiceEcho(req.ToolChoice),
		"tools":             jsonOrEmptyArray(req.Tools),
		"input":             jsonOrEmptyArray(req.Input),
	}
	body := translate.BuildResponsesObject(responseID, model, created, fullText, thinkingText, toolCalls, usage, finishReason, echo)
	writeJSONBody(w, 200, body)
}

func jsonOrNull(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}

func jsonOrEmptyArray(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return []any{}
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return []any{}
	}
	return v
}

func intOrNull(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nilIfEmptyReasoning(req translate.ResponsesRequest) any {
	if req.Reasoning == nil {
		return nil
	}
	return req.Reasoning
}

func toolChoiceEcho(raw json.RawMessage) any {
	if len(raw) == 0 {
		return "auto"
	}
	if s, ok := stringOrNull(raw); ok {
		return s
	}
	return jsonOrNull(raw)
}

func stringOrNull(raw json.RawMessage) (string, bool) {
	if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, true
		}
	}
	return "", false
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
