// Package config loads config.json with environment-variable overlay.
//
// Drop-in compatible with commandcode-proxy: same JSON field names, same
// CC_* environment variables, same defaults (port 3000 unless overridden —
// the shipped config.json uses 3050, matching the reference repo).
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port    int    `json:"port"`
	Host    string `json:"host"`
	APIBase string `json:"apiBase"`
	// APIKey is an optional fallback CC key used when a request carries none
	// (kept from the reference config; the client header is the normal path).
	APIKey                 string `json:"apiKey"`
	ProjectSlug            string `json:"projectSlug"`
	LogFile                string `json:"logFile"`
	LogLevel               string `json:"logLevel"`
	UseProviderModels      bool   `json:"useProviderModels"`
	ModelRefreshInterval   int64  `json:"modelRefreshIntervalMs"` // milliseconds
	ZDR                    bool   `json:"zdr"`
	CLIMode                string `json:"cliMode"`
	CLISessionMode         string `json:"cliSessionMode"`
	FingerprintSalt        string `json:"fingerprintSalt"`
	DeviceProjectDir       string `json:"deviceProjectDir"`
	EmptySystemPlaceholder bool   `json:"emptySystemPlaceholder"`
	UpstreamProxy          string `json:"upstreamProxy"`
}

func defaults() Config {
	return Config{
		Port:                   3000,
		Host:                   "0.0.0.0",
		APIBase:                "https://api.commandcode.ai",
		ProjectSlug:            "cc-proxy",
		LogFile:                "",
		LogLevel:               "info",
		UseProviderModels:      true,
		ModelRefreshInterval:   5 * 60 * 1000,
		ZDR:                    false,
		CLIMode:                "agent",
		CLISessionMode:         "interactive",
		FingerprintSalt:        "",
		DeviceProjectDir:       "",
		EmptySystemPlaceholder: true,
		UpstreamProxy:          "",
	}
}

// DefaultsForTest returns pristine defaults (no file/env reads) for
// in-process test harnesses.
func DefaultsForTest() *Config {
	c := defaults()
	return &c
}

// Load reads config.json (explicit CC2API_CONFIG_PATH, next to the
// executable, then the working directory) and applies the CC_* env overlay.
func Load() (*Config, error) {
	cfg := defaults()

	for _, path := range candidatePaths() {
		if raw, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(raw, &cfg); err != nil {
				fmt.Fprintf(os.Stderr, "[config] Failed to parse %s: %v\n", path, err)
			}
			break
		}
	}

	applyEnv(&cfg)
	return &cfg, nil
}

func candidatePaths() []string {
	paths := make([]string, 0, 3)
	if p := os.Getenv("CC2API_CONFIG_PATH"); p != "" {
		paths = append(paths, p)
	}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, filepath.Join(filepath.Dir(exe), "config.json"))
	}
	if cwd, err := os.Getwd(); err == nil {
		p := filepath.Join(cwd, "config.json")
		if len(paths) == 0 || paths[0] != p {
			paths = append(paths, p)
		}
	}
	return paths
}

func applyEnv(c *Config) {
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Port = n
		}
	}
	if v := os.Getenv("HOST"); v != "" {
		c.Host = v
	}
	if v := os.Getenv("CC_API_BASE"); v != "" {
		c.APIBase = v
	}
	if v := os.Getenv("PROJECT_SLUG"); v != "" {
		c.ProjectSlug = v
	}
	if v := os.Getenv("LOG_FILE"); v != "" {
		c.LogFile = v
	}
	if v := os.Getenv("CC_USE_PROVIDER_MODELS"); v != "" {
		c.UseProviderModels = v != "false"
	}
	if v, ok := os.LookupEnv("CMD_ZDR"); ok {
		c.ZDR = v == "1"
	}
	if v, ok := os.LookupEnv("CC_FINGERPRINT_SALT"); ok {
		c.FingerprintSalt = v
	}
	if v := os.Getenv("CC_DEVICE_PROJECT_DIR"); v != "" {
		c.DeviceProjectDir = v
	}
	if v := os.Getenv("CC_CLI_MODE"); v != "" {
		c.CLIMode = v
	}
	if v := os.Getenv("CC_CLI_SESSION_MODE"); v != "" {
		c.CLISessionMode = v
	}
	if v := os.Getenv("CC_EMPTY_SYSTEM_PLACEHOLDER"); v != "" {
		c.EmptySystemPlaceholder = v != "false"
	}
	if v := os.Getenv("CC_UPSTREAM_PROXY"); v != "" {
		c.UpstreamProxy = v
	}
}

// --- numeric env knobs (kept out of config.json in the reference) ---

// EnvInt returns a positive integer from env or def when unset/invalid.
func EnvInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// MaxBodyBytes mirrors CC_MAX_BODY_MB (default 100 MB).
func MaxBodyBytes() int64 {
	mb := EnvInt("CC_MAX_BODY_MB", 100)
	return int64(mb) * 1024 * 1024
}

// StreamIdleMS mirrors CC_STREAM_IDLE_MS (default 30s).
func StreamIdleMS() int { return EnvInt("CC_STREAM_IDLE_MS", 30000) }

// NonStreamIdleMS mirrors CC_NONSTREAM_IDLE_MS (default 90s).
func NonStreamIdleMS() int { return EnvInt("CC_NONSTREAM_IDLE_MS", 90000) }

// MaxInflight mirrors CC_MAX_INFLIGHT (0 = unlimited).
func MaxInflight() int { return EnvInt("CC_MAX_INFLIGHT", 0) }

// ClientDrainTimeoutMS mirrors CC_CLIENT_DRAIN_TIMEOUT_MS (0 = disabled).
func ClientDrainTimeoutMS() int { return EnvInt("CC_CLIENT_DRAIN_TIMEOUT_MS", 0) }

// KeepaliveTimeoutMS mirrors CC_KEEPALIVE_TIMEOUT_MS (default 65s).
func KeepaliveTimeoutMS() int { return EnvInt("CC_KEEPALIVE_TIMEOUT_MS", 65000) }
