# Handoff: Daemon control app — aio.agent.ai (Go daemon) → aio.flutter   (direction: backend→frontend)

## Changed since last version
- **New: Remote apps (phase 2).** Stream a desktop app's window (VS Code, Android
  Studio, IntelliJ, Brave, …) to the phone over **WebRTC** and control it with
  touch and keyboard. New actions: `stream.config`, `screen.windows`, `app.list`,
  `app.launch`, `stream.start`, `stream.stop`. See **Remote apps** below. No relay
  changes: signaling rides on the existing `command` → `result` round trip.
- **Feature-gate on `register.actions`.** Only daemons that list `stream.start`
  support streaming. In v1 that means **macOS** daemons running the desktop app
  or a cgo build. Windows streaming comes later. Hide the Remote apps UI for
  daemons that don't advertise it.
- **`daemon_id` is now stable per machine.** The desktop app saves it on first
  launch, so it's safe to key favorites and history by `daemon_id`. `name` is a
  user-editable display name; show `name`, key by `daemon_id`.
- **Users can disconnect a daemon from its tray menu.** You'll receive
  `daemon.offline` for it, the same as a network drop.
- Removed a duplicated "Message envelope" section; the envelope itself is unchanged.

## Goal
Build a Flutter app that acts as the **control plane** for the `aio-daemon`: it
shows the registered daemon(s), dispatches commands, and renders the
`ack → log → result` stream in real time. It can also stream and remote-control
desktop apps on the daemon's machine (see **Remote apps**).

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
| `stream.config` | `{}` | streaming support, ICE servers, macOS permissions (see **Remote apps**) |
| `screen.windows` | `{}` | streamable windows and displays |
| `app.list` | `{}` | apps installed on the daemon host |
| `app.launch` | `{ "name"?: "Visual Studio Code", "path"?: "/Applications/….app", "args"?: ["/abs/project"] }` | launch or focus an app; `args` paths must be inside `workspaces` |
| `stream.start` | `{ "target": {…}, "offer": {"type":"offer","sdp":"…"}, … }` | WebRTC signaling; the result carries the answer |
| `stream.stop` | `{ "session_id": "s-…" }` | end a stream |

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
| `stream.*`, `screen.windows`, `app.*` | see **Remote apps** |

## Remote apps (phase 2) — WebRTC streaming + input

### How it fits together
```
phone ──command stream.start{offer}──▶ relay ──▶ daemon
phone ◀──result{answer}─────────────── relay ◀── daemon
phone ◀═══ WebRTC (P2P, TURN fallback): H.264 video + "input" data channel ═══▶ daemon
```
- The relay carries only the one-shot signaling. Video never touches the relay
  WebSocket.
- **No trickle ICE:** both sides gather all candidates before sending their SDP.
  On the phone, wait until ICE gathering is `complete`, or 3 s at most, then
  send `getLocalDescription()`. The daemon does the same before replying.
- **The phone is the offerer.** It adds a **recv-only video** transceiver and
  creates the **`input`** data channel. The daemon answers with a send-only
  H.264 track. There is no audio.

### Typical flow
1. `stream.config` → check `supported`, read `ice_servers` and `permissions`.
2. `screen.windows` → let the user pick a window (or `app.launch` an app first,
   wait ~1–2 s, then refresh `screen.windows`).
3. Build the RTCPeerConnection with `ice_servers` → offer → gather → `stream.start`.
4. `setRemoteDescription(answer)` → video arrives on `onTrack`; the `input` channel opens.
5. Send input events on the channel. On leaving the screen, call `stream.stop`
   and close the peer connection.

### `stream.config` → `result.data`
```json
{
  "supported": true,
  "ice_servers": [ { "urls": ["stun:stun.l.google.com:19302"] },
                   { "urls": ["turn:<host>:3478"], "username": "<u>", "credential": "<p>" } ],
  "permissions": { "screen_recording": true, "accessibility": false },
  "defaults": { "max_fps": 30, "max_size": 1600, "bitrate_kbps": 4000 },
  "max_sessions": 2
}
```
- Pass `ice_servers` straight into the `RTCPeerConnection` configuration. The
  daemon owns the STUN/TURN config; **don't hardcode TURN credentials in the app.**
- `permissions.screen_recording=false` → `screen.windows` and `stream.start` will
  fail. Tell the user to approve **AIO Agent** under System Settings → Privacy &
  Security → **Screen Recording** on the Mac, then restart the app there.
- `permissions.accessibility=false` → video works, but **input is ignored**.
  Show a banner pointing to Privacy & Security → **Accessibility**.

### `screen.windows` → `result.data`
```json
{
  "windows": [
    { "id": 4721, "app": "Code", "bundle_id": "com.microsoft.VSCode", "pid": 812,
      "title": "main.go — aio.agent.ai", "width": 1512, "height": 945, "on_screen": true }
  ],
  "displays": [ { "id": 1, "width": 1512, "height": 982 } ]
}
```
- `id` is a **number** (a macOS CGWindowID). Pass it back unchanged.
- Sizes are in screen points. Only normal app windows are listed; menu bar,
  Dock and the like are filtered out. Minimized windows have `on_screen:false`
  and can't be streamed until restored.

### `app.list` → `result.data`
```json
{ "apps": [ { "name": "Visual Studio Code", "path": "/Applications/Visual Studio Code.app" },
            { "name": "Brave Browser", "path": "/Applications/Brave Browser.app" } ] }
```
Use it for a "Launch" picker; pin IDEs and browsers at the top.

### `app.launch`
- Request: `{ "name": "Android Studio" }` or `{ "path": "/Applications/Android Studio.app" }`,
  optional `"args": ["/Users/me/Development/my-app"]` to open a project or folder.
  Every `args` entry must be an absolute path inside the daemon's `workspaces`.
- Success: `{ "launched": "Android Studio" }`. If the app is already running, it
  is brought to the front. Windows can take seconds to appear, so poll
  `screen.windows`.

### `stream.start`
Request (`payload.payload`):
| field | type | required | notes |
|---|---|---|---|
| `target` | object | yes | `{ "window_id": 4721 }` **or** `{ "display_id": 1 }` |
| `offer` | object | yes | `{ "type": "offer", "sdp": "<full SDP after ICE gathering>" }` |
| `max_fps` | int | no | default 30, clamped to 1–60 |
| `max_size` | int | no | longest video edge in px; default 1600 (window is scaled down, never up) |
| `bitrate_kbps` | int | no | default 4000 |
| `focus` | bool | no | default `true`: bring the window's app to the front so input lands in it |

Success `result.data`:
```json
{ "session_id": "s-3f9c0a1b", "answer": { "type": "answer", "sdp": "…" }, "width": 1600, "height": 1000 }
```
`width`/`height` are the initial video pixel size.

### `stream.stop`
Request `{ "session_id": "s-…" }` → `{ "stopped": "s-…" }`. Stopping an unknown
or already-ended session still returns `ok:true`. A session also ends on its own
if the peer connection fails or closes (about 15 s after the phone vanishes),
if the window closes, or if the daemon disconnects.

### Errors (`result.ok=false`, `error` starts with a code — branch on the prefix)
| `error` prefix | when | what the app should do |
|---|---|---|
| `unknown action "stream.start"` | daemon OS/build has no streaming (the action isn't in `register.actions`, and `stream.config.supported` is `false`) | hide Remote apps for this daemon. You shouldn't hit this if you feature-gate on `actions` |
| `E_PERMISSION_SCREEN:` | Screen Recording not granted on the Mac | show the permission instructions above |
| `E_WINDOW_NOT_FOUND:` | window closed or minimized since the list was fetched | refresh `screen.windows` |
| `E_BAD_OFFER:` | SDP invalid, or no **H.264** codec in the offer | log it; this is a client bug or the device lacks H.264 |
| `E_LIMIT:` | already `max_sessions` streams on this daemon | ask the user to close another stream |
| `E_NOT_FOUND:` | `app.launch` app not found | show the error |
| (no prefix) | anything else, e.g. a path outside workspaces | show `error` as is |

### Video
- **H.264 Constrained Baseline**, hardware-encoded on the Mac, up to `max_fps`.
  macOS only sends a frame when the window changes, so a static window means a
  static stream. That's normal; don't treat it as a stall.
- The daemon re-sends a keyframe when it receives an RTCP PLI (flutter_webrtc
  sends these automatically) or a `{"t":"keyframe"}` message.
- Render it with `RTCVideoView(renderer, objectFit: RTCVideoViewObjectFit.RTCVideoViewObjectFitContain)`.

### `input` data channel protocol (JSON text messages)
The phone creates it: `createDataChannel('input', RTCDataChannelInit()..ordered = true)`.
**Coordinates are normalized to the video frame:** `x` and `y` run from 0 to 1,
with the origin at the top-left. Compute them against the **displayed video
rect**, excluding letterbox bars from `objectFit: contain`, using the latest
video size from `hello`/`size`.

Phone → daemon:
| message | meaning |
|---|---|
| `{"t":"pointer","a":"move","x":0.42,"y":0.31}` | move the cursor (hover) |
| `{"t":"pointer","a":"down","x":…,"y":…,"b":"left"}` | press; `b` ∈ `left` (default), `right`, `middle` |
| `{"t":"pointer","a":"up","x":…,"y":…,"b":"left"}` | release (drag = down → moves → up) |
| `{"t":"click","x":…,"y":…,"b":"left","n":1}` | down+up in one message; `n:2` = double-click |
| `{"t":"scroll","x":…,"y":…,"dx":0,"dy":120}` | scroll at a point; deltas in **pixels**, `dy>0` scrolls content down (like browser `wheel.deltaY`) |
| `{"t":"key","a":"press","code":"Enter","mods":["meta"]}` | key `press` (down+up), `down`, or `up`. `code` is a W3C `KeyboardEvent.code` (`KeyA`, `Digit1`, `Enter`, `Backspace`, `Tab`, `Escape`, `Space`, `ArrowLeft`, `F5`, `Delete`, …). `mods` ⊆ `meta` (⌘), `ctrl`, `alt` (⌥), `shift` |
| `{"t":"text","s":"hello wörld"}` | type a Unicode string; use this for soft-keyboard text |
| `{"t":"keyframe"}` | ask for an immediate keyframe |

Daemon → phone:
| message | meaning |
|---|---|
| `{"t":"hello","w":1600,"h":1000,"input":true}` | sent when the channel opens. `input:false` means Accessibility isn't granted and input will be ignored |
| `{"t":"size","w":1440,"h":900}` | the video size changed (window resized) |
| `{"t":"end","reason":"window closed"}` | the daemon is ending the session; close the peer connection and go back to the picker |

Unknown message types are ignored on both sides. Add fields freely; never
rename existing ones.

Suggested gesture mapping (UX is up to you):
- **tap** → `click`
- **double-tap** → `click` `n:2`
- **long-press** → `click` `b:right`
- **one-finger pan** → pointer down/move/up (drag)
- **two-finger pan** → `scroll`
- **pinch** → zoom the view locally; don't send it
- **Keyboard:** a hidden `TextField` drives the soft keyboard. Send typed
  characters as `text`; Backspace, Enter and Tab as `key` `press`.
- **Toolbar:** Esc, Tab, arrows, ⌘/Ctrl/⌥/⇧ toggles (applied to the next
  `key`), and common shortcuts (⌘S, ⌘P, ⌘⇧P…).

### Minimal Dart sketch (flutter_webrtc)
```dart
final cfg = await sendCommand(daemonId, 'stream.config');            // your existing helper
final pc = await createPeerConnection({'iceServers': cfg['ice_servers'], 'sdpSemantics': 'unified-plan'});
await pc.addTransceiver(kind: RTCRtpMediaType.RTCRtpMediaTypeVideo,
    init: RTCRtpTransceiverInit(direction: TransceiverDirection.RecvOnly));
final input = await pc.createDataChannel('input', RTCDataChannelInit()..ordered = true);
pc.onTrack = (e) { if (e.track.kind == 'video') renderer.srcObject = e.streams.first; };
input.onMessage = (m) { final ev = jsonDecode(m.text); /* hello | size | end */ };

final gathered = Completer<void>();
pc.onIceGatheringState = (s) {
  if (s == RTCIceGatheringState.RTCIceGatheringStateComplete && !gathered.isCompleted) gathered.complete();
};
await pc.setLocalDescription(await pc.createOffer());
await gathered.future.timeout(const Duration(seconds: 3), onTimeout: () {});
final offer = await pc.getLocalDescription();

final res = await sendCommand(daemonId, 'stream.start', {
  'target': {'window_id': windowId},
  'offer': {'type': offer!.type, 'sdp': offer.sdp},
});
await pc.setRemoteDescription(RTCSessionDescription(res['answer']['sdp'], res['answer']['type']));
sessionId = res['session_id'];

void tap(double nx, double ny) =>
    input.send(RTCDataChannelMessage(jsonEncode({'t': 'click', 'x': nx, 'y': ny})));
```

### Security
Anyone holding the control token can **see and control** the Mac's screen. Keep
the token in secure storage, and show a clear "streaming" indicator while a
session is live.

## Ownership
- **Flutter must supply:** the command `id` (unique per command), `action`, and
  the action's argument payload. For `agent.run`/`git`/`deploy`/`pr.review`, an
  **absolute `workdir`** that is inside one of the daemon's configured
  `workspaces` (get the allowed roots from `system.info` → `workspaces`).
- **Daemon resolves, do NOT send:** provider CLI resolution, availability, the
  sandbox/workspace enforcement, process spawning. Any `path`/`workdir` outside
  the daemon's workspaces is rejected with `result.ok=false` — surface that error;
  don't try to work around it.
- **Remote apps — Flutter supplies:** the WebRTC offer (after full ICE gathering),
  the `target` window or display id taken from `screen.windows`, the `input`
  data channel, and normalized input coordinates.
- **Remote apps — the daemon resolves, do NOT send or hardcode:** ICE/TURN
  servers and credentials (from `stream.config`), codec choice (H.264),
  scaling, frame rate caps, window focus, and the macOS permission state.

## Config / env (Flutter side, Mode B)
| setting | value source | notes |
|---|---|---|
| relay URL | app setting | `wss://<relay-host>/control` in prod (`ws://` only for local testing) |
| control token | app setting (secret) | **[NEEDS INPUT]** — decide the token; sent as `Authorization: Bearer <token>`. Must equal the relay's `-control-token` / `AIO_CONTROL_TOKEN`. Store a name/placeholder in the repo, never the real value; keep it in secure storage on device. |
| default daemon id | runtime, from `register` | which daemon to target when the user hasn't picked one |
| ICE servers (STUN/TURN) | runtime, from `stream.config` | not an app setting; configured on the daemon |
| packages | pubspec | `flutter_webrtc` (video + data channel). iOS: no camera or mic permissions needed (receive-only). Android: `INTERNET` permission |

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

### Remote apps acceptance (macOS daemon running the desktop app)
1. On the Mac, grant **AIO Agent** Screen Recording and Accessibility (System
   Settings → Privacy & Security), then restart the app.
2. `stream.config` → `supported:true`, both permissions `true`.
3. `app.launch {"name":"Visual Studio Code"}` → `ok:true`. After ~2 s,
   `screen.windows` lists a window with `app:"Code"`.
4. `stream.start` with that `window_id` and your offer → `ok:true` with
   `answer.sdp`. **Success signal:** video of the VS Code window renders within
   ~2 s, and the channel receives `{"t":"hello",…,"input":true}`.
5. Tap inside the editor, then send `{"t":"text","s":"hello"}`. The text appears
   in VS Code and in the stream.
6. `stream.stop` → `ok:true`, and the video stops.
7. Negative check: `stream.start` with `{"window_id": 1}` →
   `ok:false`, `error` starts with `E_WINDOW_NOT_FOUND:`.

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
- **[NEEDS INPUT] TURN server.** Until a TURN server runs on the OCI relay host,
  `stream.config` returns STUN only. Streaming then works on most Wi-Fi but can
  fail on some cellular or symmetric-NAT networks (ICE state `failed`). Show a
  "couldn't connect directly" error in that case. The daemon side will add TURN
  config without any change to this contract.
- **Windows daemons:** streaming is not supported in v1. `stream.start`,
  `screen.windows` and `app.*` are absent from `actions`, and `stream.config`
  returns `supported:false`.
