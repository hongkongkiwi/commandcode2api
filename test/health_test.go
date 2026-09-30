package contract_test

import (
	"testing"

	"github.com/hongkongkiwi/commandcode2api/test/testutil"
)

// /health must stay unauthenticated even when the pool gate is active
// (orchestrators never see 401/503 on liveness probes — reference behavior).
func TestHealthUnauthenticatedWithPool(t *testing.T) {
	mock := testutil.NewMockCC(t, testutil.WithFinish("stop")...)
	f := setupPool(t, mock.Server.URL)

	resp, raw, err := f.Proxy.Get("/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("/health status = %d, want 200\n%s", resp.StatusCode, raw)
	}
	if string(raw) != "OK" {
		t.Fatalf("/health body = %q, want OK", raw)
	}
}
