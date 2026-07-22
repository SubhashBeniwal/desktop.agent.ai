package handlers

import (
	"fmt"
	"os/exec"

	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/executor"
)

type deployReq struct {
	Workdir string   `json:"workdir"`
	Command string   `json:"command"`         // executable, or a shell line when Shell is true
	Args    []string `json:"args,omitempty"`  // args when not using Shell
	Shell   bool     `json:"shell,omitempty"` // run `sh -c "<command>"`
}

// deploy runs a deployment command/script inside a workspace and streams output.
func (h *Handlers) deploy(c *dispatch.Ctx) (any, error) {
	var req deployReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if req.Command == "" {
		return nil, fmt.Errorf("command is required")
	}
	workdir, err := h.FS.ResolveDir(req.Workdir)
	if err != nil {
		return nil, err
	}

	var cmd *exec.Cmd
	if req.Shell {
		cmd = exec.CommandContext(c.Context, "sh", "-c", req.Command)
	} else {
		cmd = exec.CommandContext(c.Context, req.Command, req.Args...)
	}
	cmd.Dir = workdir

	c.Log("system", fmt.Sprintf("deploy: %s (in %s)", req.Command, workdir))
	code, err := executor.Run(c.Context, cmd, c.Log)
	if err != nil {
		return nil, err
	}
	return map[string]any{"exit_code": code}, nil
}
