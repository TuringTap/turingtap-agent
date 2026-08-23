// Command turingtap-agent is the local companion to the TuringTap cloud MCP
// server: it maintains an outbound WSS tunnel to relay.turingtap.ai (reverse
// SOCKS into the user's LAN + browser.* RPC), manages a proxied Chromium
// instance for goto/act/ask_human, runs a local SSE MCP server on :7847, and
// shows a system-tray indicator.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/turingtap/agent/internal/browser"
	"github.com/turingtap/agent/internal/config"
	"github.com/turingtap/agent/internal/mcp"
	"github.com/turingtap/agent/internal/notify"
	"github.com/turingtap/agent/internal/tray"
	"github.com/turingtap/agent/internal/tunnel"
)

var version = "dev" // set by -ldflags at release

func main() {
	var cfgPath string
	var showVersion bool
	flag.StringVar(&cfgPath, "config", "", "path to agent.toml (default ~/.turingtap/agent.toml)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println(version)
		os.Exit(0)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	slog.Info("turingtap-agent starting", "version", version)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}
	cidrs, err := cfg.ParseCIDRs()
	if err != nil {
		slog.Error("config invalid", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Browser. Launch eagerly so the relay tunnel only comes up once
	// Chromium is ready — otherwise the first browser.goto RPC would race
	// go-rod's on-demand Chromium download (~150 MB on first run).
	br := browser.New(browser.Options{
		ProxyURL:  cfg.ProxyURL,
		ProxyAuth: cfg.ProxyAuth,
		CASPKI:    cfg.CASPKI,
		Headless:  cfg.Headless,
		CI:        cfg.CI,
	})
	if err := br.Launch(); err != nil {
		slog.Error("browser launch failed", "err", err)
		os.Exit(1)
	}
	defer br.Close()

	// Tunnel — serves CH_SOCKS + CH_RPC (browser.*) from the relay.
	tun := tunnel.New(cfg.RelayURL, cfg.APIKey, cidrs)
	tun.SetBrowser(br)
	tun.OnStateChange(tray.SetOnline)
	go func() {
		if err := tun.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Error("tunnel exited", "err", err)
		}
	}()

	// Local MCP server (optional — TT_AGENT_MCP_PORT=0 disables).
	if cfg.LocalMCPAddr != "" {
		srv := mcp.New(cfg, tun, nil)
		go func() {
			if err := srv.Run(ctx); err != nil {
				slog.Error("mcp server exited", "err", err)
				cancel()
			}
		}()
	} else {
		slog.Info("mcp: local server disabled")
	}

	// Tray (blocks on the tray build; select{} on the stub). Skip in CI.
	if !cfg.CI {
		tun.OnHandoffEnd(func(reason string) {
			msg := "Human handoff ended — control returned to your AI."
			if reason == "dismissed" {
				msg = "Your AI dismissed the handoff."
			}
			if err := notify.Toast("TuringTap", msg); err != nil {
				slog.Debug("handoff toast failed", "err", err)
			}
		})
		go tray.Run(tray.Callbacks{
			OnOpenBrowser: func() {
				if err := br.SetHeaded("Recorder — traffic is being captured"); err != nil {
					slog.Error("open browser failed", "err", err)
				}
			},
			OnQuit: cancel,
		})
	}

	<-ctx.Done()
	slog.Info("turingtap-agent shutting down")
}
