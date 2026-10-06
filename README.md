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

## Desktop app (GUI + installers)

`cmd/desktop` is a menu-bar (macOS) / system-tray (Windows) app built with
[Wails v3](https://v3.wails.io) that embeds the same daemon and adds a GUI:

- **Overview:** connection status, daemon ID, and which agent CLIs were found.
- **Settings:** relay URL, agent token, display name, workspaces (with a
  folder picker), log level, and launch at login. Saving reconnects.
- **Activity / Logs:** the commands received from the phone, and live logs.

Settings are stored in `~/Library/Application Support/AIO Agent/config.yaml`
(macOS) or `%AppData%\AIO Agent\config.yaml` (Windows). Override the path with
`AIO_CONFIG`. On first launch the generated daemon ID is saved there, so the
phone sees the same machine across restarts. When started from Finder or at
login, the app imports your login shell's `PATH`, so agent CLIs installed with
Homebrew or npm are still found.

```sh
make run-desktop        # dev build + run (macOS needs Xcode CLT for cgo)
make package-macos      # -> dist/AIO-Agent-<ver>-macos.dmg   (universal, run on a Mac)
make package-windows    # -> dist/AIO-Agent-<ver>-windows-amd64-setup.exe
                        #    (cross-compiles from macOS/Linux; needs `brew install makensis`)
```

The Windows installer is per-user (no admin prompt), adds Start Menu and
Desktop shortcuts, and registers an uninstaller. It needs the WebView2 runtime,
which ships with Windows 11 and current Windows 10.

**Unsigned builds:** the installers are not code-signed yet.
- **macOS:** after the first launch is blocked, allow it under System Settings
  → Privacy & Security → **Open Anyway**, or run
  `xattr -dr com.apple.quarantine "/Applications/AIO Agent.app"`.
- **Windows:** SmartScreen shows a warning; click **More info → Run anyway**.

To sign later, replace the ad-hoc `codesign --sign -` in
`packaging/macos/build-dmg.sh` with a Developer ID and add notarization, and
sign the `.exe` files with `signtool`/Azure Trusted Signing.

**CI:** `bitbucket-pipelines.yml` runs tests on every push. On `v*` tags it
also builds the Windows installer and uploads it to the repo's Downloads page
(set a `BB_DOWNLOADS_TOKEN` repository variable). Bitbucket Cloud has no hosted
macOS runners, so build the DMG locally with `make package-macos`.

## Remote apps (stream desktop apps to the phone)

The daemon can stream a single window (VS Code, Android Studio, IntelliJ,
Brave, …) or a whole display to the control app over **WebRTC**, and inject the
phone's touch and keyboard input back into it.
- **Signaling:** rides on normal commands. `stream.start` carries the SDP offer;
  its result carries the answer.
- **Video:** goes peer-to-peer.
- **Input:** uses a data channel named `input`.

The Flutter contract is in `docs/flutter-control-app-handoff.md`.

- **macOS:** ScreenCaptureKit capture, VideoToolbox H.264 (hardware, low-latency
  mode), and CGEvent input. Requires macOS 12.3+, and **Screen Recording** +
  **Accessibility** for AIO Agent. The desktop app's Overview tab has Grant
  buttons. macOS ties these grants to the app's signature, so ad-hoc-signed
  rebuilds need re-granting.
- **Config:** see the example below.
- **Dev tool:** `go run ./tools/streamprobe -token <control> -app Code -seconds 5
  -out /tmp/s.h264` plays the phone. It negotiates a stream through the relay
  and reports latency and throughput.

Config:

```yaml
stream:
  max_sessions: 2
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
    - urls: ["turn:turn.example.com:3478"]
      username: aio
      credential: secret
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
cmd/desktop          tray/menu-bar GUI app embedding the daemon (Wails v3)
packaging/           DMG (macOS) and NSIS installer (Windows) build scripts
tools/icongen        renders the app/tray icons into cmd/desktop/assets
internal/config      YAML + env configuration
internal/protocol    wire message types
internal/transport   outbound WebSocket client (reconnect, heartbeat)
internal/daemon      wiring + command router (ack/log/result, cancellation)
internal/dispatch    action -> handler routing
internal/handlers    agent.run, file.*, git, pr.review, deploy, system.info
internal/providers   AI agent adapters (Provider interface + registry)
internal/executor    subprocess runner with streamed output
internal/files       sandboxed filesystem access
internal/shellenv    login-shell PATH import for GUI launches
internal/logbuf      in-memory log ring for the GUI
internal/remote      Remote apps: WebRTC sessions, capture/encode/input (macOS via cgo)
tools/streamprobe    dev "phone" for testing Remote apps through the relay
```

## Known limitations / TODO

- Desktop installers are unsigned (see above). Wails v3 is pinned to a beta.
- Remote apps streaming is macOS-only (it needs cgo, so use the desktop app or a
  cgo build of the daemon; `make build` produces a `CGO_ENABLED=0` daemon
  without it). Windows capture/input is not implemented yet.
- No TURN server is configured by default (STUN only). Add one under
  `stream.ice_servers` for networks where direct WebRTC connections fail.
- Injected clicks land on whatever is on top at that point; the daemon raises
  the streamed window first, but another always-on-top window can still
  intercept input.
- `pr.review` streams the review but does not yet post it as a PR comment.
- Symlink escapes from a workspace are not fully hardened.
- No on-disk command queue: a command in flight during a disconnect keeps
  running, and its buffered logs may be dropped if the socket is down when they
  are produced.
