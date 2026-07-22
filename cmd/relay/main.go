// Command relay is a cross-network broker for the daemon and its control apps.
//
// Both sides dial OUT to this relay, so it works when the daemon (on a Mac) and
// the control app (e.g. a phone) are on different networks — neither needs to
// accept inbound connections.
//
//	daemon  ──ws──▶  /agent    relay    /control  ◀──ws──  control app
//
// The relay routes messages between them:
//   - control → daemon: a `command` frame carrying a `daemon` field naming the
//     target daemon (or, if exactly one daemon is connected, the target is
//     inferred).
//   - daemon → control: `register`/`ack`/`log`/`result` frames, each stamped
//     with a `daemon` field identifying the source, broadcast to all controls.
//
// Relay-only additions to the wire protocol:
//   - `daemon` field on the envelope (routing; the daemon itself ignores it).
//   - `daemon.offline` control message when a daemon disconnects.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aioagent/daemon/internal/protocol"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

const (
	pingEvery = 30 * time.Second
	readWait  = 70 * time.Second
)

func main() {
	addr := flag.String("addr", ":9090", "listen address")
	agentPath := flag.String("agent-path", "/agent", "path daemons connect to")
	controlPath := flag.String("control-path", "/control", "path control apps connect to")
	agentToken := flag.String("agent-token", os.Getenv("AIO_AGENT_TOKEN"), "Bearer token daemons must present (empty disables)")
	controlToken := flag.String("control-token", os.Getenv("AIO_CONTROL_TOKEN"), "Bearer token control apps must present (empty disables)")
	flag.Parse()

	h := newHub()

	http.HandleFunc(*agentPath, func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r, *agentToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.serveAgent(w, r)
	})
	http.HandleFunc(*controlPath, func(w http.ResponseWriter, r *http.Request) {
		if !authOK(r, *controlToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.serveControl(w, r)
	})

	go h.pinger()

	log.Printf("relay listening on %s (agent=%s control=%s, agent-auth=%v control-auth=%v)",
		*addr, *agentPath, *controlPath, *agentToken != "", *controlToken != "")
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// conn wraps a websocket with a write mutex (gorilla allows one writer at a time).
type conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *conn) send(raw []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteMessage(websocket.TextMessage, raw)
}

func (c *conn) ping() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
}

// routed is the wire envelope plus the relay-only `daemon` routing field.
type routed struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Time    int64           `json:"ts,omitempty"`
	Daemon  string          `json:"daemon,omitempty"`
}

type hub struct {
	mu           sync.Mutex
	daemons      map[string]*conn
	lastRegister map[string][]byte // last register frame (already stamped) per daemon
	controls     map[*conn]bool
}

func newHub() *hub {
	return &hub{
		daemons:      make(map[string]*conn),
		lastRegister: make(map[string][]byte),
		controls:     make(map[*conn]bool),
	}
}

// serveAgent handles a daemon connection: forward its frames to all controls,
// stamped with the daemon id.
func (h *hub) serveAgent(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Daemon-ID")
	if id == "" {
		id = "daemon-" + randID()
	}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws}

	// Detect a dead daemon: it pings us every ~30s; extend the deadline on ping.
	_ = ws.SetReadDeadline(time.Now().Add(readWait))
	ws.SetPingHandler(func(appData string) error {
		_ = ws.SetReadDeadline(time.Now().Add(readWait))
		return ws.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	h.mu.Lock()
	h.daemons[id] = c
	h.mu.Unlock()
	log.Printf("daemon connected: %s", id)

	defer func() {
		h.mu.Lock()
		if h.daemons[id] == c {
			delete(h.daemons, id)
			delete(h.lastRegister, id)
		}
		h.mu.Unlock()
		ws.Close()
		log.Printf("daemon disconnected: %s", id)
		if off, mErr := json.Marshal(routed{Type: "daemon.offline", Daemon: id}); mErr == nil {
			h.broadcast(off)
		}
	}()

	for {
		_, data, rErr := ws.ReadMessage()
		if rErr != nil {
			return
		}
		var m routed
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		m.Daemon = id
		out, mErr := json.Marshal(m)
		if mErr != nil {
			continue
		}
		if m.Type == protocol.TypeRegister {
			h.mu.Lock()
			h.lastRegister[id] = out
			h.mu.Unlock()
		}
		h.broadcast(out)
	}
}

// serveControl handles a control-app connection: forward its commands to the
// addressed daemon, and send a snapshot of currently-connected daemons.
func (h *hub) serveControl(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws}

	_ = ws.SetReadDeadline(time.Now().Add(readWait))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(readWait))
	})

	h.mu.Lock()
	h.controls[c] = true
	snaps := make([][]byte, 0, len(h.lastRegister))
	for _, raw := range h.lastRegister {
		snaps = append(snaps, raw)
	}
	h.mu.Unlock()
	log.Printf("control connected (%d daemon(s) known)", len(snaps))

	// Replay each connected daemon's register so the control populates its list.
	for _, raw := range snaps {
		_ = c.send(raw)
	}

	defer func() {
		h.mu.Lock()
		delete(h.controls, c)
		h.mu.Unlock()
		ws.Close()
		log.Printf("control disconnected")
	}()

	for {
		_, data, rErr := ws.ReadMessage()
		if rErr != nil {
			return
		}
		var m routed
		if json.Unmarshal(data, &m) != nil {
			continue
		}

		target := m.Daemon
		var dc *conn
		if target != "" {
			dc = h.daemonConn(target)
		} else {
			target, dc = h.singleDaemon() // convenience when only one is connected
		}
		if dc == nil {
			h.replyError(c, m.ID, target)
			continue
		}
		// Forward verbatim; the daemon ignores the extra `daemon` field.
		if sErr := dc.send(data); sErr != nil {
			h.replyError(c, m.ID, target)
		}
	}
}

// replyError tells a control app that its target daemon is unreachable, using a
// standard `result` frame so existing result-handling renders it.
func (h *hub) replyError(c *conn, cmdID, target string) {
	res := routed{Type: protocol.TypeResult, ID: cmdID, Daemon: target}
	res.Payload, _ = json.Marshal(protocol.Result{
		OK:    false,
		Error: "daemon not connected: " + target,
	})
	if b, err := json.Marshal(res); err == nil {
		_ = c.send(b)
	}
}

func (h *hub) broadcast(raw []byte) {
	h.mu.Lock()
	cs := make([]*conn, 0, len(h.controls))
	for c := range h.controls {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	for _, c := range cs {
		_ = c.send(raw)
	}
}

func (h *hub) daemonConn(id string) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.daemons[id]
}

func (h *hub) singleDaemon() (string, *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.daemons) == 1 {
		for id, c := range h.daemons {
			return id, c
		}
	}
	return "", nil
}

func (h *hub) pinger() {
	t := time.NewTicker(pingEvery)
	defer t.Stop()
	for range t.C {
		h.mu.Lock()
		cs := make([]*conn, 0, len(h.controls))
		for c := range h.controls {
			cs = append(cs, c)
		}
		h.mu.Unlock()
		for _, c := range cs {
			_ = c.ping()
		}
	}
}

func authOK(r *http.Request, token string) bool {
	if token == "" {
		return true
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func randID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
