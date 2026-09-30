package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/server"
)

// NewStallingUpstream returns a mock CC that answers pre-requests normally
// but stalls mid-stream on /alpha/generate after emitting two lines —
// exercising the proxy's idle watchdog.
func NewStallingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/alpha/generate", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io_discardRead(r)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`{"type":"text-start"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"text-delta","text":"partial-content"}` + "\n"))
		if f != nil {
			f.Flush()
		}
		// ...then never write again. The test's client read must complete via
		// the proxy's idle timeout (CC_STREAM_IDLE_MS), not hang: the request
		// context carries a deadline from the test harness.
		select {
		case <-r.Context().Done():
		case <-time.After(60 * time.Second):
		}
	})
	mux.HandleFunc("/alpha/fingerprint/record", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/alpha/lifecycle-events", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("GET /provider/v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func io_discardRead(r *http.Request) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := r.Body.Read(buf)
		total += int64(n)
		if err != nil {
			return total, nil
		}
	}
}

// SetupAgainst wires the in-process proxy to an arbitrary upstream URL.
func SetupAgainst(t *testing.T, upstream *httptest.Server) *Proxy {
	t.Helper()
	cfg := config.DefaultsForTest()
	device := cc.NewDeviceProfile("")
	states := cc.NewKeyStates()
	client, err := cc.NewClient(upstream.URL, "", "", device, states, false, "interactive")
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
	return &Proxy{Base: ts.URL, client: ts.Client()}
}

// JSONBytes decodes raw JSON into a map without testing.T coupling.
func JSONBytes(raw []byte) (map[string]any, error) {
	var m map[string]any
	err := json.Unmarshal(raw, &m)
	return m, err
}
