# Stage 01: Range Intercept Routing

Status: complete

Scope:
- Proxy config supports `proxy_public_ip -> backend_ip` mappings.
- One intercept listener handles a configured port range.
- nftables redirect sends matching traffic to the intercept port.
- Original destination lookup selects the backend node and preserves the destination port.

Build and syntax checks:
- `GOCACHE=/tmp/go-build go test ./...`
- `GOCACHE=/tmp/go-build go build ./cmd/proxy ./cmd/watcher`

Lab:
- Script: [scripts/lab/stage01_range_routing.sh](../../scripts/lab/stage01_range_routing.sh)
- Topology:
  - 2 fake proxy public IPs on a temporary Docker bridge
  - 2 backend node containers
  - 1 client container
  - 1 local Valkey instance
  - 1 proxy process in a privileged host-network container

Validated flow:
- `public-ip-a:25565 -> node-a:25565`
- `public-ip-a:25566 -> node-a:25566`
- `public-ip-b:25565 -> node-b:25565`
- `public-ip-b:25566 -> node-b:25566`

Observed result:
- Range interception and same-port backend routing worked.
- Redis created a stable per-proxy mapping for the test client.
- Dedicated `mcproxy_*` nftables tables were created and then cleaned up after the lab run.

Limitation carried into next stage:
- The backend did not yet see the assigned internal `10.x.x.x` source IP.
- Current path is still a userspace relay for backend egress, so source identity preservation is not complete.
