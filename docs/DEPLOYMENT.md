# Deployment Guide

This guide explains how to deploy the current `mcproxy` implementation based on what was actually required to build it and run the local end-to-end labs.

The implementation currently consists of:
- `cmd/proxy`: public ingress proxy
- `cmd/watcher`: ban-file watcher and node-side DNAT helper
- `cmd/dnsfailover`: Cloudflare DNS failover controller for regional proxy failover

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
GOCACHE=/tmp/go-build go build -o /usr/local/bin/dnsfailover ./cmd/dnsfailover
```

If you prefer, build to another path and update the service/unit files accordingly.

Install the tracked systemd units if you are running the services directly on the host:

```bash
install -d /etc/mcproxy
install -m 0644 deploy/mcproxy.service /etc/systemd/system/mcproxy.service
install -m 0644 deploy/mcwatcher.service /etc/systemd/system/mcwatcher.service
install -m 0644 deploy/dnsfailover.service /etc/systemd/system/dnsfailover.service
systemctl daemon-reload
```

Tracked unit files:

- [mcproxy.service](/home/onegrit/Documents/Projects/sidero-proxy/deploy/mcproxy.service)
- [mcwatcher.service](/home/onegrit/Documents/Projects/sidero-proxy/deploy/mcwatcher.service)
- [dnsfailover.service](/home/onegrit/Documents/Projects/sidero-proxy/deploy/dnsfailover.service)

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

The tracked default uses a pinned published Valkey tag rather than a floating `stable` tag.

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

If the host already has a local Redis/Valkey on `6379`, change `.env` to another port such as `6380` before starting the compose stack.

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
  "health_port": 18080,
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
- `health_port` serves `/healthz` for proxy-ingress health checks

### Start proxy

```bash
install -m 0644 deploy/config.example.json /etc/mcproxy/config.json
systemctl enable --now mcproxy
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

Inspect the service:

```bash
systemctl status mcproxy
journalctl -u mcproxy -f
curl -fsS http://127.0.0.1:18080/healthz
```

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
install -m 0644 deploy/watcher.example.json /etc/mcproxy/watcher.json
systemctl enable --now mcwatcher
```

The watcher must also run as root because it needs to:
- install nftables `mcproxy_node`
- read the ban files

Inspect the service:

```bash
systemctl status mcwatcher
journalctl -u mcwatcher -f
```

## 7. DNS Failover Controller Setup

Use the DNS failover controller if you want regional proxy hostnames to fall back to SG automatically without paying for Cloudflare Load Balancing.

Start from [failover.example.json](/home/onegrit/Documents/Projects/sidero-proxy/deploy/failover.example.json).

The controller:
- probes a dedicated proxy health URL per region
- treats health as proxy-ingress health, not customer backend health
- updates Cloudflare `A` records for server ingress hostnames
- fails whole regions over to SG, not individual customer servers
- automatically fails back after a stable recovery window

Install config and start:

```bash
install -m 0644 deploy/failover.example.json /etc/mcproxy/failover.json
systemctl enable --now dnsfailover
```

Inspect it:

```bash
systemctl status dnsfailover
journalctl -u dnsfailover -f
```

Controller requirements:
- separate controller host is preferred
- Cloudflare API token with DNS edit permissions for the relevant zone
- one hostname per server in the controller inventory
- region health URLs pointing at `http://<proxy-ip>:<health_port>/healthz`

## 8. Functional Verification

### Proxy startup sanity

On proxy host:

```bash
systemctl status mcproxy
curl -fsS http://127.0.0.1:18080/healthz
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

### DNS failover check

If the failover controller is deployed:
- break the regional proxy health endpoint or stop the regional proxy
- confirm the controller switches affected hostnames to SG proxy IPs
- restore the regional proxy
- confirm the controller waits for the failback window, then restores regional answers

### Transparent-source path check

The proxy does not dial backends from its Tailscale IP. It dials from the assigned internal identity, for example `10.1.0.10`.

That means this test is not sufficient:

```bash
nc -vz 100.64.0.18 25551
```

because it uses the proxy host source IP, not the proxy-assigned internal identity.

Use this instead on the proxy host to emulate the proxy dataplane more closely:

```bash
nc -s 10.1.0.10 -vz 100.64.0.18 25551
```

Expected:
- success means subnet routing and ACLs allow the transparent-source path
- timeout means the backend path is still blocked before the service replies
- refusal means the packet arrived but nothing accepted it on the backend path

## 9. Operational Notes

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

## 10. Troubleshooting

### Valkey image tag failure

If Docker says:

```text
manifest for valkey/valkey:stable not found
```

use the pinned published tag already tracked in the repo:

```env
VALKEY_IMAGE=valkey/valkey:9.0.3
```

### Docker port already in use

If Docker says:

```text
failed to bind host port ... 6379 ... address already in use
```

check whether the host already has Redis/Valkey running:

```bash
ss -ltnp | grep 6379
```

If another service already owns `6379`, do not stop it unless you know what depends on it. Use another port in `.env`, for example:

```env
REDIS_PORT=6380
```

and point both proxy and watcher configs at that port.

### Config directory missing

If install fails with:

```text
cannot create regular file '/etc/mcproxy/...': No such file or directory
```

create the config directory first:

```bash
install -d /etc/mcproxy
```

### Go version parsing error

If your system Go toolchain rejects:

```text
invalid go version '1.23.0': must match format 1.23
```

your local Go is stricter about the `go.mod` version format. Use a newer Go toolchain or normalize the `go` directive to `1.23`.

### Proxy can reach backend from host IP, but mcproxy still times out

This is the most important operational pitfall.

This test:

```bash
nc -vz 100.64.0.18 25551
```

only proves that `100.64.0.22 -> 100.64.0.18:25551` works.

The proxy actually dials like:

```text
10.1.0.10 -> 100.64.0.18:25551
```

If that path times out, check all of these:

```bash
ip route get 10.1.0.10
ip route show table 52
tcpdump -ni tailscale0 'tcp port 25551 and host 10.1.0.10'
```

On the backend node, success requires:
- `10.1.0.0/16` learned via Tailscale, not routed out the public NIC
- backend node DNAT installed in `mcproxy_node`
- ACLs allowing traffic from the proxy subnet to the backend node

### Headscale route approval is not enough

Even if Headscale shows the route as approved and served, the backend node must still:

- run `tailscale up --accept-routes ...`
- receive the route into table `52`
- actually select `tailscale0` for `10.1.0.0/16`

Useful checks:

```bash
tailscale debug prefs
ip rule
ip route show table 52
ip route get 10.1.0.10
```

The correct result should look like traffic to `10.1.0.10` using `tailscale0`, not the public interface.

### Proxy health endpoint stays unhealthy

The proxy health endpoint depends on proxy-local readiness, not backend customer traffic.

Useful checks:

```bash
curl -v http://127.0.0.1:18080/healthz
nft list tables | rg 'mcproxy_'
ip route show table local | grep '10\.'
redis-cli -h <redis-ip> -p <redis-port> -a '<password>' ping
```

It will report unhealthy if:
- the intercept listener is not running
- Redis/Valkey cannot be reached
- required `mcproxy_*` nftables tables are missing
- the local proxy subnet route is missing

### DNS failover controller does not switch records

Check:

```bash
systemctl status dnsfailover
journalctl -u dnsfailover -n 100 --no-pager
curl -fsS http://<proxy-ip>:18080/healthz
```

Common causes:
- Cloudflare API token lacks DNS edit permissions
- wrong zone ID
- wrong hostname inventory in `failover.json`
- health URL not reachable from the controller host
- fallback region health is also failing

### ACLs must allow proxy subnet traffic

Approving and serving the subnet route is not enough by itself. Tailscale/Headscale ACLs must also allow traffic sourced from the proxy subnet.

A scalable pattern is to reserve the full proxy subnet pool in ACL hosts, for example:

```json
"proxy-nets": "10.0.0.0/8"
```

and then allow both directions:

```json
{
  "action": "accept",
  "src": ["group:cidernet"],
  "dst": ["proxy-nets:*"]
},
{
  "action": "accept",
  "src": ["proxy-nets"],
  "dst": ["group:cidernet:*"]
}
```

This avoids adding new ACLs for every future proxy subnet.

### Watcher warnings for old banned public IPs

If watcher logs contain warnings like:

```text
lookup reverse mapping for 41.92.99.132: redis: nil
```

that usually means `banned-ips.json` contains legacy public IP bans, not proxy-assigned internal identities like `10.1.x.x`.

That warning is about identity promotion and does not by itself explain backend dial timeouts.

## 11. Rollback

To stop the system:

```bash
systemctl disable --now mcproxy || true
systemctl disable --now mcwatcher || true
systemctl disable --now dnsfailover || true
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

## 12. Related Validation Artifacts

The deployment advice above comes directly from the staged labs:
- [01-range-intercept-routing.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/01-range-intercept-routing.md)
- [02-transparent-source-identity.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/02-transparent-source-identity.md)
- [03-watcher-cross-proxy-identity.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/03-watcher-cross-proxy-identity.md)
- [04-node-dnat-range.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/04-node-dnat-range.md)
- [05-prefilter-blocklist.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/05-prefilter-blocklist.md)
- [06-refcounted-nat-lifecycle.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/06-refcounted-nat-lifecycle.md)
- [07-native-nftables-hot-path.md](/home/onegrit/Documents/Projects/sidero-proxy/docs/stages/07-native-nftables-hot-path.md)
