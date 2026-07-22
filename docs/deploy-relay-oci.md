# Deploying the relay on Oracle Cloud Free Tier (DuckDNS + Caddy)

Interim setup with **no domain**: a free DuckDNS hostname + Caddy for real TLS.
When you get a domain later, only the hostname in `deploy/Caddyfile` changes.

Target: an **Always Free Ampere A1 (ARM64)** Ubuntu VM. (An AMD `E2.1.Micro` also
works — if you use it, build with `GOARCH=amd64` instead of `arm64`.)

---

## Fast path: `deploy/setup.sh`
Steps 1–3 (VM, OCI Security List, DuckDNS) are console/DNS actions you still do by
hand. Everything **on the VM** (steps 4–7: install binary, iptables, tokens,
systemd, Caddy) is automated by `deploy/setup.sh`:

```sh
# on your Mac — build + copy binary and script
export PATH="/opt/homebrew/bin:$PATH"
GOOS=linux GOARCH=arm64 go build -o bin/aio-relay-linux-arm64 ./cmd/relay
scp bin/aio-relay-linux-arm64 deploy/setup.sh ubuntu@<VM_IP>:/tmp/

# on the VM
sudo RELAY_HOST=myrelay.duckdns.org bash /tmp/setup.sh /tmp/aio-relay-linux-arm64
```

It generates the tokens, prints them, and shows the exact daemon command to run.
Re-running is safe (tokens are preserved unless you pass `FORCE_TOKENS=1`). The
manual steps below are the same actions, for reference or troubleshooting.

---

## 1. Create the VM
- OCI console → Compute → Instances → Create.
- Image: **Ubuntu 22.04/24.04**. Shape: **VM.Standard.A1.Flex**, 1 OCPU / 6 GB
  (well within Always Free).
- Add your SSH public key.
- **Reserve the public IP:** after creation, edit the VNIC's public IP and set it
  to **Reserved** so it survives stop/start (DuckDNS points at it).

## 2. Open the ports — BOTH steps (this is the classic OCI trap)

**2a. VCN Security List (or NSG):** Networking → your VCN → Security Lists →
default → Add Ingress Rules. Add two stateful rules, source `0.0.0.0/0`,
IP protocol TCP, destination ports **80** and **443**.

**2b. The instance firewall:** OCI Ubuntu images ship with an iptables `REJECT`
rule. SSH in and insert ACCEPT rules *above* it, then persist:
```sh
sudo iptables -I INPUT -p tcp --dport 80  -j ACCEPT
sudo iptables -I INPUT -p tcp --dport 443 -j ACCEPT
sudo netfilter-persistent save
```
(If nothing connects later, 99% of the time it's one of these two steps.)

## 3. DuckDNS hostname
- Sign in at https://www.duckdns.org (GitHub/Google), create a subdomain, e.g.
  `myrelay` → you get `myrelay.duckdns.org`.
- Set its IP to your VM's **reserved public IP** (in the DuckDNS dashboard).
- Verify from your laptop: `dig +short myrelay.duckdns.org` → your VM IP.

## 4. Build the relay for the VM and copy it over
On your Mac (ARM VM → arm64):
```sh
export PATH="/opt/homebrew/bin:$PATH"
GOOS=linux GOARCH=arm64 go build -o bin/aio-relay-linux-arm64 ./cmd/relay
scp bin/aio-relay-linux-arm64 ubuntu@<VM_IP>:/tmp/aio-relay
```
On the VM:
```sh
sudo mkdir -p /opt/aio-relay
sudo mv /tmp/aio-relay /opt/aio-relay/aio-relay
sudo chmod +x /opt/aio-relay/aio-relay
```

## 5. Tokens + systemd service
On the VM (copy the files from this repo's `deploy/` dir, or recreate them):
```sh
# tokens
openssl rand -hex 32   # -> AIO_AGENT_TOKEN
openssl rand -hex 32   # -> AIO_CONTROL_TOKEN
sudo tee /etc/aio-relay.env >/dev/null <<'EOF'
AIO_AGENT_TOKEN=<paste first>
AIO_CONTROL_TOKEN=<paste second>
EOF
sudo chmod 600 /etc/aio-relay.env

# service
sudo cp aio-relay.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now aio-relay
systemctl status aio-relay          # should be active (running)
```

## 6. Caddy (TLS + reverse proxy)
```sh
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy

# put your DuckDNS hostname in the Caddyfile, then:
sudo cp Caddyfile /etc/caddy/Caddyfile     # edit YOUR_SUBDOMAIN first
sudo systemctl restart caddy
sudo journalctl -u caddy -f                 # watch it obtain the cert
```

## 7. Verify end to end
Point the daemon at the relay (on your Mac):
```sh
AIO_TOKEN=<AIO_AGENT_TOKEN> ./bin/aio-daemon -server wss://myrelay.duckdns.org/agent
```
The daemon log should show `connected to control plane`. The control app connects
to `wss://myrelay.duckdns.org/control` with `Authorization: Bearer <AIO_CONTROL_TOKEN>`.

Quick check without the app — from anywhere:
```sh
curl -i https://myrelay.duckdns.org/agent      # 400/upgrade-required = TLS + relay reachable
```

---

## Later: switching to your own domain
1. Point `relay.yourdomain.com` (A record) at the same reserved IP.
2. Change the hostname line in `/etc/caddy/Caddyfile`; `sudo systemctl reload caddy`
   (Caddy fetches a new cert automatically).
3. Update the daemon's `-server` and the app's relay URL. Done.

## Notes
- The relay keeps connected-daemon state **in memory** → run a single instance.
  Fine for Always Free; if you ever need HA you'll add sticky routing or shared
  pub/sub (not now).
- Rotate tokens by editing `/etc/aio-relay.env` and
  `sudo systemctl restart aio-relay` (daemons/app reconnect with the new token).
