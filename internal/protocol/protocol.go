// Package protocol defines the wire messages exchanged between the daemon and
// the remote control plane over the WebSocket connection.
//
// The connection is always initiated by the daemon (outbound), which keeps it
// reachable from behind NAT/firewalls without opening any inbound ports.
package protocol

import (
	"encoding/json"
	"time"
)

// Message types. Direction is a convention, not enforced by the transport.
const (
	// daemon -> server
	TypeRegister  = "register"  // sent once per connection with daemon identity
	TypeHeartbeat = "heartbeat" // periodic liveness (in addition to ws pings)
	TypeAck       = "ack"       // command received, execution starting
	TypeLog       = "log"       // streamed stdout/stderr/system line
	TypeResult    = "result"    // terminal result for a command

	// server -> daemon
	TypeCommand = "command" // a command to execute
)

// Message is the envelope for everything on the wire.
type Message struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`      // correlation id (usually the command id)
	Payload json.RawMessage `json:"payload,omitempty"` // type-specific body
	Time    int64           `json:"ts,omitempty"`      // unix millis
}

// NewMessage builds an envelope, marshalling payload (which may be nil).
func NewMessage(typ, id string, payload any) (Message, error) {
	m := Message{Type: typ, ID: id, Time: time.Now().UnixMilli()}
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Message{}, err
		}
		m.Payload = b
	}
	return m, nil
}

// Command is the payload of a TypeCommand message.
type Command struct {
	ID      string          `json:"id"`
	Action  string          `json:"action"` // e.g. "agent.run", "file.read", "git", "deploy"
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Register is the payload of a TypeRegister message, describing this daemon.
type Register struct {
	DaemonID  string   `json:"daemon_id"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	OS        string   `json:"os"`
	Arch      string   `json:"arch"`
	Providers []string `json:"providers"` // available (installed) provider names
	Actions   []string `json:"actions"`   // supported command actions
}

// LogLine is the payload of a TypeLog message.
type LogLine struct {
	Stream string `json:"stream"` // "stdout" | "stderr" | "system"
	Data   string `json:"data"`
}

// Result is the payload of a TypeResult message.
type Result struct {
	OK       bool   `json:"ok"`
	Data     any    `json:"data,omitempty"`
	Error    string `json:"error,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}