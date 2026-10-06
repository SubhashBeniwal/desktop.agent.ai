package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// h264 is Constrained Baseline, which every phone decoder supports.
var h264 = webrtc.RTPCodecCapability{
	MimeType:    webrtc.MimeTypeH264,
	ClockRate:   90000,
	SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
}

type session struct {
	id      string
	m       *Manager
	target  Target
	fps     int
	maxSize int

	pc    *webrtc.PeerConnection
	track *webrtc.TrackLocalStaticSample
	cap   capture

	done    chan struct{}
	endOnce sync.Once

	mu        sync.Mutex
	bounds    Bounds
	pid       int
	w, h      int
	dc        *webrtc.DataChannel
	lastPTS   time.Duration
	held      int // mouse button currently down, or -1
	lastKeyRq time.Time
}

func (m *Manager) newSession(id string, req startReq, fps, maxSize, kbps int) (*session, error) {
	b, pid, ok := plat.bounds(req.Target)
	if !ok {
		return nil, errors.New("E_WINDOW_NOT_FOUND: target is gone or not on screen (minimized?)")
	}
	s := &session{
		id: id, m: m, target: req.Target, fps: fps, maxSize: maxSize,
		done: make(chan struct{}), bounds: b, pid: pid, held: -1,
	}
	s.w, s.h = streamSize(b, plat.backingScale(b.X, b.Y), maxSize)

	if err := s.negotiate(req.Offer); err != nil {
		if s.pc != nil {
			_ = s.pc.Close()
		}
		return nil, err
	}

	if req.Target.WindowID != 0 && (req.Focus == nil || *req.Focus) {
		plat.raise(pid, b)
	}
	c, err := plat.startCapture(req.Target, s.w, s.h, fps, kbps, s)
	if err != nil {
		_ = s.pc.Close()
		if strings.Contains(err.Error(), "not found") {
			return nil, errors.New("E_WINDOW_NOT_FOUND: " + err.Error())
		}
		return nil, fmt.Errorf("start capture: %w", err)
	}
	s.mu.Lock()
	s.cap = c
	s.mu.Unlock()

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		s.trackBounds()
	}()
	return s, nil
}

// negotiate answers the phone's offer with a send-only H.264 track. The
// answer is returned only after ICE gathering completes (no trickle ICE).
func (s *session) negotiate(offer webrtc.SessionDescription) error {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		return err
	}
	ir := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(me, ir); err != nil {
		return err
	}
	var se webrtc.SettingEngine
	// Notice a vanished phone within ~15s instead of pion's 30s default.
	se.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithInterceptorRegistry(ir), webrtc.WithSettingEngine(se))

	var ice []webrtc.ICEServer
	for _, srv := range s.m.cfg.ICEServers {
		ice = append(ice, webrtc.ICEServer{URLs: srv.URLs, Username: srv.Username, Credential: srv.Credential})
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: ice})
	if err != nil {
		return err
	}
	s.pc = pc

	pc.OnConnectionStateChange(func(st webrtc.PeerConnectionState) {
		s.m.log.Debug("stream connection state", "session", s.id, "state", st.String())
		switch st {
		case webrtc.PeerConnectionStateConnected:
			s.requestKeyframe()
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			go s.end("connection " + st.String())
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			return
		}
		s.mu.Lock()
		s.dc = dc
		s.mu.Unlock()
		dc.OnOpen(func() {
			s.mu.Lock()
			w, h := s.w, s.h
			s.mu.Unlock()
			s.send(map[string]any{"t": "hello", "w": w, "h": h, "input": plat.permissions().Accessibility})
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) { s.handleInput(msg.Data) })
	})

	if err := pc.SetRemoteDescription(offer); err != nil {
		return fmt.Errorf("E_BAD_OFFER: %w", err)
	}
	track, err := webrtc.NewTrackLocalStaticSample(h264, "video", "aio-"+s.id)
	if err != nil {
		return err
	}
	s.track = track
	sender, err := pc.AddTrack(track)
	if err != nil {
		return fmt.Errorf("E_BAD_OFFER: %w", err)
	}
	go s.readRTCP(sender)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fmt.Errorf("E_BAD_OFFER: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return err
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second): // send what we have
	}
	return nil
}

// readRTCP turns the phone's PLI/FIR requests into keyframes.
func (s *session) readRTCP(sender *webrtc.RTPSender) {
	for {
		pkts, _, err := sender.ReadRTCP()
		if err != nil {
			return
		}
		for _, p := range pkts {
			switch p.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				s.requestKeyframe()
			}
		}
	}
}

func (s *session) requestKeyframe() {
	s.mu.Lock()
	c := s.cap
	if c == nil || time.Since(s.lastKeyRq) < 300*time.Millisecond {
		s.mu.Unlock()
		return
	}
	s.lastKeyRq = time.Now()
	s.mu.Unlock()
	c.keyframe()
}

// frame implements frameSink.
func (s *session) frame(data []byte, pts time.Duration, _ bool) {
	s.mu.Lock()
	dur := pts - s.lastPTS
	if s.lastPTS == 0 || dur <= 0 {
		dur = time.Second / time.Duration(s.fps)
	}
	s.lastPTS = pts
	s.mu.Unlock()
	_ = s.track.WriteSample(media.Sample{Data: data, Duration: dur})
}

// stopped implements frameSink.
func (s *session) stopped(reason string) {
	go s.end("capture stopped: " + reason)
}

// trackBounds follows window moves/resizes (input mapping and stream size)
// and ends the session when the window goes away.
func (s *session) trackBounds() {
	for sleepOrDone(500*time.Millisecond, s.done) {
		b, pid, ok := plat.bounds(s.target)
		if !ok {
			s.end("window closed")
			return
		}
		w, h := streamSize(b, plat.backingScale(b.X, b.Y), s.maxSize)
		s.mu.Lock()
		s.bounds, s.pid = b, pid
		resized := w != s.w || h != s.h
		c := s.cap
		s.mu.Unlock()
		if !resized {
			continue
		}
		if err := c.resize(w, h); err != nil {
			s.m.log.Warn("stream resize failed", "session", s.id, "err", err)
			continue
		}
		s.mu.Lock()
		s.w, s.h = w, h
		s.mu.Unlock()
		s.send(map[string]any{"t": "size", "w": w, "h": h})
	}
}

// end tears the session down once; safe from any goroutine.
func (s *session) end(reason string) {
	s.endOnce.Do(func() {
		close(s.done)
		if s.send(map[string]any{"t": "end", "reason": reason}) {
			s.drain()
		}
		s.mu.Lock()
		c := s.cap
		s.mu.Unlock()
		if c != nil {
			c.stop()
		}
		_ = s.pc.Close()
		s.m.remove(s.id)
		s.m.log.Info("stream ended", "session", s.id, "reason", reason)
	})
}

// send writes a JSON message to the phone; it reports whether it was sent.
func (s *session) send(v any) bool {
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	if dc == nil || dc.ReadyState() != webrtc.DataChannelStateOpen {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && dc.SendText(string(b)) == nil
}

// drain gives queued data-channel messages (the "end" notice) a moment to
// reach the phone before the connection is torn down. Bounded, because the
// phone may already be gone.
func (s *session) drain() {
	s.mu.Lock()
	dc := s.dc
	s.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for dc.BufferedAmount() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
}

// inputMsg is one message on the "input" data channel (see the handoff doc).
type inputMsg struct {
	T    string   `json:"t"`
	A    string   `json:"a"`
	X    float64  `json:"x"`
	Y    float64  `json:"y"`
	B    string   `json:"b"`
	N    int      `json:"n"`
	DX   float64  `json:"dx"`
	DY   float64  `json:"dy"`
	Code string   `json:"code"`
	Mods []string `json:"mods"`
	S    string   `json:"s"`
}

func (s *session) handleInput(raw []byte) {
	var in inputMsg
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	if in.T == "keyframe" {
		s.requestKeyframe()
		return
	}
	if !plat.permissions().Accessibility {
		return // reported to the phone via hello.input=false
	}

	s.mu.Lock()
	b, pid := s.bounds, s.pid
	x, y := b.X+clampF(in.X)*b.W, b.Y+clampF(in.Y)*b.H
	s.mu.Unlock()
	btn := button(in.B)

	switch in.T {
	case "pointer":
		switch in.A {
		case "down":
			s.raise(pid, b)
			plat.mouse(mouseDown, btn, x, y, 1, false)
			s.setHeld(btn)
		case "up":
			plat.mouse(mouseUp, btn, x, y, 1, false)
			s.setHeld(-1)
		default: // move
			held := s.getHeld()
			plat.mouse(mouseMove, max(held, ButtonLeft), x, y, 0, held >= 0)
		}
	case "click":
		s.raise(pid, b)
		for i := 1; i <= max(1, min(in.N, 3)); i++ {
			plat.mouse(mouseDown, btn, x, y, i, false)
			plat.mouse(mouseUp, btn, x, y, i, false)
		}
	case "scroll":
		plat.scroll(x, y, int(in.DX), int(in.DY))
	case "key":
		kc, ok := plat.keycode(in.Code)
		if !ok {
			return
		}
		flags := plat.modFlags(in.Mods)
		if in.A != "up" {
			plat.key(kc, true, flags)
		}
		if in.A != "down" {
			plat.key(kc, false, flags)
		}
	case "text":
		plat.text(in.S)
	}
}

// raise brings a streamed window to the front so injected input lands in it
// (CGEvents go to whatever window is under the cursor).
func (s *session) raise(pid int, b Bounds) {
	if s.target.WindowID != 0 {
		plat.raise(pid, b)
	}
}

func (s *session) setHeld(b int) {
	s.mu.Lock()
	s.held = b
	s.mu.Unlock()
}

func (s *session) getHeld() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

func button(name string) int {
	switch name {
	case "right":
		return ButtonRight
	case "middle":
		return ButtonMiddle
	}
	return ButtonLeft
}

func clampF(v float64) float64 { return min(1, max(0, v)) }
