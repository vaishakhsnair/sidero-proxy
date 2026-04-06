# mcproxy Implementation Plan

## Context

Siderocloud runs Minecraft servers in Pterodactyl containers on Hetzner and GorillaServer nodes.
Players connect via separate proxy machines. The proxy must rewrite player source IPs so the
Minecraft server can do meaningful `/ban-ip` — without any server-side modifications. This is done
via kernel SNAT (nftables map) where each real player IP gets a stable internal IP (10.x.x.x) per
proxy. Redis is the authoritative mapping store. Ban events detected on the host propagate to all
proxies via pub/sub.

**Environment confirmed:**
- Proxy machines: Ubuntu 22.04, nftables v1.0.9, kernel 6.8.0
- Hetzner host: Ubuntu 22.04, nftables v1.0.2, kernel 5.15.0
- Banned file paths: `/var/lib/pterodactyl/volumes/{uuid}/banned-ips.json`
- All nftables operations via `github.com/google/nftables` netlink — no nft binary exec

---

## Two Binaries

```
cmd/proxy/main.go    → builds mcproxy   — runs on proxy machines (Ubuntu 22.04)
cmd/watcher/main.go  → builds mcwatcher — runs on Hetzner/GorillaServer hosts
```

---

## Module

```
module sidero-proxy
go 1.21
```

Dependencies:
- `github.com/redis/go-redis/v9`
- `github.com/google/nftables` (netlink — no nft binary exec)
- `golang.org/x/sys/unix` (inotify in watcher, SO_ORIGINAL_DST for TPROXY)

---

## Routing Model — TPROXY

The proxy intercepts **all TCP traffic** on all ports destined for configured public IPs using
nftables TPROXY. A single local listener receives all intercepted connections. The original
destination IP and port are recovered via `SO_ORIGINAL_DST` syscall. This gives nginx-style
"all ports" behavior with no port range config needed.

```
Player → proxy_public_ip:ANY_PORT
  ↓
nftables TPROXY → redirect to 127.0.0.1:8080 (local listener)
  ↓
Go reads SO_ORIGINAL_DST → original dst IP + port
  ↓
Lookup tailscale_ip from nodeMap by dst IP
  ↓
Forward to tailscale_ip:original_port
```

---

## config.json (proxy)

```json
{
  "proxy_id": "proxy-1",
  "redis_addr": "100.x.x.x:6379",
  "redis_password": "...",
  "ip_ttl_seconds": 604800,
  "tproxy_port": 8080,
  "nodes": [
    { "public_ip": "1.2.3.4", "tailscale_ip": "100.x.x.1", "name": "node-1" },
    { "public_ip": "1.2.3.5", "tailscale_ip": "100.x.x.2", "name": "node-2" }
  ]
}
```

No `listen_ports` needed — TPROXY intercepts all ports on all configured public IPs.

## config.json (watcher)

```json
{
  "redis_addr": "100.x.x.x:6379",
  "redis_password": "...",
  "volumes_root": "/var/lib/pterodactyl/volumes"
}
```

`volumes_root` is required and configurable per node — GorillaServer nodes may use a different path
than Hetzner. Watcher auto-discovers all UUIDs under `volumes_root` at startup and watches for new
ones via inotify on the parent directory.

---

## Phase 1 — Basic TCP Forwarder

### `internal/config/config.go`
```go
type NodeConfig struct {
    PublicIP    string `json:"public_ip"`
    TailscaleIP string `json:"tailscale_ip"`
    Name        string `json:"name"`
}
type Config struct {
    ProxyID       string       `json:"proxy_id"`
    RedisAddr     string       `json:"redis_addr"`
    RedisPassword string       `json:"redis_password"`
    IPTTLSeconds  int          `json:"ip_ttl_seconds"`
    TProxyPort    int          `json:"tproxy_port"`
    Nodes         []NodeConfig `json:"nodes"`
}
func Load(path string) (*Config, error)
```

### `internal/tproxy/tproxy.go`
```go
// GetOriginalDst recovers the original destination IP and port from a TPROXY-intercepted conn
// Uses SO_ORIGINAL_DST via golang.org/x/sys/unix
func GetOriginalDst(conn net.Conn) (dstIP string, dstPort int, err error)
```

### `internal/forwarder/forwarder.go`
```go
// Bidirectional TCP copy between two net.Conn
func Forward(client, backend net.Conn)
// Uses two goroutines + io.Copy each direction
// Closes both conns when either side closes
```

### `internal/router/router.go`
```go
type Router struct {
    cfg     *config.Config
    nodeMap map[string]string   // public_ip → tailscale_ip, built from config
}
func New(cfg *config.Config) *Router
func (r *Router) Start(ctx context.Context) error
// Single listener: net.Listen("tcp", "127.0.0.1:{tproxy_port}")
// Accept loop → goroutine:
//   1. tproxy.GetOriginalDst(conn) → dstIP, dstPort
//   2. tailscaleIP = nodeMap[dstIP] → unknown dst? close conn
//   3. srcIP = conn.RemoteAddr() host part
//   4. enforcer.IsBanned(srcIP) → close if true
//   5. assignment.Assign(...) → "BANNED" → close; else internalIP
//   6. nat.Add(srcIP, internalIP)
//   7. dial tailscaleIP:dstPort
//   8. forwarder.Forward(client, backend)
//   9. defer nat.Delete(srcIP)
// Graceful shutdown via ctx cancellation (listener.Close)
```

### `deploy/nftables-tproxy.conf` (applied once on proxy host, not by the binary)
```
# TPROXY intercept — redirects all TCP on node public IPs to local listener
# Replace 1.2.3.4 and 1.2.3.5 with actual proxy public IPs
# Reapply when adding new nodes

table ip mangle {
  chain PREROUTING {
    type filter hook prerouting priority mangle;
    ip daddr { 1.2.3.4, 1.2.3.5 } tcp \
      tproxy to 127.0.0.1:8080 meta mark set 1
  }
}

# Policy routing — local socket must receive TPROXY packets
# Run once after boot (or add to /etc/rc.local):
# ip rule add fwmark 1 lookup 100
# ip route add local 0.0.0.0/0 dev lo table 100
```

### `cmd/proxy/main.go`
- Load config
- Build nodeMap from config.Nodes
- Write node mappings to Redis: `SET node:{public_ip} {json}` for each node
- Initialize router
- signal.NotifyContext(SIGTERM, SIGINT)
- router.Start(ctx) — blocks until ctx done

---

## Phase 2 — IP Mapping + NAT

### `internal/registration/registration.go`
```go
type ProxyInfo struct {
    Subnet       string `json:"subnet"`
    TailscaleIP  string `json:"tailscale_ip"`
    RegisteredAt int64  `json:"registered_at"`
}
func Register(ctx context.Context, rdb *redis.Client, proxyID string) (proxyNum int, subnet string, err error)
// HGET proxy:registry → proxyID → already registered? return proxyNum
// else: INCR proxy:subnet_counter → n, store proxy:registry[proxyID] = {subnet: "10.{n}.0.0/16", ...}
// Run: tailscale up --advertise-routes=10.{n}.0.0/16

func WriteNodeMap(ctx context.Context, rdb *redis.Client, nodes []config.NodeConfig) error
// For each node: SET node:{public_ip} {json: tailscale_ip, name}
// Called on every proxy startup — all proxies write the same data, last-write is fine
```

### `internal/assignment/assignment.go`

Embed Lua script:
```go
//go:embed assign.lua
var assignLua string

var assignScript = redis.NewScript(assignLua)

func Assign(ctx context.Context, rdb *redis.Client, realIP, proxyID string, proxyNum, ttl int) (string, error)
// Runs script, returns internal IP or "BANNED"
```

`internal/assignment/assign.lua` — exactly as in CONTEXT.md

### `internal/nat/nat.go`

Uses `github.com/google/nftables` via netlink — no forked processes.

```go
type NATManager struct {
    mu     sync.Mutex
    conn   *nftables.Conn  // opened once, reused
    tbl    *nftables.Table // ip nat
    natMap *nftables.Map   // nat_map
}
func New() (*NATManager, error)
// Opens netlink conn, looks up existing table ip nat + map nat_map
// If missing → fatal: "apply deploy/nftables-proxy.conf first"
// Does NOT create tables/chains — infrastructure only

func (n *NATManager) Add(realIP, internalIP string) error
// Lock, conn.AddElement on nat_map, conn.Flush()

func (n *NATManager) Delete(realIP string) error
// Lock, conn.DelElement on nat_map, conn.Flush()

func (n *NATManager) Flush() error
// Lock, flush all elements from nat_map
```

### `deploy/nftables-proxy.conf` (static infrastructure — applied once on proxy host, not by the binary)
```
# Applied once on initial server setup. Not touched by mcproxy.
table ip nat {
  map nat_map { type ipv4_addr : ipv4_addr; }
  chain POSTROUTING {
    type nat hook postrouting priority srcnat;
    snat to ip saddr map @nat_map
  }
}
```
Applied via: `nft -f deploy/nftables-proxy.conf`
Persisted via: `/etc/nftables.conf` include or systemd `ExecStartPre`

### `internal/ban/enforcer.go`
```go
type BanCache struct {
    mu     sync.RWMutex
    banned map[string]struct{} // real IPs
}
func (b *BanCache) IsBanned(ip string) bool // RLock
func (b *BanCache) Ban(ip string)           // Lock

type Enforcer struct {
    cache *BanCache
    nat   *nat.NATManager
    rdb   *redis.Client
}
func (e *Enforcer) LoadFromRedis(ctx context.Context, proxyID string) error
// SMEMBERS banned:real → populate BanCache
// SMEMBERS banned:internal:{proxyID} → add to inet filter blocklist via netlink

func (e *Enforcer) Subscribe(ctx context.Context) error
// SUBSCRIBE ban_events
// On message: parse JSON, add real_ip to BanCache
// Add internal IPs to inet filter blocklist (table inet filter, set blocklist) via netlink
```

---

## Phase 3 — Ban Watcher (host)

### `internal/ban/watcher.go`
```go
type Watcher struct {
    volumesRoot string
    rdb         *redis.Client
}
func (w *Watcher) Start(ctx context.Context) error
```

**inotify strategy:**
- Watch `volumesRoot` for IN_CREATE (new server volumes appear)
- Watch each `{uuid}/banned-ips.json` for IN_CLOSE_WRITE
- Use `golang.org/x/sys/unix` inotify syscalls directly
- `banned-players.json` is not watched

**On banned-ips.json change:**
1. Read file, diff against last-seen state (keep per-uuid snapshot in memory)
2. For each new entry: extract `ip` field (this is internal IP, e.g. 10.1.0.5)
3. `GET rmap:{internal_ip}` → `{real_ip, proxy}`
4. `HGETALL map:{real_ip}` → all internal IPs across all proxies
5. `SADD banned:real {real_ip}`
6. For each proxy→internalIP: `SADD banned:internal:{proxy} {internalIP}`
7. `PUBLISH ban_events {json}`
8. Remove entry from banned-ips.json (write file back without that entry)

**`banned-players.json` is not watched.** `/ban PlayerName` is handled entirely by vanilla
Minecraft — no session tracking or username→IP resolution needed. Ban propagation is driven
exclusively by `banned-ips.json`.

### `cmd/watcher/main.go`
- Load watcher config (redis_addr, volumes_root)
- Connect Redis
- watcher.Start(ctx)
- Signal handling same as proxy

---

## Phase 4 — DDoS Hardening

**Static infrastructure** in `deploy/nftables-filter.conf` — applied once, not touched by the proxy:
```
table inet filter {
  set blocklist { type ipv4_addr; flags dynamic,timeout; }
  chain prerouting {
    type filter hook prerouting priority -150;
    ip saddr @blocklist drop
    tcp flags syn ip saddr limit rate 10/second burst 20 packets accept
    tcp flags syn drop
    tcp flags syn ip saddr and 255.255.255.0 limit rate 50/second burst 100 packets accept
    tcp flags syn drop
  }
}
```

The proxy binary manages `inet filter blocklist` elements via netlink (same `nftables.Conn` reuse
pattern as NATManager). Full set path: table `inet filter`, set `blocklist`.
sysctl (`net.ipv4.tcp_syncookies=1`) goes in `/etc/sysctl.d/99-mcproxy.conf`, not in Go code.

---

## Phase 5 — Local Proxy Cache

Extend `assignment.go`:
- On startup, scan all `map:*` keys and load into `sync.Map`
- `Assign()` checks local cache first, goes to Redis only on miss
- `enforcer.Subscribe()` updates cache on ban events
- Cache TTL entries cleaned up via time.AfterFunc

Extend `router.go`:
- nodeMap is built directly from config at startup (plain `map[string]string`, read-only after init)
- Also written to Redis via `WriteNodeMap` so other services can read it
- Router serves all node lookups from in-memory nodeMap — never hits Redis per connection

---

## Docker Packaging

Both binaries ship as Docker containers. All nftables operations use netlink — no nft binary needed
in either image.

### `Dockerfile.proxy`
```dockerfile
FROM golang:1.21 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o mcproxy ./cmd/proxy

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /src/mcproxy /usr/local/bin/mcproxy
ENTRYPOINT ["mcproxy", "-config", "/config.json"]
```

### `Dockerfile.watcher`
```dockerfile
FROM golang:1.21 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o mcwatcher ./cmd/watcher

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /src/mcwatcher /usr/local/bin/mcwatcher
ENTRYPOINT ["mcwatcher", "-config", "/config.json"]
```

### Images
```
ghcr.io/siderocloud/mcproxy   → proxy machines
ghcr.io/siderocloud/mcwatcher → Hetzner/GorillaServer hosts
```

---

## systemd Units

### `/etc/systemd/system/mcproxy.service` (proxy machines)
```ini
[Unit]
Description=mcproxy
After=network.target docker.service
Requires=docker.service

[Service]
ExecStartPre=-/usr/bin/docker rm -f mcproxy
ExecStart=/usr/bin/docker run --rm --name mcproxy \
  --network=host \
  --cap-add=NET_ADMIN \
  -v /etc/mcproxy/config.json:/config.json:ro \
  ghcr.io/siderocloud/mcproxy:latest
ExecStop=/usr/bin/docker stop mcproxy
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

### `/etc/systemd/system/mcwatcher.service` (host nodes)
```ini
[Unit]
Description=mcwatcher
After=network.target docker.service
Requires=docker.service

[Service]
ExecStartPre=-/usr/bin/docker rm -f mcwatcher
ExecStart=/usr/bin/docker run --rm --name mcwatcher \
  --network=host \
  -v /etc/mcproxy/watcher.json:/config.json:ro \
  -v /var/lib/pterodactyl/volumes:/var/lib/pterodactyl/volumes:rw \
  ghcr.io/siderocloud/mcwatcher:latest
ExecStop=/usr/bin/docker stop mcwatcher
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

**Key flags:**
- `--network=host`: containers share host network namespace → netlink hits host kernel nftables
- `--cap-add=NET_ADMIN`: required for nftables netlink writes
- Static nftables rulesets (`deploy/`) applied on the host directly, not inside containers
- TPROXY policy routing rules must be applied on host before starting mcproxy:
  ```bash
  ip rule add fwmark 1 lookup 100
  ip route add local 0.0.0.0/0 dev lo table 100
  ```
  Add to `/etc/rc.local` or a systemd `ExecStartPre` to persist across reboots.

**Config file locations on host:**
- Proxy: `/etc/mcproxy/config.json`
- Watcher: `/etc/mcproxy/watcher.json`

---

## Implementation Order

1. `go mod init sidero-proxy`
2. Phase 1: config → tproxy → forwarder → router → cmd/proxy — build + verify
3. `deploy/nftables-tproxy.conf` + policy routing — verify TPROXY intercept works
4. Phase 2: registration → assignment (+ assign.lua) → nat → enforcer → wire into router
5. Phase 3: watcher → cmd/watcher
6. Phase 4: deploy/ static configs (nftables-proxy.conf, nftables-filter.conf, sysctl.conf)
7. Docker: Dockerfile.proxy, Dockerfile.watcher, systemd units

---

## Verification

**Phase 1 — TPROXY intercept:**
```bash
# Apply tproxy nftables config and policy routing on proxy host
nft -f deploy/nftables-tproxy.conf
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100

# Build and run
go build ./cmd/proxy && ./mcproxy -config config.json

# Connect on any port — should be intercepted and forwarded
nc -zv 1.2.3.4 25565
nc -zv 1.2.3.4 25999   # non-configured port, also intercepted
```

**Phase 1 (container):**
```bash
docker build -f Dockerfile.proxy -t mcproxy-test .
docker run --rm --network=host --cap-add=NET_ADMIN \
  -v $(pwd)/config.json:/config.json:ro mcproxy-test
```

**Phase 2:**
```bash
nft list map ip nat nat_map          # entries appear on player connect
redis-cli HGET map:1.2.3.4 proxy-1
redis-cli GET rmap:10.1.0.5
```

**Phase 3:**
```bash
redis-cli SUBSCRIBE ban_events       # watch for events
# manually append entry to a banned-ips.json
redis-cli SISMEMBER banned:real 1.2.3.4   # should be 1
```

**Phase 2 ban enforcement:**
```bash
# connect from banned IP → immediate RST, no backend traffic
```
