// Package daemon wires the configuration, providers, command handlers and
// transport together and runs the daemon.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"time"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/files"
	"github.com/aioagent/daemon/internal/handlers"
	"github.com/aioagent/daemon/internal/protocol"
	"github.com/aioagent/daemon/internal/providers"
	"github.com/aioagent/daemon/internal/transport"
)

// Daemon is the top-level application.
type Daemon struct {
	cfg     config.Config
	log     *slog.Logger
	version string
}

// New creates a Daemon from validated configuration.
func New(cfg config.Config, log *slog.Logger, version string) *Daemon {
	return &Daemon{cfg: cfg, log: log, version: version}
}

// Run starts the daemon and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	fs, err := files.NewSandbox(d.cfg.Workspaces)
	if err != nil {
		return fmt.Errorf("init sandbox: %w", err)
	}

	registry := providers.BuildRegistry(providerOverrides(d.cfg))

	h := &handlers.Handlers{
		Providers: registry,
		FS:        fs,
		Cfg:       &d.cfg,
		Version:   d.version,
		Started:   time.Now(),
	}
	disp := dispatch.New()
	h.Register(disp)

	rtr := newRouter(disp, d.log)

	d.log.Info("starting daemon",
		"id", d.cfg.DaemonID,
		"providers_available", registry.AvailableNames(),
		"actions", disp.Actions(),
		"workspaces", fs.Roots(),
	)

	client := transport.New(transport.Options{
		URL:        d.cfg.ServerURL,
		Header:     authHeader(d.cfg),
		Handler:    rtr,
		Register:   d.registerFn(registry, disp),
		Heartbeat:  d.cfg.HeartbeatInterval.D(),
		BackoffMin: d.cfg.ReconnectMin.D(),
		BackoffMax: d.cfg.ReconnectMax.D(),
		Logger:     d.log,
	})

	return client.Run(ctx)
}

func (d *Daemon) registerFn(reg *providers.Registry, disp *dispatch.Dispatcher) func() protocol.Register {
	return func() protocol.Register {
		return protocol.Register{
			DaemonID:  d.cfg.DaemonID,
			Name:      d.cfg.Name,
			Version:   d.version,
			OS:        runtime.GOOS,
			Arch:      runtime.GOARCH,
			Providers: reg.AvailableNames(),
			Actions:   disp.Actions(),
		}
	}
}

func authHeader(cfg config.Config) http.Header {
	h := http.Header{}
	if cfg.Token != "" {
		h.Set("Authorization", "Bearer "+cfg.Token)
	}
	h.Set("X-Daemon-ID", cfg.DaemonID)
	return h
}

func providerOverrides(cfg config.Config) map[string]providers.Override {
	if len(cfg.Providers) == 0 {
		return nil
	}
	out := make(map[string]providers.Override, len(cfg.Providers))
	for name, spec := range cfg.Providers {
		out[name] = providers.Override{
			Enabled: spec.Enabled,
			Bin:     spec.Bin,
			Args:    spec.Args,
			Env:     spec.Env,
		}
	}
	return out
}
