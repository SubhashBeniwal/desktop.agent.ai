// Command streamprobe is a stand-in for the phone when developing Remote apps:
// it connects to the relay as a control app, picks a window, negotiates a
// WebRTC stream exactly as the Flutter app should, and reports what arrives.
//
//	go run ./tools/streamprobe -relay ws://127.0.0.1:9099/control -token ctl -app Code -seconds 5
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/h264writer"
)

func main() {
	relay := flag.String("relay", "ws://127.0.0.1:9099/control", "relay control URL")
	token := flag.String("token", os.Getenv("AIO_CONTROL_TOKEN"), "control token")
	daemon := flag.String("daemon", "", "target daemon id (default: the only one)")
	app := flag.String("app", "", "stream the first window whose app name contains this")
	windowID := flag.Uint("window", 0, "stream this window id")
	display := flag.Uint("display", 0, "stream this display id instead of a window")
	seconds := flag.Int("seconds", 5, "how long to stream")
	out := flag.String("out", "", "write received H.264 (Annex B) here")
	typeText := flag.String("type", "", "after connecting, click the window centre and type this")
	listOnly := flag.Bool("list", false, "only print stream.config and windows")
	flag.Parse()

	c := dial(*relay, *token, *daemon)

	cfg := c.call("stream.config", nil)
	fmt.Printf("stream.config: %s\n", cfg)
	wins := c.call("screen.windows", nil)
	if *listOnly {
		fmt.Printf("screen.windows: %s\n", wins)
		return
	}

	target := map[string]any{}
	switch {
	case *display != 0:
		target["display_id"] = *display
	case *windowID != 0:
		target["window_id"] = *windowID
	default:
		var w struct {
			Windows []struct {
				ID       uint32 `json:"id"`
				App      string `json:"app"`
				Title    string `json:"title"`
				OnScreen bool   `json:"on_screen"`
			} `json:"windows"`
		}
		must(json.Unmarshal(wins, &w))
		for _, x := range w.Windows {
			if x.OnScreen && strings.Contains(strings.ToLower(x.App), strings.ToLower(*app)) {
				fmt.Printf("picked window %d: %s — %q\n", x.ID, x.App, x.Title)
				target["window_id"] = x.ID
				break
			}
		}
		if len(target) == 0 {
			log.Fatalf("no on-screen window matching %q", *app)
		}
	}

	var ice struct {
		ICEServers []webrtc.ICEServer `json:"ice_servers"`
	}
	must(json.Unmarshal(cfg, &ice))
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{ICEServers: ice.ICEServers})
	must(err)
	defer pc.Close()

	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	must(err)
	ordered := true
	input, err := pc.CreateDataChannel("input", &webrtc.DataChannelInit{Ordered: &ordered})
	must(err)
	hello := make(chan struct{})
	var helloOnce sync.Once
	input.OnMessage(func(m webrtc.DataChannelMessage) {
		fmt.Printf("datachannel ← %s\n", m.Data)
		if strings.Contains(string(m.Data), `"hello"`) {
			helloOnce.Do(func() { close(hello) })
		}
	})

	var packets, bytes atomic.Int64
	var writer *h264writer.H264Writer
	if *out != "" {
		writer, err = h264writer.New(*out)
		must(err)
		defer writer.Close()
	}
	firstVideo := make(chan time.Time, 1)
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		fmt.Printf("track: %s\n", t.Codec().MimeType+" "+t.Codec().SDPFmtpLine)
		for {
			pkt, _, err := t.ReadRTP()
			if err != nil {
				return
			}
			if packets.Add(1) == 1 {
				firstVideo <- time.Now()
			}
			bytes.Add(int64(len(pkt.Payload)))
			if writer != nil {
				_ = writer.WriteRTP(pkt)
			}
		}
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) { fmt.Printf("pc: %s\n", s) })

	offer, err := pc.CreateOffer(nil)
	must(err)
	gathered := webrtc.GatheringCompletePromise(pc)
	must(pc.SetLocalDescription(offer))
	<-gathered

	start := time.Now()
	res := c.call("stream.start", map[string]any{"target": target, "offer": pc.LocalDescription()})
	var started struct {
		SessionID string                    `json:"session_id"`
		Answer    webrtc.SessionDescription `json:"answer"`
		Width     int                       `json:"width"`
		Height    int                       `json:"height"`
	}
	must(json.Unmarshal(res, &started))
	fmt.Printf("stream.start ok: session=%s size=%dx%d (signaling %v)\n",
		started.SessionID, started.Width, started.Height, time.Since(start).Round(time.Millisecond))
	must(pc.SetRemoteDescription(started.Answer))

	select {
	case t := <-firstVideo:
		fmt.Printf("first video packet after %v\n", t.Sub(start).Round(time.Millisecond))
	case <-time.After(10 * time.Second):
		fmt.Println("no video within 10s")
	}
	if *typeText != "" {
		<-hello
		send := func(v any) { b, _ := json.Marshal(v); _ = input.SendText(string(b)) }
		send(map[string]any{"t": "click", "x": 0.5, "y": 0.5})
		time.Sleep(200 * time.Millisecond)
		send(map[string]any{"t": "text", "s": *typeText})
	}
	time.Sleep(time.Duration(*seconds) * time.Second)
	fmt.Printf("received %d RTP packets, %d KB in %ds\n", packets.Load(), bytes.Load()/1024, *seconds)
	fmt.Printf("stream.stop: %s\n", c.call("stream.stop", map[string]any{"session_id": started.SessionID}))
	time.Sleep(300 * time.Millisecond)
}

// client is a minimal relay control connection with request/response calls.
type client struct {
	ws      *websocket.Conn
	daemon  string
	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	n       int
}

func dial(url, token, daemon string) *client {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ws, _, err := websocket.DefaultDialer.Dial(url, h)
	must(err)
	c := &client{ws: ws, daemon: daemon, pending: map[string]chan json.RawMessage{}}
	registered := make(chan struct{}, 1)
	go func() {
		for {
			var m struct {
				Type    string          `json:"type"`
				ID      string          `json:"id"`
				Daemon  string          `json:"daemon"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := ws.ReadJSON(&m); err != nil {
				log.Fatalf("relay: %v", err)
			}
			switch m.Type {
			case "register":
				if c.daemon == "" {
					c.daemon = m.Daemon
				}
				select {
				case registered <- struct{}{}:
				default:
				}
			case "result":
				c.mu.Lock()
				ch := c.pending[m.ID]
				c.mu.Unlock()
				if ch != nil {
					ch <- m.Payload
				}
			}
		}
	}()
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		log.Fatal("no daemon registered with the relay")
	}
	return c
}

// call sends a command and returns result.data, exiting on ok:false.
func (c *client) call(action string, args any) json.RawMessage {
	c.mu.Lock()
	c.n++
	id := fmt.Sprintf("probe-%d", c.n)
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg := map[string]any{"type": "command", "id": id, "daemon": c.daemon,
		"payload": map[string]any{"id": id, "action": action, "payload": args}}
	must(c.ws.WriteJSON(msg))

	select {
	case raw := <-ch:
		var r struct {
			OK    bool            `json:"ok"`
			Data  json.RawMessage `json:"data"`
			Error string          `json:"error"`
		}
		must(json.Unmarshal(raw, &r))
		if !r.OK {
			log.Fatalf("%s failed: %s", action, r.Error)
		}
		return r.Data
	case <-time.After(15 * time.Second):
		log.Fatalf("%s: timed out", action)
	}
	return nil
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
