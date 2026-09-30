// Package store persists the key pool: CC keys (encrypted at rest),
// gateway keys (hashed), and usage records.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD // nil when no vault secret (plaintext storage)
}

// Open creates/opens the DB at path and applies migrations.
// vaultSecret (CC_VAULT_SECRET) enables AES-256-GCM encryption of CC keys
// at rest; empty stores plaintext with a startup warning from the caller.
func Open(path, vaultSecret string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	// Single writer; SQLite serializes anyway and one conn avoids SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if vaultSecret != "" {
		key := sha256.Sum256([]byte(vaultSecret))
		block, err := aes.NewCipher(key[:])
		if err == nil {
			s.aead, err = cipher.NewGCM(block)
			if err != nil {
				db.Close()
				return nil, fmt.Errorf("vault init: %w", err)
			}
		}
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS cc_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			key_enc TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			priority INTEGER NOT NULL DEFAULT 100,
			cooldown_until TEXT NOT NULL DEFAULT '',
			quota_json TEXT NOT NULL DEFAULT '{}',
			last_used_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS gateway_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL UNIQUE,
			enabled INTEGER NOT NULL DEFAULT 1,
			rpm_limit INTEGER NOT NULL DEFAULT 0,
			quota_total INTEGER NOT NULL DEFAULT 0,
			quota_used INTEGER NOT NULL DEFAULT 0,
			model_whitelist TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS usage_records (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			gateway_key_id INTEGER,
			cc_key_id INTEGER,
			model TEXT NOT NULL DEFAULT '',
			protocol TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0,
			completion_tokens INTEGER NOT NULL DEFAULT 0,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			status_code INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			ttft_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_records(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_cc_keys_status ON cc_keys(status)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// --- CC keys ---

type CCKey struct {
	ID            int64
	Name          string
	Status        string // active | exhausted | cooling | invalid | disabled
	Priority      int
	CooldownUntil time.Time
	QuotaJSON     string
	LastUsedAt    time.Time
	CreatedAt     time.Time
	// PlainKey is populated on read (decrypted) / write (pre-encryption).
	PlainKey string
}

func (s *Store) encrypt(plain string) (string, error) {
	if s.aead == nil {
		return plain, nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Store) decrypt(enc string) (string, error) {
	if s.aead == nil {
		return enc, nil
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize {
		return "", errors.New("ciphertext too short")
	}
	plain, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// AddCCKey stores a new pooled CC key.
func (s *Store) AddCCKey(name, plainKey string, priority int) (*CCKey, error) {
	enc, err := s.encrypt(plainKey)
	if err != nil {
		return nil, err
	}
	res, err := s.db.Exec(`INSERT INTO cc_keys (name, key_enc, priority) VALUES (?, ?, ?)`, name, enc, priority)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &CCKey{ID: id, Name: name, Status: "active", Priority: priority, PlainKey: plainKey}, nil
}

func (s *Store) ListCCKeys() ([]CCKey, error) {
	rows, err := s.db.Query(`SELECT id, name, key_enc, status, priority, cooldown_until, quota_json, last_used_at, created_at FROM cc_keys ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CCKey
	for rows.Next() {
		var k CCKey
		var enc, cooldown, lastUsed, created string
		if err := rows.Scan(&k.ID, &k.Name, &enc, &k.Status, &k.Priority, &cooldown, &k.QuotaJSON, &lastUsed, &created); err != nil {
			return nil, err
		}
		if k.PlainKey, err = s.decrypt(enc); err != nil {
			return nil, fmt.Errorf("key %d decrypt: %w", k.ID, err)
		}
		k.CooldownUntil, _ = time.Parse(time.RFC3339, cooldown)
		k.LastUsedAt, _ = time.Parse(time.RFC3339, lastUsed)
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteCCKey removes a pooled key.
func (s *Store) DeleteCCKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM cc_keys WHERE id = ?`, id)
	return err
}

// MarkCCKey writes status + optional cooldown.
func (s *Store) MarkCCKey(id int64, status string, cooldownUntil time.Time) error {
	cooldown := ""
	if !cooldownUntil.IsZero() {
		cooldown = cooldownUntil.UTC().Format(time.RFC3339)
	}
	_, err := s.db.Exec(`UPDATE cc_keys SET status = ?, cooldown_until = ? WHERE id = ?`, status, cooldown, id)
	return err
}

// TouchCCKey updates last_used_at.
func (s *Store) TouchCCKey(id int64) error {
	_, err := s.db.Exec(`UPDATE cc_keys SET last_used_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`, id)
	return err
}

// SetCCKeyQuota persists the quota probe result.
func (s *Store) SetCCKeyQuota(id int64, quotaJSON string) error {
	_, err := s.db.Exec(`UPDATE cc_keys SET quota_json = ? WHERE id = ?`, quotaJSON, id)
	return err
}

// --- Gateway keys ---

type GatewayKey struct {
	ID             int64
	Name           string
	KeyHash        string
	Enabled        bool
	RPMLimit       int
	QuotaTotal     int64
	QuotaUsed      int64
	ModelWhitelist string // comma-separated; empty = all
	CreatedAt      time.Time
}

// HashGatewayKey derives the stored hash (sha256, matching the constant-time
// compare in the pool).
func HashGatewayKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// AddGatewayKey stores a new gateway key (hash only; plaintext shown once).
func (s *Store) AddGatewayKey(name, rawKey string, rpmLimit int, whitelist string) (*GatewayKey, error) {
	res, err := s.db.Exec(
		`INSERT INTO gateway_keys (name, key_hash, rpm_limit, model_whitelist) VALUES (?, ?, ?, ?)`,
		name, HashGatewayKey(rawKey), rpmLimit, whitelist)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &GatewayKey{ID: id, Name: name, KeyHash: HashGatewayKey(rawKey), Enabled: true, RPMLimit: rpmLimit, ModelWhitelist: whitelist}, nil
}

// FindGatewayKey by raw key material (constant-time via hash lookup).
func (s *Store) FindGatewayKey(rawKey string) (*GatewayKey, error) {
	h := HashGatewayKey(rawKey)
	row := s.db.QueryRow(
		`SELECT id, name, key_hash, enabled, rpm_limit, quota_total, quota_used, model_whitelist, created_at
		 FROM gateway_keys WHERE key_hash = ?`, h)
	var k GatewayKey
	var enabled int
	var created string
	err := row.Scan(&k.ID, &k.Name, &k.KeyHash, &enabled, &k.RPMLimit, &k.QuotaTotal, &k.QuotaUsed, &k.ModelWhitelist, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k.Enabled = enabled != 0
	k.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return &k, nil
}

func (s *Store) ListGatewayKeys() ([]GatewayKey, error) {
	rows, err := s.db.Query(
		`SELECT id, name, key_hash, enabled, rpm_limit, quota_total, quota_used, model_whitelist, created_at
		 FROM gateway_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GatewayKey
	for rows.Next() {
		var k GatewayKey
		var enabled int
		var created string
		if err := rows.Scan(&k.ID, &k.Name, &k.KeyHash, &enabled, &k.RPMLimit, &k.QuotaTotal, &k.QuotaUsed, &k.ModelWhitelist, &created); err != nil {
			return nil, err
		}
		k.Enabled = enabled != 0
		k.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteGatewayKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM gateway_keys WHERE id = ?`, id)
	return err
}

// BumpGatewayQuota adds tokens to the used counter.
func (s *Store) BumpGatewayQuota(id int64, tokens int64) error {
	_, err := s.db.Exec(`UPDATE gateway_keys SET quota_used = quota_used + ? WHERE id = ?`, tokens, id)
	return err
}

// --- Usage records ---

type UsageRecord struct {
	ID               int64
	GatewayKeyID     sql.NullInt64
	CCKeyID          sql.NullInt64
	Model            string
	Protocol         string
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	StatusCode       int
	DurationMS       int64
	TTFTMS           int64
	CreatedAt        time.Time
}

func (s *Store) AddUsage(u UsageRecord) error {
	_, err := s.db.Exec(
		`INSERT INTO usage_records (gateway_key_id, cc_key_id, model, protocol, prompt_tokens, completion_tokens, cached_tokens, status_code, duration_ms, ttft_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.GatewayKeyID, u.CCKeyID, u.Model, u.Protocol, u.PromptTokens, u.CompletionTokens, u.CachedTokens, u.StatusCode, u.DurationMS, u.TTFTMS)
	return err
}

// UsageSummary aggregates recent usage for the admin dashboard.
type UsageSummary struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
}

func (s *Store) UsageSummarySince(since time.Time) (*UsageSummary, error) {
	row := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0)
		 FROM usage_records WHERE created_at >= ?`, since.UTC().Format(time.RFC3339))
	var sum UsageSummary
	err := row.Scan(&sum.Requests, &sum.PromptTokens, &sum.CompletionTokens, &sum.CachedTokens)
	return &sum, err
}
