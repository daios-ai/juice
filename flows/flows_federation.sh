# Federation flows (real multi-kernel over the libp2p transport, §13). Built on flows/lib.sh.
#
# _fed_setup boots two kernels on 127.0.0.1. R comes up first and acts as the bootstrap + relay
# for the network (every kernel now serves the DHT and relay — no separate seed process). L
# bootstraps to R's address, then the kernels are addressed only by Ed25519 public key: they
# resolve each other by key through R's DHT and L cold-resolves R's action by key — no URL anywhere.
# This is the loopback analogue of home kernels finding each other with no dialable address.

# Globals set by _fed_setup: FED_DBL FED_DBR FED_HL FED_HR FED_BPORT FED_RID FED_PROXY FED_RKEY FED_LKEY FED_BOOT.
_fed_setup() {
    local dir="$1"
    FED_DBL="$dir/l/kernel/juice.db"; FED_DBR="$dir/r/kernel/juice.db"
    FED_HL="$dir/lsys"; FED_HR="$dir/rsys"
    mkdir -p "$dir/l" "$dir/r" "$FED_HL/.juice" "$FED_HR/.juice"

    FED_BPORT=$(backend_port); start_backend "$FED_BPORT" 200 '{"greeting":"hello"}'
    # R boots first and is the flow's bootstrap+relay; L (and T, in the gossip flow) dial it. A short
    # discovery interval lets each kernel verify the others via routing discovery within the flow (§13):
    # R learns L only on R's next pass, which the multi-hop gossip flow depends on.
    # Per-kernel extra config, optionally set by the caller before calling (e.g.
    # FED_RCFG=(exposure_max=1000)). Consumed and cleared here so it never leaks into the next flow.
    start_server "$FED_DBR" "$FED_HR" kernel_handle=kernel-r discovery_interval_seconds=2 "${FED_RCFG[@]:-}" || return 1
    FED_BOOT=$(kernel_fed_addr "$FED_DBR")
    [ -n "$FED_BOOT" ] || return 1
    start_server "$FED_DBL" "$FED_HL" kernel_handle=kernel-l bootstrap_peers="$FED_BOOT" discovery_interval_seconds=2 "${FED_LCFG[@]:-}" || return 1
    unset FED_RCFG FED_LCFG
    j "$FED_DBR" "$FED_HR" auth login sys --password sys-pass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" auth login sys --password sys-pass >/dev/null 2>&1

    # Learn each kernel's own public key (federation identity; no .well-known anymore).
    FED_RKEY=$(kernel_key "$FED_DBR" "$FED_HR")
    FED_LKEY=$(kernel_key "$FED_DBL" "$FED_HL")
    [ -n "$FED_RKEY" ] && [ -n "$FED_LKEY" ] || return 1

    FED_RID=$(strfield "$(jj "$FED_DBR" "$FED_HR" action create greet --kind http --source "http://127.0.0.1:$FED_BPORT" --description "greet" --price 0)" id)
    [ -n "$FED_RID" ] || return 1
    j "$FED_DBR" "$FED_HR" action enable "$FED_RID" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" action update "$FED_RID" --visibility public >/dev/null 2>&1

    # L cold-resolves R's action by raw key — the sole cache-fill path (§8): a call resolves the signed
    # manifest on first use and caches a local proxy under an auto-alias. Retry until the freshly-booted
    # L has connected to R through the DHT/relay. Then bind the friendly kernel-r alias with admin rename
    # so the rest of the flows address sys@kernel-r/greet.
    local i
    for i in $(seq 1 15); do
        j "$FED_DBL" "$FED_HL" run "sys@$FED_RKEY/greet" '{}' >/dev/null 2>&1 && break
        sleep 1
    done
    j "$FED_DBL" "$FED_HL" admin rename -- "$FED_RKEY" kernel-r >/dev/null 2>&1 || return 1
    FED_PROXY=$(strfield "$(jj "$FED_DBL" "$FED_HL" action show "sys@kernel-r/greet")" id)
    [ -n "$FED_PROXY" ] || return 1
    return 0
}

flow_fed_rename() {
    echo "=== FLOW fed_rename ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_rename.setup" "setup failed"; return; }

    # The local mount handle is chosen with admin rename (by peer key). After renaming kernel-r to
    # myremote, R's action is addressable and callable under the new handle.
    j "$FED_DBL" "$FED_HL" admin rename -- "$FED_RKEY" myremote >/dev/null 2>&1 || { fail "fed_rename.rename" "rename failed"; return; }
    assert_json "fed_rename.proxy_remounted" "$(jj "$FED_DBL" "$FED_HL" action show sys@myremote/greet)" id "$FED_PROXY"
    assert_nonempty "fed_rename.callable_under_new" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@myremote/greet '{}')" tx_id)"
}

# _all_receipt_checks vr_json — "OK" iff valid=true and all 9 receipt checks are true.
_all_receipt_checks() {
    python3 -c "
import sys,json
vr=json.loads(sys.argv[1]); c=vr.get('checks',{}); bad=[]
if vr.get('valid') is not True: bad.append('valid')
for k in ['receipt_hash','signature','action_id','status','charge','premium','settlement_arith','charge_ceiling','refund_conservation','args_hash','reply_hash']:
    if c.get(k) is not True: bad.append(k)
print('OK' if not bad else 'FAIL:'+','.join(bad))" "$1" 2>/dev/null
}

flow_federation_import_execute() {
    echo "=== FLOW federation_import_execute ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_import.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)
    assert_nonempty "fed_import.call_succeeds" "$tx_id"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_nonempty "fed_import.remote_receipt_hash" "$(strfield "$tx" remote_receipt_hash)"
    assert_nonempty "fed_import.remote_receipt_json" "$(strfield "$tx" remote_receipt_json)"
    # Two uses: the setup's resolve-on-first-use call (§8) plus this one — local stats accumulate per call.
    assert_jnum "fed_import.local_stats_uses" "$(jj "$FED_DBL" "$FED_HL" action stats "$FED_PROXY")" uses 2
}

flow_federation_changed_reimport() {
    echo "=== FLOW federation_changed_reimport ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_reimport.setup" "setup failed"; return; }

    # A brand-new action on R resolves on first use (§8) — no operator step, no bulk sync.
    local wid; wid=$(publish "$FED_DBR" "$FED_HR" wave --kind http --source "http://127.0.0.1:$FED_BPORT" --description "wave" --price 0)
    assert_nonempty "fed_reimport.wave_callable" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/wave '{}')" tx_id)"
    assert_json "fed_reimport.wave_resolves_on_use" "$(jj "$FED_DBL" "$FED_HL" action show sys@kernel-r/wave)" active True
    assert_nonempty "fed_reimport.greet_before" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)"

    # R changes greet's contract (description is a contract field but not a deactivating one, §7): greet
    # stays active and free on R, but its contract hash changes, so L's cache is now stale.
    j "$FED_DBR" "$FED_HR" action update "$FED_RID" --description "greet, revised" >/dev/null 2>&1

    # L's cached proxy holds the old contract hash: the next call is refused pre-execution with a signed
    # refresh_proxy rejection and the proxy is deactivated (rules B/C, §13). The following call re-resolves
    # the new contract and succeeds with the row id preserved (rule A).
    assert_fails "fed_reimport.stale_call_refused" "" -- j "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}'
    assert_json "fed_reimport.proxy_deactivated" "$(jj "$FED_DBL" "$FED_HL" action show "$FED_PROXY")" active False
    assert_nonempty "fed_reimport.reresolved_call" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)"
    assert_json "fed_reimport.id_preserved" "$(jj "$FED_DBL" "$FED_HL" action show sys@kernel-r/greet)" id "$FED_PROXY"
}

flow_fed_verify_receipt() {
    echo "=== FLOW fed_verify_receipt ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_verify.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)
    [ -n "$tx_id" ] || { fail "fed_verify.call" "no tx_id"; return; }
    local vr; vr=$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")
    assert_json "fed_verify.buyer_valid"    "$vr" valid True
    assert_eq   "fed_verify.signature_check" True "$(pathf "$vr" checks.signature)"
    assert_eq   "fed_verify.receipt_hash_check" True "$(pathf "$vr" checks.receipt_hash)"

    # Verifying a non-remote-proxy (local) tx → ErrInvalidState.
    local ltx; ltx=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys/time '{}')" tx_id)
    assert_fails "fed_verify.local_tx_rejected" "" -- j "$FED_DBL" "$FED_HL" tx verify "$ltx"
}

flow_fed_all_receipt_checks() {
    echo "=== FLOW fed_all_receipt_checks ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_all_receipt.setup" "setup failed"; return; }

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)
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
    assert_nonempty "fed_suspend_blocks.first_call" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)"
    assert_contains "fed_suspend_blocks.suspend" "uspended" "$(j "$FED_DBR" "$FED_HR" admin suspend -- "$FED_LKEY" 2>&1)"
    j "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}' >/dev/null 2>&1 || true
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
    local pid; pid=$(publish "$FED_DBR" "$FED_HR" paid-svc --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price 100)
    j "$FED_DBL" "$FED_HL" admin deposit sys 1000 --ref "$(newref)" >/dev/null 2>&1

    # The CLI attributes it to THIS kernel's exhausted credit on the peer (operator remedy), with a
    # distinct exit code — never the caller's own insufficient_funds (§13 peer_unfunded).
    local run_out rc
    run_out=$(j "$FED_DBL" "$FED_HL" run sys@kernel-r/paid-svc '{}' 2>&1); rc=$?
    assert_eq "fed_denial_underfunded.run_exit_peer_unfunded" 10 "$rc"
    assert_contains "fed_denial_underfunded.run_says_exhausted" "exhausted" "$run_out"
    local tx_id; tx_id=$(find_id "$(jj "$FED_DBL" "$FED_HL" tx list)" action_name sys/paid-svc)
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
    j "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}' >/dev/null 2>&1 || true
    local tx_id; tx_id=$(find_id "$(jj "$FED_DBL" "$FED_HL" tx list)" action_name sys/greet)
    assert_nonempty "fed_disabled.tx_settled_not_pending" "$tx_id"
    assert_json "fed_disabled.tx_status_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure
    assert_eq "fed_disabled.all_9_checks" OK "$(_all_receipt_checks "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")")"
}

flow_fed_import_duty() {
    echo "=== FLOW fed_import_duty ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_import_duty.setup" "setup failed"; return; }

    # Two-step pricing (§13). mp=1000, remote_bps=500, import_bps=500:
    #   sr    = 1000 + ceil(1000*500/10000) = 1050  (serving markup, → R's sys)
    #   price = 1050 + ceil(1050*500/10000) = 1103  (import fee 53, retained by L's sys)
    local pid; pid=$(publish "$FED_DBR" "$FED_HR" duty-svc --kind http --source "http://127.0.0.1:$FED_BPORT" --description "duty" --price 1000)
    # A call cold-resolves the proxy (§8); it fails unfunded here but caches the row with its price.
    j "$FED_DBL" "$FED_HL" run sys@kernel-r/duty-svc '{}' >/dev/null 2>&1
    assert_jnum "fed_pricing.proxy_price" "$(jj "$FED_DBL" "$FED_HL" action show sys@kernel-r/duty-svc)" price 1103

    # R deposits to L's account by key (handshake-free: this both provisions and funds it, §13).
    j "$FED_DBR" "$FED_HR" admin deposit --ref "$(newref)" -- "$FED_LKEY" 5000 >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit sys 5000 --ref "$(newref)" >/dev/null 2>&1
    local ub pb; ub=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pb=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$FED_LKEY")" available)

    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/duty-svc '{}')" tx_id)
    assert_nonempty "fed_pricing.call_succeeded" "$tx_id"
    local ua pa; ua=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available); pa=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$FED_LKEY")" available)
    # L's sys is caller AND origin fee recipient: locks 1103, gets the 53 import fee back → net 1050 out.
    assert_eq "fed_pricing.user_charged" 1050 "$(( ub - ua ))"
    # L's account on R pays the cross-kernel obligation: charge 1000 + serving premium 50 = 1050.
    assert_eq "fed_pricing.peer_charged" 1050 "$(( pb - pa ))"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_jnum "fed_pricing.tx_gross" "$tx" gross 1103
    assert_jnum "fed_pricing.tx_net"   "$tx" net 1050
    assert_jnum "fed_pricing.tx_fee"   "$tx" fee 53
    assert_json "fed_pricing.tx_status" "$tx" status success

    # The operator raises the import fee to 20%. An imported action is a CATALOG entry, so its price
    # derives from the seller's price and the CURRENT fee (§16): sr stays 1050, the total becomes
    # 1050 + ceil(1050*2000/10000) = 1260 — with no re-resolve and no manifest change. Before this,
    # the total was frozen at import and only never-imported actions ever saw a fee change.
    stop_server "$FED_DBL"
    start_server "$FED_DBL" "$FED_HL" kernel_handle=kernel-l import_bps=2000 bootstrap_peers="$FED_BOOT" \
        || { fail "fed_pricing.restart_l" "L did not restart"; return; }
    j "$FED_DBL" "$FED_HL" auth login sys --password sys-pass >/dev/null 2>&1
    assert_jnum "fed_pricing.reprices_on_policy_change" "$(jj "$FED_DBL" "$FED_HL" action show sys@kernel-r/duty-svc)" price 1260

    # And the price shown is the price charged: gross on the next call is the new total, not the old.
    local ub2; ub2=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available)
    local tx2; tx2=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/duty-svc '{}')" tx_id)
    assert_jnum "fed_pricing.charges_the_new_price" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx2")" gross 1260
    # Import fee 210 returns to L's own sys, so the caller is out sr=1050 exactly, as before.
    assert_eq "fed_pricing.user_charged_after" 1050 "$(( ub2 - $(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available) ))"
}

flow_fed_failed_action_refund() {
    echo "=== FLOW fed_failed_action_refund ==="
    local dir fport; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_failed_refund.setup" "setup failed"; return; }
    fport=$(backend_port); start_backend "$fport" 500 '{"error":"boom"}'

    # Paid action on R backed by a 500 backend; two-step price = sr(105) + ceil(105*5%) = 111.
    local pid; pid=$(publish "$FED_DBR" "$FED_HR" fail-svc --kind http --source "http://127.0.0.1:$fport" --description "fails" --price 100)
    j "$FED_DBR" "$FED_HR" admin deposit --ref "$(newref)" -- "$FED_LKEY" 5000 >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit sys 1000 --ref "$(newref)" >/dev/null 2>&1
    local ub; ub=$(numfield "$(jj "$FED_DBL" "$FED_HL" user me)" available)

    # Remote 500 → remote failure receipt → full refund to L's caller.
    j "$FED_DBL" "$FED_HL" run sys@kernel-r/fail-svc '{}' >/dev/null 2>&1 || true
    assert_jnum "fed_failed_refund.balance_unchanged" "$(jj "$FED_DBL" "$FED_HL" user me)" available "$ub"
    local tx_id; tx_id=$(find_id "$(jj "$FED_DBL" "$FED_HL" tx list)" action_name sys/fail-svc)
    assert_nonempty "fed_failed_refund.tx_recorded" "$tx_id"
    local tx; tx=$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")
    assert_json "fed_failed_refund.tx_status_failure" "$tx" status failure
    assert_jnum "fed_failed_refund.tx_gross" "$tx" gross 111
    assert_jnum "fed_failed_refund.tx_net_zero" "$tx" net 0
    assert_jnum "fed_failed_refund.tx_fee_zero" "$tx" fee 0

    # The direction that matters: R's reason travels inside a SIGNED receipt that L stores forever.
    # L must adopt none of it — neither R's dialed upstream nor its response body — and must record
    # a class of its own instead. R's verbatim wording stays available in the stored receipt JSON.
    local rsn; rsn=$(strfield "$tx" reason)
    assert_nonempty     "fed_failed_refund.reason_present"           "$rsn"
    assert_not_contains "fed_failed_refund.reason_hides_peer_host"   "127.0.0.1:$fport" "$rsn"
    assert_not_contains "fed_failed_refund.reason_hides_peer_body"   "boom"             "$rsn"
    assert_json "fed_failed_refund.receipt_still_verifies" "$(jj "$FED_DBL" "$FED_HL" tx verify "$tx_id")" valid True
}

flow_fed_gossip_discovery() {
    echo "=== FLOW fed_gossip_discovery ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_gossip.setup" "L-R setup failed"; return; }

    # L calls R (price 0) and rates it, so L holds trade evidence about R to gossip (§13).
    local ltx; ltx=$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}')" tx_id)
    assert_nonempty "fed_gossip.initial_call" "$ltx"
    j "$FED_DBL" "$FED_HL" tx rate "$ltx" 1 >/dev/null 2>&1

    # R publishes greet publicly so its own gossip carries a signed manifest T can index (§13).
    local rkey; rkey=$(kernel_key "$FED_DBR" "$FED_HR")

    # Third kernel T joins the network via the seed and must discover R purely from gossip: its
    # discovery loop pulls gossip and indexes R's public action into T's lookup docs.
    local dbt ht; dbt="$dir/t/kernel/juice.db"; ht="$dir/tsys"; mkdir -p "$dir/t" "$ht/.juice"
    make_admin "$dbt" "$ht" kernel_handle=kernel-t bootstrap_peers="$FED_BOOT" discovery_interval_seconds=2 || { fail "fed_gossip.bootstrap_t" "T did not start"; return; }

    # Poll T's discovery cache until R's action surfaces in sys/lookup as a kernel-qualified reference.
    local found=no
    for _ in $(seq 1 25); do
        if jj "$dbt" "$ht" run sys/lookup '{"query":"greet"}' | grep -q -- "$rkey"; then found=yes; break; fi
        sleep 0.5
    done
    assert_eq "fed_gossip.r_discovered_via_gossip" yes "$found"

    # Routing discovery (§13): T bootstraps off the seed R alone and never dials L, yet must learn L
    # through the shared routing-discovery namespace (L advertises to R's DHT; T enumerates it), then
    # pull L's gossip directly — the verified pull is what lands L in T's merged roster. Proves
    # membership comes from the DHT namespace, not a gossip-carried hint.
    local lfound=no
    for _ in $(seq 1 45); do
        if jj "$dbt" "$ht" admin peers | grep -q -- "$FED_LKEY"; then lfound=yes; break; fi
        sleep 1
    done
    assert_eq "fed_gossip.l_discovered_via_routing" yes "$lfound"

    # T cold-resolves R's action by key with its FIRST call. No `admin rename` here on purpose:
    # first meaningful use must bind the petname itself (§13), seeded from R's nickname kernel-r.
    assert_nonempty "fed_gossip.t_resolves_and_calls" "$(strfield "$(jj "$dbt" "$ht" run "sys@$rkey/greet" '{}')" tx_id)"
    local tp; tp=$(strfield "$(jj "$dbt" "$ht" action show sys@kernel-r/greet)" id)
    assert_nonempty "fed_gossip.t_auto_bound_petname" "$tp"

    # The buying loop continues PAST the charge (§15). A bought action must stay findable, must
    # render a reference a command can consume (R8), and must re-run from that reference alone.
    local shown; shown=$(jj "$dbt" "$ht" action show "$tp")
    assert_json "fed_gossip.t_proxy_ref_qualified" "$shown" action "sys@kernel-r/greet"
    assert_json "fed_gossip.t_proxy_owner_handle" "$shown" owner_handle kernel-r
    # Searching again finds the proxy that now shadows the discovery row it replaced.
    assert_contains "fed_gossip.t_still_findable" "sys@kernel-r/greet" "$(jj "$dbt" "$ht" run sys/lookup '{"query":"greet"}')"
    # Exactly one use — T's OWN call — proving local stats are NOT inherited from R's gossiped manifest.
    assert_jnum "fed_gossip.t_stats_own_only" "$(jj "$dbt" "$ht" action stats "$tp")" uses 1
    # Re-running by the rendered reference needs no second resolve (asserted after the stats check,
    # which counts T's own calls).
    assert_nonempty "fed_gossip.t_reruns_by_ref" "$(strfield "$(jj "$dbt" "$ht" run sys@kernel-r/greet '{}')" tx_id)"

    # §13 leg-(b) evidence propagation + corroboration: T's call to R above was UNRATED, yet T now gossips
    # receipt-backed execution evidence about R (subject=R, issuer=T) — every admitted execution, not only
    # rated calls. L, a third kernel, discovers T through routing discovery and pulls BOTH T's leg-(b)
    # evidence and R's own leg-(a) execution evidence (which names T as counterparty). `admin inspect` on L
    # then shows T's interaction under T as issuer (counterparty-experience view, never folded into R's own
    # execution summary) AND marks it CORROBORATED — the two-kernel receipt link holds, so the claim is
    # verified, not taken on faith. Proves an unrated call becomes visible, attributed, and trade-backed.
    local tkey; tkey=$(kernel_key "$dbt" "$ht")
    local tevi=no
    for _ in $(seq 1 40); do
        if python3 -c "import sys,json;e=json.loads(sys.argv[1]).get('evidence',[]);r=next((x for x in e if x.get('issuer_public_key')==sys.argv[2]),None);sys.exit(0 if r and r.get('corroborated_uses',0)>=1 else 1)" "$(jj "$FED_DBL" "$FED_HL" admin inspect -- "$rkey")" "$tkey" 2>/dev/null; then tevi=yes; break; fi
        sleep 1
    done
    assert_eq "fed_gossip.unrated_call_propagates_as_verified_evidence" yes "$tevi"
}

# flow_fed_discovery: cold-start discovery. L boots with R as its only bootstrap peer and must learn
# R into its known network automatically via the startup discovery pass — routing discovery finds R
# and a gossip pull indexes its catalog (§13). Proves discovery is independent of any peering step: the
# known network (global, by key) grows before any call resolves a proxy.
flow_fed_discovery() {
    echo "=== FLOW fed_discovery ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/kernel/juice.db" hr="$dir/rsys" dbl="$dir/l/kernel/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    local bport; bport=$(backend_port); start_backend "$bport" 200 '{"greeting":"hi"}'
    start_server "$dbr" "$hr" kernel_handle=kernel-r discovery_interval_seconds=2 \
        || { fail "fed_discovery.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_discovery.boot" "no R fed addr"; return; }
    j "$dbr" "$hr" auth login sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    [ -n "$rkey" ] || { fail "fed_discovery.rkey" "no R key"; return; }

    # R publishes a public action so its gossip carries something to display. It is PRICED, so the
    # discovery card's indicative price is checkable (§9): mp=1000, remote_bps=500, import_bps=500
    # ⇒ sr = 1050 (stored on the doc), displayed = 1050 + ceil(1050*500/10000) = 1103.
    local rid; rid=$(publish "$dbr" "$hr" greet --kind http --source "http://127.0.0.1:$bport" --description greet --price 1000)

    # L joins with R as its ONLY bootstrap peer; it must discover R without subscribing to it.
    start_server "$dbl" "$hl" kernel_handle=kernel-l bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "fed_discovery.l" "L did not start"; return; }
    j "$dbl" "$hl" auth login sys --password sys-pass >/dev/null 2>&1

    # Poll L's discovery cache (§13): a discovery pass runs at startup then every 2s, pulling R's
    # gossip and indexing R's public action into L's lookup docs. sys/lookup then surfaces it as a
    # kernel-qualified reference (sys@<rkey>/greet) — discovered WITHOUT subscribing.
    local found=no i
    for i in $(seq 1 20); do
        if jj "$dbl" "$hl" run sys/lookup '{"query":"greet"}' | grep -q -- "$rkey"; then found=yes; break; fi
        sleep 1
    done
    assert_eq "fed_discovery.r_discovered_without_subscribe" yes "$found"
    # The discovered reference is kernel-qualified by raw key (a gossiped label never resolves).
    local lk; lk=$(jj "$dbl" "$hl" run sys/lookup '{"query":"greet"}')
    assert_contains "fed_discovery.qualified_action" "@$rkey/greet" "$lk"
    # The card carries an all-in price without ever resolving: L holds no proxy row for R (asserted
    # discovery-only below), so 1103 can only have come from the gossiped serving price (§13).
    local dprice; dprice=$(python3 -c "
import sys,json
r=json.loads(sys.argv[1]); res=r.get('result',r).get('results',[])
print(next((x.get('price') for x in res if sys.argv[2] in str(x.get('action',''))),'missing'))" "$lk" "@$rkey/greet" 2>/dev/null)
    assert_eq "fed_discovery.indicative_price" 1103 "$dprice"
    # L discovered R but never resolved or called it: R appears in the MERGED roster (§14) as a
    # discovery-only kernel — present, but with NO account (has_account=false). Discovery creates no
    # billing account.
    local rentry; rentry=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]);e=next((x for x in ps if x.get('public_key')==sys.argv[2]),None);print('missing' if e is None else ('account' if e.get('has_account') else 'discovery-only'))" "$(jj "$dbl" "$hl" admin peers)" "$rkey" 2>/dev/null)
    assert_eq "fed_discovery.r_is_discovery_only" discovery-only "$rentry"

    # The discovery/proxy quote equality (§4 precondition 7): the hash on a catalog card, computed
    # from the gossiped manifest, must equal the one the local proxy carries after resolve — keyed
    # on the REMOTE action id, so the local cache UUID never enters it. That equality is what lets a
    # buyer pin terms read from lookup on a FIRST cross-kernel call, before any proxy row exists.
    local dhash; dhash=$(python3 -c "
import sys,json
r=json.loads(sys.argv[1]); res=r.get('result',r).get('results',[])
print(next((x.get('quote_hash','') for x in res if sys.argv[2] in str(x.get('action',''))),''))" "$lk" "@$rkey/greet" 2>/dev/null)
    assert_nonempty "fed_discovery.card_quote_hash" "$dhash"

    j "$dbr" "$hr" admin deposit --ref "$(newref)" -- "$(kernel_key "$dbl" "$hl")" 5000 >/dev/null 2>&1
    j "$dbl" "$hl" admin deposit sys 5000 --ref "$(newref)" >/dev/null 2>&1
    assert_nonempty "fed_discovery.pinned_first_call" \
        "$(strfield "$(jj "$dbl" "$hl" run "sys@$rkey/greet" '{}' --quote-hash "$dhash")" tx_id)"
    assert_eq "fed_discovery.proxy_hash_equals_card" "$dhash" \
        "$(strfield "$(jj "$dbl" "$hl" action show "sys@$rkey/greet")" quote_hash)"
}

# flow_fed_peer_sync: the discovery timer also pulls gossip from known peers (§13 peer sync),
# caching each peer's liveness (last_seen) and OUR credit on it (peer_credit, from the peer's reported
# counterparty_balance). Proves the "better sync" surfacing: after R deposits L's proxy, L's own
# `admin peers` learns that credit without L ever calling R.
flow_fed_peer_sync() {
    echo "=== FLOW fed_peer_sync ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/kernel/juice.db" hr="$dir/rsys" dbl="$dir/l/kernel/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    start_server "$dbr" "$hr" kernel_handle=kernel-r discovery_interval_seconds=2 \
        || { fail "fed_peer_sync.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_peer_sync.boot" "no R fed addr"; return; }
    start_server "$dbl" "$hl" kernel_handle=kernel-l bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "fed_peer_sync.l" "L did not start"; return; }
    j "$dbr" "$hr" auth login sys --password sys-pass >/dev/null 2>&1
    j "$dbl" "$hl" auth login sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    local lkey; lkey=$(kernel_key "$dbl" "$hl")
    [ -n "$rkey" ] && [ -n "$lkey" ] || { fail "fed_peer_sync.rkey" "no R/L key"; return; }

    # R exposes a public action; L cold-resolves it by key to provision R's proxy locally (manifest
    # only, no backend call) and binds the kernel-r alias. R then funds L's proxy on R by key.
    local rid; rid=$(publish "$dbr" "$hr" greet --kind http --source "http://127.0.0.1:1/x" --description greet --price 0)
    j "$dbl" "$hl" run "sys@$rkey/greet" '{}' >/dev/null 2>&1  # resolve caches the proxy even if greet's dead backend fails execution
    j "$dbl" "$hl" admin rename -- "$rkey" kernel-r >/dev/null 2>&1 || { fail "fed_peer_sync.resolve" "resolve/rename failed"; return; }
    j "$dbr" "$hr" admin deposit --ref "$(newref)" -- "$lkey" 250 >/dev/null 2>&1

    # A peer-sync pass runs at startup, then every 2s. Poll L's own peer list until it has cached
    # the credit R reports for us — no call to R involved.
    local credit=""
    local i
    for i in $(seq 1 20); do
        credit=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]);p=next((x for x in ps if x.get('petname')=='kernel-r'),{});print(p.get('peer_credit') if p.get('peer_credit') is not None else '')" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)
        [ "$credit" = "250" ] && break
        sleep 1
    done
    assert_eq "fed_peer_sync.credit_cached" 250 "$credit"
    local seen; seen=$(python3 -c "import sys,json;ps=json.loads(sys.argv[1]);p=next((x for x in ps if x.get('petname')=='kernel-r'),{});print(p.get('last_seen') or '')" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)
    assert_nonempty "fed_peer_sync.last_seen_cached" "$seen"
}

# flow_fed_inspect_read_only — `admin inspect` writes nothing (§14). Its reply is live — it does do
# the gossip pull — but it must not persist what it saw: the discovery loop owns the peer-sync cache,
# so retention and display state never depend on an operator having looked. Proven with the timer
# parked at 3600s, so the only thing that could refresh the cache is the inspect itself.
flow_fed_inspect_read_only() {
    echo "=== FLOW fed_inspect_read_only ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/kernel/juice.db" hr="$dir/rsys" dbl="$dir/l/kernel/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r" "$dir/l" "$hr/.juice" "$hl/.juice"

    start_server "$dbr" "$hr" kernel_handle=kernel-r discovery_interval_seconds=3600 \
        || { fail "fed_inspect_read_only.setup" "R did not start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    [ -n "$boot" ] || { fail "fed_inspect_read_only.boot" "no R fed addr"; return; }
    start_server "$dbl" "$hl" kernel_handle=kernel-l bootstrap_peers="$boot" discovery_interval_seconds=3600 \
        || { fail "fed_inspect_read_only.l" "L did not start"; return; }
    j "$dbr" "$hr" auth login sys --password sys-pass >/dev/null 2>&1
    j "$dbl" "$hl" auth login sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    local lkey; lkey=$(kernel_key "$dbl" "$hl")
    [ -n "$rkey" ] && [ -n "$lkey" ] || { fail "fed_inspect_read_only.rkey" "no R/L key"; return; }

    # R exposes a public action; L cold-resolves it by key to provision R's proxy locally and bind the
    # kernel-r alias. R funds L's proxy by key so R reports our credit.
    local rid; rid=$(publish "$dbr" "$hr" greet --kind http --source "http://127.0.0.1:1/x" --description greet --price 0)
    j "$dbl" "$hl" run "sys@$rkey/greet" '{}' >/dev/null 2>&1  # resolve caches the proxy even if greet's dead backend fails execution
    j "$dbl" "$hl" admin rename -- "$rkey" kernel-r >/dev/null 2>&1 || { fail "fed_inspect_read_only.resolve" "resolve/rename failed"; return; }
    j "$dbr" "$hr" admin deposit --ref "$(newref)" -- "$lkey" 250 >/dev/null 2>&1

    local pc='import sys,json;ps=json.loads(sys.argv[1]);p=next((x for x in ps if x.get("petname")=="kernel-r"),{});print(p.get("peer_credit") if p.get("peer_credit") is not None else "")'
    # Baseline: with the sync pass parked at 3600s and no inspect yet, L has NOT cached R's report.
    assert_eq "fed_inspect_read_only.baseline_uncached" "" \
        "$(python3 -c "$pc" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)"

    # The cold resolve above was a real outbound contact, so it dated the peer. Inspect is not:
    # whatever that left, inspect must leave exactly as it found it.
    local ps='import sys,json;ps=json.loads(sys.argv[1]);p=next((x for x in ps if x.get("petname")=="kernel-r"),{});print(p.get("last_seen") or "")'
    local seen_before; seen_before=$(python3 -c "$ps" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)

    # The inspect itself is live: it reaches R and reports what R says right now.
    local doc; doc=$(jj "$dbl" "$hl" admin inspect kernel-r)
    assert_json "fed_inspect_read_only.inspect_live"   "$doc" source live
    assert_json "fed_inspect_read_only.inspect_online" "$doc" online True

    # ...and it left nothing behind: the cache is exactly as it was.
    assert_eq "fed_inspect_read_only.credit_still_uncached" "" \
        "$(python3 -c "$pc" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)"
    assert_eq "fed_inspect_read_only.last_seen_unchanged" "$seen_before" \
        "$(python3 -c "$ps" "$(jj "$dbl" "$hl" admin peers)" 2>/dev/null)"
}

# flow_fed_offline — every federation command has defined behavior when the peer is DOWN (§13):
# inspect degrades to local last-known data + offline reachability; a cold call fails fast;
# peers/identity are local and keep working; nothing hangs (bounded by fedOpTimeout).
flow_fed_offline() {
    echo "=== FLOW fed_offline ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_offline.setup" "setup failed"; return; }

    # Take R offline.
    stop_server "$FED_DBR"

    # inspect: still works, degrades to last-known local data and marks the peer offline. (The specific
    # action list comes from the discovery cache, populated by the background discovery loop, not by a
    # cold resolve — so this offline flow asserts the degrade, not the catalog contents.)
    local doc; doc=$(jj "$FED_DBL" "$FED_HL" admin inspect kernel-r)
    assert_json "fed_offline.inspect_source_local" "$doc" source local
    assert_json "fed_offline.inspect_offline" "$doc" online False

    # A call to the down peer FAILS FAST (§13 never-dispatched): the request provably never left L,
    # so it settles immediately as a failure with a full refund and a distinct exit code — not parked
    # pending. (greet is price 0; the assertion is the fail-fast, not the amount.)
    local run_out rc
    run_out=$(j "$FED_DBL" "$FED_HL" run sys@kernel-r/greet '{}' 2>&1); rc=$?
    assert_eq "fed_offline.run_exit_unreachable" 9 "$rc"
    assert_contains "fed_offline.run_says_unreachable" "unreachable" "$run_out"
    local tx_id; tx_id=$(find_id "$(jj "$FED_DBL" "$FED_HL" tx list)" action_name sys/greet)
    assert_nonempty "fed_offline.run_settled_not_pending" "$tx_id"
    assert_json "fed_offline.run_tx_failure" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status failure

    # peers and identity are local: succeed with the peer down.
    assert_eq "fed_offline.peers_ok"    0 "$(j "$FED_DBL" "$FED_HL" admin peers    >/dev/null 2>&1; echo $?)"
    assert_eq "fed_offline.identity_ok" 0 "$(j "$FED_DBL" "$FED_HL" admin identity >/dev/null 2>&1; echo $?)"

    # The failed call TAUGHT this kernel something: the peer's row now carries when contact last
    # failed, so the catalog can say "unreachable since" instead of presenting R's actions as fresh.
    # Retention (90 idle days) is a different question and is untouched — R stays listed either way.
    local peer_row; peer_row=$(jj "$FED_DBL" "$FED_HL" admin peers | python3 -c \
        'import sys,json;print(json.dumps(next((p for p in json.load(sys.stdin) if p.get("petname")=="kernel-r"), {})))')
    assert_nonempty "fed_offline.peer_still_listed" "$(strfield "$peer_row" public_key)"
    assert_nonempty "fed_offline.contact_failure_recorded" "$(strfield "$peer_row" last_contact_failed_at)"
}

# flow_fed_step_complete — the peer-step trap, closed (§10, §13). A step addressed to a peer used to
# be uncompletable: a key account holds no session token, and the wire carried `run` but not
# `complete`, so its parked funds were stranded with no actor able even to force-close the process
# (the process owner IS the keyless proxy user). /juice/fed/step/1 supplies the missing verb.
flow_fed_step_complete() {
    echo "=== FLOW fed_step_complete ==="
    local dir; dir=$(new_dir)
    _fed_setup "$dir" || { fail "fed_step_complete.setup" "setup failed"; return; }

    # On R: sys messages L's proxy user, parking a sys/sink step whose required caller is kernel-l.
    # R must know L as a peer for the address to resolve; a deposit both provisions and funds it.
    j "$FED_DBR" "$FED_HR" admin deposit --ref "$(newref)" -- "$FED_LKEY" 100 >/dev/null 2>&1
    local step_id
    step_id=$(resultf "$(jj "$FED_DBR" "$FED_HR" run sys/message "{\"to\":\"$FED_LKEY\",\"message\":\"approve the shipment\"}")" step_id)
    assert_nonempty "fed_step_complete.step_parked" "$step_id"
    assert_json "fed_step_complete.step_waiting" "$(jj "$FED_DBR" "$FED_HR" step show "$step_id")" status waiting

    # R flags it as waiting on a peer — the operator can see the parked funds (§14).
    assert_contains "fed_step_complete.waiting_on_peer" "waiting_on_peer" "$(jj "$FED_DBR" "$FED_HR" step list)"

    # On L: the step is visible over the wire through the window that already exists — no second
    # command — carrying its derived completion schema.
    local listed; listed=$(jj "$FED_DBL" "$FED_HL" admin inspect -- "$FED_RKEY")
    assert_contains "fed_step_complete.peer_lists_step" "$step_id" "$listed"
    assert_contains "fed_step_complete.allowed_input" "allowed_input" "$listed"
    assert_contains "fed_step_complete.partial_args_visible" "approve the shipment" "$listed"
    # A peer is served the request, not the requester (§13): no local identity crosses.
    assert_not_contains "fed_step_complete.no_owner_handle" "owner_handle" "$listed"
    assert_not_contains "fed_step_complete.no_created_by" "created_by" "$listed"

    # L completes it with the SAME command that completes a local step — a step is a step.
    # The completion runs on R, funded by the price parked there at creation.
    local first_tx
    first_tx=$(strfield "$(jj "$FED_DBL" "$FED_HL" step complete "$step_id" --peer="$FED_RKEY" '{}')" tx_id)
    assert_nonempty "fed_step_complete.completed" "$first_tx"
    assert_json "fed_step_complete.step_done" "$(jj "$FED_DBR" "$FED_HR" step show "$step_id")" status done
    assert_not_contains "fed_step_complete.queue_drained" "$step_id" "$(jj "$FED_DBL" "$FED_HL" admin inspect -- "$FED_RKEY")"

    # Repeating the SAME completion derives the same idempotency key, so R returns its STORED
    # result instead of re-executing — this is how a completion that timed out on the wire but
    # succeeded remotely is recovered. A fresh key per attempt would lose that tx and receipt.
    local retry_tx
    retry_tx=$(strfield "$(jj "$FED_DBL" "$FED_HL" step complete "$step_id" --peer="$FED_RKEY" '{}')" tx_id)
    assert_eq "fed_step_complete.retry_replays_stored_result" "$first_tx" "$retry_tx"

    # A genuinely different request is not a replay: it reaches CompleteStep and is refused,
    # because the step is one-shot and no longer waiting.
    assert_fails "fed_step_complete.different_input_refused" "waiting\|invalid\|state" -- \
        j "$FED_DBL" "$FED_HL" step complete "$step_id" --peer="$FED_RKEY" '{"different":true}'

    # A suspended peer cannot complete: suspension is the one moderation axis for peers too (§13).
    local step2
    step2=$(resultf "$(jj "$FED_DBR" "$FED_HR" run sys/message "{\"to\":\"$FED_LKEY\",\"message\":\"second\"}")" step_id)
    j "$FED_DBR" "$FED_HR" admin suspend -- "$FED_LKEY" >/dev/null 2>&1
    assert_fails "fed_step_complete.suspended_refused" "suspend\|unauth" -- \
        j "$FED_DBL" "$FED_HL" step complete "$step2" --peer="$FED_RKEY" '{}'
    j "$FED_DBR" "$FED_HR" admin unsuspend -- "$FED_LKEY" >/dev/null 2>&1
    assert_contains "fed_step_complete.unsuspend_restores" tx_id \
        "$(jj "$FED_DBL" "$FED_HL" step complete "$step2" --peer="$FED_RKEY" '{}')"
}

# flow_settlement exercises the §13 residual settlement protocol end-to-end over the real transport:
# L draws R's global exposure (unfunded paid call), so L owes R a sub-quantum debt, then `admin settle`
# runs the two-party probabilistic commit/reveal and both bilateral rows clear to zero.
flow_settlement() {
    echo "=== FLOW settlement ==="
    local dir; dir=$(new_dir)
    # R (creditor) extends global exposure and sets the fee-rational quantum; L (debtor) shares Q so it
    # takes the probabilistic branch for a sub-quantum debt. Q sits just above the debt, so the draw
    # usually comes out payable — the branch with more to go wrong — while still reaching clear. _fed_setup does the rest of the bring-up,
    # including the cold resolve of the price-0 greet that provisions R's proxy and binds kernel-r
    # without drawing credit; the paid action below then resolves on its own first call.
    FED_RCFG=(exposure_max=1000 settlement_trigger=500 settlement_quantum=12)
    FED_LCFG=(settlement_quantum=12)
    _fed_setup "$dir" || { fail "settlement.setup" "setup failed"; return; }
    local lkey="$FED_LKEY"

    # R: paid action (mp=10). L funds only its OWN caller, never prepaying on R.
    local rid; rid=$(publish "$FED_DBR" "$FED_HR" paid --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price 10)
    j "$FED_DBL" "$FED_HL" admin deposit sys 1000 --ref "$(newref)" >/dev/null 2>&1

    # L calls the paid action unfunded on R → R admits it against its global exposure → L now owes R.
    assert_nonempty "settlement.call_on_credit" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/paid '{}')" tx_id)"
    local d; d=$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)
    assert_eq "settlement.debtor_owes_r" 11 "$d"   # charge 10 + serving premium 1
    assert_eq "settlement.creditor_owed_by_l" -11 "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" available)"

    # R flags settlement_due once gross receivables reach Y (display only, FIX 3) — 11 < 500 here, so not yet.
    assert_json "settlement.identity_has_quantum" "$(jj "$FED_DBR" "$FED_HR" admin identity)" settlement_quantum 12

    # Settle: L is the debtor; d=11 < Q=100 → the probabilistic commit/reveal runs over the transport.
    local out; out=$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)
    assert_json "settlement.settled" "$out" mode probabilistic
    local outcome sid
    outcome=$(strfield "$out" outcome); sid=$(strfield "$out" settlement_id)
    assert_eq "settlement.outcome_valid" ok "$(case "$outcome" in pay|clear) echo ok;; *) echo "bad:$outcome";; esac)"

    if [ "$outcome" = "clear" ]; then
        # A cleared draw extinguishes the debt immediately on both kernels — no money moves.
        assert_json "settlement.clear_status" "$out" status settled
        assert_eq "settlement.clear_debtor_row"   0 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)"
        assert_eq "settlement.clear_creditor_row" 0 "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" available)"
    else
        # A payable draw pays the quantum and announces it, so the debtor's books close. What the
        # creditor then does with the announcement is the same machinery flow_rail_settlement drives
        # deterministically; here the point is only that the draw resolved into a real payment.
        assert_json "settlement.pay_announced" "$out" status announced
        assert_eq "settlement.pay_debtor_row" 0 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)"
        assert_contains "settlement.pay_claim_reached_creditor" "$sid" "$(j "$FED_DBR" "$FED_HR" admin deposit)"
        return
    fi

    # Nothing left to settle: a second run reports the zero position, not a new flip.
    assert_json "settlement.idempotent" "$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)" status settled
}

# flow_transfer exercises the value channel across a federated pair (§13). Value is LOCAL to a kernel:
# alice pays bob on their own kernel, the amount leaving her balance untaxed and arriving in full,
# while the peer R is present to prove the channel stops at the boundary — its stdlib is never served
# abroad, so no kernel-qualified transfer resolves and no local funds move on the attempt.
flow_transfer() {
    echo "=== FLOW transfer ==="
    local dir; dir=$(new_dir)
    FED_DBL="$dir/l/kernel/juice.db"; FED_DBR="$dir/r/kernel/juice.db"
    FED_HL="$dir/lsys"; FED_HR="$dir/rsys"
    mkdir -p "$dir/l" "$dir/r" "$FED_HL/.juice" "$FED_HR/.juice"

    start_server "$FED_DBR" "$FED_HR" kernel_handle=kernel-r exposure_max=1000 settlement_trigger=500 || { fail "transfer.setup_r" "boot"; return; }
    local boot; boot=$(kernel_fed_addr "$FED_DBR"); [ -n "$boot" ] || { fail "transfer.boot" "no addr"; return; }
    start_server "$FED_DBL" "$FED_HL" kernel_handle=kernel-l bootstrap_peers="$boot" || { fail "transfer.setup_l" "boot"; return; }
    j "$FED_DBR" "$FED_HR" auth login sys --password sys-pass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" auth login sys --password sys-pass >/dev/null 2>&1
    local rkey; rkey=$(kernel_key "$FED_DBR" "$FED_HR"); [ -n "$rkey" ] || { fail "transfer.rkey" "empty"; return; }

    # alice and bob are both local users on L; R exists to prove its stdlib is not served abroad.
    j "$FED_DBL" "$FED_HL" user create bob --password userpass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" user create alice --password userpass >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin deposit alice 1000 --ref "$(newref)" >/dev/null 2>&1
    local ahome; ahome=$(home "$dir" alice); j "$FED_DBL" "$ahome" auth login alice --password userpass >/dev/null 2>&1

    # alice sends 100 to bob on her own kernel: the execution price (0) rides the trace and is taxed,
    # while the value moves untaxed from alice's own balance to the beneficiary — two channels (§13).
    local tx_id; tx_id=$(strfield "$(jj "$FED_DBL" "$ahome" run sys/transfer '{"target":"bob","amount":100}')" tx_id)
    assert_nonempty "transfer.call_succeeded" "$tx_id"
    assert_json "transfer.tx_success" "$(jj "$FED_DBL" "$FED_HL" tx show "$tx_id")" status success

    # The beneficiary receives exactly the amount and the sender pays exactly it (price 0, no markup).
    assert_eq "transfer.alice_charged" 900 "$(numfield "$(jj "$FED_DBL" "$ahome" user me)" available)"
    assert_eq "transfer.bob_credited" 100 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show bob)" available)"

    # Both parties can read the movement. The transaction records the execution (price 0), so without
    # a ledger entry bob — no party to it — would see 100 credits arrive with nothing to read.
    local bhome; bhome=$(home "$dir" bob); j "$FED_DBL" "$bhome" auth login bob --password userpass >/dev/null 2>&1
    assert_contains "transfer.sender_ledger" "bob" "$(jj "$FED_DBL" "$ahome" user ledger)"
    assert_contains "transfer.recipient_ledger" "alice" "$(jj "$FED_DBL" "$bhome" user ledger)"
    assert_eq "transfer.ledger_amount" 100 \
        "$(jj "$FED_DBL" "$bhome" user ledger | python3 -c 'import sys,json;e=json.load(sys.stdin);print(e[0]["amount"])')"
    # The entry points back at the call that delivered it, so the two records join.
    assert_eq "transfer.ledger_names_tx" "$tx_id" \
        "$(jj "$FED_DBL" "$bhome" user ledger | python3 -c 'import sys,json;e=json.load(sys.stdin);print(e[0]["reason"])')"

    # A transfer alice cannot afford is rejected with no balance change.
    j "$FED_DBL" "$ahome" run sys/transfer '{"target":"bob","amount":100000}' >/dev/null 2>&1 || true
    assert_eq "transfer.underfunded_no_charge" 900 "$(numfield "$(jj "$FED_DBL" "$ahome" user me)" available)"

    # Value does not cross a kernel boundary (§13). The stdlib is local (§9), so R serves no manifest
    # for sys/transfer: the kernel-qualified form does not resolve, and the refusal costs nothing.
    assert_fails "transfer.remote_native_not_served" "not found\|not available\|error" -- \
        j "$FED_DBL" "$ahome" run "sys@$rkey/transfer" '{"target":"bob","amount":10}'
    assert_eq "transfer.remote_refusal_no_charge" 900 "$(numfield "$(jj "$FED_DBL" "$ahome" user me)" available)"

    # Nor by naming a beneficiary on another kernel: the target is a bare local handle, always.
    assert_fails "transfer.qualified_target_rejected" "invalid\|not found\|error" -- \
        j "$FED_DBL" "$ahome" run sys/transfer "{\"target\":\"bob@$rkey\",\"amount\":10}"
    assert_eq "transfer.qualified_target_no_charge" 900 "$(numfield "$(jj "$FED_DBL" "$ahome" user me)" available)"
}

# The provider dies in the middle of serving a call, then comes back. This is the one crash that
# costs a buyer money, and the only place a real process death can be tested: the in-process
# simulator (cmd/juice/fedsim_test.go) recovers state over a live store, while this kills a real
# `juice serve`, closes its SQLite, and starts a new process over the same home.
#
# §5 G4: "work possibly executing remotely is never presumed dead — it is re-driven under its
# original identity until signed evidence settles it." The buyer's funds must therefore neither be
# refunded while the outcome is unknown, nor stay locked once the provider is back and holds a
# receipt.
flow_fed_provider_crash_recovery() {
    echo "=== FLOW fed_provider_crash_recovery ==="
    local dir; dir=$(new_dir)
    # The provider must extend credit, or the call is refused before it can be interrupted. The
    # buyer retries on a one-second interval: the default is 60s with exponential backoff, which
    # would make the result depend on how long this flow happened to wait rather than on whether
    # the call can settle at all.
    FED_RCFG=(exposure_max=1000 settlement_trigger=500)
    FED_LCFG=(remote_retry_interval_seconds=1)
    _fed_setup "$dir" || { fail "fed_crash.setup" "setup failed"; return; }

    # A buyer on L with money, and a priced action on R that takes long enough to be interrupted.
    local ha; ha=$(home "$dir" buyer)
    make_user "$FED_DBL" "$FED_HL" "$ha" buyer
    deposit "$FED_DBL" "$FED_HL" buyer 1000

    local sport; sport=$(backend_port)
    start_slow_backend "$sport" 8
    publish "$FED_DBR" "$FED_HR" slow --kind http --source "http://127.0.0.1:${sport}/slow" \
        --description "a service slow enough to interrupt" --price 20 >/dev/null 2>&1

    # Warm the proxy so the crash lands on the call rather than on the resolve.
    j "$FED_DBL" "$ha" run sys@kernel-r/slow '{}' >/dev/null 2>&1
    local before; before=$(numfield "$(jj "$FED_DBL" "$ha" user me)" available)

    # Call again and kill the provider while it is still upstream.
    ( j "$FED_DBL" "$ha" run sys@kernel-r/slow '{}' >"$dir/crash_call.out" 2>&1 ) &
    local caller_pid=$!
    sleep 3
    stop_server "$FED_DBR"
    wait "$caller_pid" 2>/dev/null

    # The outcome is unknown, so the allocation stays reserved: not refunded, not spent.
    local proc_line
    proc_line=$(j "$FED_DBL" "$ha" process list --limit 5 | grep -c "awaiting-receipt" || true)
    assert_eq "fed_crash.call_is_parked" yes "$([ "$proc_line" -ge 1 ] && echo yes || echo no)"

    # The provider returns and recovers its own interrupted work.
    start_server "$FED_DBR" "$FED_HR" kernel_handle=kernel-r discovery_interval_seconds=2 \
        remote_retry_interval_seconds=1 "${FED_RCFG[@]}" \
        || { fail "fed_crash.restart" "provider did not restart"; return; }
    await_login "$FED_DBR" "$FED_HR" || { fail "fed_crash.provider_up" "not serving after restart"; return; }

    # The buyer re-drives the parked call. It must end: settled from the provider's signed evidence,
    # with the allocation released either way.
    local i settled=no
    for i in $(seq 1 20); do
        if [ "$(j "$FED_DBL" "$ha" process list --limit 5 | grep -c "awaiting-receipt" || true)" -eq 0 ]; then
            settled=yes; break
        fi
        sleep 1
    done
    known_defect "stranded funds after a provider crash" assert_eq "fed_crash.settles_after_provider_returns" yes "$settled"

    # The outcome must be exact, not merely bounded. mp=20, the provider's markup and the buyer's
    # import fee are both 5% by default, so the all-in price is 20 → 21 → 23. A call that ran is
    # charged that; a call that did not is refunded whole. Nothing in between, nothing left locked,
    # and exactly one transaction for the attempt.
    local after locked ntx
    after=$(numfield "$(jj "$FED_DBL" "$ha" user me)" available)
    locked=$(numfield "$(jj "$FED_DBL" "$ha" user me)" locked)
    ntx=$(python3 -c "
import sys, json
rows = json.loads(sys.argv[1])
print(sum(1 for t in rows if (t.get('action_name') or '').endswith('slow')))" "$(jj "$FED_DBL" "$ha" tx list --limit 50)")

    assert_eq "fed_crash.settled_exactly" yes \
        "$([ "$after" -eq "$before" ] || [ "$after" -eq "$((before - 23))" ] && echo yes || echo no)"
    known_defect "stranded funds after a provider crash" assert_eq "fed_crash.nothing_left_locked" 0 "$locked"
    known_defect "stranded funds after a provider crash" assert_eq "fed_crash.one_transaction_per_attempt" 2 "$ntx"
}
