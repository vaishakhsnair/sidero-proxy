# Stage 03: Watcher Cross-Proxy Identity Propagation

Status: complete

Scope:
- Watcher observes a banned internal IP from `banned-ips.json`.
- Watcher resolves that internal IP back to the real client IP through Redis.
- Watcher reserves corresponding internal identities across all registered proxies.
- A later connection through another proxy reuses the watcher-seeded internal identity.

Build and syntax checks:
- `GOCACHE=/tmp/go-build go test ./...`
- `GOCACHE=/tmp/go-build go build ./cmd/proxy ./cmd/watcher`

Lab:
- Script: [scripts/lab/stage03_watcher_cross_proxy_identity.sh](/home/onegrit/Documents/Projects/sidero-proxy/scripts/lab/stage03_watcher_cross_proxy_identity.sh)
- Topology:
  - 2 proxy identities (`proxy-1`, `proxy-2`)
  - 2 fake proxy public IPs on a temporary Docker bridge
  - 1 backend node container
  - 1 client container
  - 1 local Valkey instance
  - 1 watcher process

Validated flow:
- `proxy-2` registers first and receives subnet `10.1.0.0/16`.
- `proxy-1` later handles the first real connection and receives subnet `10.2.0.0/16`.
- First live connection through `proxy-1` caused backend peer `10.2.0.1`.
- Watcher consumed that actual internal IP from `banned-ips.json`.
- Watcher wrote permanent reserved mappings:
  - `proxy-1 -> 10.2.0.1`
  - `proxy-2 -> 10.1.0.1`
- A later connection through `proxy-2` caused backend peer `10.1.0.1`.

Observed result:
- Watcher log confirmed:
  - `real_ip=172.31.6.20`
  - `source_proxy=proxy-1`
  - `reserved_mappings=map[proxy-1:10.2.0.1 proxy-2:10.1.0.1]`
- Backend log confirmed the same player presented different per-proxy internal identities exactly as reserved.

Impact:
- The identity-preservation design now works across proxy boundaries after a watcher-triggered promotion event.
