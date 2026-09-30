// Package testutil provides the mock CC upstream and an in-process proxy
// harness (port of test/helpers.mjs).
package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/server"
)

// RecordedRequest captures a mock-upstream hit.
type RecordedRequest struct {
	Path    string
	Headers http.Header
	Body    []byte
}

// MockCC is a fake Command Code upstream.
type MockCC struct {
	Server   *httptest.Server
	NDJSON   []string
	Requests chan RecordedRequest
}

// NewMockCC starts a mock upstream replaying the given NDJSON on generate.
func NewMockCC(t *testing.T, ndjson ...string) *MockCC {
	t.Helper()
	m := &MockCC{NDJSON: ndjson, Requests: make(chan RecordedRequest, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /alpha/generate", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.record(r, body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		for _, line := range m.NDJSON {
			_, _ = w.Write([]byte(line + "\n"))
			if f != nil {
				f.Flush()
			}
		}
	})
	mux.HandleFunc("/alpha/fingerprint/record", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, nil)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/alpha/lifecycle-events", func(w http.ResponseWriter, r *http.Request) {
		m.record(r, nil)
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("GET /provider/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[{"id":"mock/model-a"},{"id":"mock/model-b"}]}`))
	})
	m.Server = httptest.NewServer(mux)
	t.Cleanup(m.Server.Close)
	return m
}

func (m *MockCC) record(r *http.Request, body []byte) {
	select {
	case m.Requests <- RecordedRequest{Path: r.URL.Path, Headers: r.Header, Body: body}:
	default:
	}
}

// NextRequest waits for a recorded request.
func (m *MockCC) NextRequest(t *testing.T) RecordedRequest {
	t.Helper()
	select {
	case req := <-m.Requests:
		return req
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for upstream request")
		return RecordedRequest{}
	}
}

// DrainRequests empties the recorded-request channel.
func (m *MockCC) DrainRequests() {
	for {
		select {
		case <-m.Requests:
		default:
			return
		}
	}
}

// Proxy wraps the in-process application handler against a mock upstream.
type Proxy struct {
	Base   string
	client *http.Client
	mock   *MockCC
}

// NewProxy builds a Proxy over an existing base URL (custom harnesses).
func NewProxy(base string) *Proxy {
	return &Proxy{Base: base, client: &http.Client{Timeout: 60 * time.Second}}
}

// Setup wires proxy→mock and returns both. Config defaults apply
// (30s/90s idle timeouts; tests that exercise timeouts pass
// config overrides via env before calling — see SetEnvDefaults).
func Setup(t *testing.T, mock *MockCC) *Proxy {
	t.Helper()
	cfg := config.DefaultsForTest()
	device := cc.NewDeviceProfile("")
	states := cc.NewKeyStates()
	client, err := cc.NewClient(mock.Server.URL, "", "", device, states, false, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	cc.SetGlobalFingerprinter(client.Fingerprinter())
	deps := &server.Deps{
		Client: client,
		States: states,
		Models: &server.Models{Client: client, RefreshEvery: time.Duration(cfg.ModelRefreshInterval) * time.Millisecond},
		Cfg:    cfg,
	}
	ts := httptest.NewServer(server.NewMux(deps, nil))
	t.Cleanup(ts.Close)
	return &Proxy{Base: ts.URL, client: ts.Client(), mock: mock}
}

// Post sends a JSON body to the proxy path with headers.
func (p *Proxy) Post(path string, body any, headers map[string]string) (*http.Response, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.Base+path, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw, err
}

// Get performs a GET against the proxy.
func (p *Proxy) Get(path string, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, p.Base+path, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, raw, err
}

// JSON decodes a response body into a map.
func JSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, truncate(raw))
	}
	return m
}

func truncate(b []byte) string {
	s := string(b)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

// Bearer / XKey are the two auth header styles used by the suites.
func Bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }
func XKey(key string) map[string]string   { return map[string]string{"X-Api-Key": key} }

// CHAT is the minimal chat request used by the suites.
func CHAT() map[string]any {
	return map[string]any{"model": "m", "messages": []map[string]any{{"role": "user", "content": "hi"}}}
}

// WithFinish builds an NDJSON sequence ending in the given finishReason.
func WithFinish(reason string) []string {
	return []string{
		`{"type":"text-start"}`,
		`{"type":"text-delta","text":"partial"}`,
		`{"type":"text-end"}`,
		fmt.Sprintf(`{"type":"finish","finishReason":"%s","totalUsage":{"inputTokens":9,"outputTokens":3}}`, reason),
	}
}

// NoFinish is an NDJSON sequence cut off before any finish event.
var NoFinish = []string{
	`{"type":"text-start"}`,
	`{"type":"text-delta","text":"partial"}`,
	`{"type":"text-end"}`,
}

// AssertContains fails when the body lacks any needle.
func AssertContains(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !strings.Contains(body, n) {
			t.Fatalf("body missing %q\nfull: %s", n, truncate([]byte(body)))
		}
	}
}

// AssertNotContains fails when the body has any banned needle.
func AssertNotContains(t *testing.T, body string, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if strings.Contains(body, n) {
			t.Fatalf("body must not contain %q\nfull: %s", n, truncate([]byte(body)))
		}
	}
}
