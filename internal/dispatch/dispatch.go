// Package dispatch routes commands to registered handlers.
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aioagent/daemon/internal/protocol"
)

// Ctx carries everything a handler needs to execute a command and stream output.
type Ctx struct {
	Context context.Context
	Command protocol.Command
	// Log streams a line back to the control plane while the command runs.
	// stream is "stdout", "stderr" or "system".
	Log func(stream, data string)
}

// HandlerFunc executes a command and returns a JSON-serializable result.
type HandlerFunc func(c *Ctx) (any, error)

// Dispatcher maps action names to handlers.
type Dispatcher struct {
	handlers map[string]HandlerFunc
}

// New returns an empty dispatcher.
func New() *Dispatcher {
	return &Dispatcher{handlers: make(map[string]HandlerFunc)}
}

// Register binds a handler to an action name.
func (d *Dispatcher) Register(action string, h HandlerFunc) {
	d.handlers[action] = h
}

// Actions returns the registered action names, sorted.
func (d *Dispatcher) Actions() []string {
	out := make([]string, 0, len(d.handlers))
	for a := range d.handlers {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// Handle runs the handler for the command's action.
func (d *Dispatcher) Handle(c *Ctx) (any, error) {
	h, ok := d.handlers[c.Command.Action]
	if !ok {
		return nil, fmt.Errorf("unknown action %q", c.Command.Action)
	}
	return h(c)
}

// Decode unmarshals a command payload into v. An empty payload is a no-op.
func Decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, v)
}
