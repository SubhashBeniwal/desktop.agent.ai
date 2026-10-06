// Package remote streams desktop windows to the control app over WebRTC and
// injects the app's touch/keyboard input back into them ("Remote apps").
//
// Signaling rides on ordinary commands (stream.start carries the SDP offer and
// its result the answer); media and input then flow peer-to-peer. The native
// capture/encode/input layer is per-OS behind the platform interface; only
// macOS (with cgo) is implemented so far.
package remote

import "time"

// Window is a streamable top-level window.
type Window struct {
	ID       uint32 `json:"id"`
	App      string `json:"app"`
	BundleID string `json:"bundle_id"`
	PID      int    `json:"pid"`
	Title    string `json:"title"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	OnScreen bool   `json:"on_screen"`
}

// Display is a streamable screen.
type Display struct {
	ID     uint32 `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Target selects what to stream: a window or a whole display.
type Target struct {
	WindowID  uint32 `json:"window_id,omitempty"`
	DisplayID uint32 `json:"display_id,omitempty"`
}

// Bounds is a rectangle in global screen points, top-left origin.
type Bounds struct{ X, Y, W, H float64 }

// Permissions reports the OS permissions streaming needs.
type Permissions struct {
	ScreenRecording bool `json:"screen_recording"`
	Accessibility   bool `json:"accessibility"`
}

// Mouse buttons.
const (
	ButtonLeft = iota
	ButtonRight
	ButtonMiddle
)

// platform is the native layer.
type platform interface {
	supported() bool
	permissions() Permissions
	listWindows() ([]Window, []Display, error)
	// bounds returns the target's current geometry and owning pid (0 for
	// displays); ok is false if it is gone or not on screen.
	bounds(t Target) (b Bounds, pid int, ok bool)
	backingScale(x, y float64) float64
	// startCapture begins capturing t, encoding to H.264 at w×h pixels and
	// passing each Annex-B access unit to sink.
	startCapture(t Target, w, h, fps, kbps int, sink frameSink) (capture, error)

	mouse(kind, button int, x, y float64, clicks int, dragging bool)
	scroll(x, y float64, dx, dy int)
	key(keycode uint16, down bool, flags uint64)
	text(s string)
	raise(pid int, b Bounds)
	// keycode maps a W3C KeyboardEvent.code to a native key code, and
	// modFlags maps modifier names to native event flags.
	keycode(code string) (uint16, bool)
	modFlags(mods []string) uint64
}

// frameSink receives encoded frames, or a non-nil stop reason when the
// capture ends on its own (e.g. the window closed).
type frameSink interface {
	frame(data []byte, pts time.Duration, keyframe bool)
	stopped(reason string)
}

type capture interface {
	keyframe()
	resize(w, h int) error
	stop()
}

// Mouse event kinds.
const (
	mouseMove = iota
	mouseDown
	mouseUp
)
