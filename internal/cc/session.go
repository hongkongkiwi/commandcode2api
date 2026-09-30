package cc

import (
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// Per-key session and initialization state (port of sessionStore /
// keyStateStore). One session per API key, 12h expiry + 1h jitter;
// fingerprint is stable for the life of the process (derived from the key).

const (
	SessionDuration = 12 * time.Hour
	SessionJitter   = time.Hour
)

type keyState struct {
	sessionID   string
	sessionExp  time.Time
	fingerprint Fingerprint
	nextInitAt  time.Time
	initMu      sync.Mutex // serializes ensureInitialized per key
}

// KeyStates holds per-API-key session + fingerprint + init-throttle state.
type KeyStates struct {
	mu   sync.Mutex
	keys map[string]*keyState
}

func NewKeyStates() *KeyStates {
	k := &KeyStates{keys: make(map[string]*keyState)}
	go k.cleanupLoop()
	return k
}

func (k *KeyStates) get(apiKey string) *keyState {
	k.mu.Lock()
	defer k.mu.Unlock()
	st, ok := k.keys[apiKey]
	if !ok {
		st = &keyState{}
		k.keys[apiKey] = st
	}
	return st
}

func (k *KeyStates) fingerprint(apiKey string) Fingerprint {
	st := k.get(apiKey)
	k.mu.Lock()
	defer k.mu.Unlock()
	if st.fingerprint.Thumbmark == "" {
		// fingerprint set by Fingerprinter via StoreFingerprint at creation;
		// defensively regenerate here if missing.
		st.fingerprint = globalFingerprinter.Generate(apiKey)
	}
	return st.fingerprint
}

// StoreFingerprint pre-populates a key's fingerprint (done at creation time
// so Generate runs exactly once per key).
func (k *KeyStates) StoreFingerprint(apiKey string, fp Fingerprint) {
	st := k.get(apiKey)
	k.mu.Lock()
	st.fingerprint = fp
	k.mu.Unlock()
}

// SessionID returns the key's active session id, creating a fresh one when
// expired or absent.
func (k *KeyStates) SessionID(apiKey string) string {
	st := k.get(apiKey)
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	if st.sessionID != "" && now.Before(st.sessionExp) {
		return st.sessionID
	}
	st.sessionID = NewUUID()
	st.sessionExp = now.Add(SessionDuration + time.Duration(rand.Int63n(int64(SessionJitter))))
	return st.sessionID
}

func (k *KeyStates) cleanupLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		k.mu.Lock()
		removed := 0
		for key, st := range k.keys {
			if now.After(st.sessionExp) && st.sessionID != "" {
				delete(k.keys, key)
				removed++
			}
		}
		k.mu.Unlock()
		if removed > 0 {
			logx.Info("Session cleanup", map[string]any{"cleaned": removed, "remaining": len(k.keys)})
		}
	}
}

// SessionIDFromHeaders applies the reference's override chain: client
// session headers first, then prompt_cache_key, then the per-key session.
func SessionIDFromHeaders(headers map[string]string, apiKey, promptCacheKey string, fresh func(string) string) string {
	candidates := []string{
		headers["x-session-id"],
		headers["x-claude-code-session-id"],
		headers["session_id"],
		promptCacheKey,
	}
	for _, id := range candidates {
		id = strings.TrimSpace(id)
		if len(id) >= 8 {
			return id
		}
	}
	return fresh(apiKey)
}

// globalFingerprinter is set by SetGlobalFingerprinter at startup.
var globalFingerprinter *Fingerprinter

// SetGlobalFingerprinter wires the process-wide fingerprinter (used by the
// defensive path in KeyStates.fingerprint).
func SetGlobalFingerprinter(f *Fingerprinter) { globalFingerprinter = f }
