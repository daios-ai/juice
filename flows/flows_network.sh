# Real-network federation check (release gate for federation-touching changes, §15).
# EXCLUDED from the default suite and from `go test ./...` — it needs the public internet and,
# for the hole-punch assertion, a peer on a different network. Run it from a machine behind a
# real NAT (e.g. a home connection):
#
#   JUICE_NETWORK_FLOWS=1 \
#   JUICE_NET_BOOTSTRAP="/dnsaddr/seed.example/p2p/<seedid>" \
#   JUICE_NET_PEER_KEY="<remote peer public key>" \
#   JUICE=/path/to/juice bash flows/flows_test.sh
#
# JUICE_NET_BOOTSTRAP  — a reachable public seed/bootstrap multiaddr (the ONLY thing configured; the
#                        remote peer must then be found through routing discovery, never a manual address).
# JUICE_NET_PEER_KEY   — a remote kernel's base64url public key on a DIFFERENT network, running and
#                        advertising the discovery namespace.
#
# When JUICE_NETWORK_FLOWS=1, BOTH vars are REQUIRED: their absence is a hard failure, not a
# note-and-pass. A vacuous gate is exactly what let the client-mode discovery regression ship (§15).

flow_network_reachability() {
    echo "=== FLOW network_reachability (real internet) ==="
    if [ "${JUICE_NETWORK_FLOWS:-0}" != "1" ]; then
        echo "  SKIP: set JUICE_NETWORK_FLOWS=1 to run the real-network check"
        return
    fi
    local boot="${JUICE_NET_BOOTSTRAP:-}"
    local peer="${JUICE_NET_PEER_KEY:-}"
    if [ -z "$boot" ]; then
        fail "net.bootstrap_missing" "JUICE_NET_BOOTSTRAP not set — the real-network gate cannot run"
        return
    fi
    if [ -z "$peer" ]; then
        fail "net.peer_missing" "JUICE_NET_PEER_KEY not set — the gate needs a remote kernel on another network to discover"
        return
    fi

    local dir; dir=$(new_dir)
    local db="$dir/n/juice.db" hm="$dir/nsys"
    mkdir -p "$dir/n" "$hm/.juice"
    # allow_local_sources=false: a REAL network run, public addresses only. Only the public bootstrap
    # is configured — the remote peer must be found through routing discovery, never a manual address.
    start_server "$db" "$hm" kernel_handle=net-node bootstrap_peers="$boot" discovery_interval_seconds=5 || {
        fail "net.boot" "kernel did not start"; return; }
    j "$db" "$hm" auth login sys --password sys-pass >/dev/null 2>&1

    # Self-identity is reachable and announced.
    local mykey; mykey=$(kernel_key "$db" "$hm")
    assert_nonempty "net.own_identity" "$mykey"

    # The NAT-side node must DISCOVER the remote peer purely through routing discovery: with no manual
    # peering and no address, the peer's key surfaces in the merged roster once its provider record and
    # gossip are pulled — the address-bearing discovery the key-only PEX path could not do.
    local found=no
    for _ in $(seq 1 30); do
        if jj "$db" "$hm" admin peers | grep -q "$peer"; then found=yes; break; fi
        sleep 2
    done
    assert_eq "net.peer_discovered" yes "$found"

    # And the live connection is hole-punched (direct) or relayed — the path loopback cannot reproduce.
    local path; path=$(pathf "$(jj "$db" "$hm" admin inspect "$peer")" reachability.path)
    echo "  reachability to remote peer: $path"
    assert_eq "net.reachable" yes "$([ "$path" = "direct" ] || [ "$path" = "relayed" ] && echo yes || echo no)"
}
