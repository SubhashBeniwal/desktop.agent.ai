package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/daemon"
	"github.com/aioagent/daemon/internal/logbuf"
	"github.com/aioagent/daemon/internal/providers"
	"github.com/aioagent/daemon/internal/remote"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Connection states reported to the UI.
const (
	StateUnconfigured = "unconfigured"
	StateStopped      = "stopped"
	StateConnecting   = "connecting"
	StateConnected    = "connected"
)

// Events emitted to the frontend.
const (
	EventStatus   = "aio:status"
	EventLog      = "aio:log"
	EventActivity = "aio:activity"
)

const maxActivity = 100

// Status is a snapshot of the daemon for the UI and tray.
type Status struct {
	State     string `json:"state"`
	LastError string `json:"lastError,omitempty"`
	Since     int64  `json:"since"` // unix millis of the last state change
	ServerURL string `json:"serverUrl"`
	DaemonID  string `json:"daemonId"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Running   int    `json:"running"` // commands in flight
}

// Activity is one command the daemon handled.
type Activity struct {
	ID       string `json:"id"`
	Action   string `json:"action"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished,omitempty"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// Settings is the editable subset of config.Config shown in the UI.
type Settings struct {
	ServerURL  string   `json:"serverUrl"`
	Token      string   `json:"token"`
	Name       string   `json:"name"`
	DaemonID   string   `json:"daemonId"`
	Workspaces []string `json:"workspaces"`
	LogLevel   string   `json:"logLevel"`
	ConfigPath string   `json:"configPath"`
}

// ProviderInfo describes an AI agent provider and whether its CLI is installed.
type ProviderInfo struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

// App owns the embedded daemon's lifecycle and is bound to the frontend as a
// Wails service; its exported methods are callable from JavaScript.
type App struct {
	version  string
	cfgPath  string
	logs     *logbuf.Buffer
	logLevel *slog.LevelVar
	logger   *slog.Logger

	// onStatus is called (without the lock held) after every status change;
	// main uses it to refresh the tray.
	onStatus func(Status)

	mu         sync.Mutex
	app        *application.App
	cfg        config.Config
	configured bool
	cancel     context.CancelFunc
	done       chan struct{}
	status     Status
	activity   []Activity
}

func newApp(version, cfgPath string) (*App, error) {
	a := &App{
		version:  version,
		cfgPath:  cfgPath,
		logs:     logbuf.New(2000),
		logLevel: new(slog.LevelVar),
	}
	a.logger = slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, a.logs),
		&slog.HandlerOptions{Level: a.logLevel}))

	var err error
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		a.cfg, err = config.Load(cfgPath)
	} else {
		a.cfg, err = config.Load("")
	}
	if err != nil {
		return nil, err
	}
	// The daemon ID is generated when absent; persist it so the phone sees the
	// same machine across restarts.
	if !config.HasDaemonID(cfgPath) {
		if err := config.Save(cfgPath, a.cfg); err != nil {
			return nil, fmt.Errorf("save config: %w", err)
		}
	}
	a.configured = a.cfg.Validate() == nil
	a.applyLogLevel()

	state := StateStopped
	if !a.configured {
		state = StateUnconfigured
	}
	a.status = Status{State: state, Since: now(), Version: version}
	a.refreshIdentity()
	return a, nil
}

// ServiceStartup is called by Wails when the app starts.
func (a *App) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	a.mu.Lock()
	a.app = application.Get()
	a.mu.Unlock()
	a.logs.OnLine(func(line string) { a.emit(EventLog, line) })
	return nil
}

// ServiceShutdown stops the daemon when the app quits.
func (a *App) ServiceShutdown() error {
	a.Disconnect()
	return nil
}

// Configured reports whether a valid server URL has been set.
func (a *App) Configured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.configured
}

// Status returns the current daemon status.
func (a *App) Status() Status {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.status
}

// Settings returns the editable configuration.
func (a *App) Settings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Settings{
		ServerURL:  a.cfg.ServerURL,
		Token:      a.cfg.Token,
		Name:       a.cfg.Name,
		DaemonID:   a.cfg.DaemonID,
		Workspaces: slices.Clone(a.cfg.Workspaces),
		LogLevel:   a.cfg.LogLevel,
		ConfigPath: a.cfgPath,
	}
}

// SaveSettings validates and persists settings, then (re)connects.
func (a *App) SaveSettings(s Settings) error {
	a.mu.Lock()
	next := a.cfg
	next.ServerURL = strings.TrimSpace(s.ServerURL)
	next.Token = strings.TrimSpace(s.Token)
	next.Name = strings.TrimSpace(s.Name)
	next.LogLevel = s.LogLevel
	next.Workspaces = nil
	for _, w := range s.Workspaces {
		if w = strings.TrimSpace(w); w != "" {
			next.Workspaces = append(next.Workspaces, w)
		}
	}
	a.mu.Unlock()

	if err := next.Validate(); err != nil {
		return err
	}
	if err := config.Save(a.cfgPath, next); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	// Reload so defaults/normalization (workspace expansion etc.) apply.
	loaded, err := config.Load(a.cfgPath)
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.cfg = loaded
	a.configured = true
	a.mu.Unlock()
	a.applyLogLevel()
	a.refreshIdentity()
	a.logger.Info("settings saved", "path", a.cfgPath)

	a.Disconnect()
	return a.Connect()
}

// Connect starts the daemon if it is not already running.
func (a *App) Connect() error {
	a.mu.Lock()
	if !a.configured {
		a.mu.Unlock()
		return errors.New("set a server URL in Settings first")
	}
	if a.cancel != nil {
		a.mu.Unlock()
		return nil
	}
	cfg := a.cfg
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	a.cancel, a.done = cancel, done
	a.mu.Unlock()

	a.setState(StateConnecting, "")

	d := daemon.New(cfg, a.logger, a.version)
	d.Hooks = daemon.Hooks{
		OnState: func(connected bool, err error) {
			if connected {
				a.setState(StateConnected, "")
			} else if ctx.Err() == nil {
				a.setState(StateConnecting, errString(err))
			}
		},
		OnCommand: a.commandStarted,
		OnResult:  a.commandFinished,
	}

	go func() {
		defer close(done)
		err := d.Run(ctx)
		if ctx.Err() == nil && err != nil {
			a.logger.Error("daemon exited", "err", err)
			a.mu.Lock()
			a.cancel, a.done = nil, nil
			a.mu.Unlock()
			a.setState(StateStopped, err.Error())
		}
	}()
	return nil
}

// Disconnect stops the daemon and waits for it to exit.
func (a *App) Disconnect() {
	a.mu.Lock()
	cancel, done := a.cancel, a.done
	a.cancel, a.done = nil, nil
	a.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.logger.Warn("daemon did not stop within 5s")
	}
	a.setState(StateStopped, "")
}

// Logs returns recent log lines, oldest first.
func (a *App) Logs() []string {
	return a.logs.Lines()
}

// Activity returns recent commands, newest first.
func (a *App) Activity() []Activity {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := slices.Clone(a.activity)
	slices.Reverse(out)
	return out
}

// Providers lists the known AI agent providers and whether each is installed.
func (a *App) Providers() []ProviderInfo {
	a.mu.Lock()
	cfg := a.cfg
	a.mu.Unlock()
	reg := providers.BuildRegistry(daemon.ProviderOverrides(cfg))
	avail := reg.AvailableNames()
	var out []ProviderInfo
	for _, n := range reg.Names() {
		out = append(out, ProviderInfo{Name: n, Available: slices.Contains(avail, n)})
	}
	return out
}

// ChooseFolder opens a native folder picker and returns the selection ("" if
// cancelled).
func (a *App) ChooseFolder() (string, error) {
	return a.app.Dialog.OpenFile().
		SetTitle("Add workspace").
		CanChooseDirectories(true).
		CanChooseFiles(false).
		PromptForSingleSelection()
}

// RemoteStatus describes remote-app streaming support and permissions.
type RemoteStatus struct {
	Supported bool `json:"supported"`
	remote.Permissions
}

// RemoteStatus reports whether streaming works here and which OS permissions
// are granted.
func (a *App) RemoteStatus() RemoteStatus {
	return RemoteStatus{Supported: remote.Supported(), Permissions: remote.CurrentPermissions()}
}

// GrantRemotePermission prompts for "screen_recording" or "accessibility" and
// opens the matching System Settings pane (macOS only prompts the first time).
func (a *App) GrantRemotePermission(kind string) {
	remote.RequestPermission(kind)
	pane := map[string]string{
		"screen_recording": "Privacy_ScreenCapture",
		"accessibility":    "Privacy_Accessibility",
	}[kind]
	if pane != "" && runtime.GOOS == "darwin" {
		_ = exec.Command("open", "x-apple.systempreferences:com.apple.preference.security?"+pane).Start()
	}
}

// LaunchAtLogin reports whether the app starts automatically at login.
func (a *App) LaunchAtLogin() bool {
	on, err := a.app.Autostart.IsEnabled()
	return err == nil && on
}

// SetLaunchAtLogin enables or disables starting the app at login.
func (a *App) SetLaunchAtLogin(on bool) error {
	if on {
		return a.app.Autostart.Enable()
	}
	return a.app.Autostart.Disable()
}

func (a *App) commandStarted(id, action string) {
	a.mu.Lock()
	a.activity = append(a.activity, Activity{ID: id, Action: action, Started: now()})
	if over := len(a.activity) - maxActivity; over > 0 {
		a.activity = slices.Delete(a.activity, 0, over)
	}
	a.status.Running++
	a.mu.Unlock()
	a.emit(EventActivity, nil)
	a.publishStatus()
}

func (a *App) commandFinished(id, _ string, ok bool, errMsg string) {
	a.mu.Lock()
	for i := len(a.activity) - 1; i >= 0; i-- {
		if a.activity[i].ID == id {
			a.activity[i].Finished, a.activity[i].OK, a.activity[i].Error = now(), ok, errMsg
			break
		}
	}
	if a.status.Running > 0 {
		a.status.Running--
	}
	a.mu.Unlock()
	a.emit(EventActivity, nil)
	a.publishStatus()
}

func (a *App) setState(state, lastErr string) {
	a.mu.Lock()
	if a.status.State != state {
		a.status.Since = now()
	}
	a.status.State, a.status.LastError = state, lastErr
	if state != StateConnected {
		a.status.Running = 0
	}
	a.mu.Unlock()
	a.publishStatus()
}

func (a *App) refreshIdentity() {
	a.mu.Lock()
	a.status.ServerURL, a.status.DaemonID, a.status.Name = a.cfg.ServerURL, a.cfg.DaemonID, a.cfg.Name
	if a.configured && a.status.State == StateUnconfigured {
		a.status.State = StateStopped
	}
	a.mu.Unlock()
	a.publishStatus()
}

func (a *App) publishStatus() {
	st := a.Status()
	if a.onStatus != nil {
		a.onStatus(st)
	}
	a.emit(EventStatus, st)
}

func (a *App) emit(name string, data any) {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app != nil {
		app.Event.Emit(name, data)
	}
}

func (a *App) applyLogLevel() {
	a.mu.Lock()
	level := a.cfg.LogLevel
	a.mu.Unlock()
	switch level {
	case "debug":
		a.logLevel.Set(slog.LevelDebug)
	case "warn":
		a.logLevel.Set(slog.LevelWarn)
	case "error":
		a.logLevel.Set(slog.LevelError)
	default:
		a.logLevel.Set(slog.LevelInfo)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func now() int64 { return time.Now().UnixMilli() }
