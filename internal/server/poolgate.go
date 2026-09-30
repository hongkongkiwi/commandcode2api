package server

import (
	"context"
	"database/sql"
	"net/http"
	"sync"

	"github.com/hongkongkiwi/commandcode2api/internal/pool"
	"github.com/hongkongkiwi/commandcode2api/internal/store"
)

// PoolGate authenticates downstream requests and injects the pool decision
// into the request context. Pass-through requests pass unchanged.
type PoolGate struct {
	Resolver *pool.Resolver
	Next     http.Handler
}

type ctxKey struct{}

func decisionFrom(ctx context.Context) *poolDecision {
	d, _ := ctx.Value(ctxKey{}).(*poolDecision)
	return d
}

type poolDecision struct {
	resolved *pool.Resolved
	// keyID holds the pooled key that served the request; forwardOnce
	// stores it on selection (atomic because failover retries mutate it).
	keyIDHolder *atomicInt64
}

// atomicInt64 is a tiny typed wrapper (avoids importing sync/atomic here).
type atomicInt64 struct {
	mu  sync.Mutex
	val int64
}

func (a *atomicInt64) Store(v int64) { a.mu.Lock(); a.val = v; a.mu.Unlock() }
func (a *atomicInt64) Load() int64   { a.mu.Lock(); defer a.mu.Unlock(); return a.val }

func (pg *PoolGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	resolved := pg.Resolver.Resolve(r.Header)
	switch resolved.Mode {
	case pool.ModeNone:
		writeErrJSON(w, 401, errPayload(
			"Missing API key. Send Authorization: Bearer <user_...> for pass-through or a gateway sk- key for pool mode",
			"auth_error"))
		return
	case pool.ModePool:
		p := pg.Resolver.Pool
		if !p.AllowGatewayKey(resolved.Gateway.ID, resolved.Gateway.RPMLimit) {
			w.Header().Set("Retry-After", "10")
			writeErrJSON(w, 429, errPayload("Gateway key rate limit exceeded", "rate_limit_error"))
			return
		}
		if resolved.Gateway.QuotaTotal > 0 && resolved.Gateway.QuotaUsed >= resolved.Gateway.QuotaTotal {
			writeErrJSON(w, 429, errPayload("Gateway key quota exhausted", "rate_limit_error"))
			return
		}
		r = r.Clone(r.Context())
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, &poolDecision{resolved: resolved, keyIDHolder: &atomicInt64{}}))
	}
	pg.Next.ServeHTTP(w, r)
}

// modelWhitelisted checks the gateway key's model whitelist (empty = all).
func modelWhitelisted(whitelist, model string) bool {
	wl := normalizeList(whitelist)
	if len(wl) == 0 {
		return true
	}
	for _, m := range wl {
		if m == model {
			return true
		}
	}
	return false
}

func normalizeList(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s + "," {
		if c == ',' {
			cur = trimSpace(cur)
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(c)
	}
	return out
}

// recordUsage stores one request row when the pool store is wired.
func (d *Deps) recordUsage(resolved *pool.Resolved, ccKeyID int64, model, protocol string, status int, prompt, completion, cached int64, durationMS int64) {
	if d.Store == nil {
		return
	}
	rec := store.UsageRecord{
		Model:            model,
		Protocol:         protocol,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		CachedTokens:     cached,
		StatusCode:       status,
		DurationMS:       durationMS,
	}
	if resolved != nil && resolved.Gateway != nil {
		rec.GatewayKeyID = nullInt64(resolved.Gateway.ID)
		if resolved.Gateway.QuotaTotal > 0 {
			_ = d.Store.BumpGatewayQuota(resolved.Gateway.ID, prompt+completion)
		}
	}
	if ccKeyID > 0 {
		rec.CCKeyID = nullInt64(ccKeyID)
	}
	_ = d.Store.AddUsage(rec)
}

func nullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: v > 0}
}

// upstreamKeyID reports which pooled key served the request (0 = pass-through
// or pool inactive). forwardOnce stores it in the decision holder.
func upstreamKeyID(ctx context.Context) int64 {
	if dec := decisionFrom(ctx); dec != nil && dec.keyIDHolder != nil {
		return dec.keyIDHolder.Load()
	}
	return 0
}
