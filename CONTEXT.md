# MCProxy — Full Architecture Context
> Hand this file to Claude Code with the instruction: "Read this file fully before writing any code."

---

## Project Overview

**SideroCloud** is a multi-tenant Minecraft hosting platform. This document describes the design and implementation plan for `mcproxy` — a custom TCP proxy system that:

- Protects backend nodes from DDoS by keeping them off the public internet
- Preserves real player IP identity behind a proxy layer
- Enables native `/ban-ip` and `/ban` semantics without any server-side modifications
- Scales horizontally across multiple proxy machines

---

## Core Problem

When a Minecraft server sits behind a TCP proxy, it only sees the proxy's IP — not the real player IP. This breaks:
- `/ban-ip` — bans the proxy IP, blocking all players
- `/ban` — Minecraft auto-bans the IP it sees (proxy IP), again useless
- Any IP-based moderation

**Constraints:**
- Cannot modify user servers (no plugins, no Velocity, no proxy protocol, no config changes)
- Must survive DDoS
- Must scale across multiple proxy machines and nodes
- Must be transparent to end users

---

## Infrastructure

### Hetzner Dedicated Host (`eu-bdg-2`)
- **Public IP:** `176.9.44.106/32` on `eno1`, Debian Trixie
- **Pterodactyl** manages Minecraft server containers on `pterodactyl0` (`172.18.0.0/16`)
- Containers **bind ports directly to the host** — e.g. a Minecraft server on port 25565 is accessible as `176.9.44.106:25565`
- IP forwarding enabled (`net.ipv4.ip_forward=1`)
- There is also a KVM VM setup on `br0` — this is **unrelated** to Minecraft hosting, ignore it

### Proxy Machines
- Separate machines with public IPs
- Run Ubuntu 22.04
- Connected to the Hetzner host via **Tailscale**
- All player traffic enters here first

### Redis Machine
- Separate internal machine (or runs on Hetzner host)
- Reachable only via Tailscale
- Acts as the central IP mapping authority

### Tailscale
- Connects proxies ↔ Hetzner host privately
- Nodes are NOT publicly exposed — all ingress goes through proxies
- Each proxy advertises its own `/16` subnet (see below)
- Hetzner host runs `tailscale up --accept-routes`

---

## Chosen Architecture

### High-Level Flow

```
Player (real IP: 1.2.3.4)
  ↓
Proxy (public IP, Ubuntu 22.04)
  ├── nftables: DDoS filter + blocklist
  ├── Check Redis: is 1.2.3.4 banned? → drop if yes
  ├── Ask Redis: assign internal IP for 1.2.3.4 → 10.1.0.5
  ├── nftables SNAT: rewrite src 1.2.3.4 → 10.1.0.5
  └── Forward over Tailscale
  ↓
Hetzner host (Tailscale IP)
  ↓
Pterodactyl container (bound to host port)
  ↓
Minecraft server sees player at 10.1.0.5
```

### Return Path

```
Minecraft responds to 10.1.0.5
  ↓
Host kernel: route lookup for 10.1.0.0/16
  ↓
Tailscale: Proxy 1 advertises 10.1.0.0/16 → send there
  ↓
Proxy 1 receives response
  ↓
Reverse NAT: 10.1.0.5 → 1.2.3.4
  ↓
Player receives response
```

---

## Per-Proxy Subnet Model

Each proxy owns a unique `/16` subnet from `10.0.0.0/8`:

```
Proxy 1 → 10.1.0.0/16  (65k IPs)
Proxy 2 → 10.2.0.0/16
Proxy 3 → 10.3.0.0/16
...
Proxy 254 → 10.254.0.0/16
```

**Why:** Multiple proxies cannot share `10.0.0.0/8` because when the host sends a response to `10.x.x.x`, Tailscale must know which proxy to route it to. Per-proxy subnets make this deterministic — each proxy advertises only its own subnet.

**Tailscale setup:**
```bash
# On each proxy (n = proxy number)
tailscale up --advertise-routes=10.{n}.0.0/16

# On Hetzner host
tailscale up --accept-routes
```

---

## Redis Data Model

### Proxy Registry
```
proxy:registry (hash)
  proxy-1 → { subnet: "10.1.0.0/16", tailscale_ip: "100.x.x.x", registered_at: <ts> }
  proxy-2 → { subnet: "10.2.0.0/16", tailscale_ip: "100.x.x.x", registered_at: <ts> }

proxy:subnet_counter → INCR to allocate next subnet slot (1, 2, 3...)
```

### IP Assignment
```
# Forward mapping — real IP to internal IP per proxy
map:1.2.3.4 (hash)
  proxy-1 → "10.1.0.5"
  proxy-2 → "10.2.0.7"

# Reverse mapping — internal IP to real IP + proxy
rmap:10.1.0.5 → { real_ip: "1.2.3.4", proxy: "proxy-1" }
rmap:10.2.0.7 → { real_ip: "1.2.3.4", proxy: "proxy-2" }

# Per-proxy IP counter (incremented atomically)
proxy:proxy-1:counter → 1, 2, 3...
proxy:proxy-2:counter → 1, 2, 3...
```

Counter → IP conversion:
```
n = INCR proxy:proxy-{id}:counter
ip = 10.{proxy_num}.{n/256 % 256}.{n%256}
```

### Ban Storage
```
banned:real (SET)          → real IPs that are banned
banned:internal:proxy-1    → internal IPs banned on proxy-1
banned:internal:proxy-2    → internal IPs banned on proxy-2

ban_events (pub/sub channel) → { real_ip, internal_ips: {proxy-1: ..., proxy-2: ...}, reason, ts }
```

---

## IP Assignment — Atomic Lua Script

Redis executes this atomically. No race conditions between concurrent connections.

```lua
-- KEYS[1] = real_ip
-- ARGV[1] = proxy_id
-- ARGV[2] = proxy_num (e.g. "1" for proxy-1)
-- ARGV[3] = ttl_seconds

-- Check if already assigned for this proxy
local existing = redis.call('HGET', 'map:' .. KEYS[1], ARGV[1])
if existing then
  redis.call('EXPIRE', 'map:' .. KEYS[1], tonumber(ARGV[3]))
  return existing
end

-- Check if real IP is banned
local banned = redis.call('SISMEMBER', 'banned:real', KEYS[1])
if banned == 1 then
  return "BANNED"
end

-- Allocate next internal IP for this proxy
local n = redis.call('INCR', 'proxy:' .. ARGV[1] .. ':counter')
local proxy_num = tonumber(ARGV[2])
local b2 = math.floor(n / 256) % 256
local b1 = n % 256
local internal_ip = '10.' .. proxy_num .. '.' .. b2 .. '.' .. b1

-- Store both mappings with TTL
redis.call('HSET', 'map:' .. KEYS[1], ARGV[1], internal_ip)
redis.call('EXPIRE', 'map:' .. KEYS[1], tonumber(ARGV[3]))
redis.call('SET', 'rmap:' .. internal_ip,
  '{"real_ip":"' .. KEYS[1] .. '","proxy":"' .. ARGV[1] .. '"}',
  'EX', tonumber(ARGV[3]))

return internal_ip
```

---

## nftables Design

### NAT Map (one rule handles all players)
```
# Create the NAT map
nft add map ip nat nat_map { type ipv4_addr : ipv4_addr\; }

# One SNAT rule using the map
nft add rule ip nat POSTROUTING \
  ip saddr != @whitelist \
  snat to ip saddr map @nat_map

# Add player dynamically (atomic element update)
nft add element ip nat nat_map { 1.2.3.4 : 10.1.0.5 }

# Remove player on disconnect
nft delete element ip nat nat_map { 1.2.3.4 }
```

### DDoS Pre-Filter (before conntrack)
```
table inet filter {
  chain prerouting {
    type filter hook prerouting priority -150\;

    # Drop banned IPs instantly, no conntrack
    ip saddr @blocklist drop

    # Rate limit new connections per source IP
    tcp flags syn ip saddr limit rate 10/second burst 20 packets accept
    tcp flags syn drop

    # Rate limit per /24 subnet (catches botnets)
    tcp flags syn ip saddr and 255.255.255.0 limit rate 50/second burst 100 packets accept
    tcp flags syn drop
  }

  chain forward {
    # Established flows skip conntrack
    ct state established,related notrack accept
  }
}
```

### Blocklist (updated via pub/sub)
```
# Dynamic set — updated at runtime
nft add set ip filter blocklist { type ipv4_addr\; flags dynamic, timeout\; }

# Add banned IP
nft add element ip filter blocklist { 1.2.3.4 }
```

---

## Ban Enforcement Flow

### On Ban Event (file watcher on node)

1. inotify watches `banned-players.json` and `banned-ips.json` on the host
2. Change detected → extract username or internal IP from entry
3. If `banned-ips.json` changed: the IP written is the **proxy IP — ignore it**. Use the username to look up the real IP instead.
4. Lookup `rmap:{internal_ip}` in Redis → get `real_ip`
5. Lookup `map:{real_ip}` → get all internal IPs across all proxies
6. Add `real_ip` to `banned:real` SET in Redis
7. Add each internal IP to `banned:internal:{proxy}` SET
8. Publish to `ban_events` pub/sub channel
9. **Remove proxy IP from `banned-ips.json` immediately** — prevents the server from blocking all players

### On Ban Event (proxy side)

1. All proxies subscribe to `ban_events`
2. On message: add real IP to local in-memory ban cache
3. Add to nftables `@blocklist` set
4. Any new connection from that IP is dropped before forwarding

### On New Connection (proxy)

```
TCP connection opens
  ↓
Check local ban cache (in-memory) → banned? RST immediately
  ↓
Ask Redis for internal IP assignment (Lua script)
  ↓
Script returns "BANNED" → RST connection
  ↓
Script returns internal IP → add nftables NAT map element
  ↓
Forward traffic
```

---

## Proxy Self-Registration Flow

When a proxy starts up:

```
1. Connect to Redis (via Tailscale)
2. Check proxy:registry → am I already registered? (by proxy_id from config)
   YES → retrieve my subnet, resume normally
   NO  → INCR proxy:subnet_counter → get slot n
         my subnet = 10.{n}.0.0/16
         store in proxy:registry
3. Run: tailscale up --advertise-routes=10.{n}.0.0/16
4. Pull banned:real from Redis → load into local memory + nftables blocklist
5. Pull banned:internal:proxy-{id} → load into nftables
6. Subscribe to ban_events pub/sub
7. Start accepting connections
```

---

## Concurrency Model

- **One goroutine per player connection** — idiomatic Go, scales to thousands
- **NATManager** — single struct with `sync.Mutex` serializing all nftables map writes
- **Redis assignment** — atomic Lua script, no application-level locking needed
- **Ban cache** — `sync.RWMutex` protected in-memory map, read on every connection, written only on ban events

---

## Go Project Structure

```
mcproxy/
  cmd/
    proxy/
      main.go              ← entrypoint, signal handling
  internal/
    config/
      config.go            ← JSON config loader
    registration/
      registration.go      ← proxy self-registration with Redis
    assignment/
      assignment.go        ← Redis Lua script execution, IP assignment
    nat/
      nat.go               ← nftables map manager, NATManager struct
    forwarder/
      forwarder.go         ← bidirectional TCP copy per connection
    router/
      router.go            ← listener per port, spawns forwarder goroutines
    ban/
      watcher.go           ← inotify watcher for banned-*.json (runs on node)
      enforcer.go          ← pub/sub subscriber, updates nftables + local cache
  config.json              ← proxy config (proxy_id, servers list, redis addr)
```

---

## config.json Format

```json
{
  "proxy_id": "proxy-1",
  "redis_addr": "100.x.x.x:6379",
  "redis_password": "your_password_here",
  "ip_ttl_seconds": 604800,
  "servers": [
    {
      "listen_port": 25565,
      "backend": "100.x.x.x:25565",
      "name": "server-001"
    },
    {
      "listen_port": 25566,
      "backend": "100.x.x.x:25566",
      "name": "server-002"
    }
  ]
}
```

---

## Redis Setup (on Hetzner host or dedicated machine)

```bash
# redis.conf — critical settings
bind <tailscale_ip>        # bind to Tailscale interface ONLY, never 0.0.0.0
requirepass your_password
maxmemory 512mb
maxmemory-policy allkeys-lru

# ACL for proxy users — restrict to only needed commands
ACL SETUSER proxy on >password ~* &* +GET +SET +HGET +HSET +EXPIRE +INCR +SISMEMBER +SADD +SPOP +PUBLISH +SUBSCRIBE +EVAL
```

---

## DDoS Strategy

**Not** relying on proxy replacement as primary defense. Layered approach:

| Layer | Mechanism |
|---|---|
| 1 | Upstream provider scrubbing (if available) |
| 2 | nftables pre-filter — rate limit per IP and per /24, drop before conntrack |
| 3 | SYN cookies (`sysctl net.ipv4.tcp_syncookies=1`) |
| 4 | NOTRACK on established flows — conntrack only sees new connections |
| 5 | Circuit breaker — if new connection rate exceeds threshold, lock to known IPs only |
| 6 | Proxy fleet — horizontal scaling, dead proxies replaced in <30s, Redis preserves all state |

Proxy death is a **recovery mechanism**, not a defense mechanism. The goal is for proxies to survive attacks via pre-filtering, with replacement as a last resort.

---

## Build Phases

### Phase 1 — Basic Routing ✅ Design complete
- TCP forwarder in Go
- Config-driven `port → backend` routing
- Tailscale connects proxy to Hetzner host
- Nodes off public internet

### Phase 2 — IP Mapping
- Proxy self-registration with Redis
- Atomic Lua IP assignment
- Per-proxy `/16` subnet allocation
- nftables map-based SNAT
- Local ban cache on proxy
- Ban event pub/sub

### Phase 3 — Ban Enforcement
- inotify file watcher on node for `banned-*.json`
- Ban resolution: internal IP → real IP via Redis
- Proxy IP removal from `banned-ips.json`
- Real IP propagation to all proxies via pub/sub

### Phase 4 — DDoS Hardening
- nftables pre-filter ruleset
- SYN cookies
- NOTRACK on established flows
- Connection rate circuit breaker

### Phase 5 — Local Proxy Cache
- Full Redis map pulled into proxy memory on startup
- Local cache served for all lookups, Redis only on miss
- pub/sub keeps cache warm
- Proxy functions if Redis briefly unreachable

### Phase 6 — Multi-Proxy HA
- Redis Sentinel (1 primary, 2 replicas)
- Load balancer in front of proxy fleet
- Health checks, automatic rotation
- Pre-baked proxy image for <30s spin-up

---

## Key Design Decisions (and Why)

| Decision | Reason |
|---|---|
| NAT over mapping-based approach | Preserves native `/ban-ip` semantics with zero server changes. Conntrack exhaustion handled by pre-filter + proxy replacement. |
| Per-proxy subnets | Return path must be deterministic. Multiple proxies can't share a subnet because Tailscale can't know which proxy to route responses to. |
| Redis as single mapping authority | Proxies are stateless. Redis holds all truth. Proxy death loses no state. |
| Atomic Lua for assignment | Prevents race conditions when thousands of players connect simultaneously. |
| nftables map over per-rule | One rule handles all players. Atomic element updates. Scales to millions of entries. |
| inotify over polling | Instant ban detection. Polling introduces a window where proxy IP stays in banned-ips.json too long. |
| Go for proxy | Single binary, low memory, goroutine-per-connection scales naturally, strong Redis client ecosystem. |

---

## Environment

- **Proxy machines:** Ubuntu 22.04, Go 1.21+
- **Host:** Debian Trixie, Pterodactyl + Wings
- **Private network:** Tailscale
- **Firewall:** nftables (not iptables)
- **Ban state:** Redis 7+
- **Redis client library:** `github.com/redis/go-redis/v9`
