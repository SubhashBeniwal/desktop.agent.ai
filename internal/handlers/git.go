package handlers

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/executor"
)

type gitReq struct {
	Workdir string   `json:"workdir"`
	Args    []string `json:"args"` // git subcommand + flags, e.g. ["status","--short"]
}

// git runs an arbitrary git command inside a workspace and streams its output.
func (h *Handlers) git(c *dispatch.Ctx) (any, error) {
	var req gitReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if len(req.Args) == 0 {
		return nil, fmt.Errorf("args is required (e.g. [\"status\"])")
	}
	workdir, err := h.FS.ResolveDir(req.Workdir)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(c.Context, "git", req.Args...)
	cmd.Dir = workdir
	c.Log("system", fmt.Sprintf("git %s (in %s)", strings.Join(req.Args, " "), workdir))

	code, err := executor.Run(c.Context, cmd, c.Log)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exit_code": code}, nil
}
