package server

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/pool"
	"github.com/hongkongkiwi/commandcode2api/internal/store"
)

// AdminREST exposes the pool management surface (bearer-token auth via
// CC_ADMIN_TOKEN). No web UI by design this phase.
type AdminREST struct {
	Store *store.Store
	Pool  *pool.Pool
	Token string
	Next  http.Handler
}

func (a *AdminREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a.Token == "" {
		writeErrJSON(w, 404, errPayload("Admin API disabled (set CC_ADMIN_TOKEN to enable)", "not_found"))
		return
	}
	auth := r.Header.Get("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		if subtle.ConstantTimeCompare([]byte(auth[7:]), []byte(a.Token)) != 1 {
			writeErrJSON(w, 401, errPayload("Invalid admin token", "authentication_error"))
			return
		}
	} else {
		writeErrJSON(w, 401, errPayload("Admin API requires Authorization: Bearer <CC_ADMIN_TOKEN>", "authentication_error"))
		return
	}
	a.route(w, r)
}

func (a *AdminREST) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/admin/status" && r.Method == http.MethodGet:
		a.status(w, r)
	case path == "/admin/keys" && r.Method == http.MethodGet:
		a.listKeys(w)
	case path == "/admin/keys" && r.Method == http.MethodPost:
		a.addKey(w, r)
	case len(path) > len("/admin/keys/") && r.Method == http.MethodDelete && matchPrefix(path, "/admin/keys/"):
		a.deleteKey(w, path[len("/admin/keys/"):])
	case path == "/admin/gateway-keys" && r.Method == http.MethodGet:
		a.listGatewayKeys(w)
	case path == "/admin/gateway-keys" && r.Method == http.MethodPost:
		a.addGatewayKey(w, r)
	case path == "/admin/usage" && r.Method == http.MethodGet:
		a.usage(w, r)
	case path == "/admin/pool/reload" && r.Method == http.MethodPost:
		a.reload(w)
	default:
		writeErrJSON(w, 404, errPayload("Not found", "not_found"))
	}
}

func matchPrefix(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }

func (a *AdminREST) status(w http.ResponseWriter, _ *http.Request) {
	sum, err := a.Store.UsageSummarySince(time.Now().Add(-24 * time.Hour))
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	writeJSONBody(w, 200, map[string]any{
		"poolKeys":  a.Pool.Size(),
		"usage24h":  sum,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

func (a *AdminREST) listKeys(w http.ResponseWriter) {
	keys, err := a.Store.ListCCKeys()
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "status": k.Status, "priority": k.Priority,
			"cooldownUntil": timeOrNil(k.CooldownUntil), "quota": jsonOrAny(k.QuotaJSON),
			"keyPreview": preview(k.PlainKey), "lastUsedAt": timeOrNil(k.LastUsedAt),
		})
	}
	writeJSONBody(w, 200, map[string]any{"data": out})
}

func (a *AdminREST) addKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string `json:"name"`
		Key      string `json:"key"`
		Priority int    `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		writeErrJSON(w, 400, errPayload("body must be {name, key, priority?}", "invalid_request_error"))
		return
	}
	if body.Priority == 0 {
		body.Priority = 100
	}
	k, err := a.Store.AddCCKey(body.Name, body.Key, body.Priority)
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	_ = a.Pool.Reload()
	writeJSONBody(w, 200, map[string]any{"id": k.ID, "name": k.Name, "status": k.Status})
}

func (a *AdminREST) deleteKey(w http.ResponseWriter, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErrJSON(w, 400, errPayload("bad key id", "invalid_request_error"))
		return
	}
	if err := a.Store.DeleteCCKey(id); err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	_ = a.Pool.Reload()
	writeJSONBody(w, 200, map[string]any{"deleted": id})
}

func (a *AdminREST) listGatewayKeys(w http.ResponseWriter) {
	keys, err := a.Store.ListGatewayKeys()
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "enabled": k.Enabled, "rpmLimit": k.RPMLimit,
			"quotaTotal": k.QuotaTotal, "quotaUsed": k.QuotaUsed, "modelWhitelist": k.ModelWhitelist,
		})
	}
	writeJSONBody(w, 200, map[string]any{"data": out})
}

func (a *AdminREST) addGatewayKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name       string `json:"name"`
		Key        string `json:"key"` // optional; generated when empty
		RPMLimit   int    `json:"rpmLimit"`
		Whitelist  string `json:"modelWhitelist"`
		QuotaTotal int64  `json:"quotaTotal"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErrJSON(w, 400, errPayload("bad body", "invalid_request_error"))
		return
	}
	if body.Key == "" {
		body.Key = "sk-" + cc.RandHex(16)
	}
	k, err := a.Store.AddGatewayKey(body.Name, body.Key, body.RPMLimit, body.Whitelist)
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	// Plaintext shown exactly once.
	writeJSONBody(w, 200, map[string]any{"id": k.ID, "name": k.Name, "key": body.Key})
}

func (a *AdminREST) usage(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if h := r.URL.Query().Get("hours"); h != "" {
		if n, err := strconv.Atoi(h); err == nil && n > 0 && n <= 720 {
			hours = n
		}
	}
	sum, err := a.Store.UsageSummarySince(time.Now().Add(-time.Duration(hours) * time.Hour))
	if err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	writeJSONBody(w, 200, map[string]any{"hours": hours, "summary": sum})
}

func (a *AdminREST) reload(w http.ResponseWriter) {
	if err := a.Pool.Reload(); err != nil {
		writeErrJSON(w, 500, errPayload(err.Error(), "internal_error"))
		return
	}
	writeJSONBody(w, 200, map[string]any{"keys": a.Pool.Size()})
}

func preview(key string) string {
	if len(key) <= 10 {
		return key[:min(len(key), 4)] + "…"
	}
	return key[:10] + "…"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func timeOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

func jsonOrAny(raw string) any {
	var v any
	if json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return raw
}
