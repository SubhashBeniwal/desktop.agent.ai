package remote

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/files"
	"github.com/pion/webrtc/v4"
)

// Defaults for stream.start options.
const (
	defaultFPS     = 30
	defaultMaxSize = 1600
	defaultKbps    = 4000
)

var defaultICE = []config.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}

// Supported reports whether this build can stream on this OS.
func Supported() bool { return plat.supported() }

// CurrentPermissions reports the OS permissions streaming needs.
func CurrentPermissions() Permissions { return plat.permissions() }

// RequestPermission shows the OS prompt for "screen_recording" or
// "accessibility". macOS only prompts once per app; afterwards the user must
// use System Settings.
func RequestPermission(kind string) {
	switch kind {
	case "screen_recording":
		requestScreen()
	case "accessibility":
		requestAccessibility()
	}
}

// Manager owns the live streaming sessions of one daemon run.
type Manager struct {
	cfg config.StreamConfig
	fs  *files.Sandbox
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	wg       sync.WaitGroup // session background goroutines
}

// New creates a Manager.
func New(cfg config.StreamConfig, fs *files.Sandbox, log *slog.Logger) *Manager {
	if len(cfg.ICEServers) == 0 {
		cfg.ICEServers = defaultICE
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = 2
	}
	return &Manager{cfg: cfg, fs: fs, log: log, sessions: make(map[string]*session)}
}

// Register adds the remote-app actions. Streaming actions are only registered
// when the platform supports them, so `register.actions` advertises the
// feature accurately; stream.config is always present.
func (m *Manager) Register(d *dispatch.Dispatcher) {
	d.Register("stream.config", m.streamConfig)
	if runtime.GOOS == "darwin" {
		d.Register("app.list", m.appList)
		d.Register("app.launch", m.appLaunch)
	}
	if plat.supported() {
		d.Register("screen.windows", m.screenWindows)
		d.Register("stream.start", m.streamStart)
		d.Register("stream.stop", m.streamStop)
	}
}

// Close ends every session and waits for their goroutines to exit.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		if s != nil { // nil = slot reserved by a stream.start in progress
			all = append(all, s)
		}
	}
	m.mu.Unlock()
	for _, s := range all {
		s.end("daemon stopping")
	}
	m.wg.Wait()
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *Manager) streamConfig(*dispatch.Ctx) (any, error) {
	return map[string]any{
		"supported":   plat.supported(),
		"ice_servers": m.cfg.ICEServers,
		"permissions": plat.permissions(),
		"defaults": map[string]int{
			"max_fps": defaultFPS, "max_size": defaultMaxSize, "bitrate_kbps": defaultKbps,
		},
		"max_sessions": m.cfg.MaxSessions,
	}, nil
}

func (m *Manager) screenWindows(*dispatch.Ctx) (any, error) {
	if !plat.permissions().ScreenRecording {
		return nil, errScreenPermission
	}
	wins, displays, err := plat.listWindows()
	if err != nil {
		return nil, err
	}
	if wins == nil {
		wins = []Window{}
	}
	return map[string]any{"windows": wins, "displays": displays}, nil
}

type startReq struct {
	Target  Target                    `json:"target"`
	Offer   webrtc.SessionDescription `json:"offer"`
	MaxFPS  int                       `json:"max_fps"`
	MaxSize int                       `json:"max_size"`
	Kbps    int                       `json:"bitrate_kbps"`
	Focus   *bool                     `json:"focus"`
}

var errScreenPermission = errors.New("E_PERMISSION_SCREEN: Screen Recording permission not granted; " +
	"approve AIO Agent in System Settings → Privacy & Security → Screen Recording, then restart it")

func (m *Manager) streamStart(c *dispatch.Ctx) (any, error) {
	var req startReq
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, fmt.Errorf("E_BAD_OFFER: invalid payload: %w", err)
	}
	if (req.Target.WindowID == 0) == (req.Target.DisplayID == 0) {
		return nil, errors.New("target must set exactly one of window_id or display_id")
	}
	if req.Offer.Type != webrtc.SDPTypeOffer || req.Offer.SDP == "" {
		return nil, errors.New("E_BAD_OFFER: offer must be {\"type\":\"offer\",\"sdp\":\"...\"}")
	}
	if !strings.Contains(strings.ToUpper(req.Offer.SDP), "H264/90000") {
		return nil, errors.New("E_BAD_OFFER: offer has no H.264 codec; the daemon streams H.264 only")
	}
	if !plat.permissions().ScreenRecording {
		return nil, errScreenPermission
	}
	fps := clamp(orDefault(req.MaxFPS, defaultFPS), 1, 60)
	maxSize := clamp(orDefault(req.MaxSize, defaultMaxSize), 160, 4096)
	kbps := clamp(orDefault(req.Kbps, defaultKbps), 200, 50000)

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("daemon stopping")
	}
	if len(m.sessions) >= m.cfg.MaxSessions {
		m.mu.Unlock()
		return nil, fmt.Errorf("E_LIMIT: already streaming %d session(s)", m.cfg.MaxSessions)
	}
	// Reserve the slot while we set up.
	id := "s-" + randHex(4)
	m.sessions[id] = nil
	m.mu.Unlock()

	s, err := m.newSession(id, req, fps, maxSize, kbps)
	if err != nil {
		m.remove(id)
		return nil, err
	}
	m.mu.Lock()
	if m.closed { // daemon stopped while we were negotiating
		m.mu.Unlock()
		s.end("daemon stopping")
		return nil, errors.New("daemon stopping")
	}
	m.sessions[id] = s
	m.mu.Unlock()

	m.log.Info("stream started", "session", id, "target", req.Target, "size", fmt.Sprintf("%dx%d", s.w, s.h))
	ld := s.pc.LocalDescription()
	return map[string]any{
		"session_id": id,
		"answer":     map[string]string{"type": ld.Type.String(), "sdp": ld.SDP},
		"width":      s.w,
		"height":     s.h,
	}, nil
}

func (m *Manager) streamStop(c *dispatch.Ctx) (any, error) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := dispatch.Decode(c.Command.Payload, &req); err != nil {
		return nil, err
	}
	m.mu.Lock()
	s := m.sessions[req.SessionID]
	m.mu.Unlock()
	if s != nil {
		s.end("stopped by control app")
	}
	return map[string]string{"stopped": req.SessionID}, nil
}

// streamSize scales a window of b points (at scale px/pt) so its long edge
// is at most maxSize pixels, never upscaling. H.264 4:2:0 needs even sizes.
func streamSize(b Bounds, scale float64, maxSize int) (int, int) {
	w, h := b.W*scale, b.H*scale
	if long := max(w, h); long > float64(maxSize) {
		f := float64(maxSize) / long
		w, h = w*f, h*f
	}
	return max(2, int(w)&^1), max(2, int(h)&^1)
}

func orDefault(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func clamp(v, lo, hi int) int { return min(hi, max(lo, v)) }

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// sleepOrDone waits d or until done is closed; it reports whether d elapsed.
func sleepOrDone(d time.Duration, done <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-done:
		return false
	}
}
