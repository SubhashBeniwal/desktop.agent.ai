package handlers

import (
	"fmt"
	"strings"

	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/executor"
	"github.com/aioagent/daemon/internal/providers"
)

type agentRunReq struct {
	Provider    string            `json:"provider"`
	Prompt      string            `json:"prompt"`
	Workdir     string            `json:"workdir,omitempty"`
	Model       string            `json:"model,omitempty"`
	Files       []string          `json:"files,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	AutoApprove bool              `json:"auto_approve,omitempty"`
}

// agentRun executes an AI coding agent and streams its output back.
func (h *Handlers) agentRun(c *dispatch.Ctx) (any, error) {
	var req agentRunReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if req.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	if req.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}

	p, ok := h.Providers.Get(req.Provider)
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (configured: %v)", req.Provider, h.Providers.Names())
	}
	if !p.Available() {
		return nil, fmt.Errorf("provider %q is not available: its CLI is not installed on this machine", req.Provider)
	}

	workdir := ""
	if req.Workdir != "" {
		abs, err := h.FS.ResolveDir(req.Workdir)
		if err != nil {
			return nil, err
		}
		workdir = abs
	}

	task := providers.Task{
		Prompt:      req.Prompt,
		Workdir:     workdir,
		Model:       req.Model,
		Files:       req.Files,
		ExtraArgs:   req.Args,
		Env:         req.Env,
		AutoApprove: req.AutoApprove,
	}

	cmd, err := p.Command(c.Context, task)
	if err != nil {
		return nil, err
	}

	c.Log("system", fmt.Sprintf("running %s in %q: %s", p.Name(), cmd.Dir, strings.Join(cmd.Args, " ")))
	code, err := executor.Run(c.Context, cmd, c.Log)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"provider":  p.Name(),
		"exit_code": code,
	}, nil
}

// agentProviders lists configured providers and whether each is installed.
func (h *Handlers) agentProviders(c *dispatch.Ctx) (any, error) {
	type entry struct {
		Name      string `json:"name"`
		Available bool   `json:"available"`
	}
	var out []entry
	for _, name := range h.Providers.Names() {
		p, _ := h.Providers.Get(name)
		out = append(out, entry{Name: name, Available: p.Available()})
	}
	return out, nil
}
