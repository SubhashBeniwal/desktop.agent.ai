#!/usr/bin/env bash
#
# One-shot on-VM setup for the AIO relay on Oracle Cloud (Ubuntu, Ampere/AMD).
# Self-contained: it embeds the systemd unit and Caddyfile, so you only need to
# copy this script + the relay binary to the VM.
#
# Usage (on the VM, as root):
#   scp bin/aio-relay-linux-arm64 deploy/setup.sh ubuntu@<VM_IP>:/tmp/
#   ssh ubuntu@<VM_IP>
#   sudo RELAY_HOST=myrelay.duckdns.org bash /tmp/setup.sh /tmp/aio-relay-linux-arm64
#
# Or pass the host as the first arg and the binary as the second:
#   sudo bash /tmp/setup.sh myrelay.duckdns.org /tmp/aio-relay-linux-arm64
#
# Idempotent: safe to re-run. Existing tokens in /etc/aio-relay.env are kept
# unless you pass FORCE_TOKENS=1.
set -euo pipefail

# ---- inputs ---------------------------------------------------------------
# RELAY_HOST may come from the environment or as the first positional arg. The
# binary path is then the remaining positional arg (arg 1 if RELAY_HOST was in
# the env, arg 2 if RELAY_HOST was positional).
if [ -n "${RELAY_HOST:-}" ]; then
	BINARY="${1:-/tmp/aio-relay}"
else
	RELAY_HOST="${1:-}"
	BINARY="${2:-/tmp/aio-relay}"
fi
ENV_FILE="/etc/aio-relay.env"
INSTALL_DIR="/opt/aio-relay"
UNIT="/etc/systemd/system/aio-relay.service"
CADDYFILE="/etc/caddy/Caddyfile"

log()  { printf '\033[1;32m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!! \033[0m %s\n' "$*"; }
die()  { printf '\033[1;31mxx \033[0m %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (use sudo)"
[ -n "$RELAY_HOST" ] || die "RELAY_HOST is required (e.g. RELAY_HOST=myrelay.duckdns.org)"
case "$RELAY_HOST" in *.*) ;; *) die "RELAY_HOST '$RELAY_HOST' doesn't look like a hostname" ;; esac
[ -f "$BINARY" ] || die "relay binary not found at '$BINARY' (scp it over first, or pass its path as arg 2)"

export DEBIAN_FRONTEND=noninteractive

# ---- 1. install the binary -----------------------------------------------
log "Installing relay binary to $INSTALL_DIR/aio-relay"
install -d "$INSTALL_DIR"
install -m 0755 "$BINARY" "$INSTALL_DIR/aio-relay"

# ---- 2. instance firewall (ports 80/443) ---------------------------------
log "Opening ports 80 and 443 in the instance firewall (iptables)"
if ! command -v netfilter-persistent >/dev/null 2>&1; then
	echo 'iptables-persistent iptables-persistent/autosave_v4 boolean true' | debconf-set-selections
	echo 'iptables-persistent iptables-persistent/autosave_v6 boolean true' | debconf-set-selections
	apt-get update -y
	apt-get install -y iptables-persistent
fi
for port in 80 443; do
	if ! iptables -C INPUT -p tcp --dport "$port" -j ACCEPT 2>/dev/null; then
		iptables -I INPUT -p tcp --dport "$port" -j ACCEPT
		log "  added ACCEPT for tcp/$port"
	else
		log "  tcp/$port already allowed"
	fi
done
netfilter-persistent save
warn "Reminder: also open TCP 80 & 443 as INGRESS in the OCI VCN Security List/NSG — the script cannot do that part."

# ---- 3. tokens ------------------------------------------------------------
if [ -f "$ENV_FILE" ] && [ "${FORCE_TOKENS:-0}" != "1" ]; then
	log "Keeping existing tokens in $ENV_FILE (pass FORCE_TOKENS=1 to regenerate)"
	# shellcheck disable=SC1090
	. "$ENV_FILE"
else
	log "Generating tokens in $ENV_FILE"
	AIO_AGENT_TOKEN="$(openssl rand -hex 32)"
	AIO_CONTROL_TOKEN="$(openssl rand -hex 32)"
	umask 077
	cat >"$ENV_FILE" <<EOF
AIO_AGENT_TOKEN=$AIO_AGENT_TOKEN
AIO_CONTROL_TOKEN=$AIO_CONTROL_TOKEN
EOF
	chmod 600 "$ENV_FILE"
fi

# ---- 4. systemd service ---------------------------------------------------
log "Installing systemd unit $UNIT"
cat >"$UNIT" <<'EOF'
[Unit]
Description=AIO agent relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/aio-relay.env
ExecStart=/opt/aio-relay/aio-relay -addr 127.0.0.1:9090
Restart=always
RestartSec=2
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now aio-relay
systemctl restart aio-relay

# ---- 5. Caddy (TLS + reverse proxy) --------------------------------------
if ! command -v caddy >/dev/null 2>&1; then
	log "Installing Caddy"
	apt-get update -y
	apt-get install -y apt-transport-https ca-certificates curl gnupg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
		| gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
	curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
		> /etc/apt/sources.list.d/caddy-stable.list
	apt-get update -y
	apt-get install -y caddy
else
	log "Caddy already installed"
fi

log "Writing $CADDYFILE for $RELAY_HOST"
install -d /etc/caddy
cat >"$CADDYFILE" <<EOF
$RELAY_HOST {
	reverse_proxy 127.0.0.1:9090
}
EOF
systemctl restart caddy

# ---- 6. summary -----------------------------------------------------------
sleep 1
echo
log "Done. Service status:"
systemctl --no-pager --lines=0 status aio-relay caddy || true
echo
# shellcheck disable=SC1090
. "$ENV_FILE"
cat <<EOF

────────────────────────────────────────────────────────────────────────
Relay is up behind Caddy at:  https://$RELAY_HOST
  daemons  connect to:  wss://$RELAY_HOST/agent    (Bearer AIO_AGENT_TOKEN)
  control  connects to: wss://$RELAY_HOST/control  (Bearer AIO_CONTROL_TOKEN)

Tokens (store securely; also set the agent token on the daemon):
  AIO_AGENT_TOKEN   = $AIO_AGENT_TOKEN
  AIO_CONTROL_TOKEN = $AIO_CONTROL_TOKEN

Next:
  • Ensure DNS: $RELAY_HOST -> this VM's public IP (DuckDNS), and that
    TCP 80/443 ingress is open in the OCI Security List (see reminder above).
  • Watch Caddy get its cert:   sudo journalctl -u caddy -f
  • From your Mac, point the daemon at it:
      AIO_TOKEN=$AIO_AGENT_TOKEN ./bin/aio-daemon -server wss://$RELAY_HOST/agent
  • Smoke test TLS from anywhere:
      curl -i https://$RELAY_HOST/agent   # a 400/'upgrade' response = reachable
────────────────────────────────────────────────────────────────────────
EOF
