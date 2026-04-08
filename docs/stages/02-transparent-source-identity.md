# Stage 02: Transparent Source Identity

Status: complete

Scope:
- Proxy installs a local route for its assigned `/16`.
- Backend connections are created with the assigned internal IP as the source address.
- Range interception and original-destination routing from Stage 01 remain intact.

Build and syntax checks:
- `GOCACHE=/tmp/go-build go test ./...`
- `GOCACHE=/tmp/go-build go build ./cmd/proxy`

Lab:
- Script: [scripts/lab/stage02_transparent_source_identity.sh](/home/onegrit/Documents/Projects/sidero-proxy/scripts/lab/stage02_transparent_source_identity.sh)
- Topology:
  - 1 fake proxy public IP on a temporary Docker bridge
  - 1 backend node container
  - 1 client container
  - 1 local Valkey instance
  - 1 proxy process in a privileged host-network container

Validated flow:
- Client connects to proxy public IP on the configured Minecraft port.
- Proxy resolves original destination and assigns internal identity `10.1.0.1`.
- Proxy dials backend with transparent source bind using `10.1.0.1`.
- Backend sees the peer as `10.1.0.1`, not the proxy host bridge IP.

Observed result:
- Client response contained `peer=10.1.0.1:<ephemeral-port>`.
- Backend log confirmed `peer=10.1.0.1:<ephemeral-port>`.
- Redis mapping remained `proxy-1 -> 10.1.0.1`.

Impact:
- This is the first stage where the implementation actually satisfies the core identity-preservation requirement of the architecture.
