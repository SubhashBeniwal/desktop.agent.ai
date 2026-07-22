package handlers

import (
	"runtime"
	"time"

	"github.com/aioagent/daemon/internal/dispatch"
)

// systemInfo reports daemon identity, capabilities and uptime.
func (h *Handlers) systemInfo(c *dispatch.Ctx) (any, error) {
	return map[string]any{
		"daemon_id":  h.Cfg.DaemonID,
		"name":       h.Cfg.Name,
		"version":    h.Version,
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"uptime_sec": int(time.Since(h.Started).Seconds()),
		"workspaces": h.FS.Roots(),
		"providers":  h.Providers.AvailableNames(),
	}, nil
}
