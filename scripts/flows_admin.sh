# Transaction access, admin supervision, make, time, and message flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel,
# start_backend, stop_backend, write_test_config, strfield, numfield, etc.

flow_transaction_access() {
    echo "=== FLOW transaction_access ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "tx_access.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice (seller) creates a paid action (price=10) callable by anyone.
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name pvd-action --kind http \
        --source "http://127.0.0.1:${backend_port}/pvd" --price 10 --description "tx access test")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # @bob (buyer) calls @alice's action 3 times.
    local i call_out a_tx_id
    for i in 1 2 3; do
        call_out=$(jj "$db" "$home_bob" run --action @alice/pvd-action --args '{}')
        a_tx_id=$(strfield "$call_out" "tx_id")
    done

    # @alice (seller) lists transactions for calls to her action — sees all 3.
    local alice_txs alice_count
    alice_txs=$(jj "$db" "$home_alice" tx list)
    alice_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$alice_txs" 2>/dev/null || echo 0)
    [ "$alice_count" -eq 3 ] \
        && ok "tx_access.seller_sees_all" \
        || fail "tx_access.seller_sees_all" "expected 3 txs for seller, got $alice_count: $alice_txs"

    # Sum of transaction net amounts equals @alice's credited balance.
    local alice_out alice_balance net_sum
    alice_out=$(jj "$db" "$home_alice" user me)
    alice_balance=$(numfield "$alice_out" "available")
    net_sum=$(python3 -c "import sys,json; rs=json.loads(sys.argv[1]); print(sum(t['net'] for t in rs))" \
        "$alice_txs" 2>/dev/null || echo -1)
    [ "$net_sum" -eq "$alice_balance" ] \
        && ok "tx_access.reconstructibility" \
        || fail "tx_access.reconstructibility" "tx net sum=$net_sum != alice balance=$alice_balance"

    # @bob (buyer) also sees the same 3 transactions.
    local bob_txs bob_count
    bob_txs=$(jj "$db" "$home_bob" tx list)
    bob_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$bob_txs" 2>/dev/null || echo 0)
    [ "$bob_count" -eq 3 ] \
        && ok "tx_access.buyer_sees_all" \
        || fail "tx_access.buyer_sees_all" "expected 3 txs for buyer, got $bob_count: $bob_txs"

    # @carol is not a party — sees none, and tx show returns not found.
    local carol_txs carol_count carol_show
    carol_txs=$(jj "$db" "$home_carol" tx list)
    carol_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$carol_txs" 2>/dev/null || echo -1)
    [ "$carol_count" -eq 0 ] \
        && ok "tx_access.non_party_sees_none" \
        || fail "tx_access.non_party_sees_none" "expected 0 txs for non-party, got $carol_count: $carol_txs"

    carol_show=$(j "$db" "$home_carol" tx show --id "$a_tx_id" 2>&1)
    echo "$carol_show" | grep -qi "not found" \
        && ok "tx_access.non_party_denied" \
        || fail "tx_access.non_party_denied" "expected not found, got: $carol_show"

    stop_backend "$backend_pid"
}

flow_admin_supervision() {
    echo "=== FLOW admin_supervision ==="
    local dir db home_sys home_alice
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    local port
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "admin.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1

    # Create alice
    local alice_id
    j "$db" "$home_sys" user create \
        --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    alice_id=$(strfield "$(jj "$db" "$home_sys" admin user show --handle @alice)" "id")

    # admin user list shows both
    local user_list
    user_list=$(jj "$db" "$home_sys" admin user list)
    echo "$user_list" | python3 -c "
import sys,json
users = json.load(sys.stdin)
handles = [u.get('handle','') for u in users]
assert '@sys' in handles and '@alice' in handles, f'missing user in {handles}'
" 2>/dev/null \
        && ok "admin.user_list" \
        || fail "admin.user_list" "expected @sys and @alice in list"

    # admin user show --handle @alice
    local show_out
    show_out=$(jj "$db" "$home_sys" admin user show --handle @alice)
    echo "$show_out" | python3 -c "
import sys,json; d=json.load(sys.stdin); assert d.get('handle')=='@alice'
" 2>/dev/null \
        && ok "admin.user_show" \
        || fail "admin.user_show" "expected Handle=@alice, got: $show_out"

    # Suspend alice
    j "$db" "$home_sys" admin user suspend --id "$alice_id" >/dev/null 2>&1
    local me_out
    me_out=$(j "$db" "$home_alice" user me)
    echo "$me_out" | grep -qi "suspended\|unauthenticated\|invalid\|error" \
        && ok "admin.suspend_blocks_alice" \
        || fail "admin.suspend_blocks_alice" "expected auth failure, got: $me_out"

    # Unsuspend alice
    j "$db" "$home_sys" admin user unsuspend --id "$alice_id" >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    local me2_out
    me2_out=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me2_out" "handle")" = "@alice" ] \
        && ok "admin.unsuspend_restores_alice" \
        || fail "admin.unsuspend_restores_alice" "expected alice me, got: $me2_out"

    # Create and enable an action for admin action tests
    local action_id
    local bport; alloc_port; bport=$_ALLOC_PORT
    start_backend "$bport" 200 '{"ok":true}' \
        || { fail "admin.backend" "backend failed to start"; return; }
    local bpid=$BACKEND_PID
    trap "kill '$bpid' 2>/dev/null; wait '$bpid' 2>/dev/null; rm -rf '$dir'" RETURN
    action_id=$(strfield "$(jj "$db" "$home_sys" action create \
        --name test --kind http \
        --source "http://127.0.0.1:$bport" \
        --description "admin test action" --price 0)" "id")
    j "$db" "$home_sys" action enable --id "$action_id" >/dev/null 2>&1

    # admin action list shows the action
    local act_list
    act_list=$(jj "$db" "$home_sys" admin action list)
    echo "$act_list" | python3 -c "
import sys,json; ids=[a['id'] for a in json.load(sys.stdin)]
assert sys.argv[1] in ids
" "$action_id" 2>/dev/null \
        && ok "admin.action_list" \
        || fail "admin.action_list" "action $action_id not in list"

    # admin action disable
    j "$db" "$home_sys" admin action disable --id "$action_id" >/dev/null 2>&1
    local show_action
    show_action=$(jj "$db" "$home_sys" action show --id "$action_id")
    echo "$show_action" | python3 -c "
import sys,json; d=json.load(sys.stdin); assert not d.get('active'), f'still active: {d}'
" 2>/dev/null \
        && ok "admin.action_disable" \
        || fail "admin.action_disable" "action still active after disable: $show_action"

    # Re-enable and call to create a process and tx for admin list tests
    j "$db" "$home_sys" action enable --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_sys" action update --id "$action_id" --public >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys" admin user deposit --handle @alice --amount 100 >/dev/null 2>&1
    local run_out run_tx_id proc_id
    run_out=$(jj "$db" "$home_alice" run --action @sys/test --args '{}')
    run_tx_id=$(strfield "$run_out" "tx_id")
    proc_id=$(strfield "$(jj "$db" "$home_alice" tx show --id "$run_tx_id")" "process_id")

    # admin process list shows the process
    local proc_list
    proc_list=$(jj "$db" "$home_sys" admin process list)
    echo "$proc_list" | python3 -c "
import sys,json; ids=[p['id'] for p in json.load(sys.stdin)]
assert sys.argv[1] in ids
" "$proc_id" 2>/dev/null \
        && ok "admin.process_list" \
        || fail "admin.process_list" "process $proc_id not in list"

    # admin tx list shows transactions
    local tx_list
    tx_list=$(jj "$db" "$home_sys" admin tx list)
    echo "$tx_list" | python3 -c "
import sys,json; txs=json.load(sys.stdin); assert len(txs)>0
" 2>/dev/null \
        && ok "admin.tx_list" \
        || fail "admin.tx_list" "expected at least one tx, got: $tx_list"

    # Non-sys user rejected from admin commands
    local alice_admin_out
    alice_admin_out=$(j "$db" "$home_alice" admin user list 2>&1)
    echo "$alice_admin_out" | grep -qi "unauthorized\|superuser" \
        && ok "admin.non_sys_rejected" \
        || fail "admin.non_sys_rejected" "expected rejection, got: $alice_admin_out"

    stop_backend "$bpid"
}

flow_make() {
    echo "=== FLOW make ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "make.boot" "bootstrap failed"; return; }

    # @sys/make must be registered, active, and public after bootstrap.
    local actions_out make_json
    actions_out=$(jj "$db" "$home_sys" action list 2>/dev/null)
    make_json=$(echo "$actions_out" | python3 -c "
import sys,json
actions = json.load(sys.stdin)
m = next((a for a in actions if a.get('name') == 'make'), None)
print(json.dumps(m) if m else 'null')
" 2>/dev/null)
    [ "$make_json" != "null" ] && [ -n "$make_json" ] \
        && ok "make.registered" \
        || fail "make.registered" "@sys/make not found in action list"

    echo "$make_json" | python3 -c "import sys,json; a=json.load(sys.stdin); assert a.get('active') and a.get('public')" 2>/dev/null \
        && ok "make.active_public" \
        || fail "make.active_public" "@sys/make not active+public: $make_json"

    local price
    price=$(echo "$make_json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('price',0))" 2>/dev/null)
    [ "$price" = "20" ] \
        && ok "make.price_20" \
        || fail "make.price_20" "expected price 20, got: $price"

    # Set up alice with credits.
    j "$db" "$home_sys"   auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 500 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Missing description → schema violation before any execution.
    local no_desc_out
    no_desc_out=$(j "$db" "$home_alice" run --action @sys/make \
        --args '{}' 2>&1) || true
    echo "$no_desc_out" | grep -qi "description\|required\|schema" \
        && ok "make.missing_description_rejected" \
        || fail "make.missing_description_rejected" "expected schema error, got: $no_desc_out"

    # Call with description only (the only accepted input).
    # Succeeds as a kernel call regardless of whether tinygo/Ollama is available.
    # Returns {status: "success"|"failure", diagnostics: [...]} — never a hard kernel error.
    local make_out make_result status make_tx_id proc_id
    make_out=$(j "$db" "$home_alice" run --action @sys/make \
        --args '{"description": "Return a fixed greeting message that says Hello followed by the name"}' 2>&1)
    make_result=$(echo "$make_out" | python3 -c "
import sys, json, re
text = sys.stdin.read()
# Extract JSON object from the result: section
m = re.search(r'result:\n(\{.*\})', text, re.DOTALL)
if m:
    try: print(json.dumps(json.loads(m.group(1))))
    except: print('{}')
else:
    print('{}')
" 2>/dev/null)
    status=$(echo "$make_result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$status" = "success" ] || [ "$status" = "failure" ] \
        && ok "make.returns_structured_result" \
        || fail "make.returns_structured_result" "expected success|failure status, got: $make_out"

    # Extract proc_id from the make tx to filter related transactions.
    make_tx_id=$(echo "$make_out" | python3 -c "
import sys, re
m = re.search(r'^tx_id:\s+(\S+)', sys.stdin.read(), re.MULTILINE)
print(m.group(1) if m else '')
" 2>/dev/null)
    proc_id=$(strfield "$(jj "$db" "$home_alice" tx show --id "$make_tx_id" 2>/dev/null)" "process_id")

    # Transactions must have been recorded for the process (make + any sub-calls).
    local tx_out tx_count
    tx_out=$(jj "$db" "$home_alice" tx list 2>/dev/null)
    tx_count=$(echo "$tx_out" | python3 -c "
import sys,json
txs = json.load(sys.stdin)
print(sum(1 for t in txs if t.get('process_id') == sys.argv[1]))
" "$proc_id" 2>/dev/null)
    [ "${tx_count:-0}" -ge 1 ] \
        && ok "make.transaction_recorded" \
        || fail "make.transaction_recorded" "expected >=1 tx for process, got count=$tx_count"
}

flow_time() {
    echo "=== FLOW time ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "time.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # @sys/time must be registered, active, public, price=0 after bootstrap.
    local actions_out time_json
    actions_out=$(jj "$db" "$home_sys" action list 2>/dev/null)
    time_json=$(echo "$actions_out" | python3 -c "
import sys,json
actions = json.load(sys.stdin)
m = next((a for a in actions if a.get('name') == 'time'), None)
print(json.dumps(m) if m else 'null')
" 2>/dev/null)
    [ "$time_json" != "null" ] && [ -n "$time_json" ] \
        && ok "time.registered" \
        || fail "time.registered" "@sys/time not found in action list"

    echo "$time_json" | python3 -c "import sys,json; a=json.load(sys.stdin); assert a.get('active') and a.get('public') and a.get('price',1)==0" 2>/dev/null \
        && ok "time.active_public_free" \
        || fail "time.active_public_free" "@sys/time not active+public+free: $time_json"

    # Call @sys/time with run (price=0, no funds needed).
    local call_out unix_val iso_val
    call_out=$(j "$db" "$home_alice" run --action @sys/time --args '{}' 2>&1)

    unix_val=$(echo "$call_out" | python3 -c "
import sys, json, re
text = sys.stdin.read()
m = re.search(r'result:\n(\{.*\})', text, re.DOTALL)
if m:
    try: print(json.loads(m.group(1)).get('unix', ''))
    except: print('')
else: print('')
" 2>/dev/null)
    iso_val=$(echo "$call_out" | python3 -c "
import sys, json, re
text = sys.stdin.read()
m = re.search(r'result:\n(\{.*\})', text, re.DOTALL)
if m:
    try: print(json.loads(m.group(1)).get('iso', ''))
    except: print('')
else: print('')
" 2>/dev/null)

    [ -n "$unix_val" ] && [ "$unix_val" -gt 0 ] 2>/dev/null \
        && ok "time.returns_unix" \
        || fail "time.returns_unix" "expected positive unix timestamp, got: $call_out"

    echo "$iso_val" | python3 -c "
import sys
from datetime import datetime
s = sys.stdin.read().strip()
try:
    datetime.fromisoformat(s.replace('Z','+00:00'))
    print('ok')
except Exception as e:
    print('fail: ' + str(e))
" 2>/dev/null | grep -q "^ok$" \
        && ok "time.returns_iso" \
        || fail "time.returns_iso" "expected RFC 3339 iso timestamp, got: $iso_val"
}

flow_message() {
    echo "=== FLOW message ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";   mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";   mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "message.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 500 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice sends @sys/message to @bob (price=0, no funds needed).
    local msg_out step_id
    msg_out=$(j "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@bob","message":"Please review doc"}' 2>&1)

    step_id=$(echo "$msg_out" | python3 -c "
import sys, json, re
text = sys.stdin.read()
m = re.search(r'result:\n(\{.*\})', text, re.DOTALL)
if m:
    try: print(json.loads(m.group(1)).get('step_id', ''))
    except: print('')
else: print('')
" 2>/dev/null)

    [ -n "$step_id" ] \
        && ok "message.step_created" \
        || fail "message.step_created" "expected step_id in result, got: $msg_out"

    # @bob can see and complete the step.
    local bob_steps step_json
    bob_steps=$(jj "$db" "$home_bob" step list 2>/dev/null)
    step_json=$(echo "$bob_steps" | python3 -c "
import sys,json
steps = json.load(sys.stdin)
m = next((s for s in steps if s.get('id') == sys.argv[1]), None)
print(json.dumps(m) if m else 'null')
" "$step_id" 2>/dev/null)
    [ "$step_json" != "null" ] && [ -n "$step_json" ] \
        && ok "message.bob_sees_step" \
        || fail "message.bob_sees_step" "@bob cannot see step $step_id"

    local complete_out
    complete_out=$(j "$db" "$home_bob" step complete \
        --id "$step_id" --args '{}' 2>&1)
    echo "$complete_out" | grep -qi "tx_id\|transaction\|success\|complete" \
        && ok "message.bob_completes_step" \
        || fail "message.bob_completes_step" "@bob failed to complete step: $complete_out"

    # Missing required 'to' → error.
    local bad_out
    bad_out=$(j "$db" "$home_alice" run --action @sys/message \
        --args '{"message":"hi"}' 2>&1) || true
    echo "$bad_out" | grep -qi "to\|required\|invalid" \
        && ok "message.missing_to_rejected" \
        || fail "message.missing_to_rejected" "expected error for missing to, got: $bad_out"

    # Unknown recipient → error.
    local unknown_out
    unknown_out=$(j "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@nobody","message":"hi"}' 2>&1) || true
    echo "$unknown_out" | grep -qi "not found\|invalid\|unknown" \
        && ok "message.unknown_recipient_rejected" \
        || fail "message.unknown_recipient_rejected" "expected error for unknown recipient, got: $unknown_out"
}
