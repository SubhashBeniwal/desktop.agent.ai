//go:build darwin && cgo

package remote

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=12.3
#cgo LDFLAGS: -framework ScreenCaptureKit -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework CoreGraphics -framework ApplicationServices -framework AppKit -framework Foundation
#include <stdlib.h>
#include "capture_darwin.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"
)

var plat platform = darwin{}

type darwin struct{}

func (darwin) supported() bool { return true }

func (darwin) permissions() Permissions {
	return Permissions{
		ScreenRecording: C.aio_preflight_screen() == 1,
		Accessibility:   C.aio_preflight_accessibility() == 1,
	}
}

func requestScreen()        { C.aio_request_screen() }
func requestAccessibility() { C.aio_request_accessibility() }

func (darwin) listWindows() ([]Window, []Display, error) {
	var cerr *C.char
	js := C.aio_list_json(&cerr)
	if js == nil {
		return nil, nil, errors.New(takeString(cerr))
	}
	var out struct {
		Windows  []Window  `json:"windows"`
		Displays []Display `json:"displays"`
	}
	err := json.Unmarshal([]byte(takeString(js)), &out)
	return out.Windows, out.Displays, err
}

func (darwin) bounds(t Target) (Bounds, int, bool) {
	var x, y, w, h C.double
	if t.WindowID != 0 {
		var pid C.int
		if C.aio_window_bounds(C.uint32_t(t.WindowID), &x, &y, &w, &h, &pid) == 0 {
			return Bounds{}, 0, false
		}
		return Bounds{float64(x), float64(y), float64(w), float64(h)}, int(pid), true
	}
	if C.aio_display_bounds(C.uint32_t(t.DisplayID), &x, &y, &w, &h) == 0 {
		return Bounds{}, 0, false
	}
	return Bounds{float64(x), float64(y), float64(w), float64(h)}, 0, true
}

func (darwin) backingScale(x, y float64) float64 {
	return float64(C.aio_backing_scale(C.double(x), C.double(y)))
}

// Native frame callbacks carry a handle; sinks are looked up here.
var (
	sinksMu sync.Mutex
	sinks   = map[uintptr]frameSink{}
	nextID  uintptr
)

type darwinCapture struct {
	handle uintptr
	ptr    unsafe.Pointer
	once   sync.Once
}

func (darwin) startCapture(t Target, w, h, fps, kbps int, sink frameSink) (capture, error) {
	sinksMu.Lock()
	nextID++
	handle := nextID
	sinks[handle] = sink
	sinksMu.Unlock()

	var cerr *C.char
	ptr := C.aio_capture_start(C.uintptr_t(handle), C.uint32_t(t.WindowID), C.uint32_t(t.DisplayID),
		C.int(w), C.int(h), C.int(fps), C.int(kbps), &cerr)
	if ptr == nil {
		forget(handle)
		return nil, errors.New(takeString(cerr))
	}
	return &darwinCapture{handle: handle, ptr: ptr}, nil
}

func (c *darwinCapture) keyframe() { C.aio_capture_keyframe(c.ptr) }

func (c *darwinCapture) resize(w, h int) error {
	var cerr *C.char
	if C.aio_capture_resize(c.ptr, C.int(w), C.int(h), &cerr) == 0 {
		return errors.New(takeString(cerr))
	}
	return nil
}

func (c *darwinCapture) stop() {
	c.once.Do(func() {
		forget(c.handle)
		C.aio_capture_stop(c.ptr)
	})
}

func forget(handle uintptr) {
	sinksMu.Lock()
	delete(sinks, handle)
	sinksMu.Unlock()
}

func sinkFor(handle C.uintptr_t) frameSink {
	sinksMu.Lock()
	defer sinksMu.Unlock()
	return sinks[uintptr(handle)]
}

//export aioGoFrame
func aioGoFrame(handle C.uintptr_t, data unsafe.Pointer, n C.int, ptsUS C.int64_t, key C.int) {
	if s := sinkFor(handle); s != nil {
		s.frame(C.GoBytes(data, n), time.Duration(ptsUS)*time.Microsecond, key == 1)
	}
}

//export aioGoStopped
func aioGoStopped(handle C.uintptr_t, reason *C.char) {
	msg := takeString(reason)
	if s := sinkFor(handle); s != nil {
		s.stopped(msg)
	}
}

func (darwin) mouse(kind, button int, x, y float64, clicks int, dragging bool) {
	C.aio_mouse(C.int(kind), C.int(button), C.double(x), C.double(y), C.int(clicks), cbool(dragging))
}

func (darwin) scroll(x, y float64, dx, dy int) {
	C.aio_scroll(C.double(x), C.double(y), C.int(dx), C.int(dy))
}

// key posts keycode with flags' modifiers pressed as real keys around it:
// system shortcuts (⌘Tab, ⌃← for Spaces, ⌃↑ for Mission Control) ignore a
// key event that only carries modifier flags.
func (darwin) key(keycode uint16, down bool, flags uint64) {
	post := func(kc uint16, down bool, flags uint64) {
		C.aio_key(C.uint16_t(kc), cbool(down), C.uint64_t(flags))
	}
	if isModifierKeycode(keycode) {
		post(keycode, down, flags)
		return
	}
	if down {
		var held uint64
		for _, m := range modifierKeys {
			if flags&m.flag != 0 {
				held |= m.flag
				post(m.keycode, true, held)
			}
		}
		post(keycode, true, flags|keyFlags(keycode))
		return
	}
	post(keycode, false, flags|keyFlags(keycode))
	held := flags
	for i := len(modifierKeys) - 1; i >= 0; i-- {
		if m := modifierKeys[i]; flags&m.flag != 0 {
			held &^= m.flag
			post(m.keycode, false, held)
		}
	}
}

// modifierKeys are pressed in this order and released in reverse.
var modifierKeys = []struct {
	flag    uint64
	keycode uint16
}{
	{flagControl, 0x3B}, {flagAlt, 0x3A}, {flagShift, 0x38}, {flagCommand, 0x37},
}

func isModifierKeycode(kc uint16) bool {
	return kc >= 0x36 && kc <= 0x3F
}

// keyFlags are the flags a physical keyboard adds to kc. macOS hotkeys on
// arrow keys (⌃← Spaces, ⌃↑ Mission Control) only match with them set.
func keyFlags(kc uint16) uint64 {
	switch kc {
	case 0x7B, 0x7C, 0x7D, 0x7E: // arrows
		return flagFn | flagNumPad
	case 0x72, 0x73, 0x74, 0x75, 0x77, 0x79, // help, home, page up, delete, end, page down
		0x7A, 0x78, 0x63, 0x76, 0x60, 0x61, 0x62, 0x64, 0x65, 0x6D, 0x67, 0x6F, // F1–F12
		0x69, 0x6B, 0x71, 0x6A, 0x40, 0x4F, 0x50, 0x5A: // F13–F20
		return flagFn
	}
	return 0
}

func (darwin) text(s string) {
	u := utf16.Encode([]rune(s))
	if len(u) == 0 {
		return
	}
	C.aio_text((*C.uint16_t)(unsafe.Pointer(&u[0])), C.int(len(u)))
}

func (darwin) raise(pid int, b Bounds) {
	C.aio_raise(C.int(pid), C.double(b.X), C.double(b.Y), C.double(b.W), C.double(b.H))
}

func (darwin) keycode(code string) (uint16, bool) {
	k, ok := macKeycodes[code]
	return k, ok
}

// CGEventFlags.
const (
	flagShift   = 1 << 17
	flagControl = 1 << 18
	flagAlt     = 1 << 19
	flagCommand = 1 << 20
	flagNumPad  = 1 << 21
	flagFn      = 1 << 23
)

func (darwin) modFlags(mods []string) uint64 {
	var f uint64
	for _, m := range mods {
		switch m {
		case "shift":
			f |= flagShift
		case "ctrl":
			f |= flagControl
		case "alt":
			f |= flagAlt
		case "meta":
			f |= flagCommand
		}
	}
	return f
}

func cbool(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// takeString copies and frees a malloc'd C string.
func takeString(s *C.char) string {
	if s == nil {
		return ""
	}
	defer C.free(unsafe.Pointer(s))
	return C.GoString(s)
}
