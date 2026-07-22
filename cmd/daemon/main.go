// Command daemon runs the AIO agent daemon: it dials out to a control plane
// over WebSocket and executes AI coding-agent commands on this machine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/daemon"
)

// version is overridable at build time: -ldflags "-X main.version=1.2.3".
var version = "0.1.0"

func main() {
	var (
		configPath = flag.String("config", os.Getenv("AIO_CONFIG"), "path to config.yaml")
		server     = flag.String("server", "", "control plane WebSocket URL (overrides config)")
		token      = flag.String("token", "", "auth token (overrides config; prefer AIO_TOKEN)")
		logLevel   = flag.String("log-level", "", "log level: debug|info|warn|error")
		showVer    = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}
	// Flag overrides win over file/env.
	if *server != "" {
		cfg.ServerURL = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	logger := newLogger(cfg.LogLevel)

	if err := cfg.Validate(); err != nil {
		logger.Error("invalid configuration", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	d := daemon.New(cfg, logger, version)
	if err := d.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("daemon exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("daemon stopped")
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	return slog.New(h)
}
