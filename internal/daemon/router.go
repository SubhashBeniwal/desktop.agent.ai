package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/aioagent/daemon/internal/dispatch"
	"github.com/aioagent/daemon/internal/protocol"
	"github.com/aioagent/daemon/internal/transport"
)

// router implements transport.CommandHandler. It acknowledges commands, runs
// them through the dispatcher, streams logs, and reports a terminal result. It
// also tracks in-flight commands so they can be cancelled.
type router struct {
	disp  *dispatch.Dispatcher
	log   *slog.Logger
	hooks Hooks

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

func newRouter(disp *dispatch.Dispatcher, log *slog.Logger, hooks Hooks) *router {
	return &router{
		disp:    disp,
		log:     log,
		hooks:   hooks,
		running: make(map[string]context.CancelFunc),
	}
}

// Handle processes a single command.
func (r *router) Handle(ctx context.Context, cmd protocol.Command, s transport.Sender) {
	// Cancellation is a control action handled inline.
	if cmd.Action == "command.cancel" {
		r.cancel(cmd, s)
		return
	}

	cctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.running[cmd.ID] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.running, cmd.ID)
		r.mu.Unlock()
		cancel()
	}()

	r.send(s, protocol.TypeAck, cmd.ID, nil)
	if r.hooks.OnCommand != nil {
		r.hooks.OnCommand(cmd.ID, cmd.Action)
	}

	logFn := func(stream, data string) {
		r.send(s, protocol.TypeLog, cmd.ID, protocol.LogLine{Stream: stream, Data: data})
	}

	data, err := r.disp.Handle(&dispatch.Ctx{Context: cctx, Command: cmd, Log: logFn})
	if err != nil {
		r.log.Warn("command failed", "id", cmd.ID, "action", cmd.Action, "err", err)
		r.send(s, protocol.TypeResult, cmd.ID, protocol.Result{OK: false, Error: err.Error()})
		r.finished(cmd, false, err.Error())
		return
	}
	r.send(s, protocol.TypeResult, cmd.ID, protocol.Result{OK: true, Data: data})
	r.finished(cmd, true, "")
}

func (r *router) finished(cmd protocol.Command, ok bool, errMsg string) {
	if r.hooks.OnResult != nil {
		r.hooks.OnResult(cmd.ID, cmd.Action, ok, errMsg)
	}
}

func (r *router) cancel(cmd protocol.Command, s transport.Sender) {
	var body struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(cmd.Payload, &body)

	r.mu.Lock()
	cancel, ok := r.running[body.ID]
	r.mu.Unlock()

	if ok {
		cancel()
	}
	r.send(s, protocol.TypeResult, cmd.ID, protocol.Result{OK: ok, Data: map[string]any{"cancelled": body.ID}})
}

func (r *router) send(s transport.Sender, typ, id string, payload any) {
	msg, err := protocol.NewMessage(typ, id, payload)
	if err != nil {
		r.log.Error("marshal message", "type", typ, "err", err)
		return
	}
	if err := s.Send(msg); err != nil {
		r.log.Warn("send failed", "type", typ, "id", id, "err", err)
	}
}
