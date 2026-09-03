#!/usr/bin/env bash
# The rail's local-chain release gate (§8). One kernel on a generated world served by a local anvil:
# money arrives as a real token transfer and leaves as one, and every claim about it is checked
# against the chain itself rather than against the kernel's own report.
#
# Opt-in (JUICE_RAIL_FLOWS=1): it needs Foundry and juice-rail's compiled mocks. The default suite
# proves the same money model on the play world, where the operator's records are the facts.

# _chain_world dir prefix — start a chain, deploy juice-rail's mocks on it, and write the world file
# that names them. Sets CHAIN_TOKEN, CHAIN_WORLD and CHAIN_CFG (the config a kernel needs to serve
# it). Both chain flows begin here, so they cannot drift onto different chains.
_chain_world() {
    local dir="$1" pfx="$2"
    start_anvil || { fail "$pfx.anvil" "chain did not start"; return 1; }
    # The venue holds native currency directly, so the wrapped token is only a label in its calldata.
    local weth=0x000000000000000000000000000000000000eEEE router
    CHAIN_TOKEN=$(anvil_deploy MockUSDT0)
    [ -n "$CHAIN_TOKEN" ] || { fail "$pfx.token" "token did not deploy"; return 1; }
    router=$(anvil_deploy MockRouter 10ether \
        "$(cast abi-encode 'f(address,address,uint24,uint256)' "$CHAIN_TOKEN" "$weth" 500 3000000000)")
    [ -n "$router" ] || { fail "$pfx.venue" "venue did not deploy"; return 1; }
    # A world is the juice-rail domain document plus a name: this one names the chain just deployed.
    CHAIN_WORLD="$dir/world.json"
    cat > "$CHAIN_WORLD" <<EOF
{
  "name": "anvil", "chainId": 31337, "token": "$CHAIN_TOKEN", "decimals": 6,
  "finality": "finalized", "fromBlock": 0,
  "venue": {"router": "$router", "quoter": "$router", "weth": "$weth", "feeTier": 500},
  "gas": {"min": "20000000000000000", "max": "50000000000000000", "feeBound": "10000000000000000",
          "slippageBps": 50, "paymentGas": 300000, "swapGas": 1500000}
}
EOF
    CHAIN_CFG=(world="$CHAIN_WORLD" rail_rpc="$ANVIL_RPC" remote_retry_interval_seconds=1)
}

# _chain_pay_in db home wallet_key amount — the logged-in user registers the wallet by signing the
# kernel's registration message with it, then pays amount from it into the kernel's vault; returns
# once the kernel has credited them. Echoes the wallet address.
_chain_pay_in() {
    local db="$1" home="$2" wkey="$3" amount="$4" base waddr kkey uid msg sig vault
    base=$(url "$db"); waddr=$(cast wallet address --private-key "$wkey")
    kkey=$(strfield "$(http_body GET "$base/health")" public_key)
    uid=$(strfield "$(jj "$db" "$home" user me)" id)
    printf -v msg 'juice address registration\nkernel: %s\nuser: %s\naddress: %s' "$kkey" "$uid" "$waddr"
    sig=$(cast wallet sign --private-key "$wkey" "$msg")
    j "$db" "$home" user address "$waddr" --signature "$sig" >/dev/null 2>&1
    vault=$(vault_of "$db")
    anvil_send "$ANVIL_KEY" "$CHAIN_TOKEN" "mint(address,uint256)" "$waddr" "$amount"
    anvil_send "$ANVIL_KEY" "$waddr" --value 1ether
    anvil_send "$wkey" "$CHAIN_TOKEN" "transfer(address,uint256)" "$vault" "$amount"
    anvil_mine 4
    local before deadline=$(( $(date +%s) + 60 ))
    before=$(numfield "$(jj "$db" "$home" user me)" available)
    while [ "$(date +%s)" -lt "$deadline" ]; do
        [ "$(numfield "$(jj "$db" "$home" user me)" available)" != "$before" ] && break
        sleep 0.5
    done
    echo "$waddr"
}

# vault_of db — where a kernel is paid, read from its own banner.
vault_of() { strfield "$(http_body GET "$(url "$1")/health")" rail_address; }

# await_peer_row db home peer want — wait for a peer row's balance to reach want, mining as we go:
# a chain settlement closes only once the payment is final and the worker has seen it.
await_peer_row() {
    local db="$1" home="$2" peer="$3" want="$4" got="" deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        anvil_mine 2
        got=$(numfield "$(jj "$db" "$home" admin show -- "$peer")" available)
        [ "$got" = "$want" ] && break
        sleep 0.5
    done
    echo "$got"
}

flow_rail_chain() {
    echo "=== FLOW rail_chain ==="
    command -v anvil >/dev/null 2>&1 && command -v cast >/dev/null 2>&1 \
        || { fail "rail_chain.foundry" "anvil and cast must be on PATH"; return; }
    rail_contracts >/dev/null \
        || { fail "rail_chain.contracts" "no compiled mocks: run 'forge build' in juice-rail/contracts, or set JUICE_RAIL_CONTRACTS"; return; }

    local dir db hs ha; dir=$(new_dir); db="$dir/kernel/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    _chain_world "$dir" rail_chain || return
    local token="$CHAIN_TOKEN"
    make_admin "$db" "$hs" "${CHAIN_CFG[@]}" || { fail "rail_chain.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    # Money sent to an address nobody proved they hold is gone, so there is nowhere to pay yet.
    assert_fails "rail_chain.withdraw_needs_an_address" "address" -- j "$db" "$ha" user withdraw 1

    # The address becomes hers by signing the kernel's registration message with the wallet itself,
    # and a payment from it into the kernel's own account, once final, is hers too.
    local akey aaddr vault
    akey=0x1111111111111111111111111111111111111111111111111111111111111111
    vault=$(vault_of "$db")
    assert_nonempty "rail_chain.kernel_serves_its_address" "$vault"
    aaddr=$(_chain_pay_in "$db" "$ha" "$akey" 25000000)
    assert_eq "rail_chain.address_registered" "${aaddr,,}" "$(strfield "$(jj "$db" "$ha" user me)" rail_address)"
    assert_jnum "rail_chain.payment_credited" "$(jj "$db" "$ha" user me)" available 25000000
    # Several more passes over the same payment: a fact seen twice is still one payment.
    sleep 3
    assert_jnum "rail_chain.credited_once" "$(jj "$db" "$ha" user me)" available 25000000

    # Money out lands at the registered address. The kernel's account needs native currency of its
    # own before it can sign anything, which is the one thing no ledger can supply.
    anvil_send "$ANVIL_KEY" "$vault" --value 1ether
    local before; before=$(anvil_uint "$token" "balanceOf(address)(uint256)" "$aaddr")
    j "$db" "$ha" user withdraw 5 >/dev/null 2>&1
    local status="" hash="" row
    deadline=$(( $(date +%s) + 90 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        anvil_mine 2
        row=$(python3 -c "
import sys, json
rows = json.loads(sys.argv[1])
print(rows[0]['status'], rows[0].get('tx_hash', '-')) if rows else print('- -')" "$(jj "$db" "$ha" user withdraw)")
        status=${row%% *}; hash=${row##* }
        [ "$status" = confirmed ] && break
        sleep 0.5
    done
    assert_eq "rail_chain.withdrawal_confirmed" confirmed "$status"
    assert_contains "rail_chain.withdrawal_names_its_transaction" "0x" "$hash"
    assert_eq "rail_chain.money_reached_the_address" $((before + 5000000)) \
        "$(anvil_uint "$token" "balanceOf(address)(uint256)" "$aaddr")"

    # The ledger and the chain describe the same money. The custody audit only speaks once the scan
    # has reached the block the holdings were read at, so give the worker a pass after the mining.
    local ident deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        ident=$(jj "$db" "$hs" admin identity)
        [ "$(pathf "$ident" custody.checked)" = True ] && break
        sleep 0.5
    done
    assert_eq "rail_chain.books_balance" 0 "$(pathf "$ident" solvency.gap)"
    assert_eq "rail_chain.custody_checked" True "$(pathf "$ident" custody.checked)"
    assert_eq "rail_chain.custody_within_bounds" True "$(pathf "$ident" custody.ok)"
}

# Two kernels settle a real debt over the chain (§8): the debtor pays from its own account, names
# the payment, and the creditor's worker closes the claim only when that payment — from the debtor's
# proven address, for the claimed amount — has finalized into its vault.
flow_rail_chain_settlement() {
    echo "=== FLOW rail_chain_settlement ==="
    command -v anvil >/dev/null 2>&1 && command -v cast >/dev/null 2>&1 \
        || { fail "rail_chain_settlement.foundry" "anvil and cast must be on PATH"; return; }
    local dir; dir=$(new_dir)
    _chain_world "$dir" rail_chain_settlement || return
    FED_RCFG=("${CHAIN_CFG[@]}" exposure_max=10000000 settlement_trigger=5000000)
    FED_LCFG=("${CHAIN_CFG[@]}")
    _fed_setup "$dir" || { fail "rail_chain_settlement.setup" "setup failed"; return; }
    local lkey="$FED_LKEY"

    # On a chain, credits come only from real payments: L's operator pays in from its own wallet.
    # The kernel's account also needs native currency of its own before it can sign a payment out.
    local lvault rvault; lvault=$(vault_of "$FED_DBL"); rvault=$(vault_of "$FED_DBR")
    assert_nonempty "rail_chain_settlement.vaults" "$lvault$rvault"
    _chain_pay_in "$FED_DBL" "$FED_HL" 0x2222222222222222222222222222222222222222222222222222222222222222 5000000 >/dev/null
    assert_jnum "rail_chain_settlement.debtor_funded" "$(jj "$FED_DBL" "$FED_HL" user me)" available 5000000
    anvil_send "$ANVIL_KEY" "$lvault" --value 1ether

    # L buys from R on credit, and now owes it.
    publish "$FED_DBR" "$FED_HR" paid --kind http --source "http://127.0.0.1:$FED_BPORT" --description paid --price 1000000 >/dev/null
    assert_nonempty "rail_chain_settlement.call_on_credit" "$(strfield "$(jj "$FED_DBL" "$FED_HL" run sys@kernel-r/paid '{}')" tx_id)"
    local d; d=$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)
    assert_eq "rail_chain_settlement.debt" 1050000 "$d"

    # R must know where L pays from before it can believe any payment is L's.
    local seen="" deadline=$(( $(date +%s) + 30 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
        seen=$(strfield "$(jj "$FED_DBR" "$FED_HR" admin show -- "$lkey")" rail_address)
        [ "$seen" = "$lvault" ] && break
        sleep 0.5
    done
    assert_eq "rail_chain_settlement.creditor_knows_debtor_address" "$lvault" "$seen"

    # The debtor pays on the chain and names the payment.
    local before; before=$(anvil_uint "$CHAIN_TOKEN" "balanceOf(address)(uint256)" "$rvault")
    local out; out=$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)
    assert_nonempty "rail_chain_settlement.settlement_id" "$(strfield "$out" settlement_id)"
    assert_eq "rail_chain_settlement.debtor_row_reserved" 0 "$(numfield "$(jj "$FED_DBL" "$FED_HL" admin show kernel-r)" available)"

    # The money lands in R's account on the chain, and R's books close by themselves.
    assert_eq "rail_chain_settlement.creditor_row_closed" 0 "$(await_peer_row "$FED_DBR" "$FED_HR" "$lkey" 0)"
    assert_eq "rail_chain_settlement.money_reached_creditor" $((before + d)) \
        "$(anvil_uint "$CHAIN_TOKEN" "balanceOf(address)(uint256)" "$rvault")"
    assert_jnum "rail_chain_settlement.debtor_books" "$(jj "$FED_DBL" "$FED_HL" admin identity)" gap 0
    assert_jnum "rail_chain_settlement.creditor_books" "$(jj "$FED_DBR" "$FED_HR" admin identity)" gap 0
    assert_json "rail_chain_settlement.nothing_left" "$(jj "$FED_DBL" "$FED_HL" admin settle kernel-r)" status settled
}
