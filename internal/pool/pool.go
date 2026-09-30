// Package pool implements the server-side CC key pool: selection,
// sticky session affinity (upstream prompt-cache preservation), cooldown
// latching, per-gateway-key RPM limits, and quota-driven demotion.
package pool

import (
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/logx"
	"github.com/hongkongkiwi/commandcode2api/internal/store"
)

// Pool manages the active CC keys.
type Pool struct {
	mu        sync.Mutex
	keys      []store.CCKey // cached view; PlainKey populated
	rrCursor  int
	sticky    map[string]stickyEntry
	gwRPM     map[int64]*rpmWindow
	store     *store.Store
	stickyTTL time.Duration
}

type stickyEntry struct {
	keyID   int64
	expires time.Time
}

type rpmWindow struct {
	windowStart time.Time
	count       int
}

func New(st *store.Store) *Pool {
	p := &Pool{
		sticky:    map[string]stickyEntry{},
		gwRPM:     map[int64]*rpmWindow{},
		store:     st,
		stickyTTL: time.Hour,
	}
	return p
}

// Reload refreshes the in-memory key view from the store.
func (p *Pool) Reload() error {
	keys, err := p.store.ListCCKeys()
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.keys = keys
	p.mu.Unlock()
	return nil
}

// Size reports how many keys the pool holds.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys)
}

// Select picks the upstream key for a request.
//
// Selection order: sticky affinity (session → key, TTL 1h) → priority tier
// round-robin over selectable keys. A key is selectable when enabled-status,
// not cooling, and has material.
func (p *Pool) Select(sessionKey string, exclude map[int64]bool) *store.CCKey {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// Sticky: keep a conversation on one key so the upstream prompt cache
	// stays warm (zcode-proxy pattern).
	if sessionKey != "" {
		if e, ok := p.sticky[sessionKey]; ok && now.Before(e.expires) && !exclude[e.keyID] {
			for i := range p.keys {
				if p.keys[i].ID == e.keyID {
					k := p.keys[i]
					return &k
				}
			}
		}
		// expired / stale
		delete(p.sticky, sessionKey)
	}

	selectable := make([]int, 0, len(p.keys))
	for i := range p.keys {
		k := &p.keys[i]
		if exclude[k.ID] {
			continue
		}
		if k.Status == "disabled" || k.Status == "invalid" || k.Status == "exhausted" {
			continue
		}
		if !k.CooldownUntil.IsZero() && now.Before(k.CooldownUntil) {
			continue
		}
		if k.PlainKey == "" {
			continue
		}
		selectable = append(selectable, i)
	}
	if len(selectable) == 0 {
		return nil
	}

	// Lowest priority number first tier; round-robin within tier.
	best := selectable[0]
	for _, i := range selectable[1:] {
		if p.keys[i].Priority < p.keys[best].Priority {
			best = i
		}
	}
	tier := make([]int, 0, len(selectable))
	for _, i := range selectable {
		if p.keys[i].Priority == p.keys[best].Priority {
			tier = append(tier, i)
		}
	}
	p.rrCursor = (p.rrCursor + 1) % len(tier)
	chosen := tier[p.rrCursor]
	k := p.keys[chosen]

	if sessionKey != "" {
		p.sticky[sessionKey] = stickyEntry{keyID: k.ID, expires: now.Add(p.stickyTTL)}
		p.pruneStickyLocked(now)
	}
	return &k
}

const stickyCap = 10000

func (p *Pool) pruneStickyLocked(now time.Time) {
	if len(p.sticky) <= stickyCap {
		return
	}
	for k, e := range p.sticky {
		if now.After(e.expires) {
			delete(p.sticky, k)
		}
	}
}

// ReportSuccess clears any transient cooldown and marks the key active.
func (p *Pool) ReportSuccess(keyID int64) {
	p.mu.Lock()
	for i := range p.keys {
		if p.keys[i].ID == keyID {
			p.keys[i].Status = "active"
			p.keys[i].CooldownUntil = time.Time{}
		}
	}
	p.mu.Unlock()
	_ = p.store.MarkCCKey(keyID, "active", time.Time{})
	_ = p.store.TouchCCKey(keyID)
}

// ReportRateLimit latches a cooldown honoring the upstream Retry-After
// (capped at 30min) on 402/429/USAGE_EXCEEDED-style failures.
func (p *Pool) ReportRateLimit(keyID int64, retryAfterSeconds int) {
	if retryAfterSeconds <= 0 {
		retryAfterSeconds = 30
	}
	if retryAfterSeconds > 1800 {
		retryAfterSeconds = 1800
	}
	until := time.Now().Add(time.Duration(retryAfterSeconds) * time.Second)
	p.mu.Lock()
	for i := range p.keys {
		if p.keys[i].ID == keyID {
			p.keys[i].Status = "cooling"
			p.keys[i].CooldownUntil = until
		}
	}
	p.mu.Unlock()
	_ = p.store.MarkCCKey(keyID, "cooling", until)
	logx.Warn("Key cooling", map[string]any{"keyId": keyID, "cooldownSeconds": retryAfterSeconds})
}

// ReportExhausted marks a key dead for the quota window (USAGE_EXCEEDED,
// 402 payment required treated as terminal until a quota probe revives it).
func (p *Pool) ReportExhausted(keyID int64) {
	until := time.Now().Add(6 * time.Hour)
	p.mu.Lock()
	for i := range p.keys {
		if p.keys[i].ID == keyID {
			p.keys[i].Status = "exhausted"
			p.keys[i].CooldownUntil = until
		}
	}
	p.mu.Unlock()
	_ = p.store.MarkCCKey(keyID, "exhausted", until)
}

// ReportInvalid marks a key 401/403 — needs human attention.
func (p *Pool) ReportInvalid(keyID int64) {
	p.mu.Lock()
	for i := range p.keys {
		if p.keys[i].ID == keyID {
			p.keys[i].Status = "invalid"
		}
	}
	p.mu.Unlock()
	_ = p.store.MarkCCKey(keyID, "invalid", time.Time{})
}

// Classify maps an upstream failure to a pool action.
func Classify(statusCode int, code string) string {
	switch {
	case statusCode == 401 || statusCode == 403:
		return "invalid"
	case statusCode == 402 || code == "USAGE_EXCEEDED":
		return "exhausted"
	case statusCode == 429:
		return "rate_limit"
	default:
		return "none"
	}
}

// AllowGatewayKey enforces the per-key RPM limit (in-process 60s sliding
// window). Returns false when over the limit.
func (p *Pool) AllowGatewayKey(gwID int64, rpmLimit int) bool {
	if rpmLimit <= 0 {
		return true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	w, ok := p.gwRPM[gwID]
	if !ok || now.Sub(w.windowStart) >= time.Minute {
		p.gwRPM[gwID] = &rpmWindow{windowStart: now, count: 1}
		return true
	}
	if w.count >= rpmLimit {
		return false
	}
	w.count++
	return true
}

// StickySessionKey derives the affinity key from request headers, mirroring
// the session-override chain (headers → explicit values; the pool falls
// back to the client API key identity which the caller supplies).
func StickySessionKey(headers map[string]string, fallback string) string {
	for _, h := range []string{"x-session-id", "x-claude-code-session-id", "session_id"} {
		if v := strings.TrimSpace(headers[h]); len(v) >= 8 {
			return v
		}
	}
	return fallback
}

// HashAffinity produces a stable short hash for affinity keys (diagnostics).
func HashAffinity(s string) string {
	h := fnv.New32a()
	_, _ = fmt.Fprint(h, s)
	return fmt.Sprintf("%08x", h.Sum32())
}

var _ = rand.Intn
