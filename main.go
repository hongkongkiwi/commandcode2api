// Command commandcode2api is a Go port of commandcode-proxy: Command Code
// API → OpenAI / Anthropic compatible endpoints, with an optional
// server-side key pool.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hongkongkiwi/commandcode2api/internal/cc"
	"github.com/hongkongkiwi/commandcode2api/internal/config"
	"github.com/hongkongkiwi/commandcode2api/internal/logx"
	"github.com/hongkongkiwi/commandcode2api/internal/server"
)

var version = "0.1.0-dev"

func main() {
	cfgPath := flag.String("config", "", "path to config.json")
	_ = flag.String("state-dir", "", "state directory (key pool DB; reserved for P3)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("commandcode2api", version)
		return
	}

	if *cfgPath != "" {
		// explicit config path: point the loader at it via chdir-free lookup
		os.Setenv("CC2API_CONFIG_PATH", *cfgPath)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load failed:", err)
		os.Exit(1)
	}
	logx.Init(cfg.LogLevel, cfg.LogFile)

	// Upstream proxy validation: refuse to start on a bad URL.
	if cfg.UpstreamProxy != "" {
		if red := cc.RedactProxyURL(cfg.UpstreamProxy); red == "(invalid upstreamProxy)" {
			logx.Error("Invalid upstreamProxy, refusing to start", map[string]any{"value": red})
			os.Exit(1)
		}
	}

	device := cc.NewDeviceProfile(cfg.DeviceProjectDir)
	states := cc.NewKeyStates()
	client, err := cc.NewClient(cfg.APIBase, cfg.UpstreamProxy, cfg.FingerprintSalt, device, states, cfg.ZDR, cfg.CLISessionMode)
	if err != nil {
		logx.Error("Invalid upstreamProxy, refusing to start", map[string]any{"value": cc.RedactProxyURL(cfg.UpstreamProxy)})
		os.Exit(1)
	}
	cc.SetGlobalFingerprinter(client.Fingerprinter())

	deps := &server.Deps{
		Client: client,
		States: states,
		Models: &server.Models{Client: client, RefreshEvery: time.Duration(cfg.ModelRefreshInterval) * time.Millisecond},
		Cfg:    cfg,
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client.StartDriftCheck(rootCtx)

	handler := server.NewMux(deps, server.NewInflight(config.MaxInflight()))

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: SSE responses live as long as upstream streams.
		IdleTimeout: time.Duration(config.KeepaliveTimeoutMS()) * time.Millisecond,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		logx.Error("Listen failed", map[string]any{"addr": srv.Addr, "error": err.Error()})
		os.Exit(1)
	}

	logx.Info("CC Proxy started", map[string]any{
		"url":                    fmt.Sprintf("http://%s:%d", cfg.Host, cfg.Port),
		"api":                    cfg.APIBase,
		"models":                 len(server.HardcodedModels),
		"session":                "12h + 1h jitter, per API key",
		"zdr":                    zdrLabel(cfg),
		"emptySystemPlaceholder": boolLabel(cfg.EmptySystemPlaceholder, "on (space placeholder for requests without system prompt, issue #17)", "off"),
		"logFile":                orDefault(cfg.LogFile, "(console only)"),
		"keepAliveTimeout":       fmt.Sprintf("%dms (reverse-proxy keepalive_timeout must be smaller)", config.KeepaliveTimeoutMS()),
		"idleTimeouts":           fmt.Sprintf("stream %dms / nonstream %dms", config.StreamIdleMS(), config.NonStreamIdleMS()),
		"maxInflight":            inflightLabel(config.MaxInflight()),
		"upstreamProxy":          cc.RedactProxyURL(cfg.UpstreamProxy),
		"protocolVersion":        cc.CCProtocolVersion,
	})

	go func() {
		<-rootCtx.Done()
		logx.Info("Shutting down", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logx.Error("Server error", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func boolLabel(b bool, on, off string) string {
	if b {
		return on
	}
	return off
}

func zdrLabel(cfg *config.Config) string {
	if cfg.ZDR {
		return "enabled (x-cmd-zdr: 1 on generation/init requests)"
	}
	return "off (CMD_ZDR=1 or per-request x-cmd-zdr: 1 to enable)"
}

func inflightLabel(max int) string {
	if max > 0 {
		return fmt.Sprintf("%d (global, /health exempt)", max)
	}
	return "unlimited (CC_MAX_INFLIGHT=0)"
}
