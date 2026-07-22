// Package handlers implements the command actions the daemon supports and
// registers them with the dispatcher.
package handlers

import (
	"time"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/files"
	"github.com/aioagent/daemon/internal/providers"
)

// Handlers bundles the dependencies shared by all command handlers.
type Handlers struct {
	Providers *providers.Registry
	FS        *files.Sandbox
	Cfg       *config.Config
	Version   string
	Started   time.Time
}

// Register wires every handler into the dispatcher.
func (h *Handlers) Register(d *dispatch.Dispatcher) {
	d.Register("agent.run", h.agentRun)
	d.Register("agent.providers", h.agentProviders)

	d.Register("file.read", h.fileRead)
	d.Register("file.write", h.fileWrite)
	d.Register("file.list", h.fileList)

	d.Register("git", h.git)
	d.Register("pr.review", h.prReview)
	d.Register("deploy", h.deploy)

	d.Register("system.info", h.systemInfo)
}
