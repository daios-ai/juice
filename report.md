# Federation discovery — diagnosis and fix (2026-08-05)

## TL;DR

The production failure is a **pre-dispatch address-resolution regression**, not a gossip
payload-size problem. The earlier payload-first hypothesis (compact gossip becoming one
all-or-nothing catalog + up to 100 evidence bundles) is **withdrawn**: the failing node never
gets far enough to decode a response. The log line is

```text
fed: request not dispatched: fed: cannot resolve peer <key>: context deadline exceeded
```

`resolve()` exhausts its `FindPeer` retries against the 10-second pull deadline on every pass.
The `gossip.served bytes=…` lines in the same transcript are this node *serving* other
requesters; they are not responses to the request that was never dispatched.

## Root cause

`v0.12.6` replaced address-bearing DHT provider discovery with key-only peer-exchange (PEX)
`known_kernels` hints. A hint carries a public key and **no address**, so every hinted
NAT-bound kernel becomes reachable only via a cold `dht.FindPeer`. For a DHT *client* peer
(the common case behind NAT) that lookup routinely fails, because a client publishes no
provider record and may not be in any server's routing table. Key-only hints therefore cannot
restore reachability — the missing capability is **address acquisition**, which only provider
records supply.

## The v0.11 baseline that was lost

`v0.11.x` (same libp2p/DHT versions, same circuit relay) advertised a fixed discovery namespace
with DHT **provider records** and enumerated it before pulling gossip. Provider results are
`peer.AddrInfo` — addresses, not bare IDs — and `FindProvidersAsync` refreshes them into the
receiver's peerstore with a temporary TTL (relay-circuit addresses included). `resolve()` then
found a fresh usable address and connected, so NAT↔NAT federation worked. `v0.12.6` removed that
address-acquisition pass; nothing replaced its addresses.

The separate `ModeAuto`→`ModeAutoServer` seed fix (also on the v0.12.6 line) was real and stays;
it was never the reason provider discovery had to be removed.

## Why the tests missed it

Every loopback transport sets `AllowPrivateAddrs=true`, which forces each test DHT into
`ModeServer` and disables the private-address filters. Under that configuration a cold
`FindPeer` succeeds between nodes that would be DHT *clients* behind NAT in production, so the
discovery test and the multi-kernel flows proved key propagation in an all-server DHT only —
never that a client-mode kernel is addressable. The real-network gate (`flows_network.sh`) was
also stale (invoking the removed `admin friend`, testing one manually-supplied peer rather than
discovery of a third kernel) and could not catch it either.

## Fix (this change)

Restore standard **libp2p routing discovery** (`p2p/discovery/routing`) over a fixed namespace:
advertise it, enumerate providers, refresh each `AddrInfo` into the peerstore, convert the peer
ID to the Juice public key, then pull gossip and verify authenticity as before. Provider records
are ephemeral transport data and grant no credit, callability, alias, or persisted identity.
The custom key-only PEX path (`known_kernels`, requester-to-stub learning, hint sampling,
freshness horizon, attempt rotation, stub eviction) is deleted; migration `036`'s two now-unused
columns stay for upgrade history. Gossip keeps first-party identity, catalog, and evidence and
no longer carries membership.

Two behavioral changes ride with the repair:
- `admin peers` now lists every known kernel merged by public key — counterparties (with an
  account) and discovery-only kernels (without) — this kernel excluded, deduplicated by key.
- Outbound leg-(b) evidence covers every receipt-settled **admitted execution**, not only rated
  calls; never-dispatched, rejected (`tx_id == idempotency_key`), and quarantined settlements are
  excluded. Ratings remain an optional attachment.

## The test the regression should have had

A client/server topology test: R is a DHT server + bootstrap; A and B are DHT clients connected
only to R and not to each other; B enumerates the namespace through R, receives A with at least
one address, and opens a gossip stream to A by public key. Plus a repaired real-network gate that
actually exercises the NAT path (hole-punch and relay fallback) and fails hard when the required
remote-topology variables are absent.

## Adjacent cleanup folded in

Internal spec citations had leaked into user-facing text. The `admin inspect` evidence header
(`cmd/juice/cmd_superuser.go`) is rewritten by this change (two evidence views, no `§` leak). The
`sys/transfer` description `(§13)` in `cmd/juice/bootstrap.go` is a **signed manifest field**
(editing it re-hashes the manifest and re-syncs proxies) and is left for a separate, deliberate
copy change rather than bundled here.
