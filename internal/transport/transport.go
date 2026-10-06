// Package transport manages the outbound WebSocket connection to the control
// plane: dialing, registration, heartbeats, reading commands and writing
// results, with automatic reconnection.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aioagent/daemon/internal/protocol"
	"github.com/gorilla/websocket"
)

// ErrNotConnected is returned by Send when there is no live connection.
var ErrNotConnected = errors.New("transport: not connected")

// Sender lets a command handler push messages back to the control plane. It is
// always backed by the current live connection, so sends keep working across
// transparent reconnections.
type Sender interface {
	Send(msg protocol.Message) error
}

// CommandHandler processes an incoming command. Implementations should not
// block the transport; long work is fine because Handle is invoked in its own
// goroutine.
type CommandHandler interface {
	Handle(ctx context.Context, cmd protocol.Command, s Sender)
}

// Options configures the client.
type Options struct {
	URL       string
	Header    http.Header
	Handler   CommandHandler
	Register  func() protocol.Register // called on each (re)connect
	Heartbeat time.Duration
	BackoffMin time.Duration
	BackoffMax time.Duration
	Logger    *slog.Logger
	// OnState, if set, is called when the connection is established
	// (connected=true) and when it drops or a dial fails (connected=false).
	OnState func(connected bool, err error)
}

// Client is the WebSocket transport. It implements Sender.
type Client struct {
	opts Options
	log  *slog.Logger

	mu   sync.Mutex
	conn *websocket.Conn
}

// New creates a Client.
func New(opts Options) *Client {
	if opts.Heartbeat <= 0 {
		opts.Heartbeat = 30 * time.Second
	}
	if opts.BackoffMin <= 0 {
		opts.BackoffMin = time.Second
	}
	if opts.BackoffMax < opts.BackoffMin {
		opts.BackoffMax = 30 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{opts: opts, log: opts.Logger}
}

// Send writes a message on the current connection. Safe for concurrent use.
func (c *Client) Send(msg protocol.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return ErrNotConnected
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.conn.WriteJSON(msg)
}

// Run connects and serves until ctx is cancelled, reconnecting with backoff.
func (c *Client) Run(ctx context.Context) error {
	backoff := c.opts.BackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		established, err := c.serve(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.notify(false, err)
		if established {
			backoff = c.opts.BackoffMin // successful session resets backoff
		}
		c.log.Warn("connection closed, reconnecting", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > c.opts.BackoffMax {
			backoff = c.opts.BackoffMax
		}
	}
}

// serve dials, registers, and runs the read loop for one connection. It returns
// whether a connection was established and the error that ended it.
func (c *Client) serve(ctx context.Context) (established bool, err error) {
	dialer := &websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, c.opts.URL, c.opts.Header)
	if err != nil {
		if resp != nil {
			return false, errors.New("dial failed: " + resp.Status)
		}
		return false, err
	}
	c.log.Info("connected to control plane", "url", c.opts.URL)
	c.notify(true, nil)

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.conn = nil
		c.mu.Unlock()
		conn.Close()
	}()

	// Register this daemon.
	if c.opts.Register != nil {
		msg, mErr := protocol.NewMessage(protocol.TypeRegister, "", c.opts.Register())
		if mErr == nil {
			if sErr := c.Send(msg); sErr != nil {
				return true, sErr
			}
		}
	}

	// Read deadline is refreshed by pongs; start the heartbeat pinger.
	readWait := 2 * c.opts.Heartbeat
	_ = conn.SetReadDeadline(time.Now().Add(readWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(readWait))
	})

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go c.pinger(pingCtx)

	// Read loop.
	for {
		_, data, rErr := conn.ReadMessage()
		if rErr != nil {
			return true, rErr
		}
		var msg protocol.Message
		if jErr := json.Unmarshal(data, &msg); jErr != nil {
			c.log.Warn("dropping malformed message", "err", jErr)
			continue
		}
		if msg.Type != protocol.TypeCommand {
			continue
		}
		var cmd protocol.Command
		if jErr := json.Unmarshal(msg.Payload, &cmd); jErr != nil {
			c.log.Warn("dropping malformed command", "err", jErr)
			continue
		}
		if cmd.ID == "" {
			cmd.ID = msg.ID
		}
		// Run each command in its own goroutine so reads keep flowing.
		go c.opts.Handler.Handle(ctx, cmd, c)
	}
}

func (c *Client) notify(connected bool, err error) {
	if c.opts.OnState != nil {
		c.opts.OnState(connected, err)
	}
}

func (c *Client) pinger(ctx context.Context) {
	ticker := time.NewTicker(c.opts.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.mu.Lock()
			conn := c.conn
			if conn != nil {
				_ = conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			}
			c.mu.Unlock()
		}
	}
}
