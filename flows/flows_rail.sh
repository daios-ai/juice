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
    # without anyone prefunding anything (U29). Serving is funded by the seller's own money.
    _fed_setup "$dir" || { fail "rail_onboard.setup" "setup failed"; return; }
    j "$FED_DBR" "$FED_HR" admin user deposit sys "$(units 5000)" --ref "$(newref)" --yes >/dev/null 2>&1
    local ha; ha=$(home "$dir" alice)
    make_user "$FED_DBL" "$FED_HL" "$ha" alice

    # A newcomer asks how to put money in and is told the truth for this world: the operator records
    # payments here, so there is no address to send to and nothing to install.
    local how; how=$(j "$FED_DBL" "$ha" user deposit)
    assert_contains "rail_onboard.deposit_explains" "operator" "$how"
    assert_not_contains "rail_onboard.no_address_here" "0x" "$how"

    # Every crossing names the payment it records: without one, a repeated command would mint money.
    assert_fails "rail_onboard.ref_required" "ref\|payment" -- j "$FED_DBL" "$FED_HL" admin user deposit alice "$(units 500)"
    local ref="invoice-77"
    j "$FED_DBL" "$FED_HL" admin user deposit alice "$(units 500)" --ref "$ref" --yes >/dev/null 2>&1
    assert_jnum "rail_onboard.funded" "$(jj "$FED_DBL" "$ha" user me)" available 500
    # Recording the same payment again moves money once.
    j "$FED_DBL" "$FED_HL" admin user deposit alice "$(units 500)" --ref "$ref" --yes >/dev/null 2>&1
    assert_jnum "rail_onboard.recorded_once" "$(jj "$FED_DBL" "$ha" user me)" available 500
    # The same payment on other terms is a different intention and is refused.
    assert_fails "rail_onboard.same_ref_other_amount" "" -- j "$FED_DBL" "$FED_HL" admin user deposit alice "$(units 900)" --ref "$ref" --yes
    # Nothing is waiting on the operator: every payment so far had an owner.
    assert_not_contains "rail_onboard.nothing_held" "$ref" "$(j "$FED_DBL" "$FED_HL" admin kernel deposits)"

    # And the point of the money: she buys a priced action on the other kernel.
    local rid; rid=$(publish "$FED_DBR" "$FED_HR" priced --kind http --source "http://127.0.0.1:$FED_BPORT" --description "a priced service" --price "$(units 100)")
    assert_nonempty "rail_onboard.remote_call" "$(strfield "$(jj "$FED_DBL" "$ha" run sys@kernel-r/priced '{}')" tx_id)"
    local left; left=$(numfield "$(jj "$FED_DBL" "$ha" user me)" available)
    # 100 to the provider, 5 for serving it, 6 the origin keeps: one all-in price, charged once.
    assert_eq "rail_onboard.charged_the_advertised_price" 389 "$left"
    assert_jnum "rail_onboard.books_balance" "$(jj "$FED_DBL" "$FED_HL" admin kernel show)" gap 0
}

# Money out is the owner's own act, and asking for it twice must not pay twice (U51).
flow_rail_withdraw() {
    echo "=== FLOW rail_withdraw ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "rail_withdraw.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    j "$db" "$hs" admin user deposit alice "$(units 500)" --ref "$(newref)" --yes >/dev/null 2>&1

    # No prompt without a terminal: agents are first-class.
    local out; out=$(j "$db" "$ha" user withdraw "$(units 200)" --yes)
    assert_contains "rail_withdraw.confirmed" "confirmed" "$out"
    assert_jnum "rail_withdraw.balance" "$(jj "$db" "$ha" user me)" available 300
    # Both legs are in her own ledger: out of her account, then out of the kernel.
    assert_contains "rail_withdraw.ledger_records_it" "sys" "$(j "$db" "$ha" user ledger)"
    # The row is listed with its outcome.
    assert_contains "rail_withdraw.listed" "confirmed" "$(j "$db" "$ha" user withdrawals)"
    # More than she holds is refused, and nothing moves.
    assert_fails "rail_withdraw.over_balance" "insufficient\|error" -- j "$db" "$ha" user withdraw "$(units 9000)" --yes
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

    assert_jnum "rail_withdraw.books_balance" "$(jj "$db" "$hs" admin kernel show)" gap 0
}

# A cross-kernel obligation whose face value is below it is paid exactly, with no draw at all — the
# deterministic path, and the one that always moves money. The buyer pays on the rail, tells the
# seller which payment settles it, and the seller closes its books when that payment arrives.
flow_rail_settlement() {
    echo "=== FLOW rail_settlement ==="
    local dir; dir=$(new_dir)
    # The buyer's rail worker runs often, so the payment it commits at settlement is sent and the
    # seller told which payment settles the obligation, inside the flow rather than a minute later.
    FED_LCFG=(remote_retry_interval_seconds=2)
    _fed_setup "$dir" || { fail "rail_settlement.setup" "setup failed"; return; }
    local lkey="$FED_LKEY"

    local rid; rid=$(publish "$FED_DBR" "$FED_HR" paid --kind http --source "http://127.0.0.1:$FED_BPORT" --description "paid" --price "$(units 10)")
    j "$FED_DBR" "$FED_HR" admin user deposit sys "$(units 5000)" --ref "$(newref)" --yes >/dev/null 2>&1
    j "$FED_DBL" "$FED_HL" admin user deposit sys "$(units 1000)" --ref "$(newref)" --yes >/dev/null 2>&1
    local before seller; before=$(balance_of "$FED_DBL" "$FED_HL"); seller=$(balance_of "$FED_DBR" "$FED_HR")

    assert_nonempty "rail_settlement.call" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/paid '{}')" tx_id)"
    # mp 10 → sr 11 → q 12. With no lottery the obligation of 11 is paid exactly, so the caller is
    # out the whole all-in price less the import fee its own kernel keeps.
    assert_eq "rail_settlement.charged_exactly" 11 "$(( before - $(balance_of "$FED_DBL" "$FED_HL") ))"
    # The obligation is a ticket, not a balance: a peer row holds nothing.
    assert_eq "rail_settlement.peer_row_is_zero" 0 "$(numfield "$(jj "$FED_DBR" "$FED_HR" admin peer show -- "$lkey")" available)"

    # The buyer pays and then tells the seller which payment settles it. On a world with no
    # addresses that signed reveal IS the payment, so the seller's books close against it with
    # nobody recording anything: no operator acts anywhere in this flow.
    local ticket; ticket=$(strfield "$(jj "$FED_DBL" "$FED_HL" tx show "$(find_id "$(jj "$FED_DBL" "$FED_HL" tx list)" action_name sys/paid)")" ticket_id)
    assert_nonempty "rail_settlement.names_its_ticket" "$ticket"
    await_eq "rail_settlement.nothing_owed" 0 owed_count "$FED_DBR" "$FED_HR" "$lkey"

    # And the money is really there: the provider is credited the whole obligation, once.
    await_eq "rail_settlement.provider_credited" $((seller + 11)) balance_of "$FED_DBR" "$FED_HR"

    # Both sets of books add up, and the cash discharged what was delivered.
    assert_jnum "rail_settlement.buyer_books" "$(jj "$FED_DBL" "$FED_HL" admin kernel show)" gap 0
    assert_jnum "rail_settlement.seller_books" "$(jj "$FED_DBR" "$FED_HR" admin kernel show)" gap 0
    assert_jnum "rail_settlement.exposure_discharged" "$(jj "$FED_DBR" "$FED_HR" admin kernel show)" exposure 0
}

# Two kernels on different networks cannot meet, even sharing a bootstrap node (acceptance 2).
flow_rail_isolation() {
    echo "=== FLOW rail_isolation ==="
    local dir; dir=$(new_dir)
    local dbr="$(kdb "$dir/r")" hr="$dir/rsys" dbl="$(kdb "$dir/l" otherworld)" hl="$dir/lsys"
    mkdir -p "$(dirname "$dbr")" "$(dirname "$dbl")" "$hr/.juice" "$hl/.juice"

    # A world of somebody's own: a file this build does not ship, therefore a different network.
    local other="$dir/other-world.json"
    printf '{"rail":"manual","decimals":6,"symbol":"OTHER","description":"a network of my own","seeds":[]}\n' > "$other"
    install_world "$dbl" "$other"

    start_server "$dbr" "$hr" kernel_handle=kernel-r discovery_interval_seconds=2 || { fail "rail_isolation.boot_r" "no start"; return; }
    know "$dbr" "$hr"
    j "$dbr" "$hr" auth login sys@$KERNEL_NAME --password sys-pass >/dev/null 2>&1
    local boot; boot=$(kernel_fed_addr "$dbr" "$hr")
    start_server "$dbl" "$hl" kernel_handle=kernel-l seed="$boot" discovery_interval_seconds=2 \
        || { fail "rail_isolation.boot_l" "no start"; return; }
    know "$dbr" "$hr"
    j "$dbr" "$hr" auth login sys@$KERNEL_NAME --password sys-pass >/dev/null 2>&1
    know "$dbl" "$hl"
    j "$dbl" "$hl" auth login sys@$KERNEL_NAME --password sys-pass >/dev/null 2>&1

    # Each kernel reports its own network, so an operator can see which one they are on.
    assert_contains "rail_isolation.r_network" "play" "$(j "$dbr" "$hr" kernel health)"
    assert_contains "rail_isolation.l_network" "otherworld" "$(j "$dbl" "$hl" kernel health)"

    local rkey; rkey=$(kernel_key "$dbr" "$hr")
    publish "$dbr" "$hr" greet --kind http --source "http://127.0.0.1:1" --description "greet" --price "$(units 0)" >/dev/null 2>&1

    # Give discovery several passes to find nothing.
    sleep 6
    assert_not_contains "rail_isolation.roster_stays_empty" "$rkey" "$(j "$dbl" "$hl" admin peer list)"
    assert_not_contains "rail_isolation.no_reverse_sighting" "$(kernel_key "$dbl" "$hl")" "$(j "$dbr" "$hr" admin peer list)"
    # Naming the other kernel's action directly fails: nothing it signs can verify here.
    assert_fails "rail_isolation.call_refused" "" -- j "$dbl" "$hl" run "sys@$rkey/greet" '{}'
}

# A database belongs to the network it was made for, and says so rather than serving another.
flow_rail_world_mismatch() {
    echo "=== FLOW rail_world_mismatch ==="
    local dir db hs; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "rail_world_mismatch.boot" "server did not start"; return; }
    stop_server "$db"

    # The same home, offered to another network: its ledger, its key and its debts mean one thing
    # only, so the boot refuses and names both networks rather than serving under a second meaning.
    local root; root=$(khome "$db")
    mv "$root/kernels/play" "$root/kernels/arbitrum-sepolia"
    local log="$dir/mismatch.log"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$hs" JUICE_HOME="$root" \
        "$JUICE" kernel serve arbitrum-sepolia --listen-addr 127.0.0.1:0 >"$log" 2>&1
    assert_ne "rail_world_mismatch.refused" 0 "$?"
    assert_contains "rail_world_mismatch.says_why" "bound to network" "$(cat "$log")"
    # It refuses before the rail: a chain world whose node is unreachable is never dialled.
    assert_not_contains "rail_world_mismatch.dialled_nothing" "dial" "$(cat "$log")"

    # Its own network still starts.
    mv "$root/kernels/arbitrum-sepolia" "$root/kernels/play"
    start_server "$db" "$hs" || fail "rail_world_mismatch.own_world_starts" "did not start"
}

# One kernel, one server: two would race the same signer and the same workers.
flow_rail_lock() {
    echo "=== FLOW rail_lock ==="
    local dir db hs; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "rail_lock.boot" "server did not start"; return; }

    local log="$(dirname "$db")/second.log"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$hs" JUICE_HOME="$(khome "$db")" \
        "$JUICE" kernel serve "$(basename "$(dirname "$db")")" --listen-addr 127.0.0.1:0 >"$log" 2>&1
    assert_ne "rail_lock.second_refused" 0 "$?"
    assert_contains "rail_lock.says_why" "another server" "$(cat "$log")"

    # Once the first stops, the home is free again.
    stop_server "$db"
    start_server "$db" "$hs" || fail "rail_lock.restarts_after_stop" "did not start"
}

# A client knows each kernel by name, pins its identity, and never hands a token to a server it has
# not pinned. A login is that name and an account at once.
flow_rail_profile() {
    echo "=== FLOW rail_profile ==="
    local dir; dir=$(new_dir)
    local dba="$(kdb "$dir/a")" hb="$(kdb "$dir/b")" hc="$dir/client"
    mkdir -p "$(dirname "$dba")" "$(dirname "$hb")" "$hc/.juice"
    start_server "$dba" "$hc" kernel_handle=kernel-a || { fail "rail_profile.boot_a" "no start"; return; }
    start_server "$hb" "$hc" kernel_handle=kernel-b || { fail "rail_profile.boot_b" "no start"; return; }

    # Registering a kernel records its key and its network, and selects nothing.
    HOME="$hc" "$JUICE" kernel add "$(url "$dba")" ka >/dev/null 2>&1 || { fail "rail_profile.pin" "add failed"; return; }
    assert_nonempty "rail_profile.pinned_key" "$(HOME="$hc" "$JUICE" --json kernel list | grep -o '"public_key": "[^"]*"' | head -1)"
    assert_contains "rail_profile.lists" "ka" "$(HOME="$hc" "$JUICE" kernel list)"
    HOME="$hc" "$JUICE" auth login sys@ka --password sys-pass >/dev/null 2>&1
    assert_json "rail_profile.logged_in" "$(HOME="$hc" "$JUICE" --json user me)" handle sys
    assert_contains "rail_profile.login_listed" "sys@ka" "$(HOME="$hc" "$JUICE" auth list)"
    assert_json "rail_profile.as_override" "$(JUICE_AS=sys@ka HOME="$hc" "$JUICE" --json user me)" handle sys

    # A login this client does not hold is refused rather than falling back to the one it does.
    assert_fails "rail_profile.as_unknown_refused" "no kernel\|USER@KERNEL\|not logged in" -- \
        env JUICE_AS=sys@nosuch HOME="$hc" "$JUICE" user me

    # The token belongs to the kernel it was issued by: pointed at another server, the client is
    # anonymous rather than handing its bearer to a stranger.
    assert_fails "rail_profile.token_stays_home" "login\|unauthenticated\|not logged in" -- \
        env HOME="$hc" "$JUICE" --server "$(url "$hb")" user me

    # A record that no longer names the kernel it pinned refuses to switch rather than guessing.
    profile_set "$hc" public_key "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    assert_fails "rail_profile.key_mismatch_refused" "" -- env HOME="$hc" "$JUICE" auth use sys@ka
    profile_set "$hc" world_fingerprint "0000000000000000000000000000000000000000000000000000000000000000"
    assert_fails "rail_profile.digest_mismatch_refused" "" -- env HOME="$hc" "$JUICE" auth use sys@ka
}

# Earning and spending, across a restart (acceptance 4): A funds a buyer, the buyer pays B, A and B
# settle, and B then buys from C out of what it earned.
flow_rail_economic_loop() {
    echo "=== FLOW rail_economic_loop ==="
    local dir; dir=$(new_dir)
    # Both buyers here settle inside the flow rather than a minute later, so their workers run often.
    FED_LCFG=(remote_retry_interval_seconds=2)
    FED_RCFG=(remote_retry_interval_seconds=2)
    _fed_setup "$dir" || { fail "rail_economic_loop.setup" "setup failed"; return; }

    # A third kernel, C, bootstrapped to the same node so B can reach it.
    local dbc="$(kdb "$dir/c")" hc="$dir/csys"
    mkdir -p "$(dirname "$dbc")" "$hc/.juice"
    start_server "$dbc" "$hc" kernel_handle=kernel-c seed="$FED_BOOT" discovery_interval_seconds=2 \
        || { fail "rail_economic_loop.boot_c" "no start"; return; }
    know "$dbc" "$hc"
    j "$dbc" "$hc" auth login sys@$KERNEL_NAME --password sys-pass >/dev/null 2>&1
    local ckey; ckey=$(kernel_key "$dbc" "$hc")
    publish "$dbc" "$hc" tooling --kind http --source "http://127.0.0.1:$FED_BPORT" --description "tooling" --price "$(units 20)" >/dev/null 2>&1
    j "$dbc" "$hc" admin user deposit sys "$(units 1000)" --ref "$(newref)" --yes >/dev/null 2>&1

    # B sells, funding its own work; A's buyer pays for it.
    publish "$FED_DBR" "$FED_HR" service --kind http --source "http://127.0.0.1:$FED_BPORT" --description "a service" --price "$(units 200)" >/dev/null 2>&1
    j "$FED_DBR" "$FED_HR" admin user deposit sys "$(units 1000)" --ref "$(newref)" --yes >/dev/null 2>&1
    local ha; ha=$(home "$dir" buyer)
    make_user "$FED_DBL" "$FED_HL" "$ha" buyer
    j "$FED_DBL" "$FED_HL" admin user deposit buyer "$(units 1000)" --ref "$(newref)" --yes >/dev/null 2>&1
    assert_nonempty "rail_economic_loop.buyer_pays_b" "$(strfield "$(jj "$FED_DBL" "$ha" run sys@kernel-r/service '{}')" tx_id)"

    # A owes B for the work, and pays it without anyone being asked: B's books clear on A's reveal.
    local ticket; ticket=$(strfield "$(jj "$FED_DBL" "$ha" tx show "$(find_id "$(jj "$FED_DBL" "$ha" tx list)" action_name sys/service)")" ticket_id)
    assert_nonempty "rail_economic_loop.names_its_ticket" "$ticket"
    await_eq "rail_economic_loop.b_books_clear" 0 owed_count "$FED_DBR" "$FED_HR" "$FED_LKEY"

    # B has earned: its operator's balance grew by the charge plus its serving markup.
    local earned; earned=$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)

    # B spends its earnings on C: what it sold for is money it can now spend.
    local ctx1; ctx1=$(strfield "$(jj "$FED_DBR" "$FED_HR" run "sys@$ckey/tooling" '{}')" tx_id)
    assert_nonempty "rail_economic_loop.b_buys_from_c" "$ctx1"
    local spent; spent=$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)
    assert_ne "rail_economic_loop.b_spent" "$earned" "$spent"
    # C's books, checked while nothing is in flight.
    assert_jnum "rail_economic_loop.c_books" "$(jj "$dbc" "$hc" admin kernel show)" gap 0

    # B restarts: the earnings, the settled debt and the resolved supplier all survive.
    stop_server "$FED_DBR"
    start_server "$FED_DBR" "$FED_HR" kernel_handle=kernel-r discovery_interval_seconds=2 \
        remote_retry_interval_seconds=2 || { fail "rail_economic_loop.restart_b" "did not restart"; return; }
    await_login "$FED_DBR" "$FED_HR" || { fail "rail_economic_loop.b_answers" "not serving after restart"; return; }
    assert_eq "rail_economic_loop.earnings_survive" "$spent" "$(numfield "$(jj "$FED_DBR" "$FED_HR" user me)" available)"
    # B was the node the others dialed, and it comes back on a new port, so C is pointed at where B
    # is now — the ordinary consequence of restarting a node others reach through.
    stop_server "$dbc"
    start_server "$dbc" "$hc" kernel_handle=kernel-c seed="$(kernel_fed_addr "$FED_DBR" "$FED_HR")" \
        discovery_interval_seconds=2 \
        || { fail "rail_economic_loop.restart_c" "did not restart"; return; }
    await_login "$dbc" "$hc" || { fail "rail_economic_loop.c_answers" "not serving after restart"; return; }
    # What B owes C survives B's restart and settles itself: B's first pass after coming back drives
    # the payment it committed and tells C, so the loop closes with no operator anywhere in it.
    await_eq "rail_economic_loop.c_paid" 0 owed_count "$dbc" "$hc" "$FED_RKEY"
    local bought=""
    for _ in $(seq 15); do
        bought=$(strfield "$(jj "$FED_DBR" "$FED_HR" run "sys@$ckey/tooling" '{}')" tx_id)
        [ -n "$bought" ] && break
        sleep 1
    done
    assert_nonempty "rail_economic_loop.buys_again_after_restart" "$bought"

    # All three sets of books add up.
    assert_jnum "rail_economic_loop.a_books" "$(jj "$FED_DBL" "$FED_HL" admin kernel show)" gap 0
    assert_jnum "rail_economic_loop.b_books" "$(jj "$FED_DBR" "$FED_HR" admin kernel show)" gap 0
}

# Two kernels under one installation: a kernel belongs to the world it serves, so a kernel on a
# second network is a sibling of the first — its own ledger, its own key, its own address — and a
# client names each by the name it registered rather than by whatever holds a port.
flow_two_kernels() {
    echo "=== FLOW two_kernels ==="
    local dir; dir=$(new_dir)
    local dba dbb hc; dba="$(kdb "$dir" play)"; dbb="$(kdb "$dir" otherworld)"; hc="$dir/client"
    mkdir -p "$(dirname "$dbb")"
    printf '{"rail":"manual","decimals":6,"symbol":"OTHER","description":"a second network","seeds":[]}\n' > "$dir/beta-world.json"
    install_world "$dbb" "$dir/beta-world.json"
    mkdir -p "$hc/.juice"
    start_server "$dba" "$hc" kernel_handle=kernel-alpha || { fail "two_kernels.boot_alpha" "no start"; return; }
    start_server "$dbb" "$hc" kernel_handle=kernel-beta  || { fail "two_kernels.boot_beta" "no start"; return; }

    assert_eq "two_kernels.alpha_home" "yes" "$([ -f "$dir/kernels/play/juice.db" ] && echo yes || echo no)"
    assert_eq "two_kernels.beta_home" "yes" "$([ -f "$dir/kernels/otherworld/juice.db" ] && echo yes || echo no)"
    assert_ne "two_kernels.separate_addresses" "$(url "$dba")" "$(url "$dbb")"

    # One client, two kernels, one login on each. Each login reaches its own.
    HOME="$hc" "$JUICE" kernel add "$(url "$dba")" alpha >/dev/null 2>&1
    HOME="$hc" "$JUICE" auth login sys@alpha --password sys-pass >/dev/null 2>&1
    HOME="$hc" "$JUICE" kernel add "$(url "$dbb")" beta >/dev/null 2>&1
    HOME="$hc" "$JUICE" auth login sys@beta --password sys-pass >/dev/null 2>&1

    local ka kb
    ka=$(strfield "$(JUICE_AS=sys@alpha HOME="$hc" "$JUICE" --json admin kernel show)" public_key)
    kb=$(strfield "$(JUICE_AS=sys@beta  HOME="$hc" "$JUICE" --json admin kernel show)" public_key)
    assert_nonempty "two_kernels.alpha_key" "$ka"
    assert_ne "two_kernels.distinct_identities" "$ka" "$kb"
    assert_json "two_kernels.alpha_handle" \
        "$(JUICE_AS=sys@alpha HOME="$hc" "$JUICE" --json admin kernel show)" handle kernel-alpha
    assert_json "two_kernels.beta_handle" \
        "$(JUICE_AS=sys@beta HOME="$hc" "$JUICE" --json admin kernel show)" handle kernel-beta

    # Money is the kernel's own: a deposit on one is invisible on the other.
    JUICE_AS=sys@alpha HOME="$hc" "$JUICE" user create onlyhere@alpha --password userpass >/dev/null 2>&1
    assert_contains "two_kernels.local_account" "onlyhere" \
        "$(JUICE_AS=sys@alpha HOME="$hc" "$JUICE" admin user list)"
    assert_not_contains "two_kernels.not_on_the_other" "onlyhere" \
        "$(JUICE_AS=sys@beta HOME="$hc" "$JUICE" admin user list)"
}

# A kernel is created once, and what it can never revise is asked for rather than defaulted: the
# network on the command line, its name on the network and its key at first boot. The worlds this
# build ships are written where the operator can read and edit them, and a world of their own is a
# file they drop beside those.
flow_first_boot() {
    echo "=== FLOW first_boot ==="
    local dir; dir=$(new_dir); local root="$dir/root"
    mkdir -p "$root"

    # No world: there is no kernel to serve, so nothing is created.
    local out; out=$(JUICE_HOME="$root" "$JUICE" kernel serve 2>&1)
    assert_contains "first_boot.world_required" "juice kernel serve WORLD" "$out"
    assert_eq "first_boot.nothing_created" "no" "$([ -d "$root/kernels" ] && echo yes || echo no)"

    # A world nobody ships and nobody wrote: refused, naming the file that would have said what the
    # network is, and leaving no kernel behind.
    out=$(JUICE_BOOTSTRAP_PASSWORD=sys-pass JUICE_HOME="$root" "$JUICE" kernel serve nope 2>&1)
    assert_contains "first_boot.unknown_world_refused" "$root/worlds/nope.json" "$out"
    assert_eq "first_boot.nothing_written" "no" "$([ -d "$root/kernels/nope" ] && echo yes || echo no)"
    # Looking for it installed the worlds this build does ship, so what an operator can serve, and
    # edit, is in front of them.
    for w in play arbitrum-sepolia arbitrum-one; do
        assert_eq "first_boot.installed_$w" "yes" "$([ -f "$root/worlds/$w.json" ] && echo yes || echo no)"
    done

    # A world with nobody to ask: creating a kernel is the operator's act, and the one answer no
    # world can give is what this kernel calls itself on the network.
    out=$(JUICE_BOOTSTRAP_PASSWORD=sys-pass JUICE_HOME="$root" "$JUICE" kernel serve play 2>&1)
    assert_contains "first_boot.unnamed_kernel_refused" "no kernel on play here" "$out"
    assert_contains "first_boot.refusal_says_what_to_write" "--kernel-handle" "$out"
    assert_eq "first_boot.no_home" "no" "$([ -d "$root/kernels/play" ] && echo yes || echo no)"

    # A misspelled key is not a key: the setting the operator meant keeps its default otherwise.
    mkdir -p "$root/kernels/play"
    echo '{"kernle_handle":"acme"}' > "$root/kernels/play/config.json"
    out=$(JUICE_BOOTSTRAP_PASSWORD=sys-pass JUICE_HOME="$root" "$JUICE" kernel serve play --listen-addr 127.0.0.1:0 2>&1)
    assert_contains "first_boot.unknown_key_refused" "kernle_handle" "$out"

    # Named kernel on a named world: it boots, and says which kernel on which network.
    echo '{"kernel_handle":"acme","log_format":"json"}' > "$root/kernels/play/config.json"
    local log="$dir/first.log"
    # Its own federation addresses: this machine may already be running a kernel on the standard
    # port, and a second kernel is refused it rather than given a share of it.
    JUICE_BOOTSTRAP_PASSWORD=sys-pass JUICE_HOME="$root" HOME="$dir" \
        "$JUICE" kernel serve play --listen-addr 127.0.0.1:0 --fed-listen-addrs /ip4/127.0.0.1/tcp/0,/ip4/127.0.0.1/udp/0/quic-v1 >"$log" 2>&1 &
    local pid=$!; track_pid "$pid"
    local i
    for i in $(seq 60); do grep -q '"msg":"server.ready"' "$log" 2>/dev/null && break; sleep 0.1; done
    assert_contains "first_boot.ready_names_the_kernel" '"handle":"acme"' "$(cat "$log")"
    assert_contains "first_boot.ready_names_the_network" '"network":"play"' "$(cat "$log")"
    assert_eq "first_boot.database_created" "yes" "$([ -f "$root/kernels/play/juice.db" ] && echo yes || echo no)"
    # The key it minted is in the file it wrote, which nothing later rewrites.
    assert_contains "first_boot.key_minted" "credentials_key" "$(cat "$root/kernels/play/config.json")"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null

    # Every setting of the file is also an option, and an option wins for that run only: the kernel
    # answers to the name given here, and the file still says what the kernel is.
    local log3="$dir/third.log"
    JUICE_HOME="$root" HOME="$dir" "$JUICE" kernel serve play --listen-addr 127.0.0.1:0 --fed-listen-addrs /ip4/127.0.0.1/tcp/0,/ip4/127.0.0.1/udp/0/quic-v1 \
        --kernel-handle bravo >"$log3" 2>&1 &
    pid=$!; track_pid "$pid"
    for i in $(seq 60); do grep -q '"msg":"server.ready"' "$log3" 2>/dev/null && break; sleep 0.1; done
    assert_contains "first_boot.option_wins_over_the_file" '"handle":"bravo"' "$(cat "$log3")"
    assert_contains "first_boot.option_is_not_written_down" '"kernel_handle": "acme"' "$(cat "$root/kernels/play/config.json")"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null

    # Naming the kernel on the command line is the same consent as writing it in a file: a machine
    # with no terminal creates a kernel with nothing written down first. It serves a world of the
    # operator's own, which is a file and nothing else — no registration, no name in the program.
    printf '{"rail":"manual","decimals":6,"symbol":"MINE","description":"a network of my own","seeds":[]}\n' > "$root/worlds/mine.json"
    local log4="$dir/cli.log"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass JUICE_HOME="$root" HOME="$dir" \
        "$JUICE" kernel serve mine --kernel-handle cli --listen-addr 127.0.0.1:0 \
        --fed-listen-addrs /ip4/127.0.0.1/tcp/0,/ip4/127.0.0.1/udp/0/quic-v1 \
        --log-format json --relay-slots 7 >"$log4" 2>&1 &
    pid=$!; track_pid "$pid"
    for i in $(seq 60); do grep -q '"msg":"server.ready"' "$log4" 2>/dev/null && break; sleep 0.1; done
    assert_contains "first_boot.created_from_the_command_line" '"handle":"cli"' "$(cat "$log4")"
    assert_contains "first_boot.command_line_network" '"network":"mine"' "$(cat "$log4")"
    assert_contains "first_boot.command_line_written_down" '"kernel_handle": "cli"' "$(cat "$root/kernels/mine/config.json")"
    # The connection limits are in the file with the rest: the one it was given, and the default
    # beside it, so a kernel run to carry the network is configured by editing what it can read.
    assert_contains "first_boot.relay_slots_written_down" '"relay_slots": 7' "$(cat "$root/kernels/mine/config.json")"
    assert_contains "first_boot.inbound_peers_default_written_down" '"max_inbound_peers": 64' "$(cat "$root/kernels/mine/config.json")"
    kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null

    # A world this installation does not know is told what it does hold.
    out=$(JUICE_HOME="$root" "$JUICE" kernel serve arbitrum-one 2>&1)
    assert_contains "first_boot.lists_what_exists" "Kernels here: mine, play" "$out"
}

# Money is written one way. What a command takes is what it shows, and an amount is never rendered
# in the base units a person did not type.
flow_money_reads_as_money() {
    echo "=== FLOW money_reads_as_money ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "money.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    deposit "$db" "$hs" alice 500

    # play counts in its own unit and says so, rather than printing a bare number.
    assert_contains "money.ledger_names_the_unit" "$SYMBOL" "$(j "$db" "$ha" user ledger)"
    assert_contains "money.identity_names_the_unit" "$SYMBOL" "$(j "$db" "$hs" admin kernel show)"

    # An irreversible movement is confirmed, and a script that has not said --yes moves nothing.
    assert_fails "money.transfer_needs_yes" "--yes" -- j "$db" "$ha" user transfer sys "$(units 10)"
    assert_jnum "money.nothing_moved" "$(jj "$db" "$ha" user me)" available 500
}

# One output policy, on every command (§14): --json is the reply the server sent, --quiet is the
# ids alone, and the human view writes money the way the command takes it. Each was true of some
# commands and not others, which is what makes a global flag unusable in a script.
flow_one_output_policy() {
    echo "=== FLOW one_output_policy ==="
    local dir db hs ha aid; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "output.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    deposit "$db" "$hs" alice 500
    aid=$(strfield "$(jj "$db" "$ha" action create greet --kind http --source "http://127.0.0.1:1/greet" --price "$(units 5)")" id)
    assert_nonempty "output.created" "$aid"

    # A price given the way this kernel writes money reads back that way wherever a person sees it,
    # while a program still reads the base units it counts in.
    assert_contains "output.detail_shows_the_unit" "$SYMBOL" "$(j "$db" "$ha" action show "$aid")"
    assert_contains "output.list_shows_the_unit" "$SYMBOL" "$(j "$db" "$ha" action list --all)"
    assert_contains "output.profile_shows_the_unit" "$SYMBOL" "$(j "$db" "$ha" user me)"
    assert_jnum "output.json_stays_in_base_units" "$(jj "$db" "$ha" action show "$aid")" price 5

    # --quiet: ids alone, one per line, on a read as on a write — and nothing at all from a command
    # that names no resource.
    assert_eq "output.quiet_detail" "$aid" "$(q "$db" "$ha" action show "$aid")"
    assert_contains "output.quiet_list" "$aid" "$(q "$db" "$ha" action list --all)"
    # A user is named by its handle here, never by a raw account id (D20), so that is what pipes on.
    assert_contains "output.quiet_admin_list" "alice" "$(q "$db" "$hs" admin user list)"
    assert_eq "output.quiet_admin_show" "alice" "$(q "$db" "$hs" admin user show alice)"
    # A verb that changes a roster entry answers with what it changed, so --quiet names it and
    # --json is the same document `show` returns.
    assert_eq "output.quiet_names_what_it_changed" "alice" "$(q "$db" "$hs" admin user suspend alice)"
    assert_json "output.change_answers_with_the_account" "$(jj "$db" "$hs" admin user unsuspend alice)" handle alice
    assert_eq "output.quiet_says_nothing_of_no_resource" "" "$(q "$db" "$ha" process end no-such-process 2>/dev/null)"

    # A client-local list obeys the same rule, though no server is asked.
    assert_contains "output.quiet_names_the_kernels" "$KERNEL_NAME" "$(q "$db" "$ha" kernel list)"
    assert_contains "output.quiet_names_the_logins" "alice@$KERNEL_NAME" "$(q "$db" "$ha" auth list)"

    # A reply of several rows is several resources: `action update` on a path answers with each
    # one, and its price reads the way it was given.
    assert_contains "output.list_reply_shows_the_unit" "$SYMBOL" "$(j "$db" "$ha" action update "$aid" --price "$(units 7)")"
    assert_contains "output.operator_waiting_list_shows_the_unit" "Work delivered" "$(j "$db" "$hs" admin kernel deposits)"

    # The commands that write this client's own records answer the same way as the rest.
    assert_contains "output.quiet_names_the_kernel_added" "$KERNEL_NAME" "$(q "$db" "$ha" kernel add "${SERVER_URL[$db]}" "$KERNEL_NAME")"
    assert_nonempty "output.json_names_the_kernel_added" "$(strfield "$(jj "$db" "$ha" kernel add "${SERVER_URL[$db]}" "$KERNEL_NAME")" outcome)"

    # --json is what the server sent, so a field the CLI does not print is still carried. The
    # network fingerprint is read here before an operator believes any other number.
    assert_nonempty "output.identity_keeps_the_digest" "$(strfield "$(jj "$db" "$hs" admin kernel show)" network_fingerprint)"
    assert_nonempty "output.health_is_the_banner" "$(strfield "$(jj "$db" "$ha" kernel health "$KERNEL_NAME")" public_key)"
}
