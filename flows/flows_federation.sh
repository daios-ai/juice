# Federation flows (real multi-kernel over the libp2p transport, §13). Built on flows/lib.sh.
#
# _fed_setup boots two kernels on 127.0.0.1. R comes up first and acts as the bootstrap + relay
# for the network (every kernel now serves the DHT and relay — no separate seed process). L
# bootstraps to R's address, then the kernels are addressed only by Ed25519 public key: they
# resolve each other by key through R's DHT and friend by key — no URL anywhere. This is the
# loopback analogue of home kernels finding each other with no dialable address. FED_RKEY holds R's key.

# Globals set by _fed_setup: FED_DBL FED_DBR FED_HL FED_HR FED_BPORT FED_RID FED_PROXY FED_RKEY FED_LKEY FED_BOOT.
_fed_setup() {
    local dir="$1"
    FED_DBL="$dir/l/juice.db"; FED_DBR="$dir/r/juice.db"
    FED_HL="$dir/lsys"; FED_HR="$dir/rsys"
    mkdir -p "$dir/l" "$dir/r" "$FED_HL/.juice" "$FED_HR/.juice"

    FED_BPORT=$(backend_port); start_backend "$FED_BPORT" 200 '{"greeting":"hello"}'
    # R boots first and is the flow's bootstrap+relay; L (and T, in the gossip flow) dial it.
    start_server "$FED_DBR" "$FED_HR" kernel_handle=@kernel-r || return 1
    FED_BOOT=$(kernel_fed_addr "$FED_DBR")
    [ -n "$FED_BOOT" ] || return 1
    start_server "$FED_DBL" "$FED_HL" kernel_handle=@kernel-l bootstrap_peers="$FED_BOOT" || return 1
    j "$FED_DBR" "$FED_HR" auth login @sys --password syspass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" auth login @sys --password syspass >/dev/null 2>&1

    # Learn each kernel's own public key (federation identity; no .well-known anymore).
    FED_RKEY=$(kernel_key "$FED_DBR" "$FED_HR")
    FED_LKEY=$(kernel_key "$FED_DBL" "$FED_HL")
    [ -n "$FED_RKEY" ] && [ -n "$FED_LKEY" ] || return 1

    FED_RID=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create greet --kind http --source "http://127.0.0.1:$FED_BPORT" --description "greet" --price 0)" id)
    [ -n "$FED_RID" ] || return 1
    j "$FED_DBR" "$FED_HR" action enable "$FED_RID" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$FED_RID" --public >/dev/null 2>&1

    # L friends R by key alone; the transport resolves the key via the seed.
    j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY" >/dev/null 2>&1 || return 1
    FED_PROXY=$(strfield "$(jj "$FED_DBL" "$FED_HL" action show "@kernel-r/sys/greet")" id)
    [ -n "$FED_PROXY" ] || return 1
    return 0
}

# _all_receipt_checks vr_json — "OK" iff valid=true and all 9 receipt checks are true.
_all_receipt_checks() {
    python3 -c "
import sys,json
vr=json.loads(sys.argv[1]); c=vr.get('checks',{}); bad=[]
if vr.get('valid') is not True: bad.append('valid')
for k in ['receipt_hash','signature','action_id','status','charge','settlement_arith','refund_conservation','args_hash','reply_hash']:
    if c.get(k) is not True: bad.append(k)
print('OK' if not bad else 'FAIL:'+','.join(bad))" "$1" 2>/dev/null
}

flow_federation_import_execute() {
    echo "=== FLOW federation_import_execute ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_import.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)
    assert_nonempty "fed_import.call_succeeds" "$tx_id"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_nonempty "fed_import.remote_receipt_hash" "$(strfield "$tx" remote_receipt_hash)"
    assert_nonempty "fed_import.remote_receipt_json" "$(strfield "$tx" remote_receipt_json)"
    assert_jnum "fed_import.local_stats_uses" "$(jj "$FED_DBL" "$FED_HL" action stats "$FED_PROXY")" uses 1
}

flow_federation_changed_reimport() {
    echo "=== FLOW federation_changed_reimport ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_reimport.setup" "setup failed"; return; }

    # Add a new action on R; re-friend must pick it up while leaving greet (unchanged) alone.
    local wid; wid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create wave --kind http --source "http://127.0.0.1:$FED_BPORT" --description "wave" --price 0)" id)
    j "$FED_DBR" "$FED_HR" action enable "$wid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$wid" --public >/dev/null 2>&1
    assert_eq "fed_reimport.refriend" 0 "$(j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY" >/dev/null 2>&1; echo $?)"

    assert_json "fed_reimport.wave_proxy_active" "$(jj "$FED_DBL" "$FED_HL" action show @kernel-r/sys/wave)" active True
    local greet; greet=$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")
    assert_json "fed_reimport.greet_still_active" "$greet" active True
    assert_json "fed_reimport.id_preserved" "$greet" id "$FED_PROXY"
    assert_nonempty "fed_reimport.wave_callable" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/wave '{}')" tx_id)"
}

flow_federation_unfriend() {
    echo "=== FLOW federation_unfriend ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_unfriend.setup" "setup failed"; return; }

    assert_contains "fed_unfriend.unfriended" "nfriended" "$(j "$FED_DBL" "$FED_HL" admin unfriend @kernel-r 2>&1)"
    assert_json "fed_unfriend.proxy_inactive" "$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")" active False
    # L's unfriend only affects L; R's original action stays active.
    assert_json "fed_unfriend.remote_still_active" "$(jj "$FED_DBR" "$FED_HR" action show "$FED_RID")" active True
    assert_fails "fed_unfriend.call_rejected" "" -- j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}'
}

flow_fed_verify_receipt() {
    echo "=== FLOW fed_verify_receipt ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_verify.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)
    [ -n "$tx_id" ] || { fail "fed_verify.call" "no tx_id"; return; }
    local vr; vr=$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")
    assert_json "fed_verify.buyer_valid"    "$vr" valid True
    assert_eq   "fed_verify.signature_check" True "$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('checks',{}).get('signature'))" "$vr" 2>/dev/null)"
    assert_eq   "fed_verify.receipt_hash_check" True "$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('checks',{}).get('receipt_hash'))" "$vr" 2>/dev/null)"

    # Verifying a non-remote-proxy (local) tx → ErrInvalidState.
    local ltx; ltx=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @sys/time '{}')" tx_id)
    assert_fails "fed_verify.local_tx_rejected" "" -- j "$FED_DBL" "$FED_HL" tx verify "$ltx"
}

flow_fed_all_receipt_checks() {
    echo "=== FLOW fed_all_receipt_checks ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_all_receipt.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)
    [ -n "$tx_id" ] || { fail "fed_all_receipt.call" "no tx_id"; return; }
    assert_eq "fed_all_receipt.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_denial_unfriended() {
    echo "=== FLOW fed_denial_unfriended ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_denial_unfriended.setup" "setup failed"; return; }

    # R unfriends L → R denies L's inbound calls; L's proxy is still active locally, so the
    # call goes out and comes back as a signed 403 denial receipt (failure tx, all checks pass).
    assert_contains "fed_denial_unfriended.unfriend" "nfriended" "$(j "$FED_DBR" "$FED_HR" admin unfriend @kernel-l 2>&1)"
    j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}' >/dev/null 2>&1 || true
    local tx_id; tx_id=$(python3 -c "import sys,json;t=json.loads(sys.argv[1]);print(t[0]['id'] if t else '')" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_denial_unfriended.tx_recorded" "$tx_id"
    assert_json "fed_denial_unfriended.tx_status_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure
    assert_eq "fed_denial_unfriended.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_denial_underfunded() {
    echo "=== FLOW fed_denial_underfunded ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_denial_underfunded.setup" "setup failed"; return; }

    # Paid action on R; L imports it but is NOT funded on R → underfunded → 402 denial receipt.
    local pid; pid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create paid-svc --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price 100)" id)
    j "$FED_DBR" "$FED_HR" action enable "$pid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$pid" --public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY" >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit @sys 1000 >/dev/null 2>&1

    j "$FED_DBL" "$FED_HL" run @kernel-r/sys/paid-svc '{}' >/dev/null 2>&1 || true
    local tx_id; tx_id=$(python3 -c "import sys,json;print(next((t['id'] for t in json.loads(sys.argv[1]) if t.get('action_name')=='sys/paid-svc'),''))" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_denial_underfunded.tx_recorded" "$tx_id"
    assert_json "fed_denial_underfunded.tx_status_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure
    assert_eq "fed_denial_underfunded.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_disabled_action_rejection() {
    echo "=== FLOW fed_disabled_action_rejection ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_disabled.setup" "setup failed"; return; }

    # R disables greet (which L has imported and still shows active locally). L's call goes out,
    # R answers with a signed zero-charge rejection instead of a receiptless error, so L settles
    # IMMEDIATELY as a failure rather than pinning funds until the 24h pending bound.
    j "$FED_DBR" "$FED_HR" action disable "$FED_RID" >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}' >/dev/null 2>&1 || true
    local tx_id; tx_id=$(python3 -c "import sys,json;print(next((t['id'] for t in json.loads(sys.argv[1]) if t.get('action_name')=='sys/greet'),''))" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_disabled.tx_settled_not_pending" "$tx_id"
    assert_json "fed_disabled.tx_status_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure
    assert_eq "fed_disabled.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_import_duty() {
    echo "=== FLOW fed_import_duty ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_import_duty.setup" "setup failed"; return; }

    # Paid action on R (1000); proxy price = 1000 + ceil(1000*500/10000) = 1050 (5% duty).
    local pid; pid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create duty-svc --kind http --source "http://127.0.0.1:$FED_BPORT" --description "duty" --price 1000)" id)
    j "$FED_DBR" "$FED_HR" action enable "$pid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$pid" --public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY" >/dev/null 2>&1
    assert_jnum "fed_import_duty.proxy_price" "$(jj "$FED_DBL" "$FED_HL" action show @kernel-r/sys/duty-svc)" price 1050

    j "$FED_DBR" "$FED_HR" admin deposit @kernel-l 5000 >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit @sys 5000 >/dev/null 2>&1
    local ub pb; ub=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pb=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show @kernel-l)" available)

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/duty-svc '{}')" tx_id)
    assert_nonempty "fed_import_duty.call_succeeded" "$tx_id"
    local ua pa; ua=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pa=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show @kernel-l)" available)
    # L's @sys is caller AND fee recipient: gross 1050 out, fee 50 back → net 1000.
    assert_eq "fed_import_duty.user_charged" 1000 "$(( ub - ua ))"
    assert_eq "fed_import_duty.peer_charged_base" 1000 "$(( pb - pa ))"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_jnum "fed_import_duty.tx_gross" "$tx" gross 1050
    assert_jnum "fed_import_duty.tx_net"   "$tx" net 1000
    assert_jnum "fed_import_duty.tx_fee"   "$tx" fee 50
    assert_json "fed_import_duty.tx_status" "$tx" status success
}

flow_fed_failed_action_refund() {
    echo "=== FLOW fed_failed_action_refund ==="
    local dir fport; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_failed_refund.setup" "setup failed"; return; }
    fport=$(backend_port); start_backend "$fport" 500 '{"error":"boom"}'

    # Paid action on R backed by a 500 backend; proxy price = 100 + ceil(100*5%) = 105.
    local pid; pid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create fail-svc --kind http --source "http://127.0.0.1:$fport" --description "fails" --price 100)" id)
    j "$FED_DBR" "$FED_HR" action enable "$pid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$pid" --public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" admin deposit @kernel-l 5000 >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit @sys 1000 >/dev/null 2>&1
    local ub; ub=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available)

    # Remote 500 → remote failure receipt → full refund to L's caller.
    j "$FED_DBL" "$FED_HL" run @kernel-r/sys/fail-svc '{}' >/dev/null 2>&1 || true
    assert_jnum "fed_failed_refund.balance_unchanged" "$(jj "$FED_DBL" "$FED_HL" user me)" available "$ub"
    local tx_id; tx_id=$(python3 -c "import sys,json;print(next((t['id'] for t in json.loads(sys.argv[1]) if t.get('action_name')=='sys/fail-svc'),''))" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_failed_refund.tx_recorded" "$tx_id"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_json "fed_failed_refund.tx_status_failure" "$tx" status failure
    assert_jnum "fed_failed_refund.tx_gross" "$tx" gross 105
    assert_jnum "fed_failed_refund.tx_net_zero" "$tx" net 0
    assert_jnum "fed_failed_refund.tx_fee_zero" "$tx" fee 0
}

flow_fed_gossip_discovery() {
    echo "=== FLOW fed_gossip_discovery ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_gossip.setup" "L-R setup failed"; return; }

    # L calls R (price 0) → R becomes a transacted friend in L's gossip with earned stats.
    assert_nonempty "fed_gossip.initial_call" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)"

    # Third kernel T discovers R by inspecting L's gossip over the transport, then friends R by key.
    local dbt ht; dbt="$dir/t/juice.db"; ht="$dir/tsys"; mkdir -p "$dir/t" "$ht/.juice"
    start_server "$dbt" "$ht" kernel_handle=@kernel-t bootstrap_peers="$FED_BOOT" || { fail "fed_gossip.bootstrap_t" "T did not start"; return; }
    j "$dbt" "$ht" auth login @sys --password syspass >/dev/null 2>&1

    # T inspects L (resolved by key via the seed); L's transacted-friends list carries R's key + stats.
    # Retry until the DHT lookup converges (loopback is usually instant, but slow under CPU load).
    local ldoc rkey=""
    for _ in $(seq 1 25); do
        ldoc=$(jj "$dbt" "$ht" admin inspect "$FED_LKEY")
        rkey=$(python3 -c "import sys,json;print(next((f['public_key'] for f in json.loads(sys.argv[1]).get('friends',[]) if 'kernel-r' in f.get('handle','')),''))" "$ldoc" 2>/dev/null)
        [ -n "$rkey" ] && break
        sleep 0.2
    done
    assert_nonempty "fed_gossip.r_in_gossip" "$rkey"
    local guses; guses=$(python3 -c "import sys,json;print(next((a.get('uses',0) for f in json.loads(sys.argv[1]).get('friends',[]) if 'kernel-r' in f.get('handle','') for a in f.get('actions',[]) if a.get('name')=='@sys/greet'),0))" "$ldoc" 2>/dev/null)
    assert_eq "fed_gossip.earned_stats" yes "$([ "${guses:-0}" -ge 1 ] && echo yes || echo no)"

    assert_eq "fed_gossip.t_friends_r" 0 "$(j "$dbt" "$ht" admin friend "$rkey" >/dev/null 2>&1; echo $?)"

    # T's greet proxy exists with default stats (uses=0, NOT inherited from gossip).
    local tp; tp=$(strfield "$(jj "$dbt" "$ht" action show @kernel-r/sys/greet)" id)
    assert_nonempty "fed_gossip.t_has_greet_proxy" "$tp"
    assert_jnum "fed_gossip.t_stats_start_default" "$(jj "$dbt" "$ht" action stats "$tp")" uses 0
    # T's own call accumulates T's own stats.
    assert_nonempty "fed_gossip.t_call_succeeds" "$(strfield "$(jj "$dbt" "$ht" run @kernel-r/sys/greet '{}')" tx_id)"
    assert_jnum "fed_gossip.t_stats_accumulate" "$(jj "$dbt" "$ht" action stats "$tp")" uses 1
}

# flow_fed_discovery: cold-start discovery. L boots with R as its only bootstrap peer and must learn
# R into its known network automatically — no `admin friend` — via the startup discovery pass that
# advertises and pulls gossip from bootstrap peers. Proves the two-network split: the known network
# (global, by key) grows without friending (which stays deliberate and local).
flow_fed_discovery() {
    echo "=== FLOW fed_discovery ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/juice.db" hr="$dir/rsys" dbl="$dir/l/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    local bport; bport=$(backend_port); start_backend "$bport" 200 '{"greeting":"hi"}'
    start_server "$dbr" "$hr" kernel_handle=@kernel-r discovery_interval_seconds=2 \
        || { fail "fed_discovery.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_discovery.boot" "no R fed addr"; return; }
    j "$dbr" "$hr" auth login @sys --password syspass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    [ -n "$rkey" ] || { fail "fed_discovery.rkey" "no R key"; return; }

    # R publishes a public action so its gossip carries something to display.
    local rid; rid=$(strfield "$(jj "$dbr" "$hr" action create greet --kind http --source "http://127.0.0.1:$bport" --description greet --price 0)" id)
    j "$dbr" "$hr" action enable "$rid" >/dev/null 2>&1
    j "$dbr" "$hr" action update "$rid" --public >/dev/null 2>&1

    # L joins with R as its ONLY bootstrap peer; it must discover R without friending it.
    start_server "$dbl" "$hl" kernel_handle=@kernel-l bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "fed_discovery.l" "L did not start"; return; }
    j "$dbl" "$hl" auth login @sys --password syspass >/dev/null 2>&1

    # Poll L's known network until R appears — a discovery pass runs at startup, then every 2s.
    local found=no i
    for i in $(seq 1 20); do
        if jj "$dbl" "$hl" admin peers --gossip | grep -q "$rkey"; then found=yes; break; fi
        sleep 1
    done
    assert_eq "fed_discovery.r_discovered_without_friend" yes "$found"
    # The roster names actions owner-qualified (@sys/greet), not a bare "greet".
    assert_contains "fed_discovery.qualified_action" "@sys/greet" "$(jj "$dbl" "$hl" admin peers --gossip)"
    # L never friended R: its friend list (proxy users) holds no R.
    assert_eq "fed_discovery.no_friend" 0 "$(jj "$dbl" "$hl" admin peers | grep -c "$rkey")"
}

# flow_fed_offline — every federation command has defined behavior when the peer is DOWN (§13):
# inspect degrades to local last-known data + offline reachability; friend fails clearly;
# unfriend/peers/identity are local and keep working; nothing hangs (bounded by fedOpTimeout).
flow_fed_offline() {
    echo "=== FLOW fed_offline ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_offline.setup" "setup failed"; return; }

    # Take R offline.
    stop_server "$FED_DBR"

    # inspect: still works, shows the last-imported action and marks the peer offline.
    local doc; doc=$(jj "$FED_DBL" "$FED_HL" admin inspect @kernel-r)
    assert_json "fed_offline.inspect_source_local" "$doc" source local
    assert_json "fed_offline.inspect_offline" "$doc" online False
    assert_contains "fed_offline.inspect_shows_action" "greet" "$doc"

    # friend a down peer: clear, prompt failure (no hang, mentions unreachable).
    assert_fails "fed_offline.friend_unreachable" "unreachable" -- j "$FED_DBL" "$FED_HL" admin friend "$FED_RKEY"

    # unfriend is local: works with the peer down.
    assert_contains "fed_offline.unfriend_local" "nfriended" "$(j "$FED_DBL" "$FED_HL" admin unfriend @kernel-r 2>&1)"

    # peers and identity are local: succeed with the peer down.
    assert_eq "fed_offline.peers_ok"    0 "$(j "$FED_DBL" "$FED_HL" admin peers    >/dev/null 2>&1; echo $?)"
    assert_eq "fed_offline.identity_ok" 0 "$(j "$FED_DBL" "$FED_HL" admin identity >/dev/null 2>&1; echo $?)"
}
