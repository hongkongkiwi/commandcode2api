package cc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/logx"
)

// Client talks to the Command Code API: /alpha/generate plus the
// fingerprint/lifecycle pre-requests and the provider model catalog.

type Client struct {
	Base           string
	HTTP           *http.Client
	Device         DeviceProfile
	States         *KeyStates
	ZDR            bool
	CLISessionMode string
	fingerprinter  *Fingerprinter

	// version state (protocol drift check)
	versionMu   sync.Mutex
	version     string
	lastDriftAt time.Time

	proxyURL *url.URL // nil = direct
}

func NewClient(base, upstreamProxy, fingerprintSalt string, device DeviceProfile, states *KeyStates, zdr bool, cliSessionMode string) (*Client, error) {
	transport := &http.Transport{
		Proxy: nil, // CC_UPSTREAM_PROXY is handled explicitly below, env proxies ignored (matches reference)
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false, // HTTP/1.1 keeps streaming simple; upstream doesn't need h2
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		// Generous buffering keeps the hot path allocation-light.
		ReadBufferSize:  32 * 1024,
		WriteBufferSize: 32 * 1024,
	}

	var proxyURL *url.URL
	if upstreamProxy != "" {
		u, err := url.Parse(upstreamProxy)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("upstreamProxy is not a valid URL (expected http://host:port)")
		}
		if u.Scheme != "http" {
			return nil, fmt.Errorf("upstreamProxy only supports http:// (CONNECT) proxies, got %s://", u.Scheme)
		}
		proxyURL = u
		transport.Proxy = http.ProxyURL(u) // Go's transport does CONNECT + TLS end-to-end, cert validated against target host
	}

	fingerprinter := NewFingerprinter(fingerprintSalt, device)
	c := &Client{
		Base:           strings.TrimRight(base, "/"),
		HTTP:           &http.Client{Transport: transport, Timeout: 0}, // no total timeout; idle watchdogs govern reads
		Device:         device,
		States:         states,
		ZDR:            zdr,
		CLISessionMode: cliSessionMode,
		fingerprinter:  fingerprinter,
		version:        CCProtocolVersion,
		proxyURL:       proxyURL,
	}
	return c, nil
}

// saltOf removed: fingerprint salt comes from config (CC_FINGERPRINT_SALT),
// not the proxy URL.

func (c *Client) Fingerprinter() *Fingerprinter { return c.fingerprinter }

func (c *Client) SetSalt(salt string) { c.fingerprinter.salt = salt }

func (c *Client) Version() string {
	c.versionMu.Lock()
	defer c.versionMu.Unlock()
	return c.version
}

// ProxyRedacted returns host:port form of the upstream proxy for logs.
func (c *Client) ProxyRedacted() string {
	if c.proxyURL == nil {
		return "(direct)"
	}
	host := c.proxyURL.Hostname()
	if p := c.proxyURL.Port(); p != "" {
		return host + ":" + p
	}
	return host
}

// redactProxyURL never includes credentials.
func RedactProxyURL(raw string) string {
	if raw == "" {
		return "(direct)"
	}
	if u, err := url.Parse(raw); err == nil {
		return u.Scheme + "//" + u.Host
	}
	return "(invalid upstreamProxy)"
}

// initHeaders are shared by fingerprint/lifecycle pre-requests.
func (c *Client) initHeaders(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("x-cli-environment", "production")
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-command-code-version", c.Version())
	if c.ZDR {
		h.Set("x-cmd-zdr", "1")
	}
	return h
}

// EnsureInitialized performs the per-key init pre-requests (fingerprint
// record + lifecycle event) on first use and every 8h + 2h jitter.
// Errors never block the request path.
func (c *Client) EnsureInitialized(ctx context.Context, apiKey string) {
	st := c.States.get(apiKey)
	st.initMu.Lock()
	defer st.initMu.Unlock()

	now := time.Now()
	if now.Before(st.nextInitAt) {
		return
	}

	if st.fingerprint.Thumbmark == "" {
		fp := c.fingerprinter.Generate(apiKey)
		c.States.StoreFingerprint(apiKey, fp)
		st.fingerprint = fp
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		body, _ := json.Marshal(st.fingerprint)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/alpha/fingerprint/record", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header = c.initHeaders(apiKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				logx.Warn("Fingerprint record error", map[string]any{"error": err.Error()})
			}
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		if resp.StatusCode >= 400 {
			logx.Warn("Fingerprint record failed", map[string]any{"status": resp.StatusCode})
		} else {
			logx.Info("Fingerprint recorded", nil)
		}
	}()

	go func() {
		defer wg.Done()
		payload := map[string]any{
			"eventType": "cli_session_exists",
			"metadata": map[string]any{
				"sessionId":  "sess_" + RandHex(8),
				"cliVersion": c.Version(),
				"mode":       c.CLISessionMode,
				"os":         st.fingerprint.Components.Platform + "-" + st.fingerprint.Components.Arch,
			},
		}
		body, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/alpha/lifecycle-events", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header = c.initHeaders(apiKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() == nil {
				logx.Warn("Lifecycle event error", map[string]any{"error": err.Error()})
			}
			return
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
		if resp.StatusCode >= 400 {
			logx.Warn("Lifecycle event failed", map[string]any{"status": resp.StatusCode})
		} else {
			logx.Info("Lifecycle event sent", nil)
		}
	}()

	wg.Wait()

	// Success: re-arm at 8h + 2h jitter. On failure leave nextInitAt zero so
	// the next request retries (matches the reference's catch path).
	jitter := time.Duration(rand.Int63n(int64(2 * time.Hour)))
	st.nextInitAt = time.Now().Add(8*time.Hour + jitter)
	logx.Info("Fingerprint/lifecycle next refresh", map[string]any{
		"nextInHours": fmt.Sprintf("%.1fh", (8*time.Hour + jitter).Hours()),
	})
}

// GenerateHeaders builds the /alpha/generate request headers (aligned with
// the CLI's buildCommandAuthHeaders: User-Agent "cli", no x-co-flag).
func (c *Client) GenerateHeaders(apiKey, sessionID string, zdrHeader bool) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "cli")
	h.Set("x-command-code-version", c.Version())
	h.Set("x-cli-environment", "production")
	h.Set("x-project-slug", SlugifyProjectPath(c.Device.ProjectDir))
	h.Set("x-taste-learning", "false")
	h.Set("x-session-id", sessionID)
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("traceparent", GenerateTraceparent())
	if c.ZDR || zdrHeader {
		h.Set("x-cmd-zdr", "1")
	}
	return h
}

// Generate POSTs the envelope and returns the raw response (caller streams
// or buffers the NDJSON body). The caller owns resp.Body.
func (c *Client) Generate(ctx context.Context, env *Envelope, apiKey, sessionID string, zdrHeader bool) (*http.Response, error) {
	body, err := MarshalEnvelope(env)
	if err != nil {
		return nil, fmt.Errorf("failed to build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	req.Header = c.GenerateHeaders(apiKey, sessionID, zdrHeader)
	return c.HTTP.Do(req)
}

// MarshalEnvelope serializes with HTML escaping off (byte-parity with
// Node's JSON.stringify for <, >, & in content).
func MarshalEnvelope(env *Envelope) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// FetchModels pulls the provider model catalog; 10s timeout.
func (c *Client) FetchModels(ctx context.Context, apiKey string) ([]map[string]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/provider/v1/models", nil)
	if err != nil {
		return nil, false
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+apiKey)
	h.Set("x-cli-environment", "production")
	h.Set("x-command-code-version", c.Version())
	req.Header = h

	resp, err := c.HTTP.Do(req)
	if err != nil {
		logx.Warn("Provider models fetch error, using hardcoded list", map[string]any{"error": err.Error()})
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		logx.Warn("Provider models fetch failed, using hardcoded list", map[string]any{"status": resp.StatusCode})
		return nil, false
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&parsed); err != nil || parsed.Data == nil {
		logx.Warn("Provider models response unusable, using hardcoded list", nil)
		return nil, false
	}
	models := make([]map[string]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		models = append(models, map[string]string{"id": m.ID, "name": m.ID})
	}
	logx.Info("Fetched models from Provider API", map[string]any{"count": len(models)})
	return models, true
}

// --- protocol drift check (warn only, never bumps the version) ---

func (c *Client) StartDriftCheck(ctx context.Context) {
	c.checkDrift(ctx)
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.checkDrift(ctx)
			}
		}
	}()
}

func (c *Client) checkDrift(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.npmjs.org/command-code/latest", nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		logx.Warn("CC version check failed", map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logx.Warn("CC version check failed", map[string]any{"error": "npm responded with " + strconv.Itoa(resp.StatusCode)})
		return
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&pkg); err != nil || pkg.Version == "" {
		logx.Warn("CC version check failed", map[string]any{"error": "unparseable npm payload"})
		return
	}
	if pkg.Version != CCProtocolVersion {
		logx.Warn("CC CLI version drift: protocol may have changed, re-align from the npm package", map[string]any{
			"implemented": CCProtocolVersion,
			"latest":      pkg.Version,
		})
	} else {
		logx.Info("CC CLI version in sync", map[string]any{"version": pkg.Version})
	}
}
