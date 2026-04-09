# Deployment Guide

This guide explains how to deploy the current `mcproxy` implementation based on what was actually required to build it and run the local end-to-end labs.

The implementation currently consists of:
- `cmd/proxy`: public ingress proxy
- `cmd/watcher`: ban-file watcher and node-side DNAT helper

The runtime was validated in staged labs for:
- port-range interception
- same-port backend routing
- backend source identity preservation with `10.x.x.x` per-proxy addresses
- watcher-driven cross-proxy identity propagation
- node-side DNAT for containers published only on the node public IP
- prefilter blocklist and SYN gate
- same-real-IP concurrent connection safety through refcounted NAT lifecycle
- native nftables runtime map updates without shelling out to `nft` on each flow event

## 1. What Must Exist

You need:
- Linux hosts
- `nftables`
- `iproute2`
- a Redis- or Valkey-compatible server
- Go only if you are building from source
- root privileges for the proxy and watcher processes

You do **not** need:
- PostgreSQL
- MySQL
- any backing database behind Redis/Valkey

Redis or Valkey is the datastore.

## 2. Roles

There are 3 runtime roles:

### Proxy host
- Owns one or more public IPs
- Runs `mcproxy`
- Accepts player traffic on a configured port range
- Redirects that range to one intercept port with nftables
- Assigns internal `10.x.x.x` identities through Redis/Valkey
- Opens backend connections with the assigned internal IP as source

### Node host
- Runs Minecraft containers
- May publish containers only on the node public IP
- Runs `mcwatcher`
- Watches `banned-ips.json` and promotes identities across proxies
- Optionally installs node-side DNAT from private interface traffic to the node public-IP-published port range

### Redis or Valkey host
- Stores:
  - proxy registry
  - per-proxy IP counters
  - `map:{real_ip}`
  - `rmap:{internal_ip}`
  - reserved identities

## 3. Build

From repo root:

```bash
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build -o /usr/local/bin/mcproxy ./cmd/proxy
GOCACHE=/tmp/go-build go build -o /usr/local/bin/mcwatcher ./cmd/watcher
```

If you prefer, build to another path and update the service/unit files accordingly.

## 4. Redis Or Valkey Setup

Any Redis-compatible server is fine. In my labs, the machine’s `redis-server` binary was actually Valkey and worked as a drop-in backend.

Minimum requirements:
- reachable from proxy and watcher hosts
- persistence enabled for production
- password if exposed outside localhost or a trusted private network

Recommended:

```conf
bind 127.0.0.1 100.x.x.x
port 6379
requirepass change-me
appendonly yes
save 60 1000
```

If it is remote, bind it only to the private interface you actually use.

### Repo-standard Docker Compose

The repo now includes a datastore-only compose deployment:

- [docker-compose.valkey.yml](/home/onegrit/Documents/Projects/sidero-proxy/deploy/docker-compose.valkey.yml)
- [.env.example](/home/onegrit/Documents/Projects/sidero-proxy/deploy/.env.example)

This is the recommended quick-start if you want the datastore in Docker while keeping `mcproxy` and `mcwatcher` on the host.

Setup:

```bash
cd deploy
cp .env.example .env
```

Edit `.env` and set:

- `REDIS_BIND_IP` to the host private IP that proxy and watcher will use
- `REDIS_PASSWORD` to a real secret
- optionally `VALKEY_IMAGE` or `VALKEY_DATA_VOLUME`

Start it:

```bash
docker compose -f docker-compose.valkey.yml up -d
```

Validate the generated config:

```bash
docker compose -f docker-compose.valkey.yml config
```

Validate the running service:

```bash
docker compose -f docker-compose.valkey.yml ps
docker exec sidero-proxy-valkey valkey-cli -a "$REDIS_PASSWORD" ping
```

If you prefer a host-side client instead:

```bash
redis-cli -h <REDIS_BIND_IP> -p 6379 -a <REDIS_PASSWORD> ping
```

Point the app configs at the same address:

```json
{
  "redis_addr": "100.100.100.10:6379",
  "redis_password": "change-me"
}
```

That `redis_addr` / `redis_password` shape is already what both example configs use.

## 5. Proxy Host Setup

### Kernel and tooling

Install:

```bash
apt-get update
apt-get install -y nftables iproute2
```

Enable IP forwarding:

```bash
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/99-mcproxy.conf
```

### Important runtime privilege requirement

`mcproxy` must run as root or equivalent because it needs to:
- add nftables tables/chains/sets/rules
- add/remove nftables NAT map elements through netlink
- install a local route for its assigned `/16`
- create transparent-source backend sockets with `IP_TRANSPARENT` and `IP_FREEBIND`

Without that, startup or forwarding will fail.

### Tailscale or private routing

The implementation expects each proxy to own a unique `/16` from `10.0.0.0/8`.

Example:

```bash
tailscale up --advertise-routes=10.1.0.0/16
```

On the node side or route-accepting side:

```bash
tailscale up --accept-routes
```

The proxy itself also installs:

```bash
ip route replace local 10.1.0.0/16 dev lo
```

That local route is required so the host can originate backend connections from the assigned internal `10.x.x.x` identity.

### Proxy config

Start from [config.example.json](/home/onegrit/Documents/Projects/sidero-proxy/deploy/config.example.json).

Example:

```json
{
  "proxy_id": "proxy-1",
  "redis_addr": "100.100.100.10:6379",
  "redis_password": "change-me",
  "ip_ttl_seconds": 604800,
  "intercept_port": 19000,
  "port_range": {
    "start": 25500,
    "end": 25600
  },
  "servers": [
    {
      "name": "node-a",
      "proxy_public_ip": "203.0.113.10",
      "backend_ip": "100.72.10.5"
    },
    {
      "name": "node-b",
      "proxy_public_ip": "203.0.113.11",
      "backend_ip": "100.72.10.6"
    }
  ]
}
```

Meaning:
- `proxy_public_ip` selects the backend node
- `port_range` is the external range to intercept
- the original destination port is preserved when dialing the backend
- `intercept_port` is the internal listener port used after nftables `redirect`

### Start proxy

```bash
mcproxy -config /etc/mcproxy/config.json
```

On startup, the proxy will:
- register or resume its subnet in Redis/Valkey
- install the local `/16` route
- create or reuse:
  - `mcproxy_filter`
  - `mcproxy_intercept`
  - `mcproxy_nat`
- listen on `0.0.0.0:<intercept_port>`

At runtime, the proxy then:
- adds `mcproxy_nat:nat_map` elements through the native Go nftables client
- refcounts live NAT state per real client IP
- removes a NAT element only after the last live connection for that real IP closes

### What nftables objects the proxy owns

The proxy manages only these dedicated tables:
- `ip mcproxy_filter`
- `ip mcproxy_intercept`
- `ip mcproxy_nat`

It does **not** delete unrelated host nftables tables.

It does flush the mcproxy-owned chains/sets it recreates, so treat those tables as application-owned.

Operationally:
- startup provisioning still uses the `nft` CLI
- per-connection NAT map updates do not shell out to `nft`
- this split is intentional so the hot path is lighter while startup stays simple

## 6. Node Host Setup

### Kernel and tooling

Install:

```bash
apt-get update
apt-get install -y nftables iproute2
```

### When node-side DNAT is needed

You need node-side DNAT if the Minecraft containers are published only on the node public IP, for example:

```text
167.235.15.102:25565->25565/tcp
```

and the proxy is dialing the node private/Tailscale IP.

In that case, private-interface traffic to `private_ip:25565` will not automatically hit the Docker-published service unless you add a host-level DNAT rule.

### Watcher config

Start from [watcher.example.json](/home/onegrit/Documents/Projects/sidero-proxy/deploy/watcher.example.json).

Example:

```json
{
  "redis_addr": "100.100.100.10:6379",
  "redis_password": "change-me",
  "volumes_root": "/var/lib/pterodactyl/volumes",
  "node_dnat": {
    "public_ip": "167.235.15.102",
    "tailscale_interface": "tailscale0",
    "port_range": {
      "start": 25500,
      "end": 25600
    }
  }
}
```

Meaning:
- watcher watches `banned-ips.json` and `banned-players.json` under `volumes_root`
- watcher installs `mcproxy_node` DNAT rules so traffic arriving on `tailscale0` to that port range is rewritten to `public_ip:same_port`

### Start watcher

```bash
mcwatcher -config /etc/mcproxy/watcher.json
```

The watcher must also run as root because it needs to:
- install nftables `mcproxy_node`
- read the ban files

## 7. Functional Verification

### Proxy startup sanity

On proxy host:

```bash
mcproxy -config /etc/mcproxy/config.json
```

Expect logs like:
- proxy registered
- port range configured
- intercept listener started

Inspect routes:

```bash
ip route | rg '10\\.'
```

Inspect owned nftables tables:

```bash
nft list tables | rg 'mcproxy_'
```

### First traffic check

Connect to one proxy public IP and port in range.

Expected:
- proxy logs a `connection established`
- Redis contains `map:<real_ip>`
- backend sees peer `10.<proxy-subnet>.*.*`, not the proxy public IP and not the host bridge/private IP

### Same-IP concurrency check

Open two TCP connections at the same time from the same client IP to the same proxy public IP and port.

Expected:
- both connections succeed
- proxy logs the same `internal_ip` for both
- backend sees both peers from the same assigned `10.x.x.x` identity
- if one connection closes first, the other keeps working

### Watcher identity propagation check

After a player has connected once through proxy A:
- look up the internal IP written under `map:<real_ip>`
- ensure `banned-ips.json` contains that actual internal IP, not a guessed one
- watcher should then reserve mappings across all registered proxies

Expected:
- `map:<real_ip>` includes entries for all registered proxies after watcher promotion
- reconnect through proxy B causes the backend to see proxy B’s reserved internal identity

### Node DNAT check

If node DNAT is configured:
- connect to the node private or Tailscale IP on a port in range
- verify the service published only on the node public IP is reached

Expected:
- client succeeds
- `nft list table ip mcproxy_node` shows the DNAT rule

## 8. Operational Notes

### Redis/Valkey persistence

If persistence is off, identity state disappears on restart.

For production, enable at least one of:
- AOF
- RDB snapshots

### Order of proxy registration matters

Subnets are assigned in registration order:
- first registered proxy gets `10.1.0.0/16`
- second gets `10.2.0.0/16`

This matters when interpreting watcher-promoted mappings in a multi-proxy environment.

### Concurrency target

For your expected floor of `400+` concurrent players:
- the refcounted NAT lifecycle is required so same-IP multi-connection traffic does not break itself
- the proxy now avoids shelling out to `nft` on first-connect and last-disconnect events
- startup still depends on nftables provisioning succeeding before traffic is accepted

This does not mean the current build has been benchmarked to exactly `400+` live players in this repo. What has been validated is the functional behavior that was most likely to fail under that concurrency pattern.

### The watcher is not a proxy-side ban enforcer

Current behavior:
- watcher uses ban-file events to preserve identity across proxies
- proxy does not reject users based on watcher state

Actual banning remains a backend Minecraft-server concern.

### Dedicated nftables ownership

The implementation owns these tables:
- `mcproxy_filter`
- `mcproxy_intercept`
- `mcproxy_nat`
- `mcproxy_node`

Do not place unrelated manual rules inside those managed chains unless you are prepared for them to be flushed by the application.

## 9. Rollback

To stop the system:

```bash
pkill mcproxy || true
pkill mcwatcher || true
```

To remove managed nftables state:

```bash
nft delete table ip mcproxy_filter 2>/dev/null || true
nft delete table ip mcproxy_intercept 2>/dev/null || true
nft delete table ip mcproxy_nat 2>/dev/null || true
nft delete table ip mcproxy_node 2>/dev/null || true
```

To remove the local route on a proxy:

```bash
ip route del local 10.1.0.0/16 dev lo
```

Replace the subnet with the actual one assigned to that proxy.

## 10. Related Validation Artifacts

The deployment advice above comes directly from the staged labs:
- [01-range-intercept-routing.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/01-range-intercept-routing.md)
- [02-transparent-source-identity.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/02-transparent-source-identity.md)
- [03-watcher-cross-proxy-identity.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/03-watcher-cross-proxy-identity.md)
- [04-node-dnat-range.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/04-node-dnat-range.md)
- [05-prefilter-blocklist.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/05-prefilter-blocklist.md)
- [06-refcounted-nat-lifecycle.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/06-refcounted-nat-lifecycle.md)
- [07-native-nftables-hot-path.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/07-native-nftables-hot-path.md)
