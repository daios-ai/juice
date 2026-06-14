# Process, ACL, and call semantics flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel, start_backend, etc.

flow_process_lifecycle() {
    echo "=== FLOW process_lifecycle ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "process_lifecycle.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 1000 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # run @sys/message from @alice to @alice: creates process+step (price=0)
    local msg_out tx_id trace_id
    msg_out=$(jj "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@alice","message":"test lifecycle"}')
    tx_id=$(strfield "$msg_out" "tx_id")
    trace_id=$(strfield "$msg_out" "trace_id")
    local step_id proc_id
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg_out" 2>/dev/null)
    proc_id=$(strfield "$(jj "$db" "$home_alice" tx show --id "$tx_id")" "process_id")

    [ -n "$proc_id" ] \
        && ok "process_lifecycle.started" \
        || fail "process_lifecycle.started" "no proc_id from tx_show"
    [ -n "$trace_id" ] \
        && ok "process_lifecycle.root_trace" \
        || fail "process_lifecycle.root_trace" "no trace_id in: $msg_out"

    # @sys/message price=0: alice.available unchanged (1000)
    local me_after
    me_after=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me_after" "available")" -eq 1000 ] \
        && ok "process_lifecycle.balance_unchanged" \
        || fail "process_lifecycle.balance_unchanged" "expected 1000, got: $me_after"

    # Process fields: funded with 0, status=open while step outstanding
    local proc_show
    proc_show=$(jj "$db" "$home_alice" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 0 ] \
        && ok "process_lifecycle.process_available" \
        || fail "process_lifecycle.process_available" "expected 0, got: $proc_show"
    [ "$(strfield "$proc_show" "status")" = "open" ] \
        && ok "process_lifecycle.status_open" \
        || fail "process_lifecycle.status_open" "expected open, got: $proc_show"

    # End process — cancels waiting steps, returns 0 (price was 0)
    j "$db" "$home_alice" process end --id "$proc_id" >/dev/null 2>&1
    local me_restored
    me_restored=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me_restored" "available")" -eq 1000 ] \
        && ok "process_lifecycle.funds_restored" \
        || fail "process_lifecycle.funds_restored" "expected 1000, got: $me_restored"
    proc_show=$(jj "$db" "$home_alice" process show --id "$proc_id")
    [ "$(strfield "$proc_show" "status")" = "closed" ] \
        && ok "process_lifecycle.status_closed" \
        || fail "process_lifecycle.status_closed" "expected closed, got: $proc_show"

    # Waiting step must now be cancelled.
    if [ -n "$step_id" ]; then
        local step_status
        step_status=$(jj "$db" "$home_alice" step list 2>/dev/null | python3 -c "
import sys,json
steps=json.load(sys.stdin)
m=next((s for s in steps if s.get('id')=='$step_id'),None)
print(m.get('status','') if m else '')
" 2>/dev/null)
        [ "$step_status" = "cancelled" ] \
            && ok "process_lifecycle.step_cancelled" \
            || fail "process_lifecycle.step_cancelled" "expected cancelled, got: $step_status"
    fi
}


flow_acl_public() {
    echo "=== FLOW acl_public ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "acl_public.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice creates and enables an action pointing to unreachable backend (port 1)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name target --kind http \
        --source "http://127.0.0.1:1/target" --price 0 --description "acl test")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable --id "$action_id" >/dev/null 2>&1

    # Private action: @bob cannot run @alice's action (checked before process creation)
    local out
    out=$(j "$db" "$home_bob" run --action @alice/target --args '{}')
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.private_denied" \
        || fail "acl_public.private_denied" "call on private action succeeded: $out"

    # Non-owner cannot make action public
    out=$(j "$db" "$home_bob" action update --id "$action_id" --public)
    echo "$out" | grep -qi "unauthorized\|error" \
        && ok "acl_public.update_public_owner_only" \
        || fail "acl_public.update_public_owner_only" "non-owner made action public: $out"

    # Make public: @bob now passes the permission check (fails at backend, not permission)
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1
    out=$(j "$db" "$home_bob" run --action @alice/target --args '{}')
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.public_passes" \
        || fail "acl_public.public_passes" "public action still denied: $out"

    # Make private: permission check enforced again
    j "$db" "$home_alice" action update --id "$action_id" --public=false >/dev/null 2>&1
    out=$(j "$db" "$home_bob" run --action @alice/target --args '{}')
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.private_enforced" \
        || fail "acl_public.private_enforced" "call after making private was accepted: $out"
}

flow_successful_paid_call() {
    echo "=== FLOW successful_paid_call ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "successful_paid_call.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 500 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"result":"ok"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action with grant-all
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name pay --kind http \
        --source "http://127.0.0.1:${backend_port}/pay" --price 100 --description "paid action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Get @sys starting balance for fee accounting
    local sys_show sys_start
    sys_show=$(jj "$db" "$home_sys" admin user show --handle @sys)
    sys_start=$(numfield "$sys_show" "available")

    # Call with fee_bps=2000 → fee=20, net=80, gross=100
    write_test_config "$db" "fee_bps=2000"
    local call_out tx_id
    call_out=$(HOME="$home_bob" \
        "$JUICE" --db "$db" --output json run --action @alice/pay --args '{}' 2>/dev/null)
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_paid_call.call_succeeded" \
        || fail "successful_paid_call.call_succeeded" "call returned no tx_id: $call_out"

    # tx fields
    local tx_show
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(numfield "$tx_show" "gross")" -eq 100 ] \
        && ok "successful_paid_call.tx_gross" \
        || fail "successful_paid_call.tx_gross" "expected Gross=100, got: $tx_show"
    [ "$(numfield "$tx_show" "fee")" -eq 20 ] \
        && ok "successful_paid_call.tx_fee" \
        || fail "successful_paid_call.tx_fee" "expected Fee=20, got: $tx_show"
    [ "$(numfield "$tx_show" "net")" -eq 80 ] \
        && ok "successful_paid_call.tx_net" \
        || fail "successful_paid_call.tx_net" "expected Net=80, got: $tx_show"
    [ "$(strfield "$tx_show" "status")" = "success" ] \
        && ok "successful_paid_call.tx_status" \
        || fail "successful_paid_call.tx_status" "expected success, got: $tx_show"

    # bob.available = 500 - 100 = 400 (process auto-closed, paid 100 to alice)
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 400 ] \
        && ok "successful_paid_call.bob_debited" \
        || fail "successful_paid_call.bob_debited" "expected 400, got: $bob_me"

    # @alice credited net=80
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 80 ] \
        && ok "successful_paid_call.target_credited" \
        || fail "successful_paid_call.target_credited" "expected 80, got: $alice_me"

    # Fee recipient (@sys) credited fee=20
    local sys_end
    sys_end=$(numfield "$(jj "$db" "$home_sys" admin user show --handle @sys)" "available")
    [ "$sys_end" -eq $(( sys_start + 20 )) ] \
        && ok "successful_paid_call.fee_credited" \
        || fail "successful_paid_call.fee_credited" "expected +20; start=$sys_start end=$sys_end"

    stop_backend "$backend_pid"
}

flow_failed_call_refund() {
    echo "=== FLOW failed_call_refund ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "failed_call_refund.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 500 >/dev/null 2>&1

    start_backend "$backend_port" 500 '{"error":"backend error"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name fail --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 100 --description "failing action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Call — backend returns 500 → execution failure (run exits non-zero)
    j "$db" "$home_bob" run --action @alice/fail --args '{}' >/dev/null 2>&1 || true

    # bob.available unchanged: process funded 100, refunded 100, auto-closed
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 500 ] \
        && ok "failed_call_refund.process_unchanged" \
        || fail "failed_call_refund.process_unchanged" "expected 500, got: $bob_me"

    # @alice received nothing
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "failed_call_refund.target_not_credited" \
        || fail "failed_call_refund.target_not_credited" "expected 0, got: $alice_me"

    # Failure tx IS recorded
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list)
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "failed_call_refund.failure_tx_recorded" \
        || fail "failed_call_refund.failure_tx_recorded" "expected >=1 tx, count=$tx_count list=$tx_list"

    # tx.Status = failure
    local tx_id tx_show
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "failed_call_refund.tx_status_failure" \
        || fail "failed_call_refund.tx_status_failure" "expected failure, got: $tx_show"

    stop_backend "$backend_pid"
}

flow_input_schema_failure() {
    echo "=== FLOW input_schema_failure ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "input_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    # @alice creates action with input schema requiring field "x" (price=0 avoids balance leak)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name schema-in --kind http \
        --source "http://127.0.0.1:1/schema-in" --price 0 --description "schema test" \
        --input-schema '{"type":"object","properties":{"x":{"type":"string","description":"the x parameter"}},"required":["x"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Call without required field "x" → schema error before trace creation
    local call_out
    call_out=$(j "$db" "$home_bob" run --action @alice/schema-in --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|required\|error" \
        && ok "input_schema_failure.error_returned" \
        || fail "input_schema_failure.error_returned" "expected schema error, got: $call_out"

    # bob.available unchanged (price=0, no debit; schema fails before trace creation)
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 300 ] \
        && ok "input_schema_failure.process_unchanged" \
        || fail "input_schema_failure.process_unchanged" "expected 300, got: $bob_me"

    # No tx created (schema fails before trace creation)
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list)
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -eq 0 ] \
        && ok "input_schema_failure.no_tx_created" \
        || fail "input_schema_failure.no_tx_created" "expected 0 txs, got $tx_count: $tx_list"
}

flow_output_schema_failure() {
    echo "=== FLOW output_schema_failure ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "output_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    # Backend returns {"ok":true} — missing required output field "id"
    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates action with output schema requiring field "id"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name schema-out --kind http \
        --source "http://127.0.0.1:${backend_port}/schema-out" --price 50 --description "schema out test" \
        --output-schema '{"type":"object","properties":{"id":{"type":"string","description":"the record id"}},"required":["id"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Call — execution runs, output schema check fails → CommitFailedCall
    local call_out
    call_out=$(j "$db" "$home_bob" run --action @alice/schema-out --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|error" \
        && ok "output_schema_failure.error_returned" \
        || fail "output_schema_failure.error_returned" "expected schema error, got: $call_out"

    # bob.available unchanged (price=50 taken then refunded; process auto-closed)
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 300 ] \
        && ok "output_schema_failure.process_refunded" \
        || fail "output_schema_failure.process_refunded" "expected 300, got: $bob_me"

    # Failure tx IS recorded (unlike input schema failure)
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list)
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "output_schema_failure.failure_tx_recorded" \
        || fail "output_schema_failure.failure_tx_recorded" "expected >=1 tx, count=$tx_count"

    # tx.Status = failure
    local tx_id_os tx_show_os
    tx_id_os=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_show_os=$(jj "$db" "$home_bob" tx show --id "$tx_id_os")
    [ "$(strfield "$tx_show_os" "status")" = "failure" ] \
        && ok "output_schema_failure.tx_status_failure" \
        || fail "output_schema_failure.tx_status_failure" "expected failure, got: $tx_show_os"

    # @alice received nothing (refund)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "output_schema_failure.target_not_credited" \
        || fail "output_schema_failure.target_not_credited" "expected 0, got: $alice_me"

    stop_backend "$backend_pid"
}
