# Clean Deployment Guide

This guide describes the intended production deployment for the current `sidero-proxy` system.

It is written as a clean setup reference:
- what each component does
- where each component should run
- how to configure and start it
- how DNS and failover fit together
- how to verify the system end to end

This guide avoids the incident-by-incident troubleshooting style of the older deployment notes.

## 1. System Overview

The system has three runtime components:

- `mcproxy`
  - public ingress proxy
  - terminates player traffic on proxy public IPs and a configured port range
  - routes primarily by exact Minecraft handshake hostname and falls back to destination IP for legacy raw-IP connects
  - allows many ingress hostnames to share the same proxy IP and port
  - preserves backend port numbers
  - opens backend connections using per-player internal `10.x.x.x` source identities

- `mcwatcher`
  - runs on backend node hosts
  - watches ban files
  - promotes player identity across proxies
  - installs node-side DNAT when Minecraft containers are published only on the node public IP

- `dnsfailover`
  - optional control-plane service
  - monitors proxy health
  - updates Cloudflare DNS records for regional failover
  - can switch server ingress from a failed regional proxy to SG fallback

Shared state is stored in Redis or Valkey.

## 2. Intended Topology

For the current target deployment:

- `SG`
  - Headscale / Tailscale control-plane
  - optional warm fallback proxy
  - optional DNS failover controller

- `Control-plane datastore host`
  - Redis / Valkey datastore
  - reachable from all proxy and backend regions

- `US`
  - one regional proxy host
  - three backend node hosts

- `EU`
  - one regional proxy host
  - two backend node hosts

Normal operation:
- US-hosted servers use the US proxy
- EU-hosted servers use the EU proxy
- SG is not used for normal player traffic unless deployed as fallback

Failure handling:
- if a regional proxy is unhealthy, DNS can move affected hostnames to SG
- backend nodes remain private behind Tailscale / Headscale

## 3. Runtime Model

### 3.1 Proxy routing

The proxy now routes in this order:

- exact Minecraft handshake hostname -> backend node
- if the handshake host is a raw IP matching the original destination, fallback to destination public proxy IP -> backend node
- original destination port -> same backend port

The intended hostname model is:

- customer vanity hostname -> SRV -> proxy-managed ingress hostname
- the final ingress hostname sent in the Minecraft handshake is the routing key
- multiple ingress hostnames may share the same proxy public IP and the same public port

So if a player connects through:

```text
<CUSTOMER_HOSTNAME> -> SRV -> <INGRESS_HOST_A>:25551
```

the proxy will usually see `<INGRESS_HOST_A>` in the handshake and dial:

```text
<backend-tailscale-ip>:25551
```

This is the Minecraft equivalent of virtual-host routing:

- one shared proxy IP and one shared public port can front multiple backend servers
- the distinguishing key is the handshake hostname, not the destination IP

### 3.2 Player identity

Each proxy receives its own `/16` from `<PROXY_SUBNET_POOL>`, for example:

- `proxy-1 -> <PROXY_SUBNET_A>`
- `proxy-2 -> <PROXY_SUBNET_B>`

For each real player IP, the proxy assigns an internal identity in its own `/16`, such as:

- `<INTERNAL_IDENTITY_A>`

The backend then sees traffic from that internal identity instead of the proxy host IP.

### 3.3 Why node-side DNAT exists

If a Minecraft container is published only on the node public IP, for example:

```text
<BACKEND_NODE_PUBLIC_IP>:25551->25551/tcp
```

then traffic sent to the node Tailscale IP will not automatically hit the container.

`mcwatcher` solves this by installing `mcproxy_node` DNAT rules:
- packet arrives on `tailscale0`
- destination is node Tailscale IP and game port
- watcher resolves the published port to the container bridge IP and port
- nftables rewrites destination directly to the container endpoint

This direct container DNAT is important. Rewriting to the node public IP would send the flow back through Docker's published-port NAT path, which can make the Minecraft server see the Docker bridge host IP instead of the assigned proxy identity.

### 3.4 Why proxy subnet routing matters

The proxy dials backends from `10.x.x.x`, not from its Tailscale IP.

That means backend nodes must:
- accept routes from proxies
- actually install proxy subnet routes through Tailscale
- allow ACL traffic from proxy subnet space to backend nodes

If that is missing, you will see backend dial timeouts even when a normal `nc` test from the proxy host IP succeeds.

## 4. Requirements

Every host that runs `mcproxy` or `mcwatcher` needs:

- Linux
- `nftables`
- `iproute2`
- root privileges

The datastore host needs:

- Redis or Valkey
- persistent storage enabled

The proxy specifically needs root because it must:

- manage nftables objects
- install a local `ip route`
- create transparent-source sockets

## 5. Config Files

Tracked examples:

- proxy config:
  - [config.example.json](../deploy/config.example.json)
- watcher config:
  - [watcher.example.json](../deploy/watcher.example.json)
- DNS failover config:
  - [failover.example.json](../deploy/failover.example.json)

Installed locations:

- `/etc/mcproxy/config.json`
- `/etc/mcproxy/watcher.json`
- `/etc/mcproxy/failover.json`

Create the config directory first:

```bash
install -d /etc/mcproxy
```

## 6. Build and Install

From the repo root:

```bash
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build -o /usr/local/bin/mcproxy ./cmd/proxy
GOCACHE=/tmp/go-build go build -o /usr/local/bin/mcwatcher ./cmd/watcher
GOCACHE=/tmp/go-build go build -o /usr/local/bin/dnsfailover ./cmd/dnsfailover
```

Systemd unit files:

- [mcproxy.service](../deploy/mcproxy.service)
- [mcwatcher.service](../deploy/mcwatcher.service)
- [dnsfailover.service](../deploy/dnsfailover.service)

Install them:

```bash
install -m 0644 deploy/mcproxy.service /etc/systemd/system/mcproxy.service
install -m 0644 deploy/mcwatcher.service /etc/systemd/system/mcwatcher.service
install -m 0644 deploy/dnsfailover.service /etc/systemd/system/dnsfailover.service
systemctl daemon-reload
```

## 7. Datastore Deployment

Redis / Valkey is the shared state store for:

- proxy registration
- proxy subnet allocation
- real IP to internal IP mappings
- reverse mappings
- watcher-promoted reserved identities

Recommended location:
- a separate control-plane host reachable from all regions
- not tied to a regional proxy host
- not assumed to be the SG Headscale host

Tracked Docker compose:

- [docker-compose.valkey.yml](../deploy/docker-compose.valkey.yml)
- [.env.example](../deploy/.env.example)

Quick start:

```bash
cd deploy
cp .env.example .env
docker compose -f docker-compose.valkey.yml up -d
```

Important:
- if the host already runs Redis / Valkey on `6379`, change `REDIS_PORT` in `.env`, for example to `6380`
- the tracked compose uses a pinned published image tag

## 8. Proxy Deployment

### 8.1 Proxy host setup

Install packages:

```bash
apt-get update
apt-get install -y nftables iproute2
```

Enable forwarding:

```bash
sysctl -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward=1' >/etc/sysctl.d/99-mcproxy.conf
```

### 8.2 Proxy config

Important proxy fields:

- `proxy_id`
  - unique per proxy host
- `redis_addr`
- `redis_password`
- `intercept_port`
  - where nftables redirects the player traffic
- `health_port`
  - serves `/healthz`
- `port_range`
  - the exposed game port range
- `servers[]`
  - each entry maps:
    - `proxy_public_ip -> backend_ip` for legacy raw-IP fallback
    - `hostnames[] -> backend_ip` for hostname-based routing

In the hostname-first model, multiple `servers[]` entries may intentionally share the same `proxy_public_ip`.

Example install:

```bash
install -m 0644 deploy/config.example.json /etc/mcproxy/config.json
```

### 8.3 Start proxy

```bash
systemctl enable --now mcproxy
```

Inspect it:

```bash
systemctl status mcproxy
journalctl -u mcproxy -f
curl -fsS http://localhost:18080/healthz
```

### 8.4 What the proxy creates

The proxy creates and manages:

- `mcproxy_filter`
- `mcproxy_intercept`
- `mcproxy_nat`

It also installs a local route like:

```text
local <PROXY_SUBNET_A> dev lo
```

The health endpoint is healthy only when:

- intercept listener is active
- Redis is reachable
- required nftables tables exist
- local proxy subnet route exists

## 9. Watcher Deployment

### 9.1 Node host setup

Install:

```bash
apt-get update
apt-get install -y nftables iproute2
```

### 9.2 Watcher config

Important watcher fields:

- `redis_addr`
- `redis_password`
- `volumes_root`
  - backend volume path containing ban files
- `node_dnat.public_ip`
  - node public IP where Docker published Minecraft ports
- `node_dnat.tailscale_interface`
  - usually `tailscale0`
- `node_dnat.docker_network`
  - Docker network name used to resolve container bridge IPs
  - defaults to `bridge`
- `node_dnat.port_range`

Install:

```bash
install -m 0644 deploy/watcher.example.json /etc/mcproxy/watcher.json
```

### 9.3 Start watcher

```bash
systemctl enable --now mcwatcher
```

Inspect:

```bash
systemctl status mcwatcher
journalctl -u mcwatcher -f
nft list table ip mcproxy_node
```

## 10. DNS and Customer Hostnames

The recommended player-facing model is:

- one hostname per server
- player enters `hostname:port`
- if the server uses a non-default Minecraft port, the customer can publish an SRV record
- many different server hostnames may all resolve to the same regional proxy IPs

Important rule:
- customers must never point SRV targets at backend node IPs
- SRV targets must point to proxy ingress hostnames

Example:

```dns
_minecraft._tcp.<CUSTOMER_HOSTNAME>.  SRV 0 0 25551 <INGRESS_HOST_A>.
```

The ingress hostname should resolve to proxy IPs, not node IPs.

The proxy should be configured with the ingress hostname it will actually see in the handshake, for example:

- `<INGRESS_HOST_A>`

Do not assume the handshake hostname will always be the vanity hostname the customer typed. With SRV, the proxy-managed ingress hostname is the safe routing identity.

This means customers no longer need a dedicated proxy IP per backend node or per server. A small regional proxy IP set can front many servers, as long as each server has a unique ingress hostname.

## 11. DNS Failover Controller

`dnsfailover` is for regional failover without paid Cloudflare Load Balancing.

It works like this:

- probes one health URL per region
- health is proxy-ingress health, not backend service health
- manages Cloudflare `A` records for server ingress hostnames
- fails whole regions over to SG
- automatically fails back after a stable recovery window

Use it when:
- US servers should normally resolve to US proxy IPs
- EU servers should normally resolve to EU proxy IPs
- SG should be returned only when a regional proxy is unhealthy

Install config:

```bash
install -m 0644 deploy/failover.example.json /etc/mcproxy/failover.json
```

Start:

```bash
systemctl enable --now dnsfailover
```

Inspect:

```bash
systemctl status dnsfailover
journalctl -u dnsfailover -f
```

Controller requirements:

- separate controller host is preferred
- Cloudflare API token with DNS edit permissions
- correct `zone_id`
- inventory of server hostnames grouped by region
- health URLs pointing at proxy `/healthz` endpoints

## 12. Headscale / Tailscale Setup

This system requires both route approval and ACL permission.

### 12.1 Proxy hosts

Each proxy must advertise its assigned subnet:

```bash
tailscale up --advertise-routes=<PROXY_SUBNET_A>
```

### 12.2 Backend nodes

Each backend node must accept routes:

```bash
tailscale up --accept-routes
```

### 12.3 ACLs

Approving a route is not enough. ACLs must also allow traffic sourced from proxy subnet space.

Recommended scalable ACL alias:

```json
"proxy-nets": "<PROXY_SUBNET_POOL>"
```

Then allow both directions:

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

### 12.4 Required routing result

On backend nodes, traffic to proxy internal identities must use `tailscale0`, not the public NIC.

Check:

```bash
ip route show table 52
ip route get <INTERNAL_IDENTITY_A>
```

Expected shape:

```text
<INTERNAL_IDENTITY_A> dev tailscale0 table 52 ...
```

## 13. Validation

### 13.1 Proxy health

```bash
curl -fsS http://localhost:18080/healthz
```

Expected:

```json
{"status":"ok"}
```

### 13.2 Proxy tables and route

```bash
nft list tables | rg 'mcproxy_'
ip route show table local | grep '10\.'
```

### 13.3 Node DNAT

```bash
nft list table ip mcproxy_node
```

### 13.4 Transparent-source path

Do not rely only on:

```bash
nc -vz <BACKEND_NODE_PRIVATE_IP> 25551
```

That only tests from the proxy host’s Tailscale IP.

Test the real dataplane shape instead:

```bash
nc -s <INTERNAL_IDENTITY_A> -vz <BACKEND_NODE_PRIVATE_IP> 25551
```

### 13.5 End-to-end traffic

Connect to one proxy public IP and one real game port.

Expected:
- proxy logs `connection established`
- backend sees a `10.x.x.x` source IP
- Redis contains `map:<real_ip>`

### 13.6 DNS failover

If `dnsfailover` is deployed:

- stop or break the regional proxy health endpoint
- confirm DNS answers move to SG
- restore the regional proxy
- confirm failback occurs after the configured stability window

## 14. Operational Notes

- The proxy health endpoint is not a customer game-port probe.
- A backend server being intentionally offline should not mark the whole proxy unhealthy.
- Watcher warnings for public IPs in old `banned-ips.json` files do not by themselves indicate proxy path failure.
- If you scale to more regional proxies later, keep ACLs aggregated under `proxy-nets` instead of adding per-proxy ACL entries.

## 15. Rollback

Stop services:

```bash
systemctl disable --now mcproxy || true
systemctl disable --now mcwatcher || true
systemctl disable --now dnsfailover || true
```

Remove managed nftables state:

```bash
nft delete table ip mcproxy_filter 2>/dev/null || true
nft delete table ip mcproxy_intercept 2>/dev/null || true
nft delete table ip mcproxy_nat 2>/dev/null || true
nft delete table ip mcproxy_node 2>/dev/null || true
```

Remove proxy local route if needed:

```bash
ip route del local <PROXY_SUBNET_A> dev lo
```
