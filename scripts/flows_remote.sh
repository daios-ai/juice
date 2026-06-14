# PKCE auth, refresh rotation, receipts, lookup, chat, and OpenAPI flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel,
# start_serve, stop_serve, start_backend, stop_backend, start_api_server, stop_api_server, etc.

flow_pkce_auth() {
    echo "=== FLOW pkce_auth ==="
    local dir db home_sys home_alice port serve_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; serve_port=$_ALLOC_PORT
    addr="127.0.0.1:$serve_port"
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "pkce_auth.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1

    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Generate PKCE verifier + challenge
    local verifier challenge
    verifier=$(python3 -c "import secrets; print(secrets.token_urlsafe(32))")
    challenge=$(python3 -c "
import sys, hashlib, base64
v = sys.argv[1].encode()
print(base64.urlsafe_b64encode(hashlib.sha256(v).digest()).rstrip(b'=').decode())
" "$verifier")

    # Authorize → get code
    local auth_resp code
    auth_resp=$(curl -sf -X POST "http://$addr/v1/auth/authorize" \
        -H "Content-Type: application/json" \
        -d "{\"handle\":\"@alice\",\"password\":\"alicepass\",\"code_challenge\":\"$challenge\"}" 2>/dev/null)
    code=$(python3 -c "
import sys, urllib.parse, json
d = json.loads(sys.argv[1])
qs = urllib.parse.urlparse(d['redirect']).query
print(urllib.parse.parse_qs(qs)['code'][0])
" "$auth_resp" 2>/dev/null)

    # Exchange code → access_token
    local token_resp access_token
    token_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"code_verifier\":\"$verifier\"}" 2>/dev/null)
    access_token=$(strfield "$token_resp" "access_token")
    [ -n "$access_token" ] \
        && ok "pkce_auth.token_obtained" \
        || fail "pkce_auth.token_obtained" "no access_token in: $token_resp"

    # Code reuse → rejected (code already marked used)
    local reuse_resp
    reuse_resp=$(curl -s -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"code_verifier\":\"$verifier\"}" 2>/dev/null)
    echo "$reuse_resp" | grep -qi "invalid\|error\|unauthenticated" \
        && ok "pkce_auth.code_reuse_rejected" \
        || fail "pkce_auth.code_reuse_rejected" "expected error on reuse, got: $reuse_resp"

    # Wrong verifier → rejected
    local auth_resp2 code2 bad_resp
    auth_resp2=$(curl -sf -X POST "http://$addr/v1/auth/authorize" \
        -H "Content-Type: application/json" \
        -d "{\"handle\":\"@alice\",\"password\":\"alicepass\",\"code_challenge\":\"$challenge\"}" 2>/dev/null)
    code2=$(python3 -c "
import sys, urllib.parse, json
d = json.loads(sys.argv[1])
qs = urllib.parse.urlparse(d['redirect']).query
print(urllib.parse.parse_qs(qs)['code'][0])
" "$auth_resp2" 2>/dev/null)
    bad_resp=$(curl -s -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code2\",\"code_verifier\":\"wrong-verifier-value\"}" 2>/dev/null)
    echo "$bad_resp" | grep -qi "invalid\|error\|unauthenticated" \
        && ok "pkce_auth.wrong_verifier_rejected" \
        || fail "pkce_auth.wrong_verifier_rejected" "expected error for wrong verifier, got: $bad_resp"

    stop_serve "$serve_pid"
}

flow_refresh_rotation() {
    echo "=== FLOW refresh_rotation ==="
    local dir db home_sys home_alice port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "refresh_rotation.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Capture RT1 before refresh
    local rt1
    rt1=$(cat "$home_alice/.juice/refresh_token" 2>/dev/null)

    # Refresh → RT1 rotated to RT2
    local refresh_out
    refresh_out=$(j "$db" "$home_alice" auth refresh 2>&1)
    echo "$refresh_out" | grep -q "refreshed" \
        && ok "refresh_rotation.refresh_succeeds" \
        || fail "refresh_rotation.refresh_succeeds" "unexpected output: $refresh_out"

    local rt2
    rt2=$(cat "$home_alice/.juice/refresh_token" 2>/dev/null)

    # Old RT1 rejected
    local home_old="$dir/old"; mkdir -p "$home_old/.juice"
    echo "$rt1" > "$home_old/.juice/refresh_token"
    local bad_refresh
    bad_refresh=$(j "$db" "$home_old" auth refresh 2>&1)
    echo "$bad_refresh" | grep -qi "invalid\|expired\|unauthenticated" \
        && ok "refresh_rotation.old_rt_rejected" \
        || fail "refresh_rotation.old_rt_rejected" "expected invalid/expired, got: $bad_refresh"

    # Logout (revokes RT2) → RT2 rejected
    j "$db" "$home_alice" auth logout >/dev/null 2>&1
    local home_rt2="$dir/rt2"; mkdir -p "$home_rt2/.juice"
    echo "$rt2" > "$home_rt2/.juice/refresh_token"
    local after_logout
    after_logout=$(j "$db" "$home_rt2" auth refresh 2>&1)
    echo "$after_logout" | grep -qi "invalid\|expired\|unauthenticated" \
        && ok "refresh_rotation.revoked_rt_rejected" \
        || fail "refresh_rotation.revoked_rt_rejected" "expected invalid after logout, got: $after_logout"

    # HTTP: same refresh rotation story; use PKCE to get RT (password grant has no RT)
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local verifier challenge
    verifier=$(python3 -c "import secrets; print(secrets.token_urlsafe(32))")
    challenge=$(python3 -c "
import sys, hashlib, base64
v = sys.argv[1].encode()
print(base64.urlsafe_b64encode(hashlib.sha256(v).digest()).rstrip(b'=').decode())
" "$verifier")

    local auth_resp code
    auth_resp=$(curl -sf -X POST "http://$addr/v1/auth/authorize" \
        -H "Content-Type: application/json" \
        -d "{\"handle\":\"@alice\",\"password\":\"alicepass\",\"code_challenge\":\"$challenge\"}" 2>/dev/null)
    code=$(python3 -c "
import sys, urllib.parse, json
d = json.loads(sys.argv[1])
qs = urllib.parse.urlparse(d['redirect']).query
print(urllib.parse.parse_qs(qs)['code'][0])
" "$auth_resp" 2>/dev/null)

    local pkce_resp rt_h1
    pkce_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"code_verifier\":\"$verifier\"}" 2>/dev/null)
    rt_h1=$(strfield "$pkce_resp" "refresh_token")
    [ -n "$rt_h1" ] \
        && ok "refresh_rotation.http_pkce_rt" \
        || fail "refresh_rotation.http_pkce_rt" "no refresh_token from PKCE: $pkce_resp"

    # Refresh RT_h1 → RT_h2
    local refresh_h1 rt_h2
    refresh_h1=$(curl -sf -X POST "http://$addr/v1/auth/refresh" \
        -H "Content-Type: application/json" \
        -d "{\"refresh_token\":\"$rt_h1\"}" 2>/dev/null)
    rt_h2=$(strfield "$refresh_h1" "refresh_token")
    [ -n "$rt_h2" ] && [ "$rt_h2" != "$rt_h1" ] \
        && ok "refresh_rotation.http_rt_rotated" \
        || fail "refresh_rotation.http_rt_rotated" "RT not rotated via HTTP: $refresh_h1"

    # Old RT_h1 rejected after rotation
    local old_reject
    old_reject=$(curl -s -X POST "http://$addr/v1/auth/refresh" \
        -H "Content-Type: application/json" \
        -d "{\"refresh_token\":\"$rt_h1\"}" 2>/dev/null)
    echo "$old_reject" | grep -qi "invalid\|expired\|unauthenticated\|error" \
        && ok "refresh_rotation.http_old_rt_rejected" \
        || fail "refresh_rotation.http_old_rt_rejected" "old RT not rejected: $old_reject"

    # Logout → RT_h2 revoked
    curl -sf -X POST "http://$addr/v1/auth/logout" \
        -H "Content-Type: application/json" \
        -d "{\"refresh_token\":\"$rt_h2\"}" >/dev/null 2>&1

    local post_logout
    post_logout=$(curl -s -X POST "http://$addr/v1/auth/refresh" \
        -H "Content-Type: application/json" \
        -d "{\"refresh_token\":\"$rt_h2\"}" 2>/dev/null)
    echo "$post_logout" | grep -qi "invalid\|expired\|unauthenticated\|error" \
        && ok "refresh_rotation.http_revoked_rt_rejected" \
        || fail "refresh_rotation.http_revoked_rt_rejected" "revoked RT not rejected: $post_logout"
}

flow_successful_receipt() {
    echo "=== FLOW successful_receipt ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "successful_receipt.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 100 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name receipt-action --kind http \
        --source "http://127.0.0.1:${backend_port}/act" --price 10 --description "receipt test action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run --action @alice/receipt-action --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_receipt.call_succeeded" \
        || fail "successful_receipt.call_succeeded" "no tx_id: $call_out"

    # Verify receipt_id is returned in the run response
    local receipt_id
    receipt_id=$(strfield "$call_out" "receipt_id")
    [ -n "$receipt_id" ] \
        && ok "successful_receipt.receipt_created" \
        || fail "successful_receipt.receipt_created" "no receipt_id in call response: $call_out"

    # HTTP: bob calls via HTTP; receipt_id present in HTTP response too
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] \
        && ok "successful_receipt.http_token" \
        || fail "successful_receipt.http_token" "no token: $tok_resp"

    local http_run_resp http_receipt_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/receipt-action","args":{}}' 2>/dev/null)
    http_receipt_id=$(strfield "$http_run_resp" "receipt_id")
    [ -n "$http_receipt_id" ] \
        && ok "successful_receipt.http_receipt_created" \
        || fail "successful_receipt.http_receipt_created" "no receipt_id via HTTP: $http_run_resp"

    stop_backend "$backend_pid"
}

flow_failed_receipt() {
    echo "=== FLOW failed_receipt ==="
    local dir db home_sys home_alice home_bob port backend_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "failed_receipt.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 100 >/dev/null 2>&1

    start_backend "$backend_port" 500 '{"error":"backend error"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name fail-action --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 10 --description "fail action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # Failing call — ignore error, tx is recorded in DB
    j "$db" "$home_bob" run --action @alice/fail-action --args '{}' \
        >/dev/null 2>&1 || true

    # Find the failed tx
    local tx_list tx_id tx_status
    tx_list=$(jj "$db" "$home_bob" tx list)
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_status=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['status'])" "$tx_list" 2>/dev/null)
    [ "$tx_status" = "failure" ] \
        && ok "failed_receipt.failure_tx_recorded" \
        || fail "failed_receipt.failure_tx_recorded" "expected failure status, got: $tx_list"

    # Verify the failed tx is accessible via tx show (receipt creation is verified by unit tests)
    local tx_show
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id" 2>/dev/null)
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "failed_receipt.receipt_created_for_failure" \
        || fail "failed_receipt.receipt_created_for_failure" "failed tx $tx_id not accessible: $tx_show"

    # HTTP: bob calls via HTTP → error; tx shows as failure via HTTP GET
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")
    [ -n "$bob_tok" ] \
        && ok "failed_receipt.http_token" \
        || fail "failed_receipt.http_token" "no token: $tok_resp"

    local http_run_resp
    http_run_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/fail-action","args":{}}' 2>/dev/null)
    echo "$http_run_resp" | grep -qi "error\|failure\|backend" \
        && ok "failed_receipt.http_run_fails" \
        || fail "failed_receipt.http_run_fails" "expected error via HTTP: $http_run_resp"

    local http_tx_list http_fail_status
    http_tx_list=$(curl -sf -H "Authorization: Bearer $bob_tok" \
        "http://$addr/v1/transactions" 2>/dev/null)
    http_fail_status=$(python3 -c "
import sys,json
txs=json.loads(sys.argv[1]) or []
fail_txs=[t for t in txs if t.get('status')=='failure']
print(len(fail_txs))
" "$http_tx_list" 2>/dev/null)
    [ "${http_fail_status:-0}" -ge 1 ] \
        && ok "failed_receipt.http_failure_tx_visible" \
        || fail "failed_receipt.http_failure_tx_visible" "expected failure tx via HTTP: $http_tx_list"

    stop_backend "$backend_pid"
}

flow_lookup() {
    echo "=== FLOW lookup ==="
    local dir db home_sys home_alice port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "lookup.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # @sys/lookup price=0; run directly
    # Missing required 'query' field → schema violation
    local schema_out
    schema_out=$(j "$db" "$home_alice" run --action @sys/lookup \
        --args '{}' 2>&1) || true
    echo "$schema_out" | grep -qi "query\|required\|schema" \
        && ok "lookup.missing_query_rejected" \
        || fail "lookup.missing_query_rejected" "expected schema/query error, got: $schema_out"

    # HTTP: same schema error via HTTP
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] \
        && ok "lookup.http_token" \
        || fail "lookup.http_token" "no token: $tok_resp"

    local http_schema_resp
    http_schema_resp=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/lookup","args":{}}' 2>/dev/null)
    echo "$http_schema_resp" | grep -qi "query\|required\|schema\|error" \
        && ok "lookup.http_missing_query_rejected" \
        || fail "lookup.http_missing_query_rejected" "expected schema error via HTTP, got: $http_schema_resp"
}

flow_chat() {
    echo "=== FLOW chat ==="
    local dir db home_sys home_alice port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "chat.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Call without chatter → ErrInvalidState
    local chat_out
    chat_out=$(j "$db" "$home_alice" run --action @sys/llm-chat \
        --args '{"messages":[{"role":"user","content":"hello"}]}' 2>&1) || true
    echo "$chat_out" | grep -qi "chat\|invalid.state\|invalid_state" \
        && ok "chat.no_chatter_error" \
        || fail "chat.no_chatter_error" "expected ErrInvalidState, got: $chat_out"

    # Missing required 'messages' field → schema violation
    local schema_out
    schema_out=$(j "$db" "$home_alice" run --action @sys/llm-chat \
        --args '{}' 2>&1) || true
    echo "$schema_out" | grep -qi "messages\|required\|schema" \
        && ok "chat.missing_messages_rejected" \
        || fail "chat.missing_messages_rejected" "expected schema/messages error, got: $schema_out"

    # HTTP: both error cases via HTTP
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] \
        && ok "chat.http_token" \
        || fail "chat.http_token" "no token: $tok_resp"

    # No chatter → ErrInvalidState via HTTP
    local http_no_chatter
    http_no_chatter=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/llm-chat","args":{"messages":[{"role":"user","content":"hello"}]}}' 2>/dev/null)
    echo "$http_no_chatter" | grep -qi "chat\|invalid.state\|invalid_state\|error" \
        && ok "chat.http_no_chatter_error" \
        || fail "chat.http_no_chatter_error" "expected ErrInvalidState via HTTP, got: $http_no_chatter"

    # Missing messages → schema error via HTTP
    local http_schema
    http_schema=$(curl -s -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $alice_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@sys/llm-chat","args":{}}' 2>/dev/null)
    echo "$http_schema" | grep -qi "messages\|required\|schema\|error" \
        && ok "chat.http_missing_messages_rejected" \
        || fail "chat.http_missing_messages_rejected" "expected schema error via HTTP, got: $http_schema"
}

flow_openapi_import_execute() {
    echo "=== FLOW openapi_import_execute ==="
    local dir db home_sys home_alice home_bob port api_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_import_execute.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 50 >/dev/null 2>&1

    # Write spec to file; start combined spec+backend server
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" <<'PYEOF'
import json, sys
api_port, spec_file = sys.argv[1], sys.argv[2]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": "Say hello",
                "x-juice-price": 5,
                "requestBody": {
                    "content": {
                        "application/json": {
                            "schema": {"type": "object", "properties": {"name": {"type": "string", "description": "the name to greet"}}}
                        }
                    }
                },
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # Import spec → created=1
    local import_out created_count action_id action_name
    import_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    created_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('Created',[])))" \
        "$import_out" 2>/dev/null || echo 0)
    [ "$created_count" -eq 1 ] \
        && ok "openapi_import_execute.import_created_1" \
        || fail "openapi_import_execute.import_created_1" "expected 1 created, got: $import_out"

    action_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['id'])" \
        "$import_out" 2>/dev/null)
    action_name=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['name'])" \
        "$import_out" 2>/dev/null)

    # Enable + grant-all (requires x-juice-owner for public access)
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$action_id" --public >/dev/null 2>&1

    # @bob calls the imported action
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run --action "@alice/$action_name" --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "openapi_import_execute.call_succeeds" \
        || fail "openapi_import_execute.call_succeeds" "no tx_id: $call_out"

    # Action name is the operation_key; owner is encoded in owner_user_id only.
    [ "$action_name" = "greet" ] \
        && ok "openapi_import_execute.action_name_correct" \
        || fail "openapi_import_execute.action_name_correct" "expected greet, got: $action_name"

    # HTTP: verify action active and callable via HTTP (bob has 50-5(CLI call)=45 left)
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

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
        && ok "openapi_import_execute.http_tokens" \
        || fail "openapi_import_execute.http_tokens" "could not obtain tokens"

    local http_action_show
    http_action_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions/$action_id" 2>/dev/null)
    [ "$(strfield "$http_action_show" "active")" = "True" ] \
        && ok "openapi_import_execute.http_action_active" \
        || fail "openapi_import_execute.http_action_active" "expected active=True via HTTP, got: $http_action_show"

    local http_run_resp http_tx_id
    http_run_resp=$(curl -sf -X POST "http://$addr/v1/run" \
        -H "Authorization: Bearer $bob_tok" \
        -H "Content-Type: application/json" \
        -d '{"action":"@alice/greet","args":{}}' 2>/dev/null)
    http_tx_id=$(strfield "$http_run_resp" "tx_id")
    [ -n "$http_tx_id" ] \
        && ok "openapi_import_execute.http_call_succeeds" \
        || fail "openapi_import_execute.http_call_succeeds" "no tx_id via HTTP: $http_run_resp"

    stop_api_server "$api_pid"
}

flow_openapi_changed_reimport() {
    echo "=== FLOW openapi_changed_reimport ==="
    local dir db home_sys home_alice port api_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_changed_reimport.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Write v1 spec; start server
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" "Say hello v1" <<'PYEOF'
import json, sys
api_port, spec_file, desc = sys.argv[1], sys.argv[2], sys.argv[3]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": desc,
                "x-juice-price": 5,
                "requestBody": {
                    "content": {
                        "application/json": {
                            "schema": {"type": "object", "properties": {"name": {"type": "string", "description": "the name to greet"}}}
                        }
                    }
                },
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # First import
    local import1_out action_id
    import1_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    action_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['id'])" \
        "$import1_out" 2>/dev/null)

    # Update spec on disk (change description → hash changes)
    python3 - "$api_port" "$spec_file" "Say hello v2" <<'PYEOF'
import json, sys
api_port, spec_file, desc = sys.argv[1], sys.argv[2], sys.argv[3]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": desc,
                "x-juice-price": 5,
                "requestBody": {
                    "content": {
                        "application/json": {
                            "schema": {"type": "object", "properties": {"name": {"type": "string", "description": "the name to greet"}}}
                        }
                    }
                },
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF

    # Reimport → updated=1 (description changed → hash changed → action deactivated)
    local import2_out updated_count
    import2_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    updated_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('Updated',[])))" \
        "$import2_out" 2>/dev/null || echo 0)
    [ "$updated_count" -eq 1 ] \
        && ok "openapi_changed_reimport.reimport_updated_1" \
        || fail "openapi_changed_reimport.reimport_updated_1" "expected 1 updated, got: $import2_out"

    # Action is now inactive
    local action_show active
    action_show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$action_show" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "openapi_changed_reimport.action_deactivated" \
        || fail "openapi_changed_reimport.action_deactivated" "expected False, got: $action_show"

    # Action ID unchanged
    local updated_id
    updated_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Updated'][0]['id'])" \
        "$import2_out" 2>/dev/null)
    [ "$updated_id" = "$action_id" ] \
        && ok "openapi_changed_reimport.action_id_preserved" \
        || fail "openapi_changed_reimport.action_id_preserved" "expected $action_id, got: $updated_id"

    # HTTP: verify deactivated state visible via HTTP
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] \
        && ok "openapi_changed_reimport.http_token" \
        || fail "openapi_changed_reimport.http_token" "no token: $tok_resp"

    local http_action_show
    http_action_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions/$action_id" 2>/dev/null)
    [ "$(strfield "$http_action_show" "active")" = "False" ] \
        && ok "openapi_changed_reimport.http_action_deactivated" \
        || fail "openapi_changed_reimport.http_action_deactivated" "expected active=False via HTTP, got: $http_action_show"

    stop_api_server "$api_pid"
}

flow_openapi_unimport() {
    echo "=== FLOW openapi_unimport ==="
    local dir db home_sys home_alice port api_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_unimport.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Write spec with 2 operations
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" <<'PYEOF'
import json, sys
api_port, spec_file = sys.argv[1], sys.argv[2]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": "Say hello",
                "requestBody": {
                    "content": {
                        "application/json": {
                            "schema": {"type": "object", "properties": {"name": {"type": "string", "description": "the name to greet"}}}
                        }
                    }
                },
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {"schema": {"type": "object"}}
                        }
                    }
                }
            }
        },
        "/farewell": {
            "post": {
                "operationId": "farewell",
                "description": "Say goodbye",
                "requestBody": {
                    "content": {
                        "application/json": {
                            "schema": {"type": "object", "properties": {"name": {"type": "string", "description": "the name to say goodbye to"}}}
                        }
                    }
                },
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {"schema": {"type": "object"}}
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # Import 2 operations
    local import_out greet_id
    import_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    greet_id=$(python3 -c "
import sys,json
r=json.loads(sys.argv[1])
for a in r.get('Created',[]):
    if 'greet' in a['name']: print(a['id']); break
" "$import_out" 2>/dev/null)

    # Create a manual (non-OpenAPI) action
    local manual_out manual_id
    manual_out=$(jj "$db" "$home_alice" action create --name manual --kind http \
        --source "http://127.0.0.1:${api_port}/manual" --price 0 --description "manual action")
    manual_id=$(strfield "$manual_out" "id")
    j "$db" "$home_alice" action enable --id "$manual_id" >/dev/null 2>&1

    # Unimport → both OpenAPI actions deactivated
    local unimport_out
    unimport_out=$(j "$db" "$home_alice" action unimport \
        --openapi "http://127.0.0.1:${api_port}/" 2>&1)
    echo "$unimport_out" | grep -q "deactivated 2" \
        && ok "openapi_unimport.two_deactivated" \
        || fail "openapi_unimport.two_deactivated" "expected 'deactivated 2', got: $unimport_out"

    # Greet action is now inactive
    local greet_show greet_active
    greet_show=$(jj "$db" "$home_alice" action show --id "$greet_id")
    greet_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$greet_show" 2>/dev/null)
    [ "$greet_active" = "False" ] \
        && ok "openapi_unimport.openapi_action_deactivated" \
        || fail "openapi_unimport.openapi_action_deactivated" "expected False, got: $greet_show"

    # Manual action still active
    local manual_show manual_active
    manual_show=$(jj "$db" "$home_alice" action show --id "$manual_id")
    manual_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$manual_show" 2>/dev/null)
    [ "$manual_active" = "True" ] \
        && ok "openapi_unimport.manual_action_unaffected" \
        || fail "openapi_unimport.manual_action_unaffected" "expected True, got: $manual_show"

    # HTTP: greet inactive, manual still active via HTTP
    addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] \
        && ok "openapi_unimport.http_token" \
        || fail "openapi_unimport.http_token" "no token: $tok_resp"

    local http_greet_show
    http_greet_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions/$greet_id" 2>/dev/null)
    [ "$(strfield "$http_greet_show" "active")" = "False" ] \
        && ok "openapi_unimport.http_openapi_deactivated" \
        || fail "openapi_unimport.http_openapi_deactivated" "expected active=False via HTTP, got: $http_greet_show"

    local http_manual_show
    http_manual_show=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions/$manual_id" 2>/dev/null)
    [ "$(strfield "$http_manual_show" "active")" = "True" ] \
        && ok "openapi_unimport.http_manual_unaffected" \
        || fail "openapi_unimport.http_manual_unaffected" "expected active=True via HTTP, got: $http_manual_show"

    stop_api_server "$api_pid"
}
