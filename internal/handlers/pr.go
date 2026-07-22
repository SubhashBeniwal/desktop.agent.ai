package handlers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/executor"
	"github.com/aioagent/daemon/internal/providers"
)

type prReviewReq struct {
	Workdir  string `json:"workdir"`            // a git checkout of the repo
	Number   int    `json:"number"`            // PR number
	Provider string `json:"provider"`          // agent used to write the review
	Model    string `json:"model,omitempty"`   // optional model override
	Post     bool   `json:"post,omitempty"`    // reserved: post the review as a comment
	Extra    string `json:"instructions,omitempty"`
}

// prReview fetches a pull request's metadata and diff via the GitHub CLI (`gh`)
// and asks an AI provider to review it, streaming the review back.
//
// It relies on `gh` being installed and authenticated in the workspace.
func (h *Handlers) prReview(c *dispatch.Ctx) (any, error) {
	var req prReviewReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	if req.Number <= 0 {
		return nil, fmt.Errorf("number (PR number) is required")
	}
	if req.Provider == "" {
		return nil, fmt.Errorf("provider is required")
	}
	workdir, err := h.FS.ResolveDir(req.Workdir)
	if err != nil {
		return nil, err
	}
	p, ok := h.Providers.Get(req.Provider)
	if !ok || !p.Available() {
		return nil, fmt.Errorf("provider %q is not available", req.Provider)
	}

	num := fmt.Sprintf("%d", req.Number)

	c.Log("system", "fetching PR metadata via gh")
	meta := runGH(c, workdir, "pr", "view", num, "--json", "title,author,body,files,additions,deletions")

	c.Log("system", "fetching PR diff via gh")
	diffCmd := exec.CommandContext(c.Context, "gh", "pr", "diff", num)
	diffCmd.Dir = workdir
	diff, err := executor.Capture(c.Context, diffCmd)
	if err != nil {
		return nil, fmt.Errorf("gh pr diff failed: %w (output: %s)", err, truncate(diff, 500))
	}

	// Stage the context on disk so the agent can read it from the workspace.
	tmp, err := os.CreateTemp(workdir, ".pr-review-*.md")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	fmt.Fprintf(tmp, "# PR #%s metadata\n\n%s\n\n# Diff\n\n```diff\n%s\n```\n", num, meta, diff)
	tmp.Close()

	rel := filepath.Base(tmp.Name())
	prompt := fmt.Sprintf(
		"Act as a senior code reviewer. Read %s which contains a pull request's "+
			"metadata and full diff. Provide a concise, actionable review covering "+
			"correctness, security, and maintainability. Reference file and line where possible.",
		rel,
	)
	if req.Extra != "" {
		prompt += " Additional instructions: " + req.Extra
	}

	task := providers.Task{
		Prompt:  prompt,
		Workdir: workdir,
		Model:   req.Model,
		Files:   []string{rel},
	}
	cmd, err := p.Command(c.Context, task)
	if err != nil {
		return nil, err
	}
	c.Log("system", fmt.Sprintf("running review with %s", p.Name()))
	code, err := executor.Run(c.Context, cmd, c.Log)
	if err != nil {
		return nil, err
	}

	if req.Post {
		// Posting the generated review as a PR comment requires capturing the
		// agent's textual output, which varies per provider. Left as a follow-up.
		c.Log("system", "note: post=true is not yet implemented; review streamed above only")
	}

	return map[string]any{
		"provider":  p.Name(),
		"pr":        req.Number,
		"exit_code": code,
	}, nil
}

// runGH runs a gh command and returns its output as a string (best-effort;
// errors are surfaced as log lines rather than failing the whole review).
func runGH(c *dispatch.Ctx, dir string, args ...string) string {
	cmd := exec.CommandContext(c.Context, "gh", args...)
	cmd.Dir = dir
	out, err := executor.Capture(c.Context, cmd)
	if err != nil {
		c.Log("stderr", fmt.Sprintf("gh %v: %v", args, err))
	}
	return string(out)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
