# WASM execution, contractor, steps, locked funds recovery, and rating flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel,
# start_backend, make_echo_wasm, make_infinite_loop_wasm, make_contractor_wasm, etc.

flow_wasm_execution() {
    echo "=== FLOW wasm_execution ==="
    local dir db home_sys home_alice home_bob port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "wasm_execution.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 200 >/dev/null 2>&1

    # @alice creates echo WASM action
    local echo_wasm="$dir/echo.wasm"
    make_echo_wasm "$echo_wasm"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name echo --kind wasm \
        --source "$echo_wasm" --price 10 --description "echo wasm")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # ArtifactHash is set after enable (WASM compiled on activation)
    local action_show artifact_hash
    action_show=$(jj "$db" "$home_alice" action show --id "$action_id")
    artifact_hash=$(strfield "$action_show" "artifact_hash")
    [ -n "$artifact_hash" ] \
        && ok "wasm_execution.artifact_hash" \
        || fail "wasm_execution.artifact_hash" "expected non-empty ArtifactHash: $action_show"

    # @bob calls echo WASM
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run --action @alice/echo --args '{"msg":"hello"}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "wasm_execution.echo_call_succeeds" \
        || fail "wasm_execution.echo_call_succeeds" "echo call returned no tx_id: $call_out"

    # Infinite-loop WASM → timeout error
    local loop_wasm="$dir/loop.wasm"
    make_infinite_loop_wasm "$loop_wasm"
    local loop_out loop_id
    loop_out=$(jj "$db" "$home_alice" action create --name loop --kind wasm \
        --source "$loop_wasm" --price 10 --description "infinite loop")
    loop_id=$(strfield "$loop_out" "id")
    j "$db" "$home_alice" action enable   --id "$loop_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$loop_id" --public >/dev/null 2>&1

    local timeout_out
    write_test_config "$db" "script_timeout_ms=200"
    timeout_out=$(HOME="$home_bob" "$JUICE" --db "$db" run --action @alice/loop --args '{}' 2>&1)
    echo "$timeout_out" | grep -qi "timeout\|timed\|execution" \
        && ok "wasm_execution.infinite_loop_timeout" \
        || fail "wasm_execution.infinite_loop_timeout" "expected timeout error, got: $timeout_out"

    # HTTP: echo action callable via HTTP; bob has 200-10(CLI echo)=190 (loop timeout refunded)
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] && [ -n "$bob_tok" ] \
        && ok "wasm_execution.http_tokens" \
        || fail "wasm_execution.http_tokens" "could not obtain tokens"

    local http_action_show
    http_action_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions/$action_id" 2>/dev/null)
    [ -n "$(strfield "$http_action_show" "artifact_hash")" ] \
        && ok "wasm_execution.http_artifact_hash" \
        || fail "wasm_execution.http_artifact_hash" "no artifact_hash via HTTP: $http_action_show"

    local http_run_resp http_tx_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/echo","args":{"msg":"hello"}}' 2>/dev/null)
    http_tx_id=$(strfield "$http_run_resp" "tx_id")
    [ -n "$http_tx_id" ] \
        && ok "wasm_execution.http_echo_succeeds" \
        || fail "wasm_execution.http_echo_succeeds" "echo call via HTTP returned no tx_id: $http_run_resp"

    # bob: 200 - 10(CLI echo) - 10(HTTP echo) = 180; loop timeout was refunded
    local http_bob_me
    http_bob_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_bob_me" "available")" -eq 180 ] \
        && ok "wasm_execution.http_bob_balance" \
        || fail "wasm_execution.http_bob_balance" "expected 180, got: $http_bob_me"
}

flow_contractor_subcall() {
    echo "=== FLOW contractor_subcall ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "contractor_subcall.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    # @carol funds the process — her process pays for the sub-call (price=50)
    j "$db" "$home_sys"   admin user deposit --handle @carol --amount 50 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @bob creates sub-target HTTP action (price=50)
    local sub_out sub_id
    sub_out=$(jj "$db" "$home_bob" action create --name sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "id")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action update --id "$sub_id" --public >/dev/null 2>&1

    # @alice creates WASM (price=50) that sub-calls @bob/sub-target
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action create --name contractor --kind wasm \
        --source "$contractor_wasm" --price 50 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "id")
    j "$db" "$home_alice" action enable   --id "$cont_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$cont_id" --public >/dev/null 2>&1

    # @carol has 50 credits (= contractor price); run funds process with 50, WASM sub-calls @bob
    local call_out tx_id
    call_out=$(jj "$db" "$home_carol" run --action @alice/contractor --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "contractor_subcall.call_succeeds" \
        || fail "contractor_subcall.call_succeeds" "contractor call returned no tx_id: $call_out"

    # @carol.available = 0 (process funded with 50, fully spent on sub-call)
    local carol_me
    carol_me=$(jj "$db" "$home_carol" user me)
    [ "$(numfield "$carol_me" "available")" -eq 0 ] \
        && ok "contractor_subcall.caller_process_unchanged" \
        || fail "contractor_subcall.caller_process_unchanged" "expected 0, got: $carol_me"

    # @alice.available = 0 (not a contractor; her balance is untouched)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "contractor_subcall.alice_debited" \
        || fail "contractor_subcall.alice_debited" "expected 0, got: $alice_me"

    # @bob.available = 50 (received net from sub-call, no fee)
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 50 ] \
        && ok "contractor_subcall.bob_credited" \
        || fail "contractor_subcall.bob_credited" "expected 50, got: $bob_me"

    # HTTP: verify balances via GET /v1/me (backend still running)
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok bob_tok carol_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@carol","password":"carolpass"}' 2>/dev/null)
    carol_tok=$(strfield "$tok_resp" "token")
    [ -n "$carol_tok" ] \
        && ok "contractor_subcall.http_tokens" \
        || fail "contractor_subcall.http_tokens" "could not obtain tokens"

    local http_carol_me http_alice_me http_bob_me
    http_carol_me=$(curl -sf -H "Authorization: Bearer $carol_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_carol_me" "available")" -eq 0 ] \
        && ok "contractor_subcall.http_carol_balance" \
        || fail "contractor_subcall.http_carol_balance" "expected 0, got: $http_carol_me"

    http_alice_me=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_alice_me" "available")" -eq 0 ] \
        && ok "contractor_subcall.http_alice_balance" \
        || fail "contractor_subcall.http_alice_balance" "expected 0, got: $http_alice_me"

    http_bob_me=$(curl -sf -H "Authorization: Bearer $bob_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_bob_me" "available")" -eq 50 ] \
        && ok "contractor_subcall.http_bob_balance" \
        || fail "contractor_subcall.http_bob_balance" "expected 50, got: $http_bob_me"

    stop_backend "$backend_pid"
}

flow_contractor_failure() {
    echo "=== FLOW contractor_failure ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "contractor_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    # @carol gets 30 credits — not enough for the sub-call (price=50) → insufficient funds
    j "$db" "$home_sys" admin user deposit --handle @carol --amount 30 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @bob creates sub-target (price=50)
    local sub_out sub_id
    sub_out=$(jj "$db" "$home_bob" action create --name sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "id")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action update --id "$sub_id" --public >/dev/null 2>&1

    # @alice creates WASM (price=50) that sub-calls @bob/sub-target
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action create --name contractor --kind wasm \
        --source "$contractor_wasm" --price 50 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "id")
    j "$db" "$home_alice" action enable   --id "$cont_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$cont_id" --public >/dev/null 2>&1

    # @carol has 30 < 50 → run fails at balance check before creating a process
    local call_out
    call_out=$(j "$db" "$home_carol" run --action @alice/contractor --args '{}' 2>&1)
    echo "$call_out" | grep -qi "insufficient\|balance\|funds\|credits\|costs" \
        && ok "contractor_failure.error_returned" \
        || fail "contractor_failure.error_returned" "expected insufficient-funds error, got: $call_out"

    # @carol.available = 30 (unchanged; run failed before process creation)
    local carol_me
    carol_me=$(jj "$db" "$home_carol" user me)
    [ "$(numfield "$carol_me" "available")" -eq 30 ] \
        && ok "contractor_failure.caller_process_unchanged" \
        || fail "contractor_failure.caller_process_unchanged" "expected 30, got: $carol_me"

    # @alice.available = 0 (unchanged, no deposit made)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "contractor_failure.alice_unchanged" \
        || fail "contractor_failure.alice_unchanged" "expected 0, got: $alice_me"

    # HTTP: carol run via HTTP → same insufficient funds error; balance unchanged
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp carol_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@carol","password":"carolpass"}' 2>/dev/null)
    carol_tok=$(strfield "$tok_resp" "token")
    [ -n "$carol_tok" ] \
        && ok "contractor_failure.http_token" \
        || fail "contractor_failure.http_token" "no token: $tok_resp"

    local http_run_resp
    http_run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $carol_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/contractor","args":{}}' 2>/dev/null)
    echo "$http_run_resp" | grep -qi "insufficient\|balance\|funds\|credits\|error" \
        && ok "contractor_failure.http_error_returned" \
        || fail "contractor_failure.http_error_returned" "expected insufficient funds via HTTP, got: $http_run_resp"

    local http_carol_me
    http_carol_me=$(curl -sf -H "Authorization: Bearer $carol_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_carol_me" "available")" -eq 30 ] \
        && ok "contractor_failure.http_balance_unchanged" \
        || fail "contractor_failure.http_balance_unchanged" "expected 30, got: $http_carol_me"

    stop_backend "$backend_pid"
}

flow_step_success() {
    echo "=== FLOW step_success ==="
    local dir db home_sys home_alice home_bob port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "step_success.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice sends @sys/message to @bob: creates a process+step (price=0, next_action=@sys/sink)
    local msg_out tx_id step_id proc_id
    msg_out=$(jj "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@bob","message":"Please review doc"}')
    tx_id=$(strfield "$msg_out" "tx_id")
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg_out" 2>/dev/null)
    proc_id=$(strfield "$(jj "$db" "$home_alice" tx show --id "$tx_id")" "process_id")

    [ -n "$step_id" ] \
        && ok "step_success.create_returns_id" \
        || fail "step_success.create_returns_id" "no step_id in: $msg_out"

    # Verify step is waiting via step show
    local step_show step_status
    step_show=$(jj "$db" "$home_alice" step show --id "$step_id")
    step_status=$(strfield "$step_show" "status")
    [ "$step_status" = "waiting" ] \
        && ok "step_success.create_status_waiting" \
        || fail "step_success.create_status_waiting" "expected waiting, got: $step_status"

    # @alice can list the step (process owner visibility).
    local list_out list_count
    list_out=$(jj "$db" "$home_alice" step list)
    list_count=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len([s for s in (d or []) if s.get('id')=='${step_id}']))" \
        "$list_out" 2>/dev/null || echo 0)
    [ "$list_count" -eq 1 ] \
        && ok "step_success.owner_sees_step" \
        || fail "step_success.owner_sees_step" "expected owner to see step, got: $list_out"

    # @bob can list the step (required_caller visibility).
    local bob_list_out bob_list_count
    bob_list_out=$(jj "$db" "$home_bob" step list)
    bob_list_count=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(len([s for s in (d or []) if s.get('id')=='${step_id}']))" \
        "$bob_list_out" 2>/dev/null || echo 0)
    [ "$bob_list_count" -eq 1 ] \
        && ok "step_success.caller_sees_step" \
        || fail "step_success.caller_sees_step" "expected caller to see step, got: $bob_list_out"

    # @bob completes the step (next_action=@sys/sink, accepts any input, price=0).
    local complete_out complete_tx complete_step_id
    complete_out=$(jj "$db" "$home_bob" step complete --id "$step_id" --args '{}')
    complete_tx=$(strfield "$complete_out" "tx_id")
    complete_step_id=$(strfield "$complete_out" "step_id")
    [ -n "$complete_tx" ] \
        && ok "step_success.complete_returns_tx" \
        || fail "step_success.complete_returns_tx" "complete returned no tx_id: $complete_out"
    [ "$complete_step_id" = "$step_id" ] \
        && ok "step_success.complete_returns_step_id" \
        || fail "step_success.complete_returns_step_id" "step_id mismatch: got $complete_step_id"

    # Step status is now done.
    local show_out show_status
    show_out=$(jj "$db" "$home_alice" step show --id "$step_id")
    show_status=$(strfield "$show_out" "status")
    [ "$show_status" = "done" ] \
        && ok "step_success.status_done_after_complete" \
        || fail "step_success.status_done_after_complete" "expected done, got: $show_status"

    # HTTP: full step lifecycle via HTTP (separate message from CLI's)
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] && [ -n "$bob_tok" ] \
        && ok "step_success.http_tokens" \
        || fail "step_success.http_tokens" "could not obtain tokens"

    local http_run_resp http_step_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/message","args":{"to":"@bob","message":"http step test"}}' 2>/dev/null)
    http_step_id=$(python3 -c "
import sys,json
d=json.loads(sys.argv[1])
print(d.get('result',{}).get('step_id',''))
" "$http_run_resp" 2>/dev/null)
    [ -n "$http_step_id" ] \
        && ok "step_success.http_step_created" \
        || fail "step_success.http_step_created" "no step_id via HTTP: $http_run_resp"

    local http_step_show
    http_step_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/steps/$http_step_id" 2>/dev/null)
    [ "$(strfield "$http_step_show" "status")" = "waiting" ] \
        && ok "step_success.http_step_waiting" \
        || fail "step_success.http_step_waiting" "expected waiting, got: $http_step_show"

    local http_steps_resp
    http_steps_resp=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/steps" 2>/dev/null)
    python3 -c "
import sys,json
steps=json.loads(sys.argv[1]) or []
assert any(s.get('id')=='$http_step_id' for s in steps), 'step not found'
" "$http_steps_resp" 2>/dev/null \
        && ok "step_success.http_bob_sees_step" \
        || fail "step_success.http_bob_sees_step" "bob cannot see step via HTTP"

    local http_complete_resp
    http_complete_resp=$(curl -sf -X POST "http://$addr/v1/steps/$http_step_id/complete" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"args":{}}' 2>/dev/null)
    [ -n "$(strfield "$http_complete_resp" "tx_id")" ] \
        && ok "step_success.http_step_completed" \
        || fail "step_success.http_step_completed" "no tx_id from step complete via HTTP: $http_complete_resp"

    http_step_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/steps/$http_step_id" 2>/dev/null)
    [ "$(strfield "$http_step_show" "status")" = "done" ] \
        && ok "step_success.http_step_done" \
        || fail "step_success.http_step_done" "expected done, got: $http_step_show"
}

flow_step_failure() {
    echo "=== FLOW step_failure ==="
    local dir db home_sys home_alice home_bob home_carol port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "step_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1

    # @alice sends @sys/message to @bob; @bob completes it.
    local msg1_out step_id
    msg1_out=$(jj "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@bob","message":"first message"}')
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg1_out" 2>/dev/null)
    jj "$db" "$home_bob" step complete --id "$step_id" --args '{}' >/dev/null 2>&1

    # Completing the step again (status=done) → ErrInvalidState
    local complete2_out
    complete2_out=$(j "$db" "$home_bob" step complete --id "$step_id" --args '{}' 2>&1)
    echo "$complete2_out" | grep -qi "invalid.state\|already.*done\|not.*waiting" \
        && ok "step_failure.double_complete_rejected" \
        || fail "step_failure.double_complete_rejected" "expected invalid state, got: $complete2_out"

    # @alice sends a second @sys/message to @bob; @carol tries to complete → ErrUnauthorized
    local msg2_out step2_id
    msg2_out=$(jj "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@bob","message":"second message"}')
    step2_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg2_out" 2>/dev/null)
    local carol_complete_out
    carol_complete_out=$(j "$db" "$home_carol" step complete --id "$step2_id" --args '{}' 2>&1)
    echo "$carol_complete_out" | grep -qi "unauthorized\|permission\|caller" \
        && ok "step_failure.wrong_caller_rejected" \
        || fail "step_failure.wrong_caller_rejected" "expected unauthorized, got: $carol_complete_out"

    # HTTP: same failure modes via HTTP
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp bob_tok carol_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@carol","password":"carolpass"}' 2>/dev/null)
    carol_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && [ -n "$carol_tok" ] \
        && ok "step_failure.http_tokens" \
        || fail "step_failure.http_tokens" "could not obtain tokens"

    # Double-complete already-done step1 via HTTP
    local http_double_resp
    http_double_resp=$(curl -s -X POST "http://$addr/v1/steps/$step_id/complete" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"args":{}}' 2>/dev/null)
    echo "$http_double_resp" | grep -qi "invalid.state\|already\|not.*waiting\|error" \
        && ok "step_failure.http_double_complete_rejected" \
        || fail "step_failure.http_double_complete_rejected" "expected error, got: $http_double_resp"

    # Wrong caller (carol) tries to complete step2 via HTTP
    local http_wrong_resp
    http_wrong_resp=$(curl -s -X POST "http://$addr/v1/steps/$step2_id/complete" \
        -H "Authorization: Bearer $carol_tok" \
        -H "Content-Type: application/json" \
        -d '{"args":{}}' 2>/dev/null)
    echo "$http_wrong_resp" | grep -qi "unauthorized\|permission\|caller\|error" \
        && ok "step_failure.http_wrong_caller_rejected" \
        || fail "step_failure.http_wrong_caller_rejected" "expected unauthorized, got: $http_wrong_resp"
}

flow_step_restart() {
    echo "=== FLOW step_restart ==="
    local dir db home_sys home_alice home_bob port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "step_restart.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice sends @sys/message to @bob; verify the created step is waiting.
    local msg_out step_id
    msg_out=$(jj "$db" "$home_alice" run --action @sys/message \
        --args '{"to":"@bob","message":"restart test"}')
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" \
        "$msg_out" 2>/dev/null)

    local step_show step_status
    step_show=$(jj "$db" "$home_alice" step show --id "$step_id")
    step_status=$(strfield "$step_show" "status")
    [ "$step_status" = "waiting" ] \
        && ok "step_restart.initial_waiting" \
        || fail "step_restart.initial_waiting" "expected waiting, got: $step_status"

    # Inject running state via direct DB write — simulates a kernel crash after ClaimStep but
    # before CompleteStep; this state cannot be produced through the public API surface.
    python3 - "$db" "$step_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
conn.execute("UPDATE steps SET status='running' WHERE id=?", [sys.argv[2]])
conn.commit()
conn.close()
PYEOF

    # Verify the injected running state is visible.
    local show_running_out running_status
    show_running_out=$(jj "$db" "$home_alice" step show --id "$step_id")
    running_status=$(strfield "$show_running_out" "status")
    [ "$running_status" = "running" ] \
        && ok "step_restart.injected_running" \
        || fail "step_restart.injected_running" "expected running after injection, got: $running_status"

    # bootstrap_kernel on the same DB → ResetRunningSteps → status=waiting
    local port2
    alloc_port; port2=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port2" >/dev/null 2>&1

    # Step must be waiting again and completable by @bob.
    local show_reset_out reset_status
    show_reset_out=$(jj "$db" "$home_alice" step show --id "$step_id")
    reset_status=$(strfield "$show_reset_out" "status")
    [ "$reset_status" = "waiting" ] \
        && ok "step_restart.reset_to_waiting" \
        || fail "step_restart.reset_to_waiting" "expected waiting after bootstrap, got: $reset_status"

    local complete_out complete_tx
    complete_out=$(jj "$db" "$home_bob" step complete --id "$step_id" --args '{}')
    complete_tx=$(strfield "$complete_out" "tx_id")
    [ -n "$complete_tx" ] \
        && ok "step_restart.completable_after_reset" \
        || fail "step_restart.completable_after_reset" "expected tx_id after complete, got: $complete_out"

    # HTTP: new port after second bootstrap; verify original step done, create+complete new step
    local port3
    alloc_port; port3=$_ALLOC_PORT
    addr="127.0.0.1:$port3"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] && [ -n "$bob_tok" ] \
        && ok "step_restart.http_tokens" \
        || fail "step_restart.http_tokens" "could not obtain tokens"

    # Original step (completed by CLI) shows done via HTTP
    local http_done_show
    http_done_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/steps/$step_id" 2>/dev/null)
    [ "$(strfield "$http_done_show" "status")" = "done" ] \
        && ok "step_restart.http_original_step_done" \
        || fail "step_restart.http_original_step_done" "expected done, got: $http_done_show"

    # Create a new message step via HTTP and complete it
    local http_run_resp http_new_step_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/message","args":{"to":"@bob","message":"post-restart http step"}}' 2>/dev/null)
    http_new_step_id=$(python3 -c "
import sys,json
d=json.loads(sys.argv[1])
print(d.get('result',{}).get('step_id',''))
" "$http_run_resp" 2>/dev/null)
    [ -n "$http_new_step_id" ] \
        && ok "step_restart.http_new_step_created" \
        || fail "step_restart.http_new_step_created" "no step_id via HTTP: $http_run_resp"

    local http_new_step
    http_new_step=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/steps/$http_new_step_id" 2>/dev/null)
    [ "$(strfield "$http_new_step" "status")" = "waiting" ] \
        && ok "step_restart.http_new_step_waiting" \
        || fail "step_restart.http_new_step_waiting" "expected waiting, got: $http_new_step"

    local http_complete_resp
    http_complete_resp=$(curl -sf -X POST "http://$addr/v1/steps/$http_new_step_id/complete" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"args":{}}' 2>/dev/null)
    [ -n "$(strfield "$http_complete_resp" "tx_id")" ] \
        && ok "step_restart.http_step_completable" \
        || fail "step_restart.http_step_completable" "no tx_id: $http_complete_resp"
}

flow_locked_funds_recovery() {
    echo "=== FLOW locked_funds_recovery ==="
    local dir db home_sys port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys"; mkdir -p "$home_sys/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "locked_funds.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys" admin user deposit --handle @sys --amount 200 >/dev/null 2>&1

    # Direct DB injection: simulate BeginRootCall for @sys/make (price=20) that crashed
    # before CommitCall/CommitFailedCall. Cannot be produced via the public API surface.
    # - CreateProcess deducted 20 from user.available into user.locked
    # - BeginRootCall moved 20 into process.locked and created a trace with available=20
    # - Kernel crashed before the call settled — leaving an orphan trace (idempotency_key=NULL)
    local proc_id
    proc_id=$(python3 - "$db" <<'PYEOF'
import sqlite3, uuid, sys
db_path = sys.argv[1]
conn = sqlite3.connect(db_path)
owner_id = conn.execute("SELECT id FROM users WHERE handle='@sys' LIMIT 1").fetchone()[0]
action_id = conn.execute("SELECT id FROM actions WHERE name='make' LIMIT 1").fetchone()[0]
proc_id = str(uuid.uuid4())
trace_id = str(uuid.uuid4())
# Simulate CreateProcess (deducted 20 from user) and BeginRootCall (locked 20 in process)
conn.execute("UPDATE users SET available=available-20, locked=locked+20 WHERE id=?", [owner_id])
conn.execute("""
    INSERT INTO processes (id, owner_user_id, available, locked, status, created_at, ended_at)
    VALUES (?, ?, 0, 20, 'open', datetime('now'), NULL)
""", [proc_id, owner_id])
conn.execute("""
    INSERT INTO traces (id, process_id, parent_trace_id, action_owner_id, action_id,
                        caller_user_id, available, locked, idempotency_key,
                        dispatch_json, created_at)
    VALUES (?, ?, NULL, ?, ?, ?, 20, 0, NULL, NULL, datetime('now'))
""", [trace_id, proc_id, owner_id, action_id, owner_id])
conn.commit()
conn.close()
print(proc_id)
PYEOF
)
    [ -n "$proc_id" ] || { fail "locked_funds.start_process" "injection failed"; return; }
    ok "locked_funds.start_process"

    # Verify injected state before restart
    local pre_show
    pre_show=$(jj "$db" "$home_sys" process show --id "$proc_id")
    [ "$(numfield "$pre_show" "locked")" -eq 20 ] \
        && ok "locked_funds.injected" \
        || fail "locked_funds.injected" "expected locked=20: $pre_show"

    # Restart: bootstrap calls Recover() which settles the orphan trace.
    # CommitFailedCall with gross=action.Price=20 and refund=trace.available=20:
    # - process.available += 20 → 20, process.locked -= 20 → 0
    # - closeProcessTx: no open steps, no orphan traces → process closes,
    #   returns available=20 to owner (user.available+=20, user.locked-=20)
    local port2
    alloc_port; port2=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port2" >/dev/null 2>&1

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1

    # Process should be closed with locked=0 after recovery
    local post_show
    post_show=$(jj "$db" "$home_sys" process show --id "$proc_id")
    [ "$(numfield "$post_show" "locked")" -eq 0 ] \
        && ok "locked_funds.locked_cleared" \
        || fail "locked_funds.locked_cleared" "expected locked=0: $post_show"
    [ "$(strfield "$post_show" "status")" = "closed" ] \
        && ok "locked_funds.process_closed" \
        || fail "locked_funds.process_closed" "expected status=closed: $post_show"

    # User balance fully restored to 200 (the 20 was refunded via closeProcessTx)
    local me_out
    me_out=$(jj "$db" "$home_sys" user me)
    [ "$(numfield "$me_out" "available")" -eq 200 ] \
        && ok "locked_funds.user_refunded" \
        || fail "locked_funds.user_refunded" "expected available=200: $me_out"

    # HTTP: verify recovery result via HTTP (new port after second bootstrap)
    local port3
    alloc_port; port3=$_ALLOC_PORT
    addr="127.0.0.1:$port3"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "locked_funds.http_token" \
        || fail "locked_funds.http_token" "no token: $tok_resp"

    local http_me
    http_me=$(curl -sf -H "Authorization: Bearer $sys_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$http_me" "available")" -eq 200 ] \
        && ok "locked_funds.http_balance_restored" \
        || fail "locked_funds.http_balance_restored" "expected 200 via HTTP, got: $http_me"

    local http_proc
    http_proc=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://$addr/v1/processes/$proc_id" 2>/dev/null)
    [ "$(strfield "$http_proc" "status")" = "closed" ] \
        && ok "locked_funds.http_process_closed" \
        || fail "locked_funds.http_process_closed" "expected closed via HTTP, got: $http_proc"
}

flow_rating() {
    echo "=== FLOW rating ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "rating.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 200 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates action (price=10)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name rate-me --kind http \
        --source "http://127.0.0.1:${backend_port}/rate" --price 10 --description "rateable action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Start serve alongside backend
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # @bob calls @alice's action → tx_id
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run --action @alice/rate-me --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")

    # Before rating: detail response has null rating field.
    local show_before
    show_before=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    python3 -c "import sys,json; d=json.loads(sys.argv[1]); assert d.get('rating') is None, d" \
        "$show_before" 2>/dev/null \
        && ok "rating.unrated_null" \
        || fail "rating.unrated_null" "expected null rating before rating, got: $show_before"

    # @bob rates tx with a note → success
    local rate_out
    rate_out=$(j "$db" "$home_bob" tx rate --id "$tx_id" --rating 1 --note "great service" 2>&1)
    echo "$rate_out" | grep -q "rated" \
        && ok "rating.rate_succeeds" \
        || fail "rating.rate_succeeds" "unexpected rate output: $rate_out"

    # stats.rating_count = 1
    local stats_out
    stats_out=$(jj "$db" "$home_bob" action stats --id "$action_id")
    [ "$(numfield "$stats_out" "rating_count")" -eq 1 ] \
        && ok "rating.stats_updated" \
        || fail "rating.stats_updated" "expected rating_count=1, got: $stats_out"

    # Buyer (@bob) sees embedded rating with value and note in detail.
    local show_buyer
    show_buyer=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    python3 -c "
import sys, json
d = json.loads(sys.argv[1])
r = d.get('rating')
assert r is not None, 'rating is null'
assert r['value'] == 1, f'value={r[\"value\"]}'
assert r['note'] == 'great service', f'note={r[\"note\"]}'
" "$show_buyer" 2>/dev/null \
        && ok "rating.buyer_sees_embedded_rating" \
        || fail "rating.buyer_sees_embedded_rating" "buyer detail wrong: $show_buyer"

    # Seller (@alice) also sees embedded rating with note in detail.
    local show_seller
    show_seller=$(jj "$db" "$home_alice" tx show --id "$tx_id")
    python3 -c "
import sys, json
d = json.loads(sys.argv[1])
r = d.get('rating')
assert r is not None, 'rating is null'
assert r['value'] == 1, f'value={r[\"value\"]}'
assert r['note'] == 'great service', f'note={r[\"note\"]}'
" "$show_seller" 2>/dev/null \
        && ok "rating.seller_sees_embedded_rating" \
        || fail "rating.seller_sees_embedded_rating" "seller detail wrong: $show_seller"

    # Embedded rating also appears in the list response.
    local list_out
    list_out=$(jj "$db" "$home_bob" tx list)
    python3 -c "
import sys, json
txs = json.loads(sys.argv[1])
match = next((t for t in txs if t['id'] == sys.argv[2]), None)
assert match is not None, 'tx not in list'
r = match.get('rating')
assert r is not None, 'rating is null in list'
assert r['value'] == 1, f'value={r[\"value\"]}'
" "$list_out" "$tx_id" 2>/dev/null \
        && ok "rating.embedded_in_list" \
        || fail "rating.embedded_in_list" "list view wrong: $list_out"

    # Duplicate rate → ErrInvalidInput
    local rate2_out
    rate2_out=$(j "$db" "$home_bob" tx rate --id "$tx_id" --rating 0 2>&1)
    echo "$rate2_out" | grep -qi "invalid.input\|already.rated\|already" \
        && ok "rating.duplicate_rejected" \
        || fail "rating.duplicate_rejected" "expected already-rated error, got: $rate2_out"

    # @alice (non-buyer) rates → ErrUnauthorized
    local rate3_out
    rate3_out=$(j "$db" "$home_alice" tx rate --id "$tx_id" --rating 1 2>&1)
    echo "$rate3_out" | grep -qi "unauthorized\|buyer\|permission" \
        && ok "rating.non_buyer_rejected" \
        || fail "rating.non_buyer_rejected" "expected unauthorized, got: $rate3_out"

    # HTTP: bob calls action via HTTP, rates the tx, stats reflect both CLI and HTTP calls
    local tok_resp alice_tok bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] && [ -n "$alice_tok" ] \
        && ok "rating.http_tokens" \
        || fail "rating.http_tokens" "could not obtain tokens"

    # bob calls via HTTP (bob: 200-10(CLI)=190, HTTP call costs 10 → 180)
    local http_run_resp http_tx_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/rate-me","args":{}}' 2>/dev/null)
    http_tx_id=$(strfield "$http_run_resp" "tx_id")
    [ -n "$http_tx_id" ] \
        && ok "rating.http_call_succeeds" \
        || fail "rating.http_call_succeeds" "no tx_id via HTTP: $http_run_resp"

    # bob rates the HTTP tx
    local http_rate_resp
    http_rate_resp=$(curl -sf -X POST "http://$addr/v1/transactions/$http_tx_id/rate" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"rating":1,"note":"http rating"}' 2>/dev/null)
    [ -n "$http_rate_resp" ] \
        && ok "rating.http_rate_succeeds" \
        || fail "rating.http_rate_succeeds" "rate via HTTP failed: $http_rate_resp"

    # stats: rating_count >= 2 (CLI call + HTTP call both rated)
    local http_stats
    http_stats=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/stats/$action_id" 2>/dev/null)
    [ "$(numfield "$http_stats" "rating_count")" -ge 2 ] \
        && ok "rating.http_stats_updated" \
        || fail "rating.http_stats_updated" "expected >=2 ratings, got: $http_stats"

    stop_backend "$backend_pid"
}
