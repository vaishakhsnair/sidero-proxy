# Stage 06: Refcounted NAT Lifecycle For Same-IP Concurrency

## Goal

Prevent one connection teardown from removing the source-NAT mapping still needed by another live connection from the same real client IP.

Before this stage, `mcproxy` added a NAT element on connect and deleted it on disconnect. That was correct only for one live flow per real IP. With concurrent connections from the same player IP, the first disconnect could remove the mapping while another connection was still active.

This stage changes NAT lifecycle management to refcount by real IP:

- first live connection for a real IP adds the nftables element
- additional connections for that same real IP increment an in-memory reference count
- disconnects decrement the count
- the nftables element is deleted only when the last live connection closes

## Code Changes

- [nat.go](../../internal/nat/nat.go)
- [nat_test.go](../../internal/nat/nat_test.go)

`NATManager` now tracks:

- `internal_ip` bound to a real IP while active
- `ref_count` for concurrent live flows

It also rejects conflicting attempts to bind the same real IP to two different internal IPs at the same time.

## Validation

Syntax and test validation:

```bash
gofmt -w internal/nat/nat.go internal/nat/nat_test.go
GOCACHE=/tmp/go-build go test ./...
GOCACHE=/tmp/go-build go build -o /tmp/mcproxy-proxy-test ./cmd/proxy
```

Lab script:

- [stage06_refcounted_nat_lifecycle.sh](../../scripts/lab/stage06_refcounted_nat_lifecycle.sh)

The lab creates:

- one proxy public IP mapped to one backend
- one fixed-IP client container
- two concurrent TCP connections from that same client IP

Traffic pattern:

1. connection A sends `hold-one`, backend replies `held`, then keeps the socket open for 3 seconds before sending `still-open`
2. connection B starts while connection A is still active and sends `quick-two`
3. connection B closes first
4. connection A must still complete successfully

## Successful Result

Client output:

```text
=== hold ===
held
still-open
=== quick ===
quick
```

Redis mapping:

```text
proxy-1
<INTERNAL_IDENTITY_A>
```

Proxy log excerpt:

```text
connection established ... real_ip=<TEST_CLIENT_IP> internal_ip=<INTERNAL_IDENTITY_A> ...
connection established ... real_ip=<TEST_CLIENT_IP> internal_ip=<INTERNAL_IDENTITY_A> ...
```

Backend log excerpt:

```text
start peer=<INTERNAL_IDENTITY_A>:44507 data=hold-one
start peer=<INTERNAL_IDENTITY_A>:59373 data=quick-two
end peer=<INTERNAL_IDENTITY_A>:59373 data=quick-two
end peer=<INTERNAL_IDENTITY_A>:44507 data=hold-one
```

That proves:

- both concurrent sockets kept the same assigned internal identity
- the short-lived connection could close first
- the long-lived connection kept working after that close
- the NAT entry was not torn down prematurely

## Operational Note

This stage fixes a correctness problem for same-IP concurrency. It does not remove the per-update `nft` process execution cost. For larger concurrency targets, the next scaling step is replacing shelling out to `nft` on each add/delete with a persistent nftables client or batched manager.
