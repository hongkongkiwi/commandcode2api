package server

import (
	"fmt"
	"net/http"
)

// NewMux builds the full application handler: CORS, in-flight cap, routes.
// main.go and tests share it.
func NewMux(d *Deps, inflight *Inflight) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", d.HandleChatCompletions)
	mux.HandleFunc("POST /v1/messages", d.HandleMessages)
	mux.HandleFunc("POST /v1/responses", d.HandleResponses)
	mux.HandleFunc("GET /v1/models", d.HandleModels)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /{$}", handleHealth)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"message":"Not found","type":"not_found"}}`)
	})
	return withCors(withInflight(inflight, mux))
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("OK"))
}

// withInflight guards business routes with the optional concurrency cap;
// /health and / are exempt so liveness probes never 503.
func withInflight(inflight *Inflight, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if inflight == nil || inflight.max <= 0 || r.URL.Path == "/health" || r.URL.Path == "/" {
			next.ServeHTTP(w, r)
			return
		}
		if !inflight.Acquire() {
			logWarn("In-flight limit reached, rejecting request", map[string]any{"path": r.URL.Path})
			w.Header().Set("Retry-After", "5")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			fmt.Fprint(w, `{"error":{"message":"Too many concurrent requests, retry shortly","type":"server_busy"},"retry_after":5}`)
			return
		}
		defer inflight.Release()
		next.ServeHTTP(w, r)
	})
}

// withCors mirrors the reference's permissive CORS.
func withCors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
