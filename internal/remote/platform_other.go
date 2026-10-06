//go:build !(darwin && cgo)

package remote

import "errors"

var plat platform = unsupported{}

type unsupported struct{}

var errUnsupported = errors.New("E_UNSUPPORTED: remote apps are not supported on this platform/build")

func (unsupported) supported() bool                        { return false }
func (unsupported) permissions() Permissions               { return Permissions{} }
func (unsupported) listWindows() ([]Window, []Display, error) { return nil, nil, errUnsupported }
func (unsupported) bounds(Target) (Bounds, int, bool)      { return Bounds{}, 0, false }
func (unsupported) backingScale(float64, float64) float64  { return 1 }
func (unsupported) startCapture(Target, int, int, int, int, frameSink) (capture, error) {
	return nil, errUnsupported
}
func (unsupported) mouse(int, int, float64, float64, int, bool) {}
func (unsupported) scroll(float64, float64, int, int)         {}
func (unsupported) key(uint16, bool, uint64)                  {}
func (unsupported) text(string)                               {}
func (unsupported) raise(int, Bounds)                         {}
func (unsupported) keycode(string) (uint16, bool)             { return 0, false }
func (unsupported) modFlags([]string) uint64                  { return 0 }

func requestScreen()        {}
func requestAccessibility() {}
