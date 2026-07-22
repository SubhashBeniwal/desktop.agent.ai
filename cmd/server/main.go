// Command server is a minimal control-plane test harness for the daemon.
//
// It accepts the daemon's outbound WebSocket connection, prints every message
// the daemon streams back (register/ack/log/result), and lets you dispatch
// commands by typing them at the terminal:
//
//	> system.info
//	> agent.providers
//	> file.list {"path":"/Users/you/code"}
//	> agent.run {"provider":"claude","prompt":"list the files here","workdir":"/Users/you/code"}
//	> git {"workdir":"/Users/you/code","args":["status","--short"]}
//	> cancel <command-id>
//
// This is a development tool, not a production control plane.
package main

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aioagent/daemon/internal/protocol"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	path := flag.String("path", "/agent", "WebSocket path daemons connect to")
	token := flag.String("token", os.Getenv("AIO_SERVER_TOKEN"), "expected Bearer token (empty disables auth)")
	flag.Parse()

	h := &hub{clients: make(map[string]*client)}

	http.HandleFunc(*path, func(w http.ResponseWriter, r *http.Request) {
		if *token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(*token)) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Printf("upgrade failed: %v", err)
			return
		}
		h.serve(conn, r.Header.Get("X-Daemon-ID"))
	})

	go h.repl()

	fmt.Printf("control-plane harness listening on %s%s (auth: %v)\n", *addr, *path, *token != "")
	fmt.Println("waiting for a daemon to connect… type 'help' once one does.")
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// client is a single connected daemon.
type client struct {
	id   string
	name string
	conn *websocket.Conn
	mu   sync.Mutex // serializes writes
}

func (c *client) send(msg protocol.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

// hub tracks connected daemons and the one the REPL currently targets.
type hub struct {
	mu       sync.Mutex
	clients  map[string]*client
	selected *client
}

func (h *hub) serve(conn *websocket.Conn, headerID string) {
	// Respond to daemon pings so its read deadline stays fresh.
	conn.SetPingHandler(func(appData string) error {
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(5*time.Second))
	})

	c := &client{id: headerID, conn: conn}
	if c.id == "" {
		c.id = "unknown-" + randID()
	}

	h.mu.Lock()
	h.clients[c.id] = c
	h.selected = c
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.clients, c.id)
		if h.selected == c {
			h.selected = nil
		}
		h.mu.Unlock()
		conn.Close()
		fmt.Printf("\n[%s] disconnected\n> ", c.id)
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg protocol.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		h.print(c, msg)
	}
}

func (h *hub) print(c *client, msg protocol.Message) {
	switch msg.Type {
	case protocol.TypeRegister:
		var reg protocol.Register
		_ = json.Unmarshal(msg.Payload, &reg)
		c.name = reg.Name
		fmt.Printf("\n[%s] registered: %s %s/%s v%s\n  providers: %v\n  actions:   %v\n> ",
			c.id, reg.Name, reg.OS, reg.Arch, reg.Version, reg.Providers, reg.Actions)
	case protocol.TypeHeartbeat:
		// ignore
	case protocol.TypeAck:
		fmt.Printf("\n[%s] ack %s\n> ", c.id, msg.ID)
	case protocol.TypeLog:
		var l protocol.LogLine
		_ = json.Unmarshal(msg.Payload, &l)
		fmt.Printf("\n[%s %s|%s] %s", c.id, short(msg.ID), l.Stream, l.Data)
	case protocol.TypeResult:
		pretty, _ := json.MarshalIndent(json.RawMessage(msg.Payload), "  ", "  ")
		fmt.Printf("\n[%s] result %s:\n  %s\n> ", c.id, short(msg.ID), pretty)
	default:
		fmt.Printf("\n[%s] %s: %s\n> ", c.id, msg.Type, msg.Payload)
	}
}

// repl reads commands from stdin and dispatches them to the selected daemon.
func (h *hub) repl() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Print("> ")
			continue
		}
		switch {
		case line == "help":
			printHelp()
		case line == "list":
			h.listClients()
		case strings.HasPrefix(line, "use "):
			h.use(strings.TrimSpace(strings.TrimPrefix(line, "use ")))
		case strings.HasPrefix(line, "cancel "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "cancel "))
			h.dispatch("command.cancel", []byte(fmt.Sprintf(`{"id":%q}`, id)))
		default:
			action, payload := splitCommand(line)
			h.dispatch(action, payload)
		}
	}
}

func (h *hub) dispatch(action string, payload []byte) {
	h.mu.Lock()
	target := h.selected
	h.mu.Unlock()
	if target == nil {
		fmt.Println("no daemon connected")
		fmt.Print("> ")
		return
	}
	if len(payload) > 0 && !json.Valid(payload) {
		fmt.Printf("invalid JSON payload: %s\n> ", payload)
		return
	}

	id := randID()
	cmd := protocol.Command{ID: id, Action: action, Payload: payload}
	msg, err := protocol.NewMessage(protocol.TypeCommand, id, cmd)
	if err != nil {
		fmt.Printf("encode error: %v\n> ", err)
		return
	}
	if err := target.send(msg); err != nil {
		fmt.Printf("send error: %v\n> ", err)
		return
	}
	fmt.Printf("[%s] sent %s id=%s\n", target.id, action, id)
}

func (h *hub) listClients() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) == 0 {
		fmt.Println("no daemons connected")
	}
	for id, c := range h.clients {
		marker := " "
		if c == h.selected {
			marker = "*"
		}
		fmt.Printf(" %s %s (%s)\n", marker, id, c.name)
	}
	fmt.Print("> ")
}

func (h *hub) use(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.clients[id]; ok {
		h.selected = c
		fmt.Printf("now targeting %s\n> ", id)
		return
	}
	fmt.Printf("no such daemon %q\n> ", id)
}

// splitCommand splits "action {json...}" into the action and raw payload.
func splitCommand(line string) (string, []byte) {
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		return line, nil
	}
	action := line[:idx]
	rest := strings.TrimSpace(line[idx+1:])
	if rest == "" {
		return action, nil
	}
	return action, []byte(rest)
}

func printHelp() {
	fmt.Print(`commands:
  <action> [json-payload]   dispatch a command to the selected daemon
  list                      list connected daemons (* = selected)
  use <daemon-id>           target a specific daemon
  cancel <command-id>       cancel an in-flight command
  help                      this message

examples:
  system.info
  agent.providers
  file.list {"path":"/Users/you/code"}
  agent.run {"provider":"claude","prompt":"summarize this repo","workdir":"/Users/you/code"}
  git {"workdir":"/Users/you/code","args":["log","--oneline","-5"]}
> `)
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func randID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
