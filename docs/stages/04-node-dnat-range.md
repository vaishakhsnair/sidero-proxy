# Stage 04: Node DNAT Range Shim

Status: complete

Scope:
- Watcher startup can install a node-side DNAT rule for a configured port range.
- Traffic arriving on a private interface can be translated to the node’s public-IP-bound service port.
- This covers the deployment reality where Docker publishes servers only on the node’s public IP.

Build and syntax checks:
- `GOCACHE=/tmp/go-build go test ./...`
- `GOCACHE=/tmp/go-build go build ./cmd/watcher`

Lab:
- Script: [scripts/lab/stage04_node_dnat_range.sh](/home/onegrit/Documents/Projects/sidero-proxy/scripts/lab/stage04_node_dnat_range.sh)
- Topology:
  - 1 fake node public IP on a temporary Docker bridge
  - 1 fake node private IP on the same bridge
  - 1 backend service published only on the fake public IP
  - 1 client container
  - 1 watcher process in a privileged host-network container

Validated flow:
- Backend service was published only at `public_ip:25565`.
- Client connected to `private_ip:25565`.
- Watcher-installed DNAT on the bridge interface redirected the traffic to `public_ip:25565`.
- Client received the backend response successfully.

Observed result:
- Client output: `node-dnat-ok`
- Watcher log confirmed `node dnat ensured`.
- nftables rule observed:
  - `iifname "<bridge>" tcp dport 25565 dnat to <public_ip>:25565`

Impact:
- The node-side reachability gap is now proven for the port-range DNAT strategy used by the watcher configuration.
