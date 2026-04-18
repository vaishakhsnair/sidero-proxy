# Stage 07: Native nftables Hot Path

## Goal

Remove shelling out to `nft` for per-connection NAT map updates in the proxy dataplane.

At this point the proxy already had:

- correct same-IP refcounted NAT lifecycle
- stable backend source identity preservation
- working range interception and backend routing

The remaining hot-path cost was that every first connect and last disconnect for a real IP spawned `nft -f -` to mutate `mcproxy_nat:nat_map`. That is avoidable overhead when the target is hundreds of concurrent players.

## Code Changes

- [main.go](../../cmd/proxy/main.go)
- [nat.go](../../internal/nat/nat.go)
- [go.mod](../../go.mod)
- [go.sum](../../go.sum)

Implementation details:

- startup/setup nftables table and rule creation still uses the existing `nft` script runner
- runtime `nat_map` element add/delete now uses `github.com/google/nftables`
- the native runner keeps one lasting netlink connection under a mutex
- the proxy now constructs `NATManager` with the native element runner in production

That means the hot path no longer pays for:

- process spawn per NAT mutation
- command-string parsing in the `nft` CLI
- shell-out overhead for first-connect / last-disconnect events

## Validation

Build and test validation:

```bash
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build -o /tmp/mcproxy-proxy-test ./cmd/proxy
```

Functional validation reused the stage 06 same-IP concurrency lab:

- [stage06_refcounted_nat_lifecycle.sh](../../scripts/lab/stage06_refcounted_nat_lifecycle.sh)

The point of rerunning the exact same scenario here was to prove that the dataplane implementation changed while externally visible behavior stayed correct.

## Successful Result

Client output:

```text
=== hold ===
held
still-open
=== quick ===
quick
```

Proxy log excerpt:

```text
connection established ... real_ip=<TEST_CLIENT_IP> internal_ip=<INTERNAL_IDENTITY_A> ...
connection established ... real_ip=<TEST_CLIENT_IP> internal_ip=<INTERNAL_IDENTITY_A> ...
```

Backend log excerpt:

```text
start peer=<INTERNAL_IDENTITY_A>:45297 data=hold-one
start peer=<INTERNAL_IDENTITY_A>:52641 data=quick-two
end peer=<INTERNAL_IDENTITY_A>:52641 data=quick-two
end peer=<INTERNAL_IDENTITY_A>:45297 data=hold-one
```

That confirms:

- the native nftables element path preserves the same functional behavior
- same-IP concurrent flows still share one backend identity
- closing one flow does not break the other

## Operational Note

This stage removes CLI process-spawn overhead from the per-flow NAT update path, which is the relevant improvement for the `400+` concurrent-player target.

Startup still uses the `nft` CLI for table/set/chain/rule provisioning. That is acceptable because it is not on the connection hot path.
