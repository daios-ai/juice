#!/usr/bin/env bash
# Rail flows (D23). Every kernel here runs the play world, where the operator's own records are the
# finalized facts — so the whole money model is exercised with no chain, no wallet and no crypto,
# which is exactly the world a newcomer starts in. The chain-only states (a payment still in flight,
# a fuel purchase, a refusal to sign) cannot arise here; they are covered by the adaptor's unit tests
# and by flow_rail_chain, which needs a local chain.

# The launch story end to end (acceptance 1): somebody joins, is funded by the operator, and buys.
flow_rail_onboard() {
    echo "=== FLOW rail_onboard ==="
    local dir; dir=$(new_dir)
    # The selling kernel serves strangers on credit, which is what makes the first call possible
    # without anyone prefunding anything (U29).
    FED_RCFG=(exposure_max=1000 settlement_trigger=500)
    _fed_setup "$dir" || { fail "rail_onboard.setup" "setup failed"; return; }
    local ha; ha=$(home "$dir" alice)
    make_user "$FED_DBL" "$FED_HL" "$ha" alice

    # A newcomer asks how to put money in and is told the truth for this world: the operator records
    # payments here, so there is no address to send to and nothing to install.
    local how; how=$(j "$FED_DBL" "$ha" user deposit)
    assert_contains "rail_onboard.deposit_explains" "operator" "$how"
    assert_not_contains "rail_onboard.no_address_here" "0x" "$how"

    # Every crossing names the payment it records: without one, a repeated command would mint money.
    assert_fails "rail_onboard.ref_required" "ref\|payment" -- j "$FED_DBL" "$FED_HL" admin deposit alice 500
    local ref="invoice-77"
    j "$FED_DBL" "$FED_HL" admin deposit alice 500 --ref "$ref" >/dev/null 2>&1
    assert_jnum "rail_onboard.funded" "$(jj "$FED_DBL" "$ha" user me)" available 500
    # Recording the same payment again moves money once.
    j "$FED_DBL" "$FED_HL" admin deposit alice 500 --ref "$ref" >/dev/null 2>&1
    assert_jnum "rail_onboard.recorded_once" "$(jj "$FED_DBL" "$ha" user me)" available 500
    # The same payment on other terms is a different intention and is refused.
    assert_fails "rail_onboard.same_ref_other_amount" "" -- j "$FED_DBL" "$FED_HL" admin deposit alice 900 --ref "$ref"
    # Nothing is waiting on the operator: every payment so far had an owner.
    assert_not_contains "rail_onboard.nothing_held" "$ref" "$(j "$FED_DBL" "$FED_HL" admin deposit)"

    # And the point of the money: she buys a priced action on the other kernel.
    local rid; rid=$(publish "$FED_DBR" "$FED_HR" priced --kind http --source "http://127.0.0.1:$FED_BPORT" --description "a priced service" --price 100)
    assert_nonempty "rail_onboard.remote_call" "$(strfield "$(jj "$FED_DBL" "$ha" run sys@kernel-r/priced '{}')" tx_id)"
    local left; left=$(numfield "$(jj "$FED_DBL" "$ha" user me)" available)
    # 100 to the provider, 5 for serving it, 6 the origin keeps: one all-in price, charged once.
    assert_eq "rail_onboard.charged_the_advertised_price" 389 "$left"
    assert_jnum "rail_onboard.books_balance" "$(jj "$FED_DBL" "$FED_HL" admin identity)" gap 0
}

# Money out is the owner's own act, and asking for it twice must not pay twice (U51).
flow_rail_withdraw() {
    echo "=== FLOW rail_withdraw ==="
    local dir db hs ha; dir=$(new_dir); db="$dir/kernel/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "rail_withdraw.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    j "$db" "$hs" admin deposit alice 500 --ref "$(newref)" >/dev/null 2>&1

    # No prompt without a terminal: agents are first-class.
    local out; out=$(j "$db" "$ha" user withdraw 200)
    assert_contains "rail_withdraw.confirmed" "confirmed" "$out"
    assert_jnum "rail_withdraw.balance" "$(jj "$db" "$ha" user me)" available 300
    # Both legs are in her own ledger: out of her account, then out of the kernel.
    assert_contains "rail_withdraw.ledger_records_it" "sys" "$(j "$db" "$ha" user ledger)"
    # The row is listed with its outcome.
    assert_contains "rail_withdraw.listed" "confirmed" "$(j "$db" "$ha" user withdraw)"
    # More than she holds is refused, and nothing moves.
    assert_fails "rail_withdraw.over_balance" "insufficient\|error" -- j "$db" "$ha" user withdraw 9000
    assert_jnum "rail_withdraw.unchanged_after_refusal" "$(jj "$db" "$ha" user me)" available 300

    # A reply lost in transit is safe to ask for again: the same request id returns the same row.
    local base tok id
    base=$(url "$db"); tok=$(profile_get "$ha" token); id="11111111-2222-3333-4444-555555555555"
    local first second
    first=$(http_body POST "$base/v1/withdrawals" "{\"id\":\"$id\",\"amount\":50}" "$tok")
    second=$(http_body POST "$base/v1/withdrawals" "{\"id\":\"$id\",\"amount\":50}" "$tok")
    assert_eq "rail_withdraw.replay_same_row" "$(strfield "$first" id)" "$(strfield "$second" id)"
    assert_jnum "rail_withdraw.replay_moves_nothing" "$(jj "$db" "$ha" user me)" available 250
    # The same id on other terms is a different intention and is refused.
    assert_status "rail_withdraw.same_id_other_terms" 422 POST "$base/v1/withdrawals" "{\"id\":\"$id\",\"amount\":10}" "$tok"

    assert_jnum "rail_withdraw.books_balance" "$(jj "$db" "$hs" admin identity)" gap 0
}

# Settlement pays the whole debt and names the payment; the creditor closes its own books against it.
flow_rail_settlement() {
    echo "=== FLOW rail_settlement ==="
    local dir; dir=$(new_dir)
    # No quantum, so the debt is settled exactly rather than drawn for — the deterministic path, and
    # the one that always moves money.
    FED_RCFG=(exposure_max=1000 settlement_trigger=500)
    _fed_setup "$dir" || { fail "rail_settlement.setup" "setup failed"; return; }
    local lkey="$FED_LKEY"

    local rid; rid=$(publish "$FED_DBR" "$FED_HR" paid --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price 10)
    j "$FED_DBL" "$FED_HL" admin deposit sys 1000 --ref "$(newref)" >/dev/null 2>&1
    assert_nonempty "rail_settlement.call_on_credit" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/paid '{}')" tx_id)"
    assert_eq "rail_settlement.debt" 11 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)"

    local out sid
    out=$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)
    sid=$(strfield "$out" settlement_id)
    assert_json "rail_settlement.paid" "$out" status announced
    assert_json "rail_settlement.mode" "$out" mode exact
    assert_nonempty "rail_settlement.names_the_payment" "$sid"
    # The debtor's books close as soon as the payment is final.
    assert_eq "rail_settlement.debtor_clear" 0 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)"
    # The creditor's do not: it is owed until it records that the money arrived.
    assert_eq "rail_settlement.creditor_still_owed" -11 "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" available)"
    assert_contains "rail_settlement.claim_waiting" "$sid" "$(j "$FED_DBR" "$FED_HR" admin deposit)"

    # Recording some other payment from the same peer does not close this settlement: only the
    # payment it names does. (A reference to a payment that never happened is refusable only where
    # something outside can be asked; here the operator's word is the fact.)
    j "$FED_DBR" "$FED_HR" admin deposit --ref "unrelated-payment" -- "$lkey" 5 >/dev/null 2>&1
    assert_contains "rail_settlement.claim_still_open" "$sid" "$(j "$FED_DBR" "$FED_HR" admin deposit)"

    j "$FED_DBR" "$FED_HR" admin deposit --ref "$sid" -- "$lkey" 11 >/dev/null 2>&1 || { fail "rail_settlement.record" "failed"; return; }
    assert_not_contains "rail_settlement.claim_closed" "$sid" "$(j "$FED_DBR" "$FED_HR" admin deposit)"
    local after; after=$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" available)
    # Recording it again moves nothing.
    j "$FED_DBR" "$FED_HR" admin deposit --ref "$sid" -- "$lkey" 11 >/dev/null 2>&1
    assert_eq "rail_settlement.record_idempotent" "$after" "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" available)"

    # Both sets of books add up, and there is nothing left to settle.
    assert_jnum "rail_settlement.debtor_books" "$(jj "$FED_DBL" "$FED_HL" admin identity)" gap 0
    assert_jnum "rail_settlement.creditor_books" "$(jj "$FED_DBR" "$FED_HR" admin identity)" gap 0
    assert_json "rail_settlement.nothing_left" "$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)" status settled
}

# Two kernels on different networks cannot meet, even sharing a bootstrap node (acceptance 2).
flow_rail_isolation() {
    echo "=== FLOW rail_isolation ==="
    local dir; dir=$(new_dir)
    local dbr="$dir/r/kernel/juice.db" hr="$dir/rsys" dbl="$dir/l/kernel/juice.db" hl="$dir/lsys"
    mkdir -p "$dir/r/kernel" "$dir/l/kernel" "$hr/.juice" "$hl/.juice"

    # A world of somebody's own: same shape, different name, therefore a different network.
    local other="$dir/other-world.json"
    printf '{"name":"otherworld","decimals":0}\n' > "$other"

    start_server "$dbr" "$hr" kernel_handle=kernel-r discovery_interval_seconds=2 || { fail "rail_isolation.boot_r" "no start"; return; }
    local boot; boot=$(kernel_fed_addr "$dbr")
    start_server "$dbl" "$hl" kernel_handle=kernel-l world="$other" bootstrap_peers="$boot" discovery_interval_seconds=2 \
        || { fail "rail_isolation.boot_l" "no start"; return; }
    j "$dbr" "$hr" auth login sys --password sys-pass >/dev/null 2>&1
    j "$dbl" "$hl" auth login sys --password sys-pass >/dev/null 2>&1

    # Each kernel reports its own network, so an operator can see which one they are on.
    assert_contains "rail_isolation.r_network" "play" "$(j "$dbr" "$hr" health)"
    assert_contains "rail_isolation.l_network" "otherworld" "$(j "$dbl" "$hl" health)"

    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    publish "$dbr" "$hr" greet --kind http --source "http://127.0.0.1:1" --description "greet" --price 0 >/dev/null 2>&1

    # Give discovery several passes to find nothing.
    sleep 6
    assert_not_contains "rail_isolation.roster_stays_empty" "$rkey" "$(j "$dbl" "$hl" admin peers)"
    assert_not_contains "rail_isolation.no_reverse_sighting" "$(kernel_key "$dbl" "$hl")" "$(j "$dbr" "$hr" admin peers)"
    # Naming the other kernel's action directly fails: nothing it signs can verify here.
    assert_fails "rail_isolation.call_refused" "" -- j "$dbl" "$hl" run "sys@$rkey/greet" '{}'
}

# A database belongs to the network it was made for, and says so rather than serving another.
flow_rail_world_mismatch() {
    echo "=== FLOW rail_world_mismatch ==="
    local dir db hs; dir=$(new_dir); db="$dir/kernel/juice.db"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "rail_world_mismatch.boot" "server did not start"; return; }
    stop_server "$db"

    local other="$dir/other-world.json"
    printf '{"name":"otherworld","decimals":0}\n' > "$other"
    local log="$dir/kernel/mismatch.log"
    write_config "$db" world="$other"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$hs" JUICE_HOME="$(khome "$db")" \
        "$JUICE" serve --addr 127.0.0.1:0 >"$log" 2>&1
    local code=$?
    assert_ne "rail_world_mismatch.refused" 0 "$code"
    assert_contains "rail_world_mismatch.says_why" "network" "$(cat "$log")"

    # Its own network still starts.
    write_config "$db"
    start_server "$db" "$hs" || fail "rail_world_mismatch.own_world_starts" "did not start"
}

# One kernel, one server: two would race the same signer and the same workers.
flow_rail_lock() {
    echo "=== FLOW rail_lock ==="
    local dir db hs; dir=$(new_dir); db="$dir/kernel/juice.db"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "rail_lock.boot" "server did not start"; return; }

    local log="$dir/kernel/second.log"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$hs" JUICE_HOME="$(khome "$db")" \
        "$JUICE" serve --addr 127.0.0.1:0 >"$log" 2>&1
    assert_ne "rail_lock.second_refused" 0 "$?"
    assert_contains "rail_lock.says_why" "another server" "$(cat "$log")"

    # Once the first stops, the home is free again.
    stop_server "$db"
    start_server "$db" "$hs" || fail "rail_lock.restarts_after_stop" "did not start"
}

# A client pins each kernel it knows, and never hands its token to a server it has not pinned.
flow_rail_profile() {
    echo "=== FLOW rail_profile ==="
    local dir; dir=$(new_dir)
    local dba="$dir/a/kernel/juice.db" hb="$dir/b/kernel/juice.db" hc="$dir/client"
    mkdir -p "$dir/a/kernel" "$dir/b/kernel" "$hc/.juice"
    start_server "$dba" "$hc" kernel_handle=kernel-a || { fail "rail_profile.boot_a" "no start"; return; }
    start_server "$hb" "$hc" kernel_handle=kernel-b || { fail "rail_profile.boot_b" "no start"; return; }

    # Pinning a kernel records its key and its network.
    HOME="$hc" "$JUICE" use ka --endpoint "$(url "$dba")" >/dev/null 2>&1 || { fail "rail_profile.pin" "use failed"; return; }
    assert_nonempty "rail_profile.pinned_key" "$(profile_get "$hc" public_key)"
    assert_contains "rail_profile.lists" "ka" "$(HOME="$hc" "$JUICE" use)"
    HOME="$hc" "$JUICE" auth login sys --password sys-pass >/dev/null 2>&1
    assert_json "rail_profile.logged_in" "$(HOME="$hc" "$JUICE" --json user me)" handle sys
    assert_json "rail_profile.env_override" "$(JUICE_PROFILE=ka HOME="$hc" "$JUICE" --json user me)" handle sys

    # The token belongs to the kernel it was issued by: pointed at another server, the client is
    # anonymous rather than handing its bearer to a stranger.
    assert_fails "rail_profile.token_stays_home" "login\|unauthenticated\|not logged in" -- \
        env HOME="$hc" "$JUICE" --server "$(url "$hb")" user me

    # A profile that no longer names the kernel it pinned refuses to switch rather than guessing.
    profile_set "$hc" public_key "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    assert_fails "rail_profile.key_mismatch_refused" "" -- env HOME="$hc" "$JUICE" use ka
    profile_set "$hc" world_digest "0000000000000000000000000000000000000000000000000000000000000000"
    assert_fails "rail_profile.digest_mismatch_refused" "" -- env HOME="$hc" "$JUICE" use ka
}

# Earning and spending, across a restart (acceptance 4): A funds a buyer, the buyer pays B, A and B
# settle, and B then buys from C out of what it earned.
flow_rail_economic_loop() {
    echo "=== FLOW rail_economic_loop ==="
    local dir; dir=$(new_dir)
    FED_RCFG=(exposure_max=1000 settlement_trigger=500)
    _fed_setup "$dir" || { fail "rail_economic_loop.setup" "setup failed"; return; }

    # A third kernel, C, bootstrapped to the same node so B can reach it.
    local dbc="$dir/c/kernel/juice.db" hc="$dir/csys"
    mkdir -p "$dir/c/kernel" "$hc/.juice"
    start_server "$dbc" "$hc" kernel_handle=kernel-c bootstrap_peers="$FED_BOOT" discovery_interval_seconds=2 \
        exposure_max=1000 settlement_trigger=500 \
        || { fail "rail_economic_loop.boot_c" "no start"; return; }
    j "$dbc" "$hc" auth login sys --password sys-pass >/dev/null 2>&1
    local ckey; ckey=$(kernel_key "$dbc" "$hc")
    publish "$dbc" "$hc" tooling --kind http --source "http://127.0.0.1:$FED_BPORT" --description "tooling" --price 20 >/dev/null 2>&1

    # B sells; A's buyer pays for it on credit.
    publish "$FED_DBR" "$FED_HR" service --kind http --source "http://127.0.0.1:$FED_BPORT" --description "a service" --price 200 >/dev/null 2>&1
    local ha; ha=$(home "$dir" buyer)
    make_user "$FED_DBL" "$FED_HL" "$ha" buyer
    j "$FED_DBL" "$FED_HL" admin deposit buyer 1000 --ref "$(newref)" >/dev/null 2>&1
    assert_nonempty "rail_economic_loop.buyer_pays_b" "$(strfield "$(jj "$FED_DBL" "$ha" run sys@kernel-r/service '{}')" tx_id)"

    # B has earned: its operator's balance grew by the charge plus its serving markup.
    local earned; earned=$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)
    assert_eq "rail_economic_loop.b_earned" 210 "$earned"

    # A owes B; they settle over the rail and B records the payment.
    local out sid; out=$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r); sid=$(strfield "$out" settlement_id)
    assert_json "rail_economic_loop.settled" "$out" status announced
    j "$FED_DBR" "$FED_HR" admin deposit --ref "$sid" -- "$FED_LKEY" 210 >/dev/null 2>&1
    assert_eq "rail_economic_loop.b_books_clear" 0 "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$FED_LKEY")" available)"

    # B spends its earnings on C: what it sold for is money it can now spend.
    assert_nonempty "rail_economic_loop.b_buys_from_c" "$(strfield "$(jj "$FED_DBR" "$FED_HR" run "sys@$ckey/tooling" '{}')" tx_id)"
    local spent; spent=$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)
    assert_ne "rail_economic_loop.b_spent" "$earned" "$spent"
    # C's books, checked while nothing is in flight.
    assert_jnum "rail_economic_loop.c_books" "$(jj "$dbc" "$hc" admin identity)" gap 0

    # B restarts: the earnings, the settled debt and the resolved supplier all survive.
    stop_server "$FED_DBR"
    start_server "$FED_DBR" "$FED_HR" kernel_handle=kernel-r discovery_interval_seconds=2 "${FED_RCFG[@]}" \
        || { fail "rail_economic_loop.restart_b" "did not restart"; return; }
    await_login "$FED_DBR" "$FED_HR" || { fail "rail_economic_loop.b_answers" "not serving after restart"; return; }
    assert_eq "rail_economic_loop.earnings_survive" "$spent" "$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)"
    # B was the node the others dialed, and it comes back on a new port, so C is pointed at where B
    # is now — the ordinary consequence of restarting a node others reach through.
    stop_server "$dbc"
    start_server "$dbc" "$hc" kernel_handle=kernel-c bootstrap_peers="$(kernel_fed_addr "$FED_DBR")" \
        discovery_interval_seconds=2 exposure_max=1000 settlement_trigger=500 \
        || { fail "rail_economic_loop.restart_c" "did not restart"; return; }
    await_login "$dbc" "$hc" || { fail "rail_economic_loop.c_answers" "not serving after restart"; return; }
    local bought=""
    for _ in $(seq 15); do
        bought=$(strfield "$(jj "$FED_DBR" "$FED_HR" run "sys@$ckey/tooling" '{}')" tx_id)
        [ -n "$bought" ] && break
        sleep 1
    done
    assert_nonempty "rail_economic_loop.buys_again_after_restart" "$bought"

    # All three sets of books add up.
    assert_jnum "rail_economic_loop.a_books" "$(jj "$FED_DBL" "$FED_HL" admin identity)" gap 0
    assert_jnum "rail_economic_loop.b_books" "$(jj "$FED_DBR" "$FED_HR" admin identity)" gap 0
}
