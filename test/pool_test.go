// Pool-mode integration tests: dual auth (user_* pass-through + sk-*
// gateway), key rotation on upstream failures, sticky affinity.
package contract_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/pool"
	"github.com/hongkongkiwi/commandcode2api/internal/server"
	"github.com/hongkongkiwi/commandcode2api/internal/store"
	"github.com/hongkongkiwi/commandcode2api/test/testutil"
)

// poolFixture wires proxy + pool + admin against a mock upstream.
type poolFixture struct {
	Proxy    *testutil.Proxy
	Store    *store.Store
	Pool     *pool.Pool
	Mock     *testutil.MockCC
	authSeen atomic.Value // last Authorization header the mock upstream saw
}

func setupPool(t *testing.T, upstreamURL string) *poolFixture {
	t.Helper()
	st, err := store.Open(t.TempDir()+"/pool.db", "test-vault-secret")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	p := pool.New(st)
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultsForTest()
	device := cc.NewDeviceProfile("")
	states := cc.NewKeyStates()
	client, err := cc.NewClient(upstreamURL, "", "", device, states, false, "interactive")
	if err != nil {
		t.Fatal(err)
	}
	cc.SetGlobalFingerprinter(client.Fingerprinter())

	deps := &server.Deps{
		Client: client, States: states,
		Models: &server.Models{Client: client, RefreshEvery: time.Duration(cfg.ModelRefreshInterval) * time.Millisecond},
		Cfg:    cfg, Pool: p, Store: st,
	}
	resolver := &pool.Resolver{Store: st, Pool: p, PassEnabled: true}
	core := server.NewMux(deps, nil)
	gated := &server.PoolGate{Resolver: resolver, Next: core}
	admin := &server.AdminREST{Store: st, Pool: p, Token: "admin-test-token"}
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin)
	mux.Handle("/", gated)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	return &poolFixture{
		Proxy: testutil.NewProxy(ts.URL),
		Store: st, Pool: p, Mock: nil,
	}
}

func TestPoolDualAuth(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	f := setupPool(t, mock.Server.URL)

	// seed two keys
	for _, k := range []string{"user_k1", "user_k2"} {
		if _, err := f.Store.AddCCKey(k, k, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pool.Reload(); err != nil {
		t.Fatal(err)
	}

	// pass-through: user_* key goes upstream unchanged
	_, raw, err := f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("user_direct"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertContains(t, string(raw), `"finish_reason":"stop"`)
	req := mock.NextRequest(t)
	for req.Path != "/alpha/generate" {
		req = mock.NextRequest(t)
	}
	if got := req.Headers.Get("Authorization"); got != "Bearer user_direct" {
		t.Fatalf("pass-through Authorization = %q, want user key unchanged", got)
	}

	// pool mode: gateway key substitutes a pooled key upstream
	gw, err := f.Store.AddGatewayKey("test-gw", "sk-gw-123", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = gw
	_, raw, err = f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("sk-gw-123"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.AssertContains(t, string(raw), `"finish_reason":"stop"`)
	req = mock.NextRequest(t)
	for req.Path != "/alpha/generate" {
		req = mock.NextRequest(t)
	}
	auth := req.Headers.Get("Authorization")
	if auth != "Bearer user_k1" && auth != "Bearer user_k2" {
		t.Fatalf("pool Authorization = %q, want a pooled user_k* key", auth)
	}

	// bad gateway key → 401
	resp, _, err := f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("sk-wrong"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("bad gateway key status = %d, want 401", resp.StatusCode)
	}
}

func TestPoolRateLimitRotation(t *testing.T) {
	// Upstream: first key gets 429, second succeeds.
	var calls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/generate", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","code":"RATE_LIMIT"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, line := range testutil.WithFinish("stop") {
			_, _ = w.Write([]byte(line + "\n"))
		}
	})
	for _, p := range []string{"/alpha/fingerprint/record", "/alpha/lifecycle-events"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		})
	}
	mux.HandleFunc("GET /provider/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	f := setupPool(t, up.URL)
	for _, k := range []string{"user_a", "user_b"} {
		if _, err := f.Store.AddCCKey(k, k, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pool.Reload(); err != nil {
		t.Fatal(err)
	}

	gw, _ := f.Store.AddGatewayKey("gw", "sk-gw", 0, "")
	_, raw, err := f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("sk-gw"))
	if err != nil {
		t.Fatal(err)
	}
	// Success here proves failover worked (first key got 429).
	testutil.AssertContains(t, string(raw), `"finish_reason":"stop"`)
	if got := calls.Load(); got < 2 {
		t.Fatalf("upstream calls = %d, want >=2 (failover)", got)
	}
	_ = gw
}

func TestPoolStickyAffinity(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	f := setupPool(t, mock.Server.URL)
	for _, k := range []string{"user_s1", "user_s2"} {
		if _, err := f.Store.AddCCKey(k, k, 100); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Pool.Reload(); err != nil {
		t.Fatal(err)
	}
	gw, _ := f.Store.AddGatewayKey("gw", "sk-gw", 0, "")
	_ = gw

	hdrs := func() map[string]string {
		return map[string]string{"Authorization": "Bearer sk-gw", "X-Session-Id": "sticky-session-abc"}
	}
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		if _, _, err := f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), hdrs()); err != nil {
			t.Fatal(err)
		}
		var req testutil.RecordedRequest
		for {
			req = mock.NextRequest(t)
			if req.Path == "/alpha/generate" {
				break
			}
		}
		seen[req.Headers.Get("Authorization")] = true
	}
	if len(seen) != 1 {
		t.Fatalf("sticky session hit %d distinct keys, want 1: %v", len(seen), seen)
	}
}

func TestAdminAuthAndEndpoints(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	f := setupPool(t, mock.Server.URL)

	// no token → 401
	resp, _, err := f.Proxy.Get("/admin/status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("admin without token = %d, want 401", resp.StatusCode)
	}

	auth := map[string]string{"Authorization": "Bearer admin-test-token"}
	// add a key via API
	body := map[string]any{"name": "via-api", "key": "user_added", "priority": 50}
	resp, raw, err := f.Proxy.Post("/admin/keys", body, auth)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("add key = %d: %s", resp.StatusCode, raw)
	}
	// list shows it (preview only — never the full key)
	resp, raw, err = f.Proxy.Get("/admin/keys", auth)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "user_added") {
		t.Fatal("admin listing leaked the full CC key")
	}
	testutil.AssertContains(t, string(raw), `"name":"via-api"`)
	// status
	resp, raw, err = f.Proxy.Get("/admin/status", auth)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("status = %d err=%v", resp.StatusCode, err)
	}
	var status struct {
		PoolKeys int `json:"poolKeys"`
	}
	_ = json.Unmarshal(raw, &status)
	if status.PoolKeys != 1 {
		t.Fatalf("poolKeys = %d, want 1", status.PoolKeys)
	}
}

func TestUsageRecorded(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	f := setupPool(t, mock.Server.URL)
	if _, err := f.Store.AddCCKey("user_u", "user_u", 100); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.Reload(); err != nil {
		t.Fatal(err)
	}
	gw, _ := f.Store.AddGatewayKey("gw", "sk-gw", 0, "")
	if _, _, err := f.Proxy.Post("/v1/chat/completions", testutil.CHAT(), testutil.Bearer("sk-gw")); err != nil {
		t.Fatal(err)
	}
	_ = gw
	sum, err := f.Store.UsageSummarySince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 1 {
		t.Fatalf("usage requests = %d, want 1", sum.Requests)
	}
}

var _ = fmt.Sprintf
