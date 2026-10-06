// Package config loads daemon configuration from a YAML file with environment
// variable overrides for secrets.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level daemon configuration.
type Config struct {
	// ServerURL is the WebSocket endpoint of the control plane, e.g.
	// wss://control.example.com/agent. Overridable via AIO_SERVER_URL.
	ServerURL string `yaml:"server_url"`

	// Token authenticates the daemon to the control plane. Sent as a
	// Bearer token. Overridable via AIO_TOKEN.
	Token string `yaml:"token"`

	// DaemonID uniquely identifies this daemon. Defaults to the hostname.
	DaemonID string `yaml:"daemon_id"`
	// Name is a human-friendly label for this daemon.
	Name string `yaml:"name"`

	// Workspaces are the absolute root directories the daemon is allowed to
	// read/write and run git/deploy commands in. Paths outside these roots
	// are rejected. Empty means "the user's home directory".
	Workspaces []string `yaml:"workspaces"`

	// HeartbeatInterval controls how often ws pings are sent.
	HeartbeatInterval Duration `yaml:"heartbeat_interval"`
	// ReconnectMin/Max bound the exponential reconnect backoff.
	ReconnectMin Duration `yaml:"reconnect_min"`
	ReconnectMax Duration `yaml:"reconnect_max"`

	// LogLevel is one of debug|info|warn|error.
	LogLevel string `yaml:"log_level"`

	// Providers holds per-provider overrides keyed by provider name
	// (claude, codex, gemini, aider, openhands, cursor, copilot).
	Providers map[string]ProviderSpec `yaml:"providers,omitempty"`

	// Stream configures remote app streaming (WebRTC).
	Stream StreamConfig `yaml:"stream,omitempty"`
}

// StreamConfig configures remote app streaming.
type StreamConfig struct {
	// ICEServers are handed to the phone and used by the daemon's peer
	// connection. Defaults to a public STUN server; add a TURN server for
	// networks where direct connections fail.
	ICEServers []ICEServer `yaml:"ice_servers,omitempty"`
	// MaxSessions caps concurrent streams. Defaults to 2.
	MaxSessions int `yaml:"max_sessions,omitempty"`
}

// ICEServer is a STUN or TURN server.
type ICEServer struct {
	URLs       []string `yaml:"urls" json:"urls"`
	Username   string   `yaml:"username,omitempty" json:"username,omitempty"`
	Credential string   `yaml:"credential,omitempty" json:"credential,omitempty"`
}

// ProviderSpec overrides the defaults for a single provider.
type ProviderSpec struct {
	// Enabled toggles the provider. Nil means enabled.
	Enabled *bool `yaml:"enabled"`
	// Bin overrides the executable name/path (e.g. an absolute path).
	Bin string `yaml:"bin"`
	// Args are extra global arguments prepended to every invocation.
	Args []string `yaml:"args"`
	// Env are extra environment variables for the provider process.
	Env map[string]string `yaml:"env"`
}

// Default returns a Config populated with sensible defaults.
func Default() Config {
	return Config{
		DaemonID:          defaultDaemonID(),
		Name:              hostname(),
		HeartbeatInterval: Duration(30 * time.Second),
		ReconnectMin:      Duration(1 * time.Second),
		ReconnectMax:      Duration(30 * time.Second),
		LogLevel:          "info",
		Stream:            StreamConfig{MaxSessions: 2},
	}
}

// Load reads config from path (if non-empty and present) and applies
// environment overrides. A missing file is not an error when path is "".
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %q: %w", path, err)
		}
	}

	applyEnv(&cfg)
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("AIO_SERVER_URL"); v != "" {
		cfg.ServerURL = v
	}
	if v := os.Getenv("AIO_TOKEN"); v != "" {
		cfg.Token = v
	}
	if v := os.Getenv("AIO_DAEMON_ID"); v != "" {
		cfg.DaemonID = v
	}
	if v := os.Getenv("AIO_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
}

func (cfg *Config) normalize() error {
	if cfg.DaemonID == "" {
		cfg.DaemonID = defaultDaemonID()
	}
	if cfg.Name == "" {
		cfg.Name = cfg.DaemonID
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = Duration(30 * time.Second)
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = Duration(time.Second)
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = cfg.ReconnectMin
	}
	if cfg.Stream.MaxSessions <= 0 {
		cfg.Stream.MaxSessions = 2
	}

	// Expand and absolutize workspaces.
	home, _ := os.UserHomeDir()
	if len(cfg.Workspaces) == 0 && home != "" {
		cfg.Workspaces = []string{home}
	}
	for i, w := range cfg.Workspaces {
		if strings.HasPrefix(w, "~") && home != "" {
			w = filepath.Join(home, strings.TrimPrefix(w, "~"))
		}
		abs, err := filepath.Abs(w)
		if err != nil {
			return fmt.Errorf("workspace %q: %w", w, err)
		}
		cfg.Workspaces[i] = filepath.Clean(abs)
	}
	return nil
}

// DefaultPath returns the per-user config file location used by the desktop
// app, e.g. ~/Library/Application Support/AIO Agent/config.yaml on macOS and
// %AppData%\AIO Agent\config.yaml on Windows.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "AIO Agent", "config.yaml"), nil
}

// HasDaemonID reports whether the config file at path exists and sets
// daemon_id explicitly (rather than relying on a generated one).
func HasDaemonID(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw struct {
		DaemonID string `yaml:"daemon_id"`
	}
	return yaml.Unmarshal(data, &raw) == nil && raw.DaemonID != ""
}

// Save writes cfg to path as YAML, creating parent directories. The file is
// written owner-only because it contains the auth token.
func Save(path string, cfg Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Validate returns an error if required fields are missing.
func (cfg Config) Validate() error {
	if cfg.ServerURL == "" {
		return fmt.Errorf("server_url is required (set it in config or AIO_SERVER_URL)")
	}
	if !strings.HasPrefix(cfg.ServerURL, "ws://") && !strings.HasPrefix(cfg.ServerURL, "wss://") {
		return fmt.Errorf("server_url must be a ws:// or wss:// URL, got %q", cfg.ServerURL)
	}
	return nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "aio-daemon"
	}
	return h
}

func defaultDaemonID() string {
	h := hostname()
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%s-%s", h, hex.EncodeToString(b[:]))
	}
	return h
}

// Duration is a time.Duration that unmarshals from a YAML string ("30s") or a
// plain number of seconds.
type Duration time.Duration

// D returns the underlying time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// MarshalYAML implements yaml.Marshaler, writing durations as "30s".
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err == nil {
		parsed, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", s, err)
		}
		*d = Duration(parsed)
		return nil
	}
	var n int64
	if err := value.Decode(&n); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\" or a number of seconds")
	}
	*d = Duration(time.Duration(n) * time.Second)
	return nil
}