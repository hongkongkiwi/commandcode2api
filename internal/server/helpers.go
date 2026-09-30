// Package server wires the HTTP surface: routing, auth, body limits,
// in-flight cap, and the three protocol handlers.
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sync/atomic"

	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// writeErrJSON emits {"error":{...}} with optional retry_after, mirroring
// the reference's sendJSON (Retry-After header when present).
func writeErrJSON(w http.ResponseWriter, status int, body map[string]any) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	if ra, ok := body["retry_after"]; ok {
		h.Set("Retry-After", toString(ra))
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(bytes.Trim(b, `"`))
}

func errPayload(message, typ string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": typ}}
}

func errPayloadCode(message, typ, code string) map[string]any {
	e := map[string]any{"message": message, "type": typ}
	if code != "" {
		e["code"] = code
	}
	return map[string]any{"error": e}
}

// readBody ports readBody: enforce CC_MAX_BODY_MB with a 413 that drains up
// to 32MB so keep-alive connections survive an oversized upload.
func readBody(r *http.Request, maxBytes int64) (json.RawMessage, *RequestError) {
	limited := io.LimitReader(r.Body, maxBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, &RequestError{Status: 400, Body: errPayload("Invalid JSON body", "invalid_request_error")}
	}
	if int64(len(buf)) > maxBytes {
		// Drain a bounded amount so the client sees 413, not a reset.
		go func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 32*1024*1024))
		}()
		mb := maxBytes / (1024 * 1024)
		return nil, &RequestError{
			Status: 413,
			Body:   errPayload("Request body exceeds "+itoa(int(mb))+"MB limit", "invalid_request_error"),
		}
	}
	if err := json.Unmarshal(buf, new(json.RawMessage)); err != nil && len(bytes.TrimSpace(buf)) > 0 {
		return nil, &RequestError{Status: 400, Body: errPayload("Invalid JSON body", "invalid_request_error")}
	}
	return json.RawMessage(buf), nil
}

// RequestError carries a pre-rendered error response through handlers.
type RequestError struct {
	Status int
	Body   map[string]any
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ExtractAPIKey ports getApiKey: Authorization Bearer (regex user_*), then
// x-api-key. Returns "" when absent/malformed.
var userKeyRe = regexp.MustCompile(`user_[a-zA-Z0-9_-]+`)

func ExtractAPIKey(header http.Header) string {
	auth := header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		if m := userKeyRe.FindString(auth[7:]); m != "" {
			return m
		}
	}
	if x := header.Get("X-Api-Key"); x != "" {
		if m := userKeyRe.FindString(x); m != "" {
			return m
		}
	}
	return ""
}

// Inflight is the optional global concurrency cap (CC_MAX_INFLIGHT).
type Inflight struct {
	cur atomic.Int64
	max int64
}

func NewInflight(max int) *Inflight { return &Inflight{max: int64(max)} }

// Acquire reserves a slot; false means over-capacity (503).
func (i *Inflight) Acquire() bool {
	if i.max <= 0 {
		return true
	}
	n := i.cur.Add(1)
	if n > i.max {
		i.cur.Add(-1)
		return false
	}
	return true
}

func (i *Inflight) Release() {
	if i.max > 0 {
		i.cur.Add(-1)
	}
}

// consecutive timeout hint counter (process-wide, like the reference).
var consecutiveTimeouts atomic.Int64

const timeoutReduceContextThreshold = 3

func timeoutMessage() string {
	if consecutiveTimeouts.Add(1) >= timeoutReduceContextThreshold {
		return "Response timeout - try reducing context length (summarize earlier messages)"
	}
	return "Response timeout - request timed out"
}

func resetTimeouts() { consecutiveTimeouts.Store(0) }

// logDisconnect mirrors the reference's client-disconnect warn line.
func logDisconnect(path, model, id string, elapsedMs int64, lastEvent string) {
	logx.Warn("Client disconnected", map[string]any{
		"path": path, "model": model, "id": id,
		"elapsedMs": elapsedMs, "lastCcEvent": lastEvent,
	})
}

func logWarn(msg string, data any) { logx.Warn(msg, data) }

var _ = config.EnvInt // referenced so the import stays if handlers move
