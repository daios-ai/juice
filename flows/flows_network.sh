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
    local db="$(kdb "$dir/n")" hm="$dir/nsys"
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
        if jj "$db" "$hm" admin peers | grep -q -- "$peer"; then found=yes; break; fi
        sleep 2
    done
    assert_eq "net.peer_discovered" yes "$found"

    # And the live connection is hole-punched (direct) or relayed — the path loopback cannot reproduce.
    local path; path=$(pathf "$(jj "$db" "$hm" admin inspect -- "$peer")" reachability.path)
    echo "  reachability to remote peer: $path"
    assert_eq "net.reachable" yes "$([ "$path" = "direct" ] || [ "$path" = "relayed" ] && echo yes || echo no)"

    # §8's conformance line asks for more than reachability: "resolve a remote peer's action by key,
    # call it both directions with the path hole-punched (asserted via `admin inspect`), then force a
    # relay fallback and a restart-mid-call recovery". Discovery alone does not establish that a
    # NAT-bound kernel can trade, which is what U34 actually promises.
    assert_eq "net.path_is_hole_punched" direct "$path"

    # Both directions. The remote side must have published something callable and must be funded
    # here, or prepared to extend credit; JUICE_NETWORK_ACTION names what to call.
    local remote_action=${JUICE_NETWORK_ACTION:-}
    if [ -z "$remote_action" ]; then
        fail "net.two_way_call" "set JUICE_NETWORK_ACTION to a callable action on the remote peer (owner@key/name)"
    else
        local ha; ha=$(home "$dir" natbuyer)
        make_user "$db" "$hm" "$ha" natbuyer
        deposit "$db" "$hm" natbuyer 1000
        local out; out=$(jj "$db" "$ha" run "$remote_action" '{}' 2>&1)
        assert_nonempty "net.outbound_call_settles" "$(strfield "$out" tx_id)"

        # The receipt for a call over a real network must verify offline, against the peer's key
        # alone (§5 G7) — the point of signing every payload rather than trusting the channel.
        local txid; txid=$(strfield "$out" tx_id)
        assert_json "net.remote_receipt_verifies" "$(jj "$db" "$ha" tx verify "$txid")" valid True

        # Restart mid-call: the kernel goes down while a call to the remote peer is in flight, comes
        # back, and the call settles from the peer's signed evidence with nothing left reserved.
        ( j "$db" "$ha" run "$remote_action" '{}' >"$dir/nat_call.out" 2>&1 ) &
        local caller=$!
        sleep 1
        stop_server "$db"
        wait "$caller" 2>/dev/null
        start_server "$db" "$hm" kernel_handle=net-node bootstrap_peers="$boot" discovery_interval_seconds=5 \
            || { fail "net.restart" "kernel did not come back"; return; }
        await_login "$db" "$hm" || { fail "net.restart_serving" "not serving after restart"; return; }
        local i settled=no
        for i in $(seq 1 40); do
            [ "$(numfield "$(jj "$db" "$ha" user me)" locked)" -eq 0 ] && { settled=yes; break; }
            sleep 3
        done
        assert_eq "net.restart_mid_call_settles" yes "$settled"
    fi

    # Forced relay. A direct path exists, so the relay leg needs the direct one denied; that is a
    # host firewall rule, not something a flow may impose on the machine it runs on.
    # §8 requires the relayed path to be asserted, not merely mentioned. A direct path exists here
    # by construction, so the relay leg needs the direct route denied at the host — a firewall rule
    # this flow must not impose on the machine it runs on. It is therefore a required, separate
    # invocation, and its absence is a gap in the gate rather than a pass.
    if [ "${JUICE_NETWORK_FORCE_RELAY:-0}" = "1" ]; then
        local rpath; rpath=$(pathf "$(jj "$db" "$hm" admin inspect -- "$peer")" reachability.path)
        assert_eq "net.relay_fallback_is_relayed" relayed "$rpath"
        if [ -n "${JUICE_NETWORK_ACTION:-}" ]; then
            local ha2; ha2=$(home "$dir" relaybuyer)
            make_user "$db" "$hm" "$ha2" relaybuyer
            deposit "$db" "$hm" relaybuyer 1000
            assert_nonempty "net.relayed_call_settles" \
                "$(strfield "$(jj "$db" "$ha2" run "$JUICE_NETWORK_ACTION" '{}')" tx_id)"
        fi
    else
        fail "net.relay_fallback" \
            "not exercised — block the direct route to the peer and re-run with JUICE_NETWORK_FORCE_RELAY=1 (§8 requires it)"
    fi
}
