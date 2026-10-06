package remote

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aioagent/daemon/internal/config"
	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/protocol"
	"github.com/pion/webrtc/v4"
)

// fakePlatform records input and emits synthetic H.264 frames.
type fakePlatform struct {
	mu        sync.Mutex
	b         Bounds
	gone      bool
	events    []string
	keyframes int
	stops     int
}

func (f *fakePlatform) supported() bool { return true }
func (f *fakePlatform) permissions() Permissions {
	return Permissions{ScreenRecording: true, Accessibility: true}
}
func (f *fakePlatform) listWindows() ([]Window, []Display, error) { return nil, nil, nil }
func (f *fakePlatform) bounds(Target) (Bounds, int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.b, 42, !f.gone
}
func (f *fakePlatform) backingScale(float64, float64) float64 { return 2 }
func (f *fakePlatform) startCapture(_ Target, w, h, fps, _ int, sink frameSink) (capture, error) {
	c := &fakeCapture{f: f, done: make(chan struct{})}
	go func() {
		// SPS-less IDR-ish NAL; pion only needs Annex-B framing.
		frame := []byte{0, 0, 0, 1, 0x65, 0x88, 0x84, 0x00, 0x33, 0xff}
		t := time.NewTicker(time.Second / time.Duration(fps))
		defer t.Stop()
		pts := time.Duration(0)
		for {
			select {
			case <-c.done:
				return
			case <-t.C:
				pts += time.Second / time.Duration(fps)
				sink.frame(frame, pts, true)
			}
		}
	}()
	return c, nil
}
func (f *fakePlatform) record(format string, a ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(a)
	f.events = append(f.events, format+" "+string(b))
}
func (f *fakePlatform) mouse(kind, button int, x, y float64, clicks int, dragging bool) {
	f.record("mouse", kind, button, x, y, clicks, dragging)
}
func (f *fakePlatform) scroll(x, y float64, dx, dy int)       { f.record("scroll", x, y, dx, dy) }
func (f *fakePlatform) key(k uint16, down bool, flags uint64) { f.record("key", k, down, flags) }
func (f *fakePlatform) text(s string)                         { f.record("text", s) }
func (f *fakePlatform) raise(pid int, _ Bounds)               { f.record("raise", pid) }
func (f *fakePlatform) keycode(code string) (uint16, bool) {
	k, ok := map[string]uint16{"KeyS": 1, "Enter": 36}[code]
	return k, ok
}
func (f *fakePlatform) modFlags(mods []string) uint64 { return uint64(len(mods)) }

func (f *fakePlatform) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

type fakeCapture struct {
	f    *fakePlatform
	done chan struct{}
	once sync.Once
}

func (c *fakeCapture) keyframe() { c.f.mu.Lock(); c.f.keyframes++; c.f.mu.Unlock() }
func (c *fakeCapture) resize(int, int) error { return nil }
func (c *fakeCapture) stop() {
	c.once.Do(func() { close(c.done); c.f.mu.Lock(); c.f.stops++; c.f.mu.Unlock() })
}

func useFake(t *testing.T) *fakePlatform {
	f := &fakePlatform{b: Bounds{X: 100, Y: 50, W: 800, H: 600}}
	old := plat
	plat = f
	t.Cleanup(func() { plat = old })
	return f
}

// newTestManager must be called after useFake so its Close (registered later,
// run earlier) stops sessions before the real platform is restored.
func newTestManager(t *testing.T) *Manager {
	m := New(config.StreamConfig{MaxSessions: 1}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.cfg.ICEServers = nil // host candidates only: no network needed
	t.Cleanup(m.Close)
	return m
}

func call(m *Manager, h dispatch.HandlerFunc, payload any) (map[string]any, error) {
	raw, _ := json.Marshal(payload)
	out, err := h(&dispatch.Ctx{Context: context.Background(), Command: protocol.Command{Payload: raw}})
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out)
	var res map[string]any
	_ = json.Unmarshal(b, &res)
	return res, nil
}

// phone is the offerer side, like the Flutter app.
type phone struct {
	pc      *webrtc.PeerConnection
	input   *webrtc.DataChannel
	msgs    chan map[string]any
	packets chan struct{}
}

func newPhone(t *testing.T) (*phone, webrtc.SessionDescription) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	p := &phone{pc: pc, msgs: make(chan map[string]any, 16), packets: make(chan struct{}, 1)}
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	if p.input, err = pc.CreateDataChannel("input", nil); err != nil {
		t.Fatal(err)
	}
	p.input.OnMessage(func(m webrtc.DataChannelMessage) {
		var v map[string]any
		_ = json.Unmarshal(m.Data, &v)
		p.msgs <- v
	})
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			if _, _, err := tr.ReadRTP(); err != nil {
				return
			}
			select {
			case p.packets <- struct{}{}:
			default:
			}
		}
	})
	offer, _ := pc.CreateOffer(nil)
	done := webrtc.GatheringCompletePromise(pc)
	_ = pc.SetLocalDescription(offer)
	<-done
	return p, *pc.LocalDescription()
}

func (p *phone) next(t *testing.T, typ string) map[string]any {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case m := <-p.msgs:
			if m["t"] == typ {
				return m
			}
		case <-timeout:
			t.Fatalf("no %q message", typ)
		}
	}
}

func (p *phone) send(v any) {
	b, _ := json.Marshal(v)
	_ = p.input.SendText(string(b))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStreamEndToEnd(t *testing.T) {
	f := useFake(t)
	m := newTestManager(t)
	p, offer := newPhone(t)

	res, err := call(m, m.streamStart, map[string]any{"target": map[string]any{"window_id": 7}, "offer": offer})
	if err != nil {
		t.Fatal(err)
	}
	// 800x600 pt at 2x = 1600x1200, already within max_size 1600.
	if res["width"] != 1600.0 || res["height"] != 1200.0 {
		t.Fatalf("size = %vx%v", res["width"], res["height"])
	}
	ans := res["answer"].(map[string]any)
	if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)}); err != nil {
		t.Fatal(err)
	}

	hello := p.next(t, "hello")
	if hello["w"] != 1600.0 || hello["input"] != true {
		t.Fatalf("hello = %v", hello)
	}
	select {
	case <-p.packets:
	case <-time.After(10 * time.Second):
		t.Fatal("no video RTP received")
	}

	// A second session exceeds max_sessions=1.
	_, offer2 := newPhone(t)
	if _, err := call(m, m.streamStart, map[string]any{"target": map[string]any{"window_id": 7}, "offer": offer2}); err == nil || !strings.HasPrefix(err.Error(), "E_LIMIT:") {
		t.Fatalf("second session err = %v, want E_LIMIT", err)
	}

	// Input: normalized coords map into the window's bounds (100,50 800x600).
	p.send(map[string]any{"t": "click", "x": 0.5, "y": 0.25, "b": "right"})
	p.send(map[string]any{"t": "key", "a": "press", "code": "KeyS", "mods": []string{"meta"}})
	p.send(map[string]any{"t": "text", "s": "hé"})
	p.send(map[string]any{"t": "keyframe"})
	want := []string{
		`raise [42]`, // focus on stream start
		`raise [42]`, // before the click
		`mouse [1,1,500,200,1,false]`,
		`mouse [2,1,500,200,1,false]`,
		`key [1,true,1]`,
		`key [1,false,1]`,
		`text ["hé"]`,
	}
	waitFor(t, "input events", func() bool { return len(f.snapshot()) >= len(want) })
	got := f.snapshot()
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("event %d = %s, want %s (all: %v)", i, got[i], w, got)
		}
	}

	if _, err := call(m, m.streamStop, map[string]any{"session_id": res["session_id"]}); err != nil {
		t.Fatal(err)
	}
	end := p.next(t, "end")
	if end["reason"] != "stopped by control app" {
		t.Fatalf("end = %v", end)
	}
	waitFor(t, "capture stop", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.stops == 1 })
	if f.keyframes == 0 {
		t.Fatal("expected a keyframe request (on connect or explicit)")
	}
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions left after stop", n)
	}
}

func TestStreamEndsWhenWindowCloses(t *testing.T) {
	f := useFake(t)
	m := newTestManager(t)
	p, offer := newPhone(t)
	res, err := call(m, m.streamStart, map[string]any{"target": map[string]any{"window_id": 7}, "offer": offer})
	if err != nil {
		t.Fatal(err)
	}
	ans := res["answer"].(map[string]any)
	_ = p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)})
	p.next(t, "hello")

	f.mu.Lock()
	f.gone = true
	f.mu.Unlock()
	if end := p.next(t, "end"); end["reason"] != "window closed" {
		t.Fatalf("end = %v", end)
	}
}

func TestStreamStartErrors(t *testing.T) {
	f := useFake(t)
	m := newTestManager(t)

	_, err := call(m, m.streamStart, map[string]any{
		"target": map[string]any{"window_id": 7},
		"offer":  map[string]any{"type": "offer", "sdp": "v=0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=rtpmap:96 VP8/90000\r\n"},
	})
	if err == nil || !strings.HasPrefix(err.Error(), "E_BAD_OFFER:") {
		t.Fatalf("VP8-only offer err = %v, want E_BAD_OFFER", err)
	}

	f.gone = true
	_, offer := newPhone(t)
	_, err = call(m, m.streamStart, map[string]any{"target": map[string]any{"window_id": 7}, "offer": offer})
	if err == nil || !strings.HasPrefix(err.Error(), "E_WINDOW_NOT_FOUND:") {
		t.Fatalf("missing window err = %v, want E_WINDOW_NOT_FOUND", err)
	}
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed start leaked %d session slot(s)", n)
	}
}

func TestStreamSize(t *testing.T) {
	for _, tc := range []struct {
		b      Bounds
		scale  float64
		max    int
		w, h   int
	}{
		{Bounds{W: 1512, H: 945}, 2, 1600, 1600, 1000}, // retina, capped
		{Bounds{W: 640, H: 480}, 1, 1600, 640, 480},    // never upscaled
		{Bounds{W: 801, H: 601}, 1, 1600, 800, 600},    // rounded to even
		{Bounds{W: 600, H: 1000}, 2, 1000, 600, 1000},  // portrait
	} {
		if w, h := streamSize(tc.b, tc.scale, tc.max); w != tc.w || h != tc.h {
			t.Errorf("streamSize(%v,%v,%d) = %dx%d, want %dx%d", tc.b, tc.scale, tc.max, w, h, tc.w, tc.h)
		}
	}
}
