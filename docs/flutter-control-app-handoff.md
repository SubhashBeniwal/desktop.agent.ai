# Handoff: Daemon control app — aio.agent.ai (Go daemon) → aio.flutter   (direction: backend→frontend)

## Goal
Build a Flutter app that acts as the **control plane** for the `aio-daemon`: it
shows the registered daemon(s), dispatches commands, and renders the
`ack → log → result` stream in real time.

## Pick a topology first
There are two supported modes. They share the **same message contract**; only how
the app connects differs.

| | **Mode A — direct (LAN)** | **Mode B — relay (cross-network) ✅ recommended** |
|---|---|---|
| Flutter app is a… | WebSocket **server** (daemon dials in) | WebSocket **client** (dials out to relay) |
| Works across different networks? | No — same machine/LAN only | **Yes** — phone and Mac can be anywhere |
| Extra infra | none | one small always-on relay (`cmd/relay` in the daemon repo) |
| Addressing daemons | one connection = one daemon | relay multiplexes many daemons; app targets by `daemon` id |

**Build Mode B (relay client) unless you specifically need zero-infra LAN-only
control.** It's a plain WebSocket client — far simpler in Flutter than hosting a
server, and it's the realistic phone↔Mac setup. Mode A is documented at the end
for completeness. The message contract sections below apply to **both**; Mode B
adds a `daemon` routing field on every frame.

## Mode B — connecting to the relay (recommended path)
The relay (`cmd/relay` in the daemon repo) is a small always-on broker. Both the
daemon and the Flutter app dial OUT to it, so it works across any networks with no
inbound ports on either side:

```
daemon (Mac) ──ws──▶  /agent   RELAY   /control  ◀──ws── Flutter app (phone)
```

- **You connect as a WebSocket client** to `wss://<relay-host>/control`.
- **Auth:** send header `Authorization: Bearer <CONTROL_TOKEN>` on the handshake
  (the relay is started with `-control-token` / `AIO_CONTROL_TOKEN`). A mismatch
  is rejected with HTTP 401.
- **Heartbeats:** the relay sends WebSocket PING frames every ~30s; the Flutter
  `WebSocketChannel` / `dart:io` socket **auto-responds with PONG**, so there's
  nothing to do. Don't expect a JSON `"heartbeat"` message.
- **Reconnect:** if the socket drops, reconnect with backoff; on reconnect the
  relay re-sends a `register` snapshot for every currently-connected daemon.

### Multiplexing: the `daemon` field (Mode B only)
The relay serves **many daemons over one control socket**, so every frame carries
an extra top-level `daemon` field that names which daemon it concerns:

- **Incoming** (`register`/`ack`/`log`/`result`): `daemon` = the source daemon id.
  Key all your UI state by this value.
- **Outgoing** (`command`): set `daemon` = the target daemon id (from a `register`
  you received). If exactly one daemon is connected you may omit it and the relay
  infers the target — but always set it explicitly once you support >1 daemon.
- **`daemon.offline`**: a relay-only frame `{ "type":"daemon.offline", "daemon":"<id>" }`
  sent when a daemon disconnects. Mark that daemon offline in the UI.

If you target a daemon that isn't connected, the relay replies with a normal
`result` frame `{ ok:false, error:"daemon not connected: <id>" }` (same `id` you
sent), so your existing error handling covers it.

The daemon-discovery flow: on connect you receive one `register` per online
daemon → build the daemon list. New daemons send a fresh `register` when they come
online; dropped ones send `daemon.offline`.

## Message envelope (both directions)
Every frame is this envelope (the `daemon` field is present only in Mode B):
```json
{ "type": "<string>", "id": "<correlation-id>", "payload": { }, "daemon": "<daemon-id>", "ts": 1700000000000 }
```
- `id` correlates a command with its `ack`/`log`/`result`. `ts` is unix millis (optional).
- `payload` shape depends on `type` (tables below).

## Message envelope (both directions)
Every frame is this envelope:
```json
{ "type": "<string>", "id": "<correlation-id>", "payload": { }, "ts": 1700000000000 }
```
- `id` correlates a command with its `ack`/`log`/`result`. `ts` is unix millis (optional).
- `payload` shape depends on `type` (tables below).

### Directions
- **Flutter → daemon:** only `command`.
- **daemon → Flutter:** `register` (once, on connect), then per command: `ack`, zero-or-more `log`, one `result`.

## Contract — Flutter → daemon: `command`
Send this to dispatch. **You generate the `id`** (a UUID/random hex) and set it on
**both** the envelope and the nested command. In Mode B also set the top-level
`daemon` target:
```json
{
  "type": "command",
  "id": "c-8f3a...",
  "daemon": "Admins-MacBook-Pro-2.local-5d02c3da",
  "payload": { "id": "c-8f3a...", "action": "system.info", "payload": { } }
}
```
- `daemon` — target daemon id (Mode B). Omit in Mode A (direct), where the socket
  already is one specific daemon.
- `payload.action` — one of the actions below.
- `payload.payload` — the action's arguments (may be omitted for no-arg actions).

### Actions and their argument payloads
| action | argument payload | notes |
|---|---|---|
| `system.info` | `{}` | daemon identity + capabilities |
| `agent.providers` | `{}` | list AI providers + install status |
| `agent.run` | `{ "provider": "claude", "prompt": "...", "workdir": "/abs/path", "model"?: "", "files"?: ["a.go"], "args"?: ["--flag"], "env"?: {"K":"V"}, "auto_approve"?: false }` | runs an AI coding agent; streams output as `log` |
| `file.read` | `{ "path": "/abs/path" }` | |
| `file.write` | `{ "path": "/abs/path", "content": "...", "mode"?: 420, "append"?: false }` | `mode` is decimal file mode (420 = 0644) |
| `file.list` | `{ "path": "/abs/dir" }` | |
| `git` | `{ "workdir": "/abs/repo", "args": ["status","--short"] }` | raw git args |
| `pr.review` | `{ "workdir": "/abs/repo", "number": 42, "provider": "claude", "model"?: "", "instructions"?: "" }` | needs `gh` installed on the daemon host |
| `deploy` | `{ "workdir": "/abs/dir", "command": "./deploy.sh", "args"?: [], "shell"?: true }` | `shell:true` runs `sh -c "<command>"` |
| `command.cancel` | `{ "id": "<id-of-running-command>" }` | cancels an in-flight command |

## Contract — daemon → Flutter

### `register` (sent once, immediately after connect; has no `id`)
```json
{ "type": "register", "payload": {
  "daemon_id": "Admins-MacBook-Pro-2.local-5d02c3da",
  "name": "Admins-MacBook-Pro-2.local",
  "version": "dev",
  "os": "darwin", "arch": "arm64",
  "providers": ["claude"],
  "actions": ["agent.providers","agent.run","deploy","file.list","file.read","file.write","git","pr.review","system.info"]
}, "ts": 1700000000000 }
```
Use `providers` to enable/disable provider choices in the UI, and `actions` to
know what this daemon supports.

### `ack` (command received, execution starting)
```json
{ "type": "ack", "id": "c-8f3a...", "ts": ... }   // payload is null/absent
```

### `log` (streamed while the command runs — may arrive many times)
```json
{ "type": "log", "id": "c-8f3a...", "payload": { "stream": "stdout", "data": "one line of output" }, "ts": ... }
```
- `stream` ∈ `"stdout" | "stderr" | "system"` (`system` = daemon's own progress notes).
- `data` is a single line (no trailing newline). Append to a per-command log view keyed by `id`.

### `result` (terminal — exactly one per command)
```json
{ "type": "result", "id": "c-8f3a...", "payload": { "ok": true, "data": { }, "error": "", "exit_code": 0 }, "ts": ... }
```
- `ok:false` → show `error`. `ok:true` → render `data` (shape depends on action, below).

### `result.data` shapes per action (for typed UI rendering)
| action | `data` on success |
|---|---|
| `system.info` | `{ daemon_id, name, version, os, arch, uptime_sec, workspaces: [".."], providers: [".."] }` |
| `agent.providers` | `[ { "name": "claude", "available": true }, ... ]` |
| `file.read` | `{ path, size, content }` |
| `file.write` | `{ path, written }` |
| `file.list` | `{ path, entries: [ { name, is_dir, size, mode, mod_time } ] }` |
| `agent.run` | `{ provider, exit_code }` (the real output arrived as `log` lines) |
| `git` | `{ exit_code }` (output arrived as `log`) |
| `deploy` | `{ exit_code }` (output arrived as `log`) |
| `pr.review` | `{ provider, pr, exit_code }` (the review text arrived as `log`) |
| `command.cancel` | `{ cancelled: "<id>" }` |

## Ownership
- **Flutter must supply:** the command `id` (unique per command), `action`, and
  the action's argument payload. For `agent.run`/`git`/`deploy`/`pr.review`, an
  **absolute `workdir`** that is inside one of the daemon's configured
  `workspaces` (get the allowed roots from `system.info` → `workspaces`).
- **Daemon resolves, do NOT send:** provider CLI resolution, availability, the
  sandbox/workspace enforcement, process spawning. Any `path`/`workdir` outside
  the daemon's workspaces is rejected with `result.ok=false` — surface that error;
  don't try to work around it.

## Config / env (Flutter side, Mode B)
| setting | value source | notes |
|---|---|---|
| relay URL | app setting | `wss://<relay-host>/control` in prod (`ws://` only for local testing) |
| control token | app setting (secret) | **[NEEDS INPUT]** — decide the token; sent as `Authorization: Bearer <token>`. Must equal the relay's `-control-token` / `AIO_CONTROL_TOKEN`. Store a name/placeholder in the repo, never the real value; keep it in secure storage on device. |
| default daemon id | runtime, from `register` | which daemon to target when the user hasn't picked one |

There are **no** OAuth/redirect/CORS concerns — this is a raw WebSocket with a
bearer token, not a browser OAuth flow.

## Integration steps (Mode B, in order)
1. Connect a WebSocket client to `wss://<relay-host>/control` with header
   `Authorization: Bearer <CONTROL_TOKEN>`. (Use `web_socket_channel` — pass the
   header via `IOWebSocketChannel.connect(url, headers: {...})`.)
2. On connect you'll receive one `register` frame per online daemon → build the
   daemon list from `payload` + the top-level `daemon` id.
3. To dispatch: generate an `id`, send a `command` envelope with `type:"command"`,
   `id`, `daemon:<target id>`, and the nested command (see contract). Track pending
   commands by `id`.
4. Route incoming frames by their `daemon` field (which daemon) and `id` (which
   command): append `log` lines; finalize on `result`.
5. Handle `daemon.offline` → mark that daemon offline. Handle a new `register` →
   add/refresh a daemon.
6. On socket close, reconnect with backoff; the relay re-sends the `register`
   snapshot so your list rehydrates.

### Minimal Dart client skeleton (Mode B)
```dart
import 'dart:convert';
import 'package:web_socket_channel/io.dart';

void connect() {
  final ch = IOWebSocketChannel.connect(
    Uri.parse('wss://relay.example.com/control'),
    headers: {'Authorization': 'Bearer $controlToken'},
  );

  ch.stream.listen((frame) {
    final msg = jsonDecode(frame as String) as Map<String, dynamic>;
    final daemon = msg['daemon'] as String?;      // which daemon this concerns
    switch (msg['type']) {
      case 'register':       /* add/refresh daemon $daemon with msg['payload'] */ break;
      case 'daemon.offline': /* mark $daemon offline */ break;
      case 'ack':            /* mark command msg['id'] running */ break;
      case 'log':            /* append msg['payload'] to log[msg['id']] */ break;
      case 'result':         /* finalize msg['id'] with msg['payload'] */ break;
    }
  }, onDone: () {/* reconnect with backoff */});

  // dispatch example:
  const id = 'c-0001';
  ch.sink.add(jsonEncode({
    'type': 'command', 'id': id, 'daemon': targetDaemonId,
    'payload': {'id': id, 'action': 'system.info'},
  }));
}
```

## Test / acceptance (Mode B)
Run all three locally first:
1. `./bin/aio-relay -addr :9090`
2. `./bin/aio-daemon -server ws://localhost:9090/agent`
3. Point the Flutter app at `ws://localhost:9090/control`.
4. **Success signal:** the app receives a `register` frame (with a `daemon` id) and
   lists the daemon with `providers: ["claude", ...]`.
5. Dispatch `system.info` with `daemon` set → `ack` then a `result` (`ok:true`,
   `data.workspaces` present), all carrying the same `daemon` id.
6. Dispatch `git` `{"workdir":"<repo in workspaces>","args":["status","--short"]}`
   → streamed `log` lines then `result` `exit_code:0`.

---

## Appendix — Mode A (direct LAN, Flutter as server)
Only if you deliberately want zero-infra LAN-only control. Here the Flutter app is
a WebSocket **server** and the daemon dials into it; there is **no `daemon` field**
(one socket = one daemon).

- Run a `dart:io` `HttpServer`; accept upgrades on path `/agent`.
- Read handshake headers: `Authorization: Bearer <token>` (validate; 401 on
  mismatch) and `X-Daemon-ID` (use as the connection key).
- The daemon sends WS PING frames every ~30s; `dart:io` auto-pongs.
- Same `register`/`ack`/`log`/`result` contract, minus `daemon`. Send commands
  without the `daemon` field.
- Start the daemon with `-server ws://<flutter-host>:8080/agent`.
- Reference implementation (server role, no GUI): `cmd/server/main.go` in the
  daemon repo.

## Open questions
- **[NEEDS INPUT] Control token value** and where the app stores it (secure
  storage on device). Also decide the relay's public host/URL for QA/PROD.
