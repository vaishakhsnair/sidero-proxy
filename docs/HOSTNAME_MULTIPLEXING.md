# Hostname Multiplexing

## Summary

Hostname multiplexing is the ability to host many Minecraft servers behind the same public IP address and the same public port, usually `25565`, while still sending each connection to the correct backend server.

The routing decision is made from the hostname the client presents in the Minecraft handshake, similar in spirit to how HTTP virtual hosts use the `Host` header.

Example:

- `<SERVER_HOST_A>` -> Survival backend
- `<SERVER_HOST_B>` -> Creative backend
- `<SERVER_HOST_C>` -> Events backend

All three can share the same public IP and the same public port.

## What Problem It Solves

Without hostname multiplexing, each Minecraft server generally needs one of:

- its own public IP
- its own distinct public port
- its own dedicated edge proxy/IP mapping

That causes product and operational friction:

- customers have to use non-standard ports
- IPv4 consumption grows quickly
- DDoS-protected ingress becomes more expensive if every server needs a unique public endpoint
- migrating a server between nodes is harder if the customer-facing endpoint is tightly coupled to the backend

Hostname multiplexing solves this by allowing the platform to expose:

- one hostname per server
- one standard port for all servers
- a smaller number of public ingress IPs

## How It Works

The player connects using a hostname such as `<SERVER_HOST_A>`.

The Minecraft client resolves DNS and opens a TCP connection, but it also includes a hostname value in the initial Minecraft handshake. The ingress layer reads that hostname and uses it as the routing key.

Conceptually:

1. Player enters `<SERVER_HOST_A>`
2. DNS resolves to a shared ingress IP
3. Client connects to `ingress-ip:25565`
4. Client handshake includes the hostname context
5. Ingress routes to the configured backend for `<SERVER_HOST_A>`

This allows many hostnames to share one public ingress address and one public port.

## Why It Can Matter As A Product Feature

This is not just a networking trick. It changes the product surface.

### Cleaner customer experience

Customers can advertise:

- `<CUSTOMER_HOSTNAME>`

instead of:

- `<SERVER_PUBLIC_IP>:25581`

That is easier to remember, easier to brand, and closer to what customers expect from premium game hosting.

### Standard port for everyone

Every customer can use `25565`, which avoids awkward support cases around custom ports, SRV setup confusion, and client-side friction.

### Better IPv4 efficiency

Many servers can share the same ingress IP, which reduces public IPv4 requirements and any cost attached to protected public endpoints.

### Easier DNS-based platforming

If the service model is "one hostname per server", then DNS becomes a clean customer-facing abstraction. The backend placement becomes an internal concern.

### Possible server mobility

If routing is keyed by hostname instead of by node IP, the platform can move a server between nodes while keeping the customer-facing hostname stable.

This is useful, but it should be treated as a secondary benefit unless server mobility is an actual operational requirement.

## What It Does Not Solve

Hostname multiplexing is not a complete platform strategy by itself.

It does not inherently solve:

- DDoS protection
- authorization or privacy between servers
- backend identity preservation
- backend health management
- instant failover

It also does not help when players connect by raw IP instead of hostname. The feature works best when hostname-based access is the intended product model.

## Security And Abuse Considerations

Hostnames should not be treated as secrets.

If a server is routable by hostname, then a determined person may guess or enumerate hostnames. That means hostname multiplexing should be seen as a routing feature, not an access-control feature.

Private server access still needs to be enforced by the Minecraft stack itself, such as:

- whitelist
- online mode / proper proxy auth chain
- panel-managed controls
- plugin-level authorization

Reasonable edge protections still help:

- exact hostname matching only
- reject unknown hostnames quickly
- rate-limit invalid-hostname scanning
- observe and block abusive probing

## When It Is Worth Shipping

Hostname multiplexing is worth shipping if the platform wants to offer:

- one hostname per server
- standard `25565` access for all servers
- reduced IPv4 consumption
- cleaner branded customer endpoints

It becomes especially attractive when upstream DDoS protection already exists, because the platform can focus on UX and routing rather than on inventing a separate defensive network edge.

## When It Is Probably Not Worth Shipping

It may not be worth the complexity if:

- customers are happy using raw IP and custom ports
- the platform does not need branded hostnames
- the platform already allocates enough public IPs comfortably
- the only original motivation was DDoS protection, and that is now handled elsewhere

In that case, hostname multiplexing can be an unnecessary abstraction layer.

## Product Questions To Scrutinize

Before building or deploying it broadly, the right questions are:

- Do customers strongly value "my own hostname on 25565"?
- Does this improve conversion or retention versus plain `IP:port` hosting?
- Does it reduce public IPv4 spend enough to justify the engineering and operational cost?
- How often will server mobility actually be used in production?
- Is DNS-based customer branding already part of the product direction?
- Is raw IP access still expected, or can hostnames be the canonical access model?

## Practical Framing

The strongest case for hostname multiplexing is:

- a product feature that makes Minecraft hosting feel cleaner and more premium
- while reducing endpoint sprawl and public IP consumption

The weakest case is:

- using it only because a large proxy system already exists

If adopted, it should be justified as a customer-facing ingress feature, not as complexity looking for a reason to exist.
