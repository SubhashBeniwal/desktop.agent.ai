// Package providers defines the interface for AI coding agents and a registry
// of adapters that shell out to their respective CLIs.
package providers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
)

// Task describes a single agent invocation.
type Task struct {
	Prompt      string            // the instruction/task for the agent
	Workdir     string            // absolute working directory (validated by caller)
	Model       string            // optional model override
	Files       []string          // optional files to focus on (provider-dependent)
	ExtraArgs   []string          // extra CLI args appended verbatim
	Env         map[string]string // extra environment variables
	AutoApprove bool              // run without interactive approval prompts
}

// Provider is an AI coding agent adapter.
type Provider interface {
	// Name is the stable identifier (e.g. "claude", "codex").
	Name() string
	// Available reports whether the provider's binary is installed.
	Available() bool
	// Command builds the exec.Cmd for the task. It does not start it.
	Command(ctx context.Context, task Task) (*exec.Cmd, error)
}

// Registry holds the configured providers.
type Registry struct {
	providers map[string]Provider
}

// Get returns the provider by name.
func (r *Registry) Get(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names returns all configured provider names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// AvailableNames returns the names of providers whose binary is installed.
func (r *Registry) AvailableNames() []string {
	var out []string
	for n, p := range r.providers {
		if p.Available() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// Override customizes a provider at registry-build time.
type Override struct {
	Enabled *bool
	Bin     string
	Args    []string
	Env     map[string]string
}

// BuildRegistry constructs the registry from the built-in defaults, applying
// per-provider overrides (from config).
func BuildRegistry(overrides map[string]Override) *Registry {
	r := &Registry{providers: make(map[string]Provider)}
	for _, def := range defaultProviders() {
		if ov, ok := overrides[def.name]; ok {
			if ov.Enabled != nil && !*ov.Enabled {
				continue
			}
			if ov.Bin != "" {
				def.bin = ov.Bin
			}
			if len(ov.Args) > 0 {
				def.baseArgs = append(append([]string{}, ov.Args...), def.baseArgs...)
			}
			if len(ov.Env) > 0 {
				def.env = mergeMap(def.env, ov.Env)
			}
		}
		p := def // capture
		r.providers[p.name] = &p
	}
	return r
}

// cliProvider is a generic adapter that turns a Task into CLI arguments.
type cliProvider struct {
	name     string
	bin      string
	baseArgs []string          // global args placed before the task-specific args
	env      map[string]string // provider-scoped environment
	build    func(t Task) []string
}

func (p *cliProvider) Name() string { return p.name }

func (p *cliProvider) Available() bool {
	_, err := exec.LookPath(p.bin)
	return err == nil
}

func (p *cliProvider) Command(ctx context.Context, t Task) (*exec.Cmd, error) {
	bin, err := exec.LookPath(p.bin)
	if err != nil {
		return nil, fmt.Errorf("provider %q: binary %q not found in PATH", p.name, p.bin)
	}
	args := append([]string{}, p.baseArgs...)
	if p.build != nil {
		args = append(args, p.build(t)...)
	}
	args = append(args, t.ExtraArgs...)

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = t.Workdir
	cmd.Env = mergeEnv(os.Environ(), p.env, t.Env)
	return cmd, nil
}

func mergeEnv(base []string, maps ...map[string]string) []string {
	env := append([]string{}, base...)
	for _, m := range maps {
		for k, v := range m {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func mergeMap(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}