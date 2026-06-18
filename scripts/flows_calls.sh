# Process, ACL, and call semantics flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel, start_backend, etc.

flow_process_lifecycle() {
    echo "=== FLOW process_lifecycle ==="
    local dir db home_sys home_alice port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "process_lifecycle.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin deposit @alice 1000 >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1

    # run @sys/message from @alice to @alice: creates process+step (price=0)
    local msg_out tx_id trace_id
    msg_out=$(jj "$db" "$home_alice" run @sys/message \
        '{"to":"@alice","message":"test lifecycle"}')
    tx_id=$(strfield "$msg_out" "tx_id")
    trace_id=$(strfield "$msg_out" "trace_id")
    local step_id proc_id
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg_out" 2>/dev/null)
    proc_id=$(strfield "$(jj "$db" "$home_alice" tx show "$tx_id")" "process_id")

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
    proc_show=$(jj "$db" "$home_alice" process show "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 0 ] \
        && ok "process_lifecycle.process_available" \
        || fail "process_lifecycle.process_available" "expected 0, got: $proc_show"
    [ "$(strfield "$proc_show" "status")" = "open" ] \
        && ok "process_lifecycle.status_open" \
        || fail "process_lifecycle.status_open" "expected open, got: $proc_show"

    # End process — cancels waiting steps, returns 0 (price was 0)
    j "$db" "$home_alice" process end "$proc_id" >/dev/null 2>&1
    local me_restored
    me_restored=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me_restored" "available")" -eq 1000 ] \
        && ok "process_lifecycle.funds_restored" \
        || fail "process_lifecycle.funds_restored" "expected 1000, got: $me_restored"
    proc_show=$(jj "$db" "$home_alice" process show "$proc_id")
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

    # HTTP surface coverage
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] && ok "process_lifecycle.http_token" \
        || fail "process_lifecycle.http_token" "no token: $tok_resp"

    local run_resp http_tx_id
    run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/message","args":{"to":"@alice","message":"http lifecycle"}}' 2>/dev/null)
    http_tx_id=$(strfield "$run_resp" "tx_id")
    [ -n "$http_tx_id" ] && ok "process_lifecycle.http_run" \
        || fail "process_lifecycle.http_run" "no tx_id: $run_resp"

    local tx_resp http_proc_id
    tx_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/transactions/$http_tx_id" 2>/dev/null)
    http_proc_id=$(strfield "$tx_resp" "process_id")
    [ -n "$http_proc_id" ] && ok "process_lifecycle.http_proc_from_tx" \
        || fail "process_lifecycle.http_proc_from_tx" "no proc_id: $tx_resp"

    local proc_resp
    proc_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/processes/$http_proc_id" 2>/dev/null)
    [ "$(strfield "$proc_resp" "status")" = "open" ] \
        && ok "process_lifecycle.http_status_open" \
        || fail "process_lifecycle.http_status_open" "expected open, got: $proc_resp"

    curl -sf -X POST -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/processes/$http_proc_id/end" >/dev/null 2>&1
    proc_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/processes/$http_proc_id" 2>/dev/null)
    [ "$(strfield "$proc_resp" "status")" = "closed" ] \
        && ok "process_lifecycle.http_status_closed" \
        || fail "process_lifecycle.http_status_closed" "expected closed, got: $proc_resp"

    local steps_resp step_status_http
    steps_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/steps?process_id=$http_proc_id" 2>/dev/null)
    step_status_http=$(python3 -c "
import sys,json
steps=json.loads(sys.argv[1]) or []
print(steps[0]['status'] if steps else '')
" "$steps_resp" 2>/dev/null)
    [ "$step_status_http" = "cancelled" ] \
        && ok "process_lifecycle.http_step_cancelled" \
        || fail "process_lifecycle.http_step_cancelled" "expected cancelled, got: $step_status_http from $steps_resp"
}


flow_acl_public() {
    echo "=== FLOW acl_public ==="
    local dir db home_sys home_alice home_bob port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "acl_public.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1

    # @alice creates and enables an action pointing to unreachable backend (port 1)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create target --kind http \
        --source "http://127.0.0.1:1/target" --price 0 --description "acl test")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1

    # Private action: @bob cannot run @alice's action (checked before process creation)
    local out
    out=$(j "$db" "$home_bob" run @alice/target '{}')
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.private_denied" \
        || fail "acl_public.private_denied" "call on private action succeeded: $out"

    # Non-owner cannot make action public
    out=$(j "$db" "$home_bob" action update "$action_id" --public)
    echo "$out" | grep -qi "unauthorized\|error" \
        && ok "acl_public.update_public_owner_only" \
        || fail "acl_public.update_public_owner_only" "non-owner made action public: $out"

    # Make public: @bob now passes the permission check (fails at backend, not permission)
    j "$db" "$home_alice" action update "$action_id" --public >/dev/null 2>&1
    out=$(j "$db" "$home_bob" run @alice/target '{}')
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.public_passes" \
        || fail "acl_public.public_passes" "public action still denied: $out"

    # Make private: permission check enforced again
    j "$db" "$home_alice" action update "$action_id" --public=false >/dev/null 2>&1
    out=$(j "$db" "$home_bob" run @alice/target '{}')
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.private_enforced" \
        || fail "acl_public.private_enforced" "call after making private was accepted: $out"

    # HTTP surface coverage
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local alice_tok bob_tok tok_resp
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] && [ -n "$bob_tok" ] \
        && ok "acl_public.http_tokens" \
        || fail "acl_public.http_tokens" "could not obtain tokens"

    # Action is currently private — bob run must fail with permission error
    local run_resp
    run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/target","args":{}}' 2>/dev/null)
    echo "$run_resp" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.http_private_denied" \
        || fail "acl_public.http_private_denied" "expected permission error, got: $run_resp"

    # Make public via HTTP (alice)
    curl -sf -X PUT "http://$addr/v1/actions/$action_id" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"public":true}' >/dev/null 2>&1
    # Bob run: passes permission check (backend unreachable → connection error, not permission denied)
    run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/target","args":{}}' 2>/dev/null)
    echo "$run_resp" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.http_public_passes" \
        || fail "acl_public.http_public_passes" "public action still denied via HTTP: $run_resp"

    # Make private via HTTP (alice)
    curl -sf -X PUT "http://$addr/v1/actions/$action_id" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"public":false}' >/dev/null 2>&1
    run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/target","args":{}}' 2>/dev/null)
    echo "$run_resp" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.http_private_enforced" \
        || fail "acl_public.http_private_enforced" "private not enforced via HTTP: $run_resp"
}

flow_successful_paid_call() {
    echo "=== FLOW successful_paid_call ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "successful_paid_call.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin deposit @bob 500 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"result":"ok"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action with grant-all
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create pay --kind http \
        --source "http://127.0.0.1:${backend_port}/pay" --price 100 --description "paid action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update "$action_id" --public >/dev/null 2>&1

    # Get @sys starting balance for fee accounting
    local sys_show sys_start
    sys_show=$(jj "$db" "$home_sys" admin show @sys)
    sys_start=$(numfield "$sys_show" "available")

    # Write config with fee_bps=2000 before starting serve so server reads it
    write_test_config "$db" "fee_bps=2000"

    # Start serve AFTER write_test_config so server picks up fee_bps=2000
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Call with fee_bps=2000 → fee=20, net=80, gross=100
    local call_out tx_id
    call_out=$(HOME="$home_bob" \
        "$JUICE" --db "$db" --json run @alice/pay '{}' 2>/dev/null)
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_paid_call.call_succeeded" \
        || fail "successful_paid_call.call_succeeded" "call returned no tx_id: $call_out"

    # tx fields
    local tx_show
    tx_show=$(jj "$db" "$home_bob" tx show "$tx_id")
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
    sys_end=$(numfield "$(jj "$db" "$home_sys" admin show @sys)" "available")
    [ "$sys_end" -eq $(( sys_start + 20 )) ] \
        && ok "successful_paid_call.fee_credited" \
        || fail "successful_paid_call.fee_credited" "expected +20; start=$sys_start end=$sys_end"

    # HTTP surface: verify CLI-created tx and balances via HTTP GETs (no second run to avoid double-charging)
    local alice_tok bob_tok tok_resp
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && [ -n "$alice_tok" ] \
        && ok "successful_paid_call.http_tokens" \
        || fail "successful_paid_call.http_tokens" "could not obtain tokens"

    local http_tx_show
    http_tx_show=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/transactions/$tx_id" 2>/dev/null)
    [ "$(numfield "$http_tx_show" "gross")" -eq 100 ] \
        && ok "successful_paid_call.http_tx_gross" \
        || fail "successful_paid_call.http_tx_gross" "expected gross=100, got: $http_tx_show"
    [ "$(numfield "$http_tx_show" "fee")" -eq 20 ] \
        && ok "successful_paid_call.http_tx_fee" \
        || fail "successful_paid_call.http_tx_fee" "expected fee=20, got: $http_tx_show"
    [ "$(strfield "$http_tx_show" "status")" = "success" ] \
        && ok "successful_paid_call.http_tx_status" \
        || fail "successful_paid_call.http_tx_status" "expected status=success, got: $http_tx_show"

    local bob_http_me
    bob_http_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$bob_http_me" "available")" -eq 400 ] \
        && ok "successful_paid_call.http_bob_balance" \
        || fail "successful_paid_call.http_bob_balance" "expected 400, got: $bob_http_me"

    local alice_http_me
    alice_http_me=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$alice_http_me" "available")" -eq 80 ] \
        && ok "successful_paid_call.http_alice_balance" \
        || fail "successful_paid_call.http_alice_balance" "expected 80, got: $alice_http_me"

    stop_backend "$backend_pid"
}

flow_failed_call_refund() {
    echo "=== FLOW failed_call_refund ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "failed_call_refund.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin deposit @bob 500 >/dev/null 2>&1

    start_backend "$backend_port" 500 '{"error":"backend error"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create fail --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 100 --description "failing action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update "$action_id" --public >/dev/null 2>&1

    # Start serve alongside backend
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Call — backend returns 500 → execution failure (run exits non-zero)
    j "$db" "$home_bob" run @alice/fail '{}' >/dev/null 2>&1 || true

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
    tx_show=$(jj "$db" "$home_bob" tx show "$tx_id")
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "failed_call_refund.tx_status_failure" \
        || fail "failed_call_refund.tx_status_failure" "expected failure, got: $tx_show"

    # HTTP surface: verify refunded balance and failure tx via HTTP GETs
    local bob_tok tok_resp
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && ok "failed_call_refund.http_token" \
        || fail "failed_call_refund.http_token" "no token: $tok_resp"

    local http_bob_me
    http_bob_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_bob_me" "available")" -eq 500 ] \
        && ok "failed_call_refund.http_bob_refunded" \
        || fail "failed_call_refund.http_bob_refunded" "expected 500, got: $http_bob_me"

    local http_tx_show
    http_tx_show=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/transactions/$tx_id" 2>/dev/null)
    [ "$(strfield "$http_tx_show" "status")" = "failure" ] \
        && ok "failed_call_refund.http_tx_status" \
        || fail "failed_call_refund.http_tx_status" "expected failure, got: $http_tx_show"

    stop_backend "$backend_pid"
}

flow_input_schema_failure() {
    echo "=== FLOW input_schema_failure ==="
    local dir db home_sys home_alice home_bob port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "input_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin deposit @bob 300 >/dev/null 2>&1

    # @alice creates action with input schema requiring field "x" (price=0 avoids balance leak)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create schema-in --kind http \
        --source "http://127.0.0.1:1/schema-in" --price 0 --description "schema test" \
        --input-schema '{"type":"object","properties":{"x":{"type":"string","description":"the x parameter"}},"required":["x"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update "$action_id" --public >/dev/null 2>&1

    # Call without required field "x" → schema error before trace creation
    local call_out
    call_out=$(j "$db" "$home_bob" run @alice/schema-in '{}')
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

    # HTTP surface: schema error via POST /v1/run, balance and tx still unchanged
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local bob_tok tok_resp
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && ok "input_schema_failure.http_token" \
        || fail "input_schema_failure.http_token" "no token: $tok_resp"

    local http_run_resp
    http_run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/schema-in","args":{}}' 2>/dev/null)
    echo "$http_run_resp" | grep -qi "schema\|invalid\|required\|error" \
        && ok "input_schema_failure.http_error_returned" \
        || fail "input_schema_failure.http_error_returned" "expected schema error via HTTP, got: $http_run_resp"

    local http_bob_me
    http_bob_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_bob_me" "available")" -eq 300 ] \
        && ok "input_schema_failure.http_balance_unchanged" \
        || fail "input_schema_failure.http_balance_unchanged" "expected 300, got: $http_bob_me"

    local http_tx_list http_tx_count
    http_tx_list=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/transactions" 2>/dev/null)
    http_tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$http_tx_list" 2>/dev/null || echo 0)
    [ "$http_tx_count" -eq 0 ] \
        && ok "input_schema_failure.http_no_tx" \
        || fail "input_schema_failure.http_no_tx" "expected 0 txs via HTTP, got $http_tx_count"
}

flow_output_schema_failure() {
    echo "=== FLOW output_schema_failure ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "output_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin deposit @bob 300 >/dev/null 2>&1

    # Backend returns {"ok":true} — missing required output field "id"
    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates action with output schema requiring field "id"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create schema-out --kind http \
        --source "http://127.0.0.1:${backend_port}/schema-out" --price 50 --description "schema out test" \
        --output-schema '{"type":"object","properties":{"id":{"type":"string","description":"the record id"}},"required":["id"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update "$action_id" --public >/dev/null 2>&1

    # Start serve alongside backend
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Call — execution runs, output schema check fails → CommitFailedCall
    local call_out
    call_out=$(j "$db" "$home_bob" run @alice/schema-out '{}')
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
    tx_show_os=$(jj "$db" "$home_bob" tx show "$tx_id_os")
    [ "$(strfield "$tx_show_os" "status")" = "failure" ] \
        && ok "output_schema_failure.tx_status_failure" \
        || fail "output_schema_failure.tx_status_failure" "expected failure, got: $tx_show_os"

    # @alice received nothing (refund)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "output_schema_failure.target_not_credited" \
        || fail "output_schema_failure.target_not_credited" "expected 0, got: $alice_me"

    # HTTP surface: do HTTP run (price=50, also refunded → bob balance stays 300)
    local bob_tok tok_resp
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && ok "output_schema_failure.http_token" \
        || fail "output_schema_failure.http_token" "no token: $tok_resp"

    local http_run_resp
    http_run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/schema-out","args":{}}' 2>/dev/null)
    echo "$http_run_resp" | grep -qi "schema\|invalid\|error" \
        && ok "output_schema_failure.http_error_returned" \
        || fail "output_schema_failure.http_error_returned" "expected schema error via HTTP, got: $http_run_resp"

    local http_bob_me
    http_bob_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_bob_me" "available")" -eq 300 ] \
        && ok "output_schema_failure.http_balance_refunded" \
        || fail "output_schema_failure.http_balance_refunded" "expected 300, got: $http_bob_me"

    local http_tx_list http_tx_count
    http_tx_list=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/transactions" 2>/dev/null)
    http_tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$http_tx_list" 2>/dev/null || echo 0)
    [ "$http_tx_count" -ge 2 ] \
        && ok "output_schema_failure.http_txs_recorded" \
        || fail "output_schema_failure.http_txs_recorded" "expected >=2 txs (CLI+HTTP), got $http_tx_count"

    stop_backend "$backend_pid"
}
