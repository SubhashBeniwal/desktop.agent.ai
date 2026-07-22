package providers

// defaultProviders returns the built-in adapters for the supported AI coding
// agents. Each maps a Task to a best-effort non-interactive CLI invocation.
//
// These CLIs evolve quickly, so every binary name and its flags are
// overridable from config (providers.<name>.bin / .args). The goal here is a
// sensible default that runs headlessly and streams output.
func defaultProviders() []cliProvider {
	return []cliProvider{
		// Anthropic Claude Code — https://docs.claude.com/claude-code
		// `claude -p "<prompt>"` runs in non-interactive "print" mode.
		{
			name: "claude",
			bin:  "claude",
			build: func(t Task) []string {
				args := []string{"-p", t.Prompt}
				if t.Model != "" {
					args = append(args, "--model", t.Model)
				}
				if t.AutoApprove {
					args = append(args, "--permission-mode", "acceptEdits")
				}
				return args
			},
		},

		// OpenAI Codex CLI — `codex exec "<prompt>"` runs non-interactively.
		{
			name: "codex",
			bin:  "codex",
			build: func(t Task) []string {
				args := []string{"exec"}
				if t.Model != "" {
					args = append(args, "-m", t.Model)
				}
				if t.AutoApprove {
					args = append(args, "--full-auto")
				}
				return append(args, t.Prompt)
			},
		},

		// Google Gemini CLI — `gemini -p "<prompt>"` prints a single response.
		{
			name: "gemini",
			bin:  "gemini",
			build: func(t Task) []string {
				args := []string{"-p", t.Prompt}
				if t.Model != "" {
					args = append(args, "-m", t.Model)
				}
				if t.AutoApprove {
					args = append(args, "-y") // --yolo
				}
				return args
			},
		},

		// Aider — `aider --message "<prompt>" [files...]` applies one edit pass.
		{
			name: "aider",
			bin:  "aider",
			build: func(t Task) []string {
				args := []string{"--message", t.Prompt}
				if t.AutoApprove {
					args = append(args, "--yes-always")
				}
				if t.Model != "" {
					args = append(args, "--model", t.Model)
				}
				return append(args, t.Files...)
			},
		},

		// OpenHands — headless task run. Binary/flags vary by install; override
		// via config if needed (e.g. bin: "python", args: ["-m", "openhands..."]).
		{
			name: "openhands",
			bin:  "openhands",
			build: func(t Task) []string {
				return []string{"--task", t.Prompt}
			},
		},

		// Cursor Agent CLI — `cursor-agent -p "<prompt>"` prints a response.
		{
			name: "cursor",
			bin:  "cursor-agent",
			build: func(t Task) []string {
				args := []string{"-p", t.Prompt}
				if t.Model != "" {
					args = append(args, "-m", t.Model)
				}
				return args
			},
		},

		// GitHub Copilot CLI — `copilot -p "<prompt>"`.
		{
			name: "copilot",
			bin:  "copilot",
			build: func(t Task) []string {
				args := []string{"-p", t.Prompt}
				if t.AutoApprove {
					args = append(args, "--allow-all-tools")
				}
				return args
			},
		},
	}
}
