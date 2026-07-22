# AIO Agent Daemon

A small Go daemon for your MacBook that connects **outbound** to a control
plane over WebSocket and executes AI coding-agent commands locally.

Because the daemon dials out, it works from behind NAT/firewalls with **no
inbound ports** open. Commands are pushed down the socket; logs and results
stream back up the same connection.

## Responsibilities

- Receive commands from a control plane (persistent outbound WebSocket)
- Execute AI coding agents (pluggable providers, see below)
- Read/write files (sandboxed to configured workspaces)
- Git operations
- PR review (via the GitHub CLI + an AI provider)
- Deployment (run scripts/commands in a workspace)
- Stream logs and results back
- Auto-reconnect with exponential backoff + heartbeats

## Supported providers

Each provider is a thin adapter that shells out to the agent's CLI and streams
its output. A provider is only usable if its CLI is installed on the machine.

| Provider  | Name        | Default binary  |
|-----------|-------------|-----------------|
| Claude Code       | `claude`    | `claude`        |
| OpenAI Codex CLI  | `codex`     | `codex`         |
| Gemini CLI        | `gemini`    | `gemini`        |
| Aider             | `aider`     | `aider`         |
| OpenHands         | `openhands` | `openhands`     |
| Cursor Agent      | `cursor`    | `cursor-agent`  |
| GitHub Copilot CLI| `copilot`   | `copilot`       |

Binaries, args, and env vars are overridable per provider in `config.yaml`.
Adding a **custom model/provider** later means adding one adapter in
`internal/providers/defaults.go` (or implementing the `providers.Provider`
interface).

## Build & run

Requires Go 1.23+ (`brew install go`).

```sh
make tidy      # download deps, write go.sum
make build     # -> bin/aio-daemon
cp config.example.yaml config.yaml   # then edit
AIO_TOKEN=... ./bin/aio-daemon -config config.yaml
```

Or point it directly at a server:

```sh
AIO_TOKEN=secret ./bin/aio-daemon -server wss://control.example.com/agent
```

## Topologies

**Direct (LAN):** the daemon dials a control plane on the same machine/LAN. The
`cmd/server` harness above plays that role.

**Relay (cross-network):** for controlling the Mac from a phone on a different
network, run `cmd/relay` somewhere reachable. Both the daemon and the control app
dial OUT to it — no inbound ports on either side:

```
daemon (Mac) ──ws──▶  /agent   RELAY   /control  ◀──ws──  control app (phone)
```

```sh
make build-relay
AIO_AGENT_TOKEN=a AIO_CONTROL_TOKEN=b ./bin/aio-relay -addr :9090
# daemon:
./bin/aio-daemon -server ws://<relay-host>:9090/agent   # + AIO_TOKEN=a
# control app connects to ws://<relay-host>:9090/control  (Bearer b)
```

The relay multiplexes many daemons over each control connection; every frame to a
control carries a `daemon` field identifying the source, and commands carry a
`daemon` field naming the target. See `docs/flutter-control-app-handoff.md`.

## Wire protocol

JSON messages over WebSocket. Envelope:

```json
{ "type": "command", "id": "c123", "payload": { ... }, "ts": 1700000000000 }
```

Server → daemon: `command`.
Daemon → server: `register`, `heartbeat`, `ack`, `log`, `result`.

A `command` payload:

```json
{ "id": "c123", "action": "agent.run", "payload": { ... } }
```

### Actions

| Action            | Payload                                                             | Description |
|-------------------|--------------------------------------------------------------------|-------------|
| `agent.run`       | `{provider, prompt, workdir?, model?, files?, args?, env?, auto_approve?}` | Run an AI coding agent |
| `agent.providers` | `{}`                                                               | List providers + availability |
| `file.read`       | `{path}`                                                           | Read a file |
| `file.write`      | `{path, content, mode?, append?}`                                 | Write a file |
| `file.list`       | `{path}`                                                           | List a directory |
| `git`             | `{workdir, args:[...]}`                                            | Run a git command |
| `pr.review`       | `{workdir, number, provider, model?, instructions?}`              | AI review of a GitHub PR (needs `gh`) |
| `deploy`          | `{workdir, command, args?, shell?}`                               | Run a deploy command/script |
| `system.info`     | `{}`                                                               | Daemon identity + capabilities |
| `command.cancel`  | `{id}`                                                             | Cancel a running command |

While a command runs, the daemon streams `log` messages
(`{stream: "stdout"|"stderr"|"system", data}`) correlated by the command `id`,
then a terminal `result` (`{ok, data?, error?, exit_code?}`).

## Security notes

- All file/git/deploy paths are confined to the configured `workspaces`.
- The daemon executes agent CLIs and shell commands with the privileges of the
  user running it. Only connect it to a control plane you trust, and scope
  `workspaces` tightly.
- `auto_approve` passes each agent's "skip confirmation" flag — use with care.

## Project layout

```
cmd/daemon           entrypoint (flags, signals, logging)
cmd/server           dev control-plane test harness (direct mode)
cmd/relay            cross-network broker (daemon <-> control app)
internal/config      YAML + env configuration
internal/protocol    wire message types
internal/transport   outbound WebSocket client (reconnect, heartbeat)
internal/daemon      wiring + command router (ack/log/result, cancellation)
internal/dispatch    action -> handler routing
internal/handlers    agent.run, file.*, git, pr.review, deploy, system.info
internal/providers   AI agent adapters (Provider interface + registry)
internal/executor    subprocess runner with streamed output
internal/files       sandboxed filesystem access
```

## Known limitations / TODO

- `pr.review` streams the review but does not yet post it as a PR comment.
- Symlink escapes from a workspace are not fully hardened.
- No on-disk command queue: a command in flight during a disconnect keeps
  running, and its buffered logs may be dropped if the socket is down when they
  are produced.
