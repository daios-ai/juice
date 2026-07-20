# Federation flows (real multi-kernel over the libp2p transport, §13). Built on flows/lib.sh.
#
# _fed_setup boots two kernels on 127.0.0.1. R comes up first and acts as the bootstrap + relay
# for the network (every kernel now serves the DHT and relay — no separate seed process). L
# bootstraps to R's address, then the kernels are addressed only by Ed25519 public key: they
# resolve each other by key through R's DHT and L subscribes to R by key — no URL anywhere. This
# is the loopback analogue of home kernels finding each other with no dialable address.

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
    j "$FED_DBR" "$FED_HR" auth login @sys --password sys-pass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" auth login @sys --password sys-pass >/dev/null 2>&1

    # Learn each kernel's own public key (federation identity; no .well-known anymore).
    FED_RKEY=$(kernel_key "$FED_DBR" "$FED_HR")
    FED_LKEY=$(kernel_key "$FED_DBL" "$FED_HL")
    [ -n "$FED_RKEY" ] && [ -n "$FED_LKEY" ] || return 1

    FED_RID=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create greet --kind http --source "http://127.0.0.1:$FED_BPORT" --description "greet" --price 0)" id)
    [ -n "$FED_RID" ] || return 1
    j "$FED_DBR" "$FED_HR" action enable "$FED_RID" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$FED_RID" --visibility public >/dev/null 2>&1

    # L subscribes to R by key alone — a purely local import; the transport resolves the key via the
    # seed. R mounts under its self-reported @kernel-r.
    j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1 || return 1
    FED_PROXY=$(strfield "$(jj "$FED_DBL" "$FED_HL" action show "@kernel-r/sys/greet")" id)
    [ -n "$FED_PROXY" ] || return 1
    return 0
}

flow_fed_rename() {
    echo "=== FLOW fed_rename ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_rename.setup" "setup failed"; return; }

    # The local mount handle is chosen with admin rename (by peer key), not at subscribe time. After
    # renaming @kernel-r to @myremote, R's action is addressable and callable under the new handle.
    j "$FED_DBL" "$FED_HL" admin rename "$FED_RKEY" @myremote >/dev/null 2>&1 || { fail "fed_rename.rename" "rename failed"; return; }
    assert_json "fed_rename.proxy_remounted" "$(jj "$FED_DBL" "$FED_HL" action show @myremote/sys/greet)" id "$FED_PROXY"
    assert_nonempty "fed_rename.callable_under_new" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @myremote/sys/greet '{}')" tx_id)"
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

    # Add a new action on R; re-subscribe must pick it up while leaving greet (unchanged) alone.
    local wid; wid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create wave --kind http --source "http://127.0.0.1:$FED_BPORT" --description "wave" --price 0)" id)
    j "$FED_DBR" "$FED_HR" action enable "$wid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$wid" --visibility public >/dev/null 2>&1
    assert_eq "fed_reimport.resubscribe" 0 "$(j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1; echo $?)"

    assert_json "fed_reimport.wave_proxy_active" "$(jj "$FED_DBL" "$FED_HL" action show @kernel-r/sys/wave)" active True
    local greet; greet=$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")
    assert_json "fed_reimport.greet_still_active" "$greet" active True
    assert_json "fed_reimport.id_preserved" "$greet" id "$FED_PROXY"
    assert_nonempty "fed_reimport.wave_callable" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/wave '{}')" tx_id)"
}

flow_federation_unsubscribe() {
    echo "=== FLOW federation_unsubscribe ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_unsubscribe.setup" "setup failed"; return; }

    assert_contains "fed_unsubscribe.unsubscribed" "nsubscribed" "$(j "$FED_DBL" "$FED_HL" admin unsubscribe @kernel-r 2>&1)"
    assert_json "fed_unsubscribe.proxy_inactive" "$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")" active False
    # The deactivated proxy drops out of the default action list (active-only), but --all still shows it.
    assert_not_contains "fed_unsubscribe.list_hides_inactive" "$FED_PROXY" "$(jj "$FED_DBL" "$FED_HL" action list)"
    assert_contains "fed_unsubscribe.all_shows_inactive" "$FED_PROXY" "$(jj "$FED_DBL" "$FED_HL" action list --all)"
    # L's unsubscribe only affects L; R's original action stays active.
    assert_json "fed_unsubscribe.remote_still_active" "$(jj "$FED_DBR" "$FED_HR" action show "$FED_RID")" active True
    assert_fails "fed_unsubscribe.call_rejected" "" -- j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}'
}

flow_federation_resubscribe() {
    echo "=== FLOW federation_resubscribe ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_resubscribe.setup" "setup failed"; return; }

    # Baseline: subscribe (done by setup) → proxy is listed and callable.
    assert_contains "fed_resubscribe.listed_before" "$FED_PROXY" "$(jj "$FED_DBL" "$FED_HL" action list)"
    assert_nonempty "fed_resubscribe.call_before" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)"

    # Unsubscribe deactivates the imported catalog only. The peer account stays known (not suspended),
    # so it remains in admin peers; only the proxy drops out of the default action list.
    j "$FED_DBL" "$FED_HL" admin unsubscribe @kernel-r >/dev/null 2>&1
    assert_not_contains "fed_resubscribe.gone_after_unsubscribe" "$FED_PROXY" "$(jj "$FED_DBL" "$FED_HL" action list)"
    assert_contains "fed_resubscribe.peer_still_listed" "kernel-r" "$(j "$FED_DBL" "$FED_HL" admin peers)"

    # Subscribe again must reactivate the SAME proxy (unchanged manifest ⇒ reconcile Unchanged).
    assert_eq "fed_resubscribe.resubscribe_ok" 0 "$(j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1; echo $?)"
    local proxy2; proxy2=$(strfield "$(jj "$FED_DBL" "$FED_HL" action show @kernel-r/sys/greet)" id)
    assert_eq "fed_resubscribe.id_preserved" "$FED_PROXY" "$proxy2"
    assert_json "fed_resubscribe.active_again" "$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")" active True
    assert_contains "fed_resubscribe.listed_again" "$FED_PROXY" "$(jj "$FED_DBL" "$FED_HL" action list)"

    # And it's callable again, producing a fresh remote receipt.
    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)
    assert_nonempty "fed_resubscribe.call_again" "$tx_id"
    assert_nonempty "fed_resubscribe.remote_receipt" "$(strfield "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" remote_receipt_hash)"
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

flow_fed_suspend_blocks() {
    echo "=== FLOW fed_suspend_blocks ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_suspend_blocks.setup" "setup failed"; return; }

    # L's first call provisions L's billing account on R (handshake-free, §13). R then suspends L by
    # key; L's proxy is still active locally, so the next call goes out and comes back as a signed
    # rejection receipt (failure tx, all checks pass).
    assert_nonempty "fed_suspend_blocks.first_call" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}')" tx_id)"
    assert_contains "fed_suspend_blocks.suspend" "uspended" "$(j "$FED_DBR" "$FED_HR" admin suspend "$FED_LKEY" 2>&1)"
    j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}' >/dev/null 2>&1 || true
    local tx_id; tx_id=$(python3 -c "import sys,json;t=json.loads(sys.argv[1]);print(t[0]['id'] if t else '')" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_suspend_blocks.tx_recorded" "$tx_id"
    assert_json "fed_suspend_blocks.tx_status_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure
    assert_eq "fed_suspend_blocks.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_denial_underfunded() {
    echo "=== FLOW fed_denial_underfunded ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_denial_underfunded.setup" "setup failed"; return; }

    # Paid action on R; L imports it but is NOT funded on R → underfunded → 402 denial receipt.
    local pid; pid=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create paid-svc --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price 100)" id)
    j "$FED_DBR" "$FED_HR" action enable "$pid" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$pid" --visibility public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit @sys 1000 >/dev/null 2>&1

    # The CLI attributes it to THIS kernel's exhausted credit on the peer (operator remedy), with a
    # distinct exit code — never the caller's own insufficient_funds (§13 peer_unfunded).
    local run_out rc
    run_out=$(j "$FED_DBL" "$FED_HL" run @kernel-r/sys/paid-svc '{}' 2>&1); rc=$?
    assert_eq "fed_denial_underfunded.run_exit_peer_unfunded" 10 "$rc"
    assert_contains "fed_denial_underfunded.run_says_exhausted" "exhausted" "$run_out"
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
    j "$FED_DBR" "$FED_HR" action update "$pid" --visibility public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1
    assert_jnum "fed_import_duty.proxy_price" "$(jj "$FED_DBL" "$FED_HL" action show @kernel-r/sys/duty-svc)" price 1050

    # R deposits to L's account by key (handshake-free: this both provisions and funds it, §13).
    j "$FED_DBR" "$FED_HR" admin deposit "$FED_LKEY" 5000 >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit @sys 5000 >/dev/null 2>&1
    local ub pb; ub=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pb=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show "$FED_LKEY")" available)

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run @kernel-r/sys/duty-svc '{}')" tx_id)
    assert_nonempty "fed_import_duty.call_succeeded" "$tx_id"
    local ua pa; ua=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pa=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show "$FED_LKEY")" available)
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
    j "$FED_DBR" "$FED_HR" action update "$pid" --visibility public >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" admin deposit "$FED_LKEY" 5000 >/dev/null 2>&1
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
    j "$dbt" "$ht" auth login @sys --password sys-pass >/dev/null 2>&1

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

    assert_eq "fed_gossip.t_subscribes_r" 0 "$(j "$dbt" "$ht" admin subscribe "$rkey" >/dev/null 2>&1; echo $?)"

    # T's greet proxy exists with default stats (uses=0, NOT inherited from gossip).
    local tp; tp=$(strfield "$(jj "$dbt" "$ht" action show @kernel-r/sys/greet)" id)
    assert_nonempty "fed_gossip.t_has_greet_proxy" "$tp"
    assert_jnum "fed_gossip.t_stats_start_default" "$(jj "$dbt" "$ht" action stats "$tp")" uses 0
    # T's own call accumulates T's own stats.
    assert_nonempty "fed_gossip.t_call_succeeds" "$(strfield "$(jj "$dbt" "$ht" run @kernel-r/sys/greet '{}')" tx_id)"
    assert_jnum "fed_gossip.t_stats_accumulate" "$(jj "$dbt" "$ht" action stats "$tp")" uses 1
}

# flow_fed_discovery: cold-start discovery. L boots with R as its only bootstrap peer and must learn
# R into its known network automatically — no `admin subscribe` — via the startup discovery pass that
# advertises and pulls gossip from bootstrap peers. Proves the two-network split: the known network
# (global, by key) grows without subscribing (which stays deliberate and local).
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
    j "$dbr" "$hr" auth login @sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    [ -n "$rkey" ] || { fail "fed_discovery.rkey" "no R key"; return; }

    # R publishes a public action so its gossip carries something to display.
    local rid; rid=$(strfield "$(jj "$dbr" "$hr" action create greet --kind http --source "http://127.0.0.1:$bport" --description greet --price 0)" id)
    j "$dbr" "$hr" action enable "$rid" >/dev/null 2>&1
    j "$dbr" "$hr" action update "$rid" --visibility public >/dev/null 2>&1

    # L joins with R as its ONLY bootstrap peer; it must discover R without subscribing to it.
    start_server "$dbl" "$hl" kernel_handle=@kernel-l bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "fed_discovery.l" "L did not start"; return; }
    j "$dbl" "$hl" auth login @sys --password sys-pass >/dev/null 2>&1

    # Poll L's known network until R appears — a discovery pass runs at startup, then every 2s.
    local found=no i
    for i in $(seq 1 20); do
        if jj "$dbl" "$hl" admin peers --gossip | grep -q "$rkey"; then found=yes; break; fi
        sleep 1
    done
    assert_eq "fed_discovery.r_discovered_without_subscribe" yes "$found"
    # The roster names actions owner-qualified (@sys/greet), not a bare "greet".
    assert_contains "fed_discovery.qualified_action" "@sys/greet" "$(jj "$dbl" "$hl" admin peers --gossip)"
    # L never subscribed to R: its peer list (proxy users) holds no R.
    assert_eq "fed_discovery.no_subscription" 0 "$(jj "$dbl" "$hl" admin peers | grep -c "$rkey")"
}

# flow_fed_peer_sync: the discovery timer also pulls gossip from known peers (§13 peer sync),
# caching each peer's liveness (last_seen) and OUR credit on it (peer_credit, from the peer's reported
# counterparty_balance). Proves the "better sync" surfacing: after R deposits L's proxy, L's own
# `admin peers` learns that credit without L ever calling R.
flow_fed_peer_sync() {
    echo "=== FLOW fed_peer_sync ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/juice.db" hr="$dir/rsys" dbl="$dir/l/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    start_server "$dbr" "$hr" kernel_handle=@kernel-r discovery_interval_seconds=2 \
        || { fail "fed_peer_sync.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_peer_sync.boot" "no R fed addr"; return; }
    start_server "$dbl" "$hl" kernel_handle=@kernel-l bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "fed_peer_sync.l" "L did not start"; return; }
    j "$dbr" "$hr" auth login @sys --password sys-pass >/dev/null 2>&1
    j "$dbl" "$hl" auth login @sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    local lkey; lkey=$(kernel_key "$dbl" "$hl")
    [ -n "$rkey" ] && [ -n "$lkey" ] || { fail "fed_peer_sync.rkey" "no R/L key"; return; }

    # L subscribes to R, then R funds L's proxy on R by key (handshake-free: deposit provisions it).
    j "$dbl" "$hl" admin subscribe "$rkey" >/dev/null 2>&1 || { fail "fed_peer_sync.subscribe" "subscribe failed"; return; }
    j "$dbr" "$hr" admin deposit "$lkey" 250 >/dev/null 2>&1

    # A peer-sync pass runs at startup, then every 2s. Poll L's own peer list until it has cached
    # the credit R reports for us — no call to R involved.
    local credit=""
    local i
    for i in $(seq 1 20); do
        credit=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]).get('peers',[]);p=next((x for x in ps if x.get('handle')=='@kernel-r'),{});print(p.get('peer_credit') if p.get('peer_credit') is not None else '')" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)
        [ "$credit" = "250" ] && break
        sleep 1
    done
    assert_eq "fed_peer_sync.credit_cached" 250 "$credit"
    local seen; seen=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]).get('peers',[]);p=next((x for x in ps if x.get('handle')=='@kernel-r'),{});print(p.get('last_seen') or '')" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)
    assert_nonempty "fed_peer_sync.last_seen_cached" "$seen"
}

# flow_fed_inspect_sync — `admin inspect` refreshes the peer-sync cache ON DEMAND, not only on the
# discovery timer (§13 peer sync). inspect already does the live gossip pull (OnInspect == OnGossip),
# so it persists last_seen + our credit there the moment an operator looks. Proven with the timer
# parked at 3600s: the cache can only be refreshed by the inspect, never by a background pass.
flow_fed_inspect_sync() {
    echo "=== FLOW fed_inspect_sync ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/juice.db" hr="$dir/rsys" dbl="$dir/l/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    start_server "$dbr" "$hr" kernel_handle=@kernel-r discovery_interval_seconds=3600 \
        || { fail "fed_inspect_sync.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_inspect_sync.boot" "no R fed addr"; return; }
    start_server "$dbl" "$hl" kernel_handle=@kernel-l bootstrap_peers="$boot" discovery_interval_seconds=3600 \
        || { fail "fed_inspect_sync.l" "L did not start"; return; }
    j "$dbr" "$hr" auth login @sys --password sys-pass >/dev/null 2>&1
    j "$dbl" "$hl" auth login @sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    local lkey; lkey=$(kernel_key "$dbl" "$hl")
    [ -n "$rkey" ] && [ -n "$lkey" ] || { fail "fed_inspect_sync.rkey" "no R/L key"; return; }

    # L subscribes to R; R funds L's proxy by key so R reports our credit.
    j "$dbl" "$hl" admin subscribe "$rkey" >/dev/null 2>&1 || { fail "fed_inspect_sync.subscribe" "subscribe failed"; return; }
    j "$dbr" "$hr" admin deposit "$lkey" 250 >/dev/null 2>&1

    local pc='import sys,json;ps=json.loads(sys.argv[1]).get("peers",[]);p=next((x for x in ps if x.get("handle")=="@kernel-r"),{});print(p.get("peer_credit") if p.get("peer_credit") is not None else "")'
    # Baseline: with the sync pass parked at 3600s and no inspect yet, L has NOT cached R's report.
    assert_eq "fed_inspect_sync.baseline_uncached" "" \
        "$(python3 -c "$pc" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)"

    # A single live inspect must persist last_seen + peer_credit (the on-demand refresh).
    local doc; doc=$(jj "$dbl" "$hl" admin inspect @kernel-r)
    assert_json "fed_inspect_sync.inspect_live"   "$doc" source live
    assert_json "fed_inspect_sync.inspect_online" "$doc" online True

    # The cache is now fresh — set by inspect alone, no timer pass involved.
    assert_eq "fed_inspect_sync.credit_after_inspect" 250 \
        "$(python3 -c "$pc" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)"
    local seen; seen=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]).get('peers',[]);p=next((x for x in ps if x.get('handle')=='@kernel-r'),{});print(p.get('last_seen') or '')" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)
    assert_nonempty "fed_inspect_sync.last_seen_after_inspect" "$seen"
}

# flow_fed_offline — every federation command has defined behavior when the peer is DOWN (§13):
# inspect degrades to local last-known data + offline reachability; subscribe fails clearly;
# unsubscribe/peers/identity are local and keep working; nothing hangs (bounded by fedOpTimeout).
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

    # A call to the down peer FAILS FAST (§13 never-dispatched): the request provably never left L,
    # so it settles immediately as a failure with a full refund and a distinct exit code — not parked
    # pending. (greet is price 0; the assertion is the fail-fast, not the amount.)
    local run_out rc
    run_out=$(j "$FED_DBL" "$FED_HL" run @kernel-r/sys/greet '{}' 2>&1); rc=$?
    assert_eq "fed_offline.run_exit_unreachable" 9 "$rc"
    assert_contains "fed_offline.run_says_unreachable" "unreachable" "$run_out"
    local tx_id; tx_id=$(python3 -c "import sys,json;print(next((t['id'] for t in json.loads(sys.argv[1]) if t.get('action_name')=='sys/greet'),''))" "$(jj "$FED_DBL" "$FED_HL" tx list)" 2>/dev/null)
    assert_nonempty "fed_offline.run_settled_not_pending" "$tx_id"
    assert_json "fed_offline.run_tx_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure

    # subscribe to a down peer: clear, prompt failure (no hang, mentions unreachable).
    assert_fails "fed_offline.subscribe_unreachable" "unreachable" -- j "$FED_DBL" "$FED_HL" admin subscribe "$FED_RKEY"

    # unsubscribe is local: works with the peer down.
    assert_contains "fed_offline.unsubscribe_local" "nsubscribed" "$(j "$FED_DBL" "$FED_HL" admin unsubscribe @kernel-r 2>&1)"

    # peers and identity are local: succeed with the peer down.
    assert_eq "fed_offline.peers_ok"    0 "$(j "$FED_DBL" "$FED_HL" admin peers    >/dev/null 2>&1; echo $?)"
    assert_eq "fed_offline.identity_ok" 0 "$(j "$FED_DBL" "$FED_HL" admin identity >/dev/null 2>&1; echo $?)"
}

# flow_fed_step_complete — the peer-step trap, closed (§10, §13). A step addressed to a peer used to
# be uncompletable: a key account holds no session token, and the wire carried `run` but not
# `complete`, so its parked funds were stranded with no actor able even to force-close the process
# (the process owner IS the keyless proxy user). /juice/fed/step/1 supplies the missing verb.
flow_fed_step_complete() {
    echo "=== FLOW fed_step_complete ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_step_complete.setup" "setup failed"; return; }

    # On R: @sys messages L's proxy user, parking a @sys/sink step whose required caller is @kernel-l.
    # R must know L as a peer for the address to resolve; a deposit both provisions and funds it.
    j "$FED_DBR" "$FED_HR" admin deposit "$FED_LKEY" 100 >/dev/null 2>&1
    local step_id
    step_id=$(resultf "$(jj "$FED_DBR" "$FED_HR" run @sys/message "{\"to\":\"$FED_LKEY\",\"message\":\"approve the shipment\"}")" step_id)
    assert_nonempty "fed_step_complete.step_parked" "$step_id"
    assert_json "fed_step_complete.step_waiting" "$(jj "$FED_DBR" "$FED_HR" step show "$step_id")" status waiting

    # R flags it as waiting on a peer — the operator can see the parked funds (§14).
    assert_contains "fed_step_complete.waiting_on_peer" "waiting_on_peer" "$(jj "$FED_DBR" "$FED_HR" step list)"

    # On L: the step is visible over the wire through the window that already exists — no second
    # command — carrying its derived completion schema.
    local listed; listed=$(jj "$FED_DBL" "$FED_HL" admin inspect "$FED_RKEY")
    assert_contains "fed_step_complete.peer_lists_step" "$step_id" "$listed"
    assert_contains "fed_step_complete.allowed_input" "allowed_input" "$listed"
    assert_contains "fed_step_complete.partial_args_visible" "approve the shipment" "$listed"
    # A peer is served the request, not the requester (§13): no local identity crosses.
    assert_not_contains "fed_step_complete.no_owner_handle" "owner_handle" "$listed"
    assert_not_contains "fed_step_complete.no_created_by" "created_by" "$listed"

    # L completes it with the SAME command that completes a local step — a step is a step.
    # The completion runs on R, funded by the price parked there at creation.
    local first_tx
    first_tx=$(strfield "$(jj "$FED_DBL" "$FED_HL" step complete "$step_id" --peer "$FED_RKEY" '{}')" tx_id)
    assert_nonempty "fed_step_complete.completed" "$first_tx"
    assert_json "fed_step_complete.step_done" "$(jj "$FED_DBR" "$FED_HR" step show "$step_id")" status done
    assert_not_contains "fed_step_complete.queue_drained" "$step_id" "$(jj "$FED_DBL" "$FED_HL" admin inspect "$FED_RKEY")"

    # Repeating the SAME completion derives the same idempotency key, so R returns its STORED
    # result instead of re-executing — this is how a completion that timed out on the wire but
    # succeeded remotely is recovered. A fresh key per attempt would lose that tx and receipt.
    local retry_tx
    retry_tx=$(strfield "$(jj "$FED_DBL" "$FED_HL" step complete "$step_id" --peer "$FED_RKEY" '{}')" tx_id)
    assert_eq "fed_step_complete.retry_replays_stored_result" "$first_tx" "$retry_tx"

    # A genuinely different request is not a replay: it reaches CompleteStep and is refused,
    # because the step is one-shot and no longer waiting.
    assert_fails "fed_step_complete.different_input_refused" "waiting\|invalid\|state" -- \
        j "$FED_DBL" "$FED_HL" step complete "$step_id" --peer "$FED_RKEY" '{"different":true}'

    # A suspended peer cannot complete: suspension is the one moderation axis for peers too (§13).
    local step2
    step2=$(resultf "$(jj "$FED_DBR" "$FED_HR" run @sys/message "{\"to\":\"$FED_LKEY\",\"message\":\"second\"}")" step_id)
    j "$FED_DBR" "$FED_HR" admin suspend "$FED_LKEY" >/dev/null 2>&1
    assert_fails "fed_step_complete.suspended_refused" "suspend\|unauth" -- \
        j "$FED_DBL" "$FED_HL" step complete "$step2" --peer "$FED_RKEY" '{}'
    j "$FED_DBR" "$FED_HR" admin unsuspend "$FED_LKEY" >/dev/null 2>&1
    assert_contains "fed_step_complete.unsuspend_restores" tx_id \
        "$(jj "$FED_DBL" "$FED_HL" step complete "$step2" --peer "$FED_RKEY" '{}')"
}
