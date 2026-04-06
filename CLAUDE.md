# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**mcproxy** is a TCP proxy system for SideroCloud Minecraft hosting. It preserves real player IPs behind a proxy layer so that vanilla Minecraft `/ban-ip` works correctly, without any server-side modifications. Players are SNAT'd to stable internal IPs (`10.x.x.x`) per proxy using kernel nftables maps. Ban events propagate across all proxies via Redis pub/sub.

**Two binaries:**
- `cmd/proxy/main.go` → `mcproxy` — runs on proxy machines (Ubuntu 22.04)
- `cmd/watcher/main.go` → `mcwatcher` — runs on Hetzner/GorillaServer host nodes

## Build Commands

```bash
# Initialize module (first time only)
go mod init sidero-proxy

# Build proxy binary
go build -o mcproxy ./cmd/proxy

# Build watcher binary
go build -o mcwatcher ./cmd/watcher

# Build both (Docker)
docker build -f Dockerfile.proxy -t mcproxy .
docker build -f Dockerfile.watcher -t mcwatcher .

# Run proxy locally
./mcproxy -config config.json

# Run watcher locally
./mcwatcher -config watcher.json

# Run tests
go test ./...

# Run a single test package
go test ./internal/assignment/...
```

## Architecture

### Routing Model — TPROXY

The proxy uses nftables TPROXY to intercept **all TCP traffic** on configured public IPs (no per-port config needed). The original destination is recovered via `SO_ORIGINAL_DST`:

```
Player → proxy_public_ip:ANY_PORT
  → nftables TPROXY → 127.0.0.1:8080
  → Go reads SO_ORIGINAL_DST → original dst IP + port
  → nodeMap lookup by dst IP → tailscale_ip
  → Forward to tailscale_ip:original_port
```

The TPROXY policy routing rules must be applied on the host before running mcproxy:
```bash
ip rule add fwmark 1 lookup 100
ip route add local 0.0.0.0/0 dev lo table 100
```

### IP Mapping and SNAT

Each proxy owns a unique `/16` subnet (`10.{n}.0.0/16`). Every player gets a stable internal IP per proxy. The nftables `nat_map` maps real IP → internal IP for SNAT. Return traffic is routed back via Tailscale subnet advertisement.

Redis holds all mapping state — proxies are stateless and replaceable:
- `map:{real_ip}` (hash): `proxy-id → internal_ip` per proxy
- `rmap:{internal_ip}` (string): `{real_ip, proxy}` — reverse lookup
- `proxy:proxy-{id}:counter` (int): atomic counter for IP allocation within the `/16`

IP assignment uses an **atomic Lua script** (`internal/assignment/assign.lua`) to prevent race conditions on concurrent connects.

### Ban Enforcement

1. `mcwatcher` watches `banned-ips.json` via inotify (`IN_CLOSE_WRITE`)
2. On change: resolves internal IP → real IP via Redis, propagates ban to all proxies via `ban_events` pub/sub, **removes the entry from `banned-ips.json`** (prevents blocking all players via the proxy IP)
3. `mcproxy` subscribes to `ban_events`, updates in-memory `BanCache` and nftables `inet filter blocklist` set

On each new connection:
1. Check local `BanCache` (in-memory, `sync.RWMutex`) → RST if banned
2. Run Redis Lua assignment script → RST if returns `"BANNED"`, else get internal IP
3. Add `nat_map` element via nftables netlink
4. Forward bidirectionally; remove `nat_map` element on disconnect

### Key Implementation Notes

- **All nftables operations use `github.com/google/nftables` netlink — never exec the `nft` binary**
- The proxy binary expects the nftables tables/maps to already exist (created by `deploy/nftables-proxy.conf`); it fatals if missing
- `NATManager` uses a single `nftables.Conn` with `sync.Mutex` for all map writes
- Concurrency model: one goroutine per player connection; ban cache uses `sync.RWMutex`; Redis assignment is atomic via Lua

### Static Infrastructure (deploy/)

Applied once on the host, never touched by the binaries:
- `deploy/nftables-tproxy.conf` — TPROXY intercept rules
- `deploy/nftables-proxy.conf` — `ip nat` table with `nat_map` and SNAT rule
- `deploy/nftables-filter.conf` — DDoS pre-filter (`inet filter blocklist`, SYN rate limits)

### Module & Dependencies

```
module sidero-proxy
go 1.21
```

Key dependencies:
- `github.com/redis/go-redis/v9`
- `github.com/google/nftables` (netlink)
- `golang.org/x/sys/unix` (inotify, `SO_ORIGINAL_DST`)

### Deployment

Both binaries run in Docker with `--network=host` (to reach host kernel nftables via netlink) and `--cap-add=NET_ADMIN`. Config files are mounted at `/config.json`.

- Proxy config: `/etc/mcproxy/config.json`
- Watcher config: `/etc/mcproxy/watcher.json`

### Implementation Phases

The plan is structured in phases (see `PLAN.md` for full detail):
1. **Phase 1** — TPROXY intercept, basic TCP forwarder, nodeMap from config
2. **Phase 2** — IP mapping, Redis Lua assignment, nftables NAT, ban enforcer
3. **Phase 3** — `mcwatcher` inotify ban detection on host
4. **Phase 4** — DDoS nftables pre-filter, SYN cookies
5. **Phase 5** — Local proxy cache (Redis state pulled into memory on startup)

> **PLAN.md is authoritative** — it supersedes CONTEXT.md where they differ (e.g., config format uses `nodes` array with TPROXY, not per-port `servers` array).
