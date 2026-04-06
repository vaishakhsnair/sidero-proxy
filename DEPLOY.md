# Deployment Guide

## Architecture recap

```
Players → Proxy machines (Ubuntu 22.04, public IPs)
              ↓ Tailscale
          Hetzner/GorillaServer host nodes (Pterodactyl)
              ↓
          Redis machine (Tailscale-only, no public IP)
```

Each proxy advertises a unique `/16` subnet (`10.{n}.0.0/16`) via Tailscale so return traffic reaches the correct proxy.

---

## 1. Tailscale

Run on every machine in the fleet (proxies, host nodes, Redis machine):

```bash
curl -fsSL https://tailscale.com/install.sh | sh
```

**On each proxy** (substitute `n` = proxy number, e.g. 1, 2, 3...):
```bash
tailscale up --advertise-routes=10.{n}.0.0/16
```

**On each host node** (Hetzner, GorillaServer):
```bash
tailscale up --accept-routes
```

**On the Redis machine:**
```bash
tailscale up
```

---

## 2. Redis machine

```bash
apt install -y docker.io docker-compose-plugin
```

Clone the repo and configure:
```bash
git clone https://github.com/siderocloud/sidero-proxy /opt/mcproxy
cd /opt/mcproxy
cp .env.example .env
```

Edit `.env`:
```
REDIS_BIND=<tailscale IP of this machine>
REDIS_PASSWORD=<strong password>
```

Start Redis:
```bash
docker compose --profile redis up -d
```

Verify:
```bash
redis-cli -h <tailscale IP> -a <password> ping
# → PONG
```

---

## 3. Proxy machine (repeat for each proxy)

### 3a. Install Docker

```bash
apt install -y docker.io docker-compose-plugin
```

### 3b. Apply static nftables rules (once per machine)

```bash
git clone https://github.com/siderocloud/sidero-proxy /opt/mcproxy
cd /opt/mcproxy

# NAT table (nat_map for SNAT)
nft -f deploy/nftables-proxy.conf

# DDoS pre-filter (blocklist + SYN rate limits)
nft -f deploy/nftables-filter.conf

# TPROXY intercept — edit the file first to set your actual public IPs
nano deploy/nftables-tproxy.conf
nft -f deploy/nftables-tproxy.conf
```

Persist nftables across reboots:
```bash
# Option A — include in system nftables config
echo 'include "/opt/mcproxy/deploy/nftables-proxy.conf"' >> /etc/nftables.conf
echo 'include "/opt/mcproxy/deploy/nftables-filter.conf"' >> /etc/nftables.conf
echo 'include "/opt/mcproxy/deploy/nftables-tproxy.conf"' >> /etc/nftables.conf
systemctl enable nftables

# Option B — ExecStartPre in mcproxy.service (see deploy/mcproxy.service)
```

### 3c. Policy routing for TPROXY (once per machine)

```bash
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100
```

Persist across reboots — add to `/etc/rc.local` (create if missing):
```bash
cat >> /etc/rc.local << 'EOF'
ip rule add fwmark 1 lookup 100 2>/dev/null || true
ip route add local 0.0.0.0/0 dev lo table 100 2>/dev/null || true
EOF
chmod +x /etc/rc.local
```

### 3d. Sysctl (once per machine)

```bash
cp /opt/mcproxy/deploy/sysctl.conf /etc/sysctl.d/99-mcproxy.conf
sysctl -p /etc/sysctl.d/99-mcproxy.conf
```

### 3e. Configure and start mcproxy

```bash
mkdir -p /etc/mcproxy
cp /opt/mcproxy/deploy/config.example.json /etc/mcproxy/config.json
nano /etc/mcproxy/config.json
```

Required fields:
| Field | Value |
|---|---|
| `proxy_id` | Unique per proxy, e.g. `proxy-1` |
| `redis_addr` | `<tailscale IP of Redis machine>:6379` |
| `redis_password` | Matches `.env` `REDIS_PASSWORD` |
| `tproxy_port` | `8080` (must match `nftables-tproxy.conf`) |
| `nodes[].public_ip` | Public IP of each game node |
| `nodes[].tailscale_ip` | Tailscale IP of that node |

```bash
cd /opt/mcproxy
docker compose --profile proxy up -d
docker compose logs -f mcproxy
```

Expected startup log:
```
proxy proxy-1 registered: subnet 10.1.0.0/16 (num 1)
enforcer: loaded 0 banned real IPs, 0 internal IPs
router: listening on 127.0.0.1:8080
```

### 3f. Advertise Tailscale subnet

After first startup the proxy has its subnet allocated. If not done already:
```bash
# Confirm subnet from Redis
redis-cli -h <redis tailscale IP> -a <password> HGET proxy:registry proxy-1
# → {"subnet":"10.1.0.0/16",...}

tailscale up --advertise-routes=10.1.0.0/16
```

Approve the route in the Tailscale admin console (or via ACL if using taildrop).

---

## 4. Host node (Hetzner / GorillaServer)

```bash
apt install -y docker.io docker-compose-plugin
git clone https://github.com/siderocloud/sidero-proxy /opt/mcproxy
cd /opt/mcproxy

mkdir -p /etc/mcproxy
cp deploy/watcher.example.json /etc/mcproxy/watcher.json
nano /etc/mcproxy/watcher.json
```

Required fields:
| Field | Value |
|---|---|
| `redis_addr` | `<tailscale IP of Redis machine>:6379` |
| `redis_password` | Matches Redis config |
| `volumes_root` | Path to Pterodactyl volumes (default: `/var/lib/pterodactyl/volumes`) |

```bash
docker compose --profile watcher up -d
docker compose logs -f mcwatcher
```

Expected startup log:
```
mcwatcher: watching /var/lib/pterodactyl/volumes
```

---

## 5. Verification

### TPROXY intercept working

From an external machine, connect to any port on a proxy public IP:
```bash
nc -zv <proxy public IP> 25565
nc -zv <proxy public IP> 25999   # arbitrary port — also intercepted
```

### IP mapping and SNAT

Connect a Minecraft client. Then on the proxy host:
```bash
nft list map ip nat nat_map
# → elements = { 1.2.3.4 : 10.1.0.5 }

redis-cli -h <redis IP> -a <password> HGET map:1.2.3.4 proxy-1
# → 10.1.0.5

redis-cli -h <redis IP> -a <password> GET rmap:10.1.0.5
# → {"real_ip":"1.2.3.4","proxy":"proxy-1"}
```

The Minecraft server should log the player connected from `10.1.0.x`.

### Ban enforcement

```bash
# Watch for ban events in real time
redis-cli -h <redis IP> -a <password> SUBSCRIBE ban_events

# Manually trigger: append an entry to a server's banned-ips.json
# (use the internal IP the server sees, e.g. 10.1.0.5)
echo '[{"ip":"10.1.0.5","source":"Operator","expires":"forever","reason":"test","created":"2024-01-01 00:00:00 +0000"}]' \
  > /var/lib/pterodactyl/volumes/<uuid>/banned-ips.json

# Within seconds you should see the ban_events message,
# the file should be cleared, and:
redis-cli -h <redis IP> -a <password> SISMEMBER banned:real 1.2.3.4
# → 1
```

---

## 6. Adding a new node

1. Add the node entry to `config.json` on every proxy:
   ```json
   { "public_ip": "NEW.IP", "tailscale_ip": "100.x.x.NEW", "name": "node-3" }
   ```

2. Update `nftables-tproxy.conf` to include the new public IP in the `ip daddr` set, then reapply:
   ```bash
   nft -f deploy/nftables-tproxy.conf
   ```

3. Restart mcproxy on each proxy machine:
   ```bash
   docker compose --profile proxy restart mcproxy
   ```

4. Run mcwatcher on the new host node (see section 4).

---

## 7. Upgrading

```bash
cd /opt/mcproxy
git pull
docker compose --profile <proxy|watcher|redis> build
docker compose --profile <proxy|watcher|redis> up -d
```

mcproxy flushes its NAT map on startup — active connections will drop during the restart window (~1–2s).
