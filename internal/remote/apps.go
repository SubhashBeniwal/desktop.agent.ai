package remote

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/aioagent/daemon/internal/dispatch"
)

// App is a launchable application bundle.
type App struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func appDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{
		"/Applications", "/Applications/Utilities", "/System/Applications",
		filepath.Join(home, "Applications"),
		// JetBrains Toolbox installs IDEs here.
		filepath.Join(home, "Applications", "JetBrains Toolbox"),
	}
}

func (m *Manager) appList(*dispatch.Ctx) (any, error) {
	seen := map[string]bool{}
	apps := []App{}
	for _, dir := range appDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name, ok := strings.CutSuffix(e.Name(), ".app")
			if !ok || seen[name] {
				continue
			}
			seen[name] = true
			apps = append(apps, App{Name: name, Path: filepath.Join(dir, e.Name())})
		}
	}
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return map[string]any{"apps": apps}, nil
}

func (m *Manager) appLaunch(c *dispatch.Ctx) (any, error) {
	var req struct {
		Name string   `json:"name"`
		Path string   `json:"path"`
		Args []string `json:"args"`
	}
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	app, label := req.Path, req.Path
	switch {
	case req.Path != "":
		if !strings.HasSuffix(req.Path, ".app") || !filepath.IsAbs(req.Path) {
			return nil, errors.New("path must be an absolute .app bundle path")
		}
		if _, err := os.Stat(req.Path); err != nil {
			return nil, errors.New("E_NOT_FOUND: " + req.Path)
		}
		label = strings.TrimSuffix(filepath.Base(req.Path), ".app")
	case req.Name != "":
		app, label = req.Name, req.Name
	default:
		return nil, errors.New("set name or path")
	}

	// Files/folders to open must be inside the workspaces.
	argv := []string{"-a", app}
	for _, a := range req.Args {
		p, err := m.fs.Resolve(a)
		if err != nil {
			return nil, err
		}
		argv = append(argv, p)
	}
	out, err := exec.CommandContext(c.Context, "open", argv...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "Unable to find application") {
			return nil, errors.New("E_NOT_FOUND: no application named " + label)
		}
		return nil, errors.New("open failed: " + msg)
	}
	return map[string]string{"launched": label}, nil
}
