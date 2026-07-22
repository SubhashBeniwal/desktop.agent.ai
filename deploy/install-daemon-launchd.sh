#!/usr/bin/env bash
#
# Install the aio-daemon as a macOS LaunchAgent so it runs at login and keeps
# reconnecting. The real plist (with your token) is written to
# ~/Library/LaunchAgents — never into the repo.
#
# Usage:
#   AIO_TOKEN=<agent-token> ./deploy/install-daemon-launchd.sh \
#       [server-url] [path-to-aio-daemon-binary]
#
# Defaults: server = wss://aio-relay-sb.duckdns.org/agent
#           binary = ./bin/aio-daemon (built by `make build`)
#
# Re-run any time to update settings; it reloads the agent.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_URL="${1:-wss://aio-relay-sb.duckdns.org/agent}"
BINARY="${2:-$REPO_ROOT/bin/aio-daemon}"
LABEL="com.aioagent.daemon"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOGDIR="$HOME/Library/Logs/aio-daemon"
TEMPLATE="$REPO_ROOT/deploy/com.aioagent.daemon.plist.example"

die() { printf 'error: %s\n' "$*" >&2; exit 1; }

[ -n "${AIO_TOKEN:-}" ] || die "AIO_TOKEN env var is required (your relay agent token)"
[ -f "$TEMPLATE" ] || die "template not found: $TEMPLATE"
BINARY="$(cd "$(dirname "$BINARY")" && pwd)/$(basename "$BINARY")" # absolutize
[ -x "$BINARY" ] || die "daemon binary not found/executable at $BINARY (run: make build)"

mkdir -p "$LOGDIR" "$(dirname "$PLIST")"

# Fill the template. Use a safe delimiter since values contain slashes.
sed \
	-e "s|__BINARY__|$BINARY|g" \
	-e "s|__SERVER_URL__|$SERVER_URL|g" \
	-e "s|__AIO_TOKEN__|$AIO_TOKEN|g" \
	-e "s|__LOGDIR__|$LOGDIR|g" \
	"$TEMPLATE" > "$PLIST"
chmod 600 "$PLIST" # contains the token

# Reload: unload if already present, then load.
launchctl unload "$PLIST" 2>/dev/null || true
launchctl load -w "$PLIST"

echo "installed LaunchAgent: $PLIST"
echo "  server: $SERVER_URL"
echo "  binary: $BINARY"
echo "  logs:   $LOGDIR/aio-daemon.{out,err}.log"
echo
echo "status:"
launchctl list | grep "$LABEL" || echo "  (not listed yet — check the error log)"
echo
echo "manage it:"
echo "  tail -f \"$LOGDIR/aio-daemon.err.log\"     # watch it connect"
echo "  launchctl unload \"$PLIST\"                 # stop + disable"
echo "  launchctl load -w \"$PLIST\"                # start + enable"
