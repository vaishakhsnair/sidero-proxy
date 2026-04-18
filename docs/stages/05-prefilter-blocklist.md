# Stage 05: Prefilter Blocklist And SYN Gate

Status: complete

Scope:
- Proxy startup now installs a dedicated `mcproxy_filter` nftables table.
- The filter scopes itself to the configured proxy public IPs and port range.
- The filter provides:
  - a dynamic blocklist set
  - a SYN rate-limit accept rule
  - a final SYN drop rule

Build and syntax checks:
- `GOCACHE=/tmp/go-build go test ./...`
- `GOCACHE=/tmp/go-build go build ./cmd/proxy`

Lab:
- Script: [scripts/lab/stage05_prefilter_blocklist.sh](../../scripts/lab/stage05_prefilter_blocklist.sh)
- Topology:
  - 1 fake proxy public IP on a temporary Docker bridge
  - 1 backend node container
  - 1 client container
  - 1 local Valkey instance
  - 1 proxy process in a privileged host-network container

Validated flow:
- First connection from the client to the proxy public IP succeeded.
- Backend received the connection from the assigned internal IP.
- The client IP was then added to the `mcproxy_filter` blocklist set.
- The next connection attempt from the same client timed out before reaching the backend.

Observed result:
- First client output: `prefilter-ok`
- Second client output: timed out
- Backend log contained only the first connection
- nftables filter table contained:
  - public IP set
  - blocklist set with the client IP
  - blocklist drop rule
  - SYN rate-limit rule
  - SYN drop fallback rule

Impact:
- The proxy now has a verified pre-conntrack-style ingress control layer in front of the redirect and proxy data path.
