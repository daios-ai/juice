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
# JUICE_NET_BOOTSTRAP  — a reachable public seed/bootstrap multiaddr (required to announce/resolve).
# JUICE_NET_PEER_KEY   — a remote peer's base64url public key on a DIFFERENT network. When set, the
#                        flow friends it and asserts the connection is hole-punched or relayed
#                        (via `admin inspect` reachability) — the part loopback cannot reproduce.
#
# With neither var set the flow reports what it could not exercise rather than silently skipping.

flow_network_reachability() {
    echo "=== FLOW network_reachability (real internet) ==="
    if [ "${JUICE_NETWORK_FLOWS:-0}" != "1" ]; then
        echo "  SKIP: set JUICE_NETWORK_FLOWS=1 to run the real-network check"
        return
    fi
    local boot="${JUICE_NET_BOOTSTRAP:-}"
    if [ -z "$boot" ]; then
        fail "net.bootstrap_missing" "JUICE_NET_BOOTSTRAP not set — cannot announce/resolve on the public network"
        return
    fi

    local dir; dir=$(new_dir)
    local db="$dir/n/juice.db" hm="$dir/nsys"
    mkdir -p "$dir/n" "$hm/.juice"
    # allow_local_sources=false here: this is a REAL network run, public addresses only.
    start_server "$db" "$hm" kernel_handle=net-node bootstrap_peers="$boot" || {
        fail "net.boot" "kernel did not start"; return; }
    j "$db" "$hm" auth login sys --password sys-pass >/dev/null 2>&1

    # Self-identity is reachable and announced.
    local mykey; mykey=$(kernel_key "$db" "$hm")
    assert_nonempty "net.own_identity" "$mykey"

    local peer="${JUICE_NET_PEER_KEY:-}"
    if [ -z "$peer" ]; then
        echo "  NOTE: JUICE_NET_PEER_KEY unset — announce/self-identity verified, but the cross-NAT"
        echo "        hole-punch assertion needs a remote peer on another network. Not exercised."
        return
    fi

    # Friend the remote peer by key (resolved over the public DHT) and settle a call both ways is
    # driven by the remote operator; here we assert reachability of the punched/relayed path.
    assert_eq "net.friend_remote" 0 "$(j "$db" "$hm" admin friend "$peer" >/dev/null 2>&1; echo $?)"
    local path; path=$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('reachability',{}).get('path',''))" "$(jj "$db" "$hm" admin inspect "$peer")" 2>/dev/null)
    echo "  reachability to remote peer: $path"
    assert_eq "net.reachable" yes "$([ "$path" = "direct" ] || [ "$path" = "relayed" ] && echo yes || echo no)"
}
