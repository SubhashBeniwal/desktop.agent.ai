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

func (darwin) key(keycode uint16, down bool, flags uint64) {
	C.aio_key(C.uint16_t(keycode), cbool(down), C.uint64_t(flags))
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
