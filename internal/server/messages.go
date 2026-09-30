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

// HandleMessages ports handleMessages (Anthropic /v1/messages).
func (d *Deps) HandleMessages(w http.ResponseWriter, r *http.Request) {
	raw, rerr := readBody(r, config.MaxBodyBytes())
	if rerr != nil {
		writeAnthropicError(w, rerr.Status, "invalid_request_error", firstString(rerr.Body), 0)
		return
	}
	apiKey := ExtractAPIKey(r.Header)
	if apiKey == "" {
		writeErrJSON(w, 401, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "authentication_error", "message": "Missing API key. Send in Authorization: Bearer <key> or x-api-key header"},
		})
		return
	}

	var areq translate.AnthropicRequest
	if err := json.Unmarshal(raw, &areq); err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "Invalid JSON body", 0)
		return
	}

	req := translate.ConvertAnthropicToIR(&areq)
	stream := areq.Stream
	model := req.Model
	messageID := "msg_" + cc.UUID12()
	start := time.Now()

	resp, ferr := d.forwardOnce(r.Context(), req, apiKey, r.Header)
	if ferr != nil || resp == nil {
		if ferr != nil {
			m, ok := ferr.Body["error"].(map[string]any)
			if !ok {
				m = map[string]any{"type": "internal_error", "message": "error"}
			}
			typ, _ := m["type"].(string)
			msg, _ := m["message"].(string)
			writeAnthropicError(w, ferr.Status, typ, msg, 0)
		}
		return
	}
	defer resp.Body.Close()

	if stream {
		d.streamMessages(w, r, resp, model, messageID, start)
	} else {
		d.nonStreamMessages(w, r, resp, model, messageID, start)
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, message string, retryAfter int) {
	body := map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if retryAfter > 0 {
		body["retry_after"] = retryAfter
		h.Set("Retry-After", itoa(retryAfter))
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func firstString(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			return m
		}
	}
	return "error"
}

func (d *Deps) streamMessages(w http.ResponseWriter, r *http.Request, resp *http.Response, model, messageID string, start time.Time) {
	tr := translate.NewAnthropicTranslator(model, messageID)
	tr.Start()
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
			logDisconnect("/v1/messages", model, messageID, time.Since(start).Milliseconds(), lastEvent)
			return
		case err == translate.ErrIdleTimeout:
			logx.Warn("Stream idle timeout", map[string]any{
				"path": "/v1/messages", "model": model, "streaming": true,
				"timeoutMs": config.StreamIdleMS(), "elapsedMs": time.Since(start).Milliseconds(),
				"id": messageID, "lastCcEvent": lastEvent,
			})
			msg := timeoutMessage()
			if !started {
				writeAnthropicError(w, 429, "rate_limit_error", msg, 0)
				return
			}
			var tail translate.SSEBuf
			tail.Raw("event: error\ndata: ")
			tail.Data(map[string]any{
				"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": msg},
				"retry_after": 5,
			})
			_, _ = w.Write(tail)
			return
		default:
			logx.Error("Anthropic stream error", map[string]any{"message": err.Error()})
			if !started {
				writeAnthropicError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
				return
			}
			var tail translate.SSEBuf
			tail.Raw("event: error\ndata: ")
			tail.Data(map[string]any{
				"type": "error", "error": map[string]any{"type": "internal_error", "message": err.Error()},
			})
			_, _ = w.Write(tail)
			return
		}
	}

	if r.Context().Err() != nil {
		return
	}
	resetTimeouts()

	if tr.UpstreamError != nil {
		if !started {
			writeAnthropicError(w, tr.UpstreamError.Status, tr.UpstreamError.Body.Error.Type, tr.UpstreamError.Body.Error.Message, 0)
			return
		}
		// error event already emitted mid-stream; it terminates per spec
		return
	}

	_, ok := tr.Finalize()
	if !ok && !started {
		// zero-output with no visible events → JSON 429 for SDK retry
		writeAnthropicError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}
	flushBatch(w, flusher, tr.Take())
}

func (d *Deps) nonStreamMessages(w http.ResponseWriter, r *http.Request, resp *http.Response, model, messageID string, start time.Time) {
	var fullText, thinkingText string
	var toolCalls []translate.ChatTool
	finishReason := "stop"
	sawFinish := false
	var usage *cc.CCUsage
	var upstreamError *cc.MappedError

	err := translate.PumpLines(r.Context(), resp.Body, time.Duration(config.NonStreamIdleMS())*time.Millisecond, func(line string) {
		line = trimSpace(line)
		if line == "" || line == "[DONE]" {
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
			writeAnthropicError(w, 429, "rate_limit_error", timeoutMessage(), 5)
			return
		default:
			writeAnthropicError(w, 502, "proxy_error", "Upstream error: "+err.Error(), 10)
			return
		}
	}
	resetTimeouts()

	if upstreamError != nil {
		writeAnthropicError(w, upstreamError.Status, upstreamError.Body.Error.Type, upstreamError.Body.Error.Message, 0)
		return
	}
	if detail := cc.IncompleteDetail(sawFinish, finishReason); detail != "" {
		logx.Warn("Upstream stream incomplete", map[string]any{"path": "/v1/messages", "reason": detail})
		mapped := cc.IncompleteUpstreamError(detail)
		writeAnthropicError(w, mapped.Status, mapped.Body.Error.Type, mapped.Body.Error.Message, mapped.Body.RetryAfter)
		return
	}
	// Zero-output by content (upstream occasionally omits totalUsage).
	if fullText == "" && thinkingText == "" && len(toolCalls) == 0 {
		writeAnthropicError(w, 429, "rate_limit_error", "Empty response from upstream (zero output tokens)", 10)
		return
	}

	body := translate.BuildAnthropicResponse(model, fullText, toolCalls, finishReason, usage, thinkingText)
	writeJSONBody(w, 200, body)
}

func writeJSONBody(w http.ResponseWriter, status int, body any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func trimSpace(s string) string {
	start := 0
	for start < len(s) {
		c := s[start]
		if c == ' ' || c == '\t' || c == '\r' {
			start++
			continue
		}
		break
	}
	end := len(s)
	for end > start {
		c := s[end-1]
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			end--
			continue
		}
		break
	}
	return s[start:end]
}
