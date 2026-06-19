# Foundation flows: bootstrap, auth, users, deposits, action lifecycle.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel, etc.

flow_bootstrap() {
    echo "=== FLOW bootstrap ==="
    local dir db home_sys port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys"; mkdir -p "$home_sys/.juice"
    alloc_port; port=$_ALLOC_PORT

    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "bootstrap.first_boot" "bootstrap_kernel failed"; return; }
    ok "bootstrap.first_boot"

    # Log in as @sys and verify handle
    j "$db" "$home_sys" auth login @sys --password syspass >/dev/null 2>&1
    local me
    me=$(jj "$db" "$home_sys" user me)
    [ "$(strfield "$me" "handle")" = "@sys" ] \
        && ok "bootstrap.sys_user" \
        || fail "bootstrap.sys_user" "unexpected handle: $me"

    # Signing keypair written to config (action manifest signing requires it)
    local lookup_out
    lookup_out=$(jj "$db" "$home_sys" action list)
    echo "$lookup_out" | python3 -c "
import sys,json
acts = json.load(sys.stdin)
names = [a['name'] for a in acts]
assert 'lookup' in names, f'lookup missing; got {names}'
assert 'llm/chat' in names, f'llm/chat missing; got {names}'
" 2>/dev/null \
        && ok "bootstrap.native_actions_registered" \
        || fail "bootstrap.native_actions_registered" "lookup or llm-chat not in action list"

    # Second boot is idempotent — starts cleanly without re-creating @sys
    local port2
    alloc_port; port2=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port2" \
        || { fail "bootstrap.idempotent" "second boot failed"; return; }
    ok "bootstrap.idempotent"

    # @sys still works after second boot
    local me2
    me2=$(jj "$db" "$home_sys" user me)
    [ "$(strfield "$me2" "handle")" = "@sys" ] \
        && ok "bootstrap.state_preserved" \
        || fail "bootstrap.state_preserved" "state lost after second boot: $me2"

    # HTTP surface.
    local addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "bootstrap.http_token" \
        || fail "bootstrap.http_token" "no token in: $tok_resp"

    local acts_resp
    acts_resp=$(curl -sf "http://$addr/v1/actions" 2>/dev/null)
    echo "$acts_resp" | python3 -c "
import sys,json
acts = json.load(sys.stdin)
names = [a['name'] for a in acts]
assert 'lookup' in names, names
assert 'llm/chat' in names, names
" 2>/dev/null \
        && ok "bootstrap.http_native_actions" \
        || fail "bootstrap.http_native_actions" "native actions missing: $acts_resp"

    local me_resp
    me_resp=$(curl -sf -H "Authorization: Bearer $sys_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(strfield "$me_resp" "handle")" = "@sys" ] \
        && ok "bootstrap.http_me" \
        || fail "bootstrap.http_me" "GET /v1/me returned: $me_resp"
}

flow_local_auth() {
    echo "=== FLOW local_auth ==="
    local dir db home_sys port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys"; mkdir -p "$home_sys/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "local_auth.boot" "bootstrap failed"; return; }

    # Login stores tokens
    j "$db" "$home_sys" auth login @sys --password syspass >/dev/null 2>&1
    local tdir; tdir=$(juice_token_dir "$home_sys" "$db")
    [ -f "$tdir/token" ] \
        && ok "local_auth.token_stored" \
        || fail "local_auth.token_stored" "token file missing"

    # Authenticated command works
    local me
    me=$(jj "$db" "$home_sys" user me)
    [ "$(strfield "$me" "handle")" = "@sys" ] \
        && ok "local_auth.me_succeeds" \
        || fail "local_auth.me_succeeds" "user me failed: $me"

    # Save old refresh token before rotation
    local old_rt=""
    [ -f "$tdir/refresh_token" ] && old_rt=$(cat "$tdir/refresh_token")

    # Refresh rotates both tokens
    j "$db" "$home_sys" auth refresh >/dev/null 2>&1
    local new_rt=""
    [ -f "$tdir/refresh_token" ] && new_rt=$(cat "$tdir/refresh_token")
    [ -n "$new_rt" ] && [ "$new_rt" != "$old_rt" ] \
        && ok "local_auth.refresh_rotates_token" \
        || fail "local_auth.refresh_rotates_token" "refresh token not rotated"

    # Authenticated command still works after refresh
    me=$(jj "$db" "$home_sys" user me)
    [ "$(strfield "$me" "handle")" = "@sys" ] \
        && ok "local_auth.me_after_refresh" \
        || fail "local_auth.me_after_refresh" "user me failed after refresh"

    # Restoring the old refresh token and refreshing again must fail
    if [ -n "$old_rt" ]; then
        mkdir -p "$tdir"
        echo "$old_rt" > "$tdir/refresh_token"
        local reuse_out
        reuse_out=$(j "$db" "$home_sys" auth refresh 2>&1)
        echo "$reuse_out" | grep -qi "expired\|invalid\|error\|failed" \
            && ok "local_auth.refresh_token_rotation_enforced" \
            || fail "local_auth.refresh_token_rotation_enforced" "reused refresh token was accepted"
        # Restore new token so logout works
        echo "$new_rt" > "$tdir/refresh_token"
    fi

    # Logout revokes and removes tokens
    j "$db" "$home_sys" auth logout >/dev/null 2>&1
    [ ! -f "$tdir/token" ] \
        && ok "local_auth.logout_removes_token" \
        || fail "local_auth.logout_removes_token" "token file still present after logout"

    # Commands after logout fail — use j so the error message reaches stdout
    local post_logout
    post_logout=$(j "$db" "$home_sys" user me)
    echo "$post_logout" | grep -qi "login\|not logged in\|error" \
        && ok "local_auth.post_logout_rejected" \
        || fail "local_auth.post_logout_rejected" "post-logout command succeeded unexpectedly"

    # HTTP surface.
    local addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp http_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    http_tok=$(strfield "$tok_resp" "token")
    [ -n "$http_tok" ] \
        && ok "local_auth.http_token" \
        || fail "local_auth.http_token" "no token in: $tok_resp"

    local me_resp
    me_resp=$(curl -sf -H "Authorization: Bearer $http_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(strfield "$me_resp" "handle")" = "@sys" ] \
        && ok "local_auth.http_me" \
        || fail "local_auth.http_me" "GET /v1/me returned: $me_resp"

    local bad_resp
    bad_resp=$(curl -s -H "Authorization: Bearer bad.token.here" "http://$addr/v1/me" 2>/dev/null)
    echo "$bad_resp" | grep -qi "unauthenticated\|unauthorized\|error" \
        && ok "local_auth.http_invalid_rejected" \
        || fail "local_auth.http_invalid_rejected" "bad token accepted: $bad_resp"
}

flow_suspension() {
    echo "=== FLOW suspension ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";   mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "suspension.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys" auth login @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys" user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1

    # Start HTTP server so we can mirror CLI assertions via curl.
    local addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Get alice's HTTP token.
    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")

    # @alice works before suspension (CLI)
    local me
    me=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me" "handle")" = "@alice" ] \
        && ok "suspension.alice_active" \
        || fail "suspension.alice_active" "alice me failed: $me"

    # @alice works before suspension (HTTP)
    local me_resp
    me_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(strfield "$me_resp" "handle")" = "@alice" ] \
        && ok "suspension.http_alice_active" \
        || fail "suspension.http_alice_active" "GET /v1/me before suspend: $me_resp"

    # @sys suspends @alice by ID
    local alice_id
    alice_id=$(strfield "$me" "id")
    j "$db" "$home_sys" admin suspend @alice >/dev/null 2>&1

    # @alice's existing token is now rejected — use j so the error message is in stdout
    local suspended_out
    suspended_out=$(j "$db" "$home_alice" user me)
    echo "$suspended_out" | grep -qi "suspended\|unauthenticated\|error" \
        && ok "suspension.suspended_token_rejected" \
        || fail "suspension.suspended_token_rejected" "suspended user was not rejected: $suspended_out"

    # HTTP: alice's token rejected after suspension.
    local susp_resp
    susp_resp=$(curl -s -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    echo "$susp_resp" | grep -qi "suspended\|unauthenticated\|error" \
        && ok "suspension.http_suspended_rejected" \
        || fail "suspension.http_suspended_rejected" "suspended token accepted via HTTP: $susp_resp"

    # Data preserved — @sys can still see @alice
    local alice_show
    alice_show=$(jj "$db" "$home_sys" admin show @alice)
    [ "$(strfield "$alice_show" "handle")" = "@alice" ] \
        && ok "suspension.data_preserved" \
        || fail "suspension.data_preserved" "admin show failed: $alice_show"

    # @sys unsuspends @alice
    j "$db" "$home_sys" admin unsuspend @alice >/dev/null 2>&1

    # @alice's existing token works again (no re-login required) — CLI
    me=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me" "handle")" = "@alice" ] \
        && ok "suspension.unsuspend_restores_access" \
        || fail "suspension.unsuspend_restores_access" "alice still rejected after unsuspend: $me"

    # HTTP: token works again after unsuspend.
    me_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(strfield "$me_resp" "handle")" = "@alice" ] \
        && ok "suspension.http_unsuspend_restores_access" \
        || fail "suspension.http_unsuspend_restores_access" "alice still rejected via HTTP after unsuspend: $me_resp"
}

flow_deposits() {
    echo "=== FLOW deposits ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";   mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";   mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "deposits.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys" auth login @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys" user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys" user create @bob bob@test.com --password bobpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1

    # Start HTTP server to mirror CLI balance checks via curl.
    local addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")

    # Initial balance is zero (CLI)
    local me
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 0 ] \
        && ok "deposits.initial_zero" \
        || fail "deposits.initial_zero" "initial balance not zero: $me"

    # Initial balance is zero (HTTP)
    local me_resp
    me_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$me_resp" "available")" -eq 0 ] \
        && ok "deposits.http_initial_zero" \
        || fail "deposits.http_initial_zero" "HTTP initial balance not zero: $me_resp"

    # @sys deposits 500 to @alice
    j "$db" "$home_sys" admin deposit @alice 500 >/dev/null 2>&1
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 500 ] \
        && ok "deposits.balance_updated" \
        || fail "deposits.balance_updated" "expected 500, got: $me"

    # HTTP reflects the deposit immediately.
    me_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$me_resp" "available")" -eq 500 ] \
        && ok "deposits.http_balance_updated" \
        || fail "deposits.http_balance_updated" "HTTP expected 500, got: $me_resp"

    # Second deposit accumulates
    j "$db" "$home_sys" admin deposit @alice 200 >/dev/null 2>&1
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 700 ] \
        && ok "deposits.accumulates" \
        || fail "deposits.accumulates" "expected 700, got: $me"

    # HTTP reflects accumulated balance.
    me_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/me" 2>/dev/null)
    [ "$(numfield "$me_resp" "available")" -eq 700 ] \
        && ok "deposits.http_accumulates" \
        || fail "deposits.http_accumulates" "HTTP expected 700, got: $me_resp"

    # Non-sys user cannot deposit
    local bob_deposit
    bob_deposit=$(j "$db" "$home_bob" admin deposit @alice 10 2>&1)
    echo "$bob_deposit" | grep -qi "unauthorized\|superuser\|error" \
        && ok "deposits.non_sys_rejected" \
        || fail "deposits.non_sys_rejected" "non-sys deposit was accepted: $bob_deposit"

    # @bob's balance unaffected
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 0 ] \
        && ok "deposits.other_user_unaffected" \
        || fail "deposits.other_user_unaffected" "bob balance changed: $bob_me"
}

flow_action_lifecycle() {
    echo "=== FLOW action_lifecycle ==="
    local dir db home_sys home_alice port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";   mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    local home_bob="$dir/bob"; mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "action_lifecycle.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create @bob bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login @bob   --password bobpass   >/dev/null 2>&1

    # Create action — inactive by default
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create greet --kind http \
        --source "http://127.0.0.1:1/greet" --description "hello world" --price 5)
    action_id=$(strfield "$create_out" "id")
    [ -n "$action_id" ] \
        && ok "action_lifecycle.created" \
        || fail "action_lifecycle.created" "action create returned no ID; output: $create_out"

    # Active is a Go bool — serialises as JSON false (Python False)
    local active
    active=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d['active'])" \
        "$create_out" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "action_lifecycle.inactive_by_default" \
        || fail "action_lifecycle.inactive_by_default" "expected False, got Active=$active"

    # Enable → active
    j "$db" "$home_alice" action enable "$action_id" >/dev/null 2>&1
    local show
    show=$(jj "$db" "$home_alice" action show "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$show" 2>/dev/null)
    [ "$active" = "True" ] \
        && ok "action_lifecycle.enabled" \
        || fail "action_lifecycle.enabled" "expected True, got $active; show: $show"

    # Disable → inactive
    j "$db" "$home_alice" action disable "$action_id" >/dev/null 2>&1
    show=$(jj "$db" "$home_alice" action show "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$show" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "action_lifecycle.disabled" \
        || fail "action_lifecycle.disabled" "expected False, got $active; show: $show"

    # Update description — persisted
    j "$db" "$home_alice" action update "$action_id" --description "updated desc" >/dev/null 2>&1
    show=$(jj "$db" "$home_alice" action show "$action_id")
    echo "$show" | grep -q "updated desc" \
        && ok "action_lifecycle.update_persisted" \
        || fail "action_lifecycle.update_persisted" "description not updated; show: $show"

    # Delete — use j (text mode) so the error message is in stdout for grep
    j "$db" "$home_alice" action delete "$action_id" >/dev/null 2>&1
    local show_deleted
    show_deleted=$(j "$db" "$home_alice" action show "$action_id")
    echo "$show_deleted" | grep -qi "not found\|error" \
        && ok "action_lifecycle.deleted" \
        || fail "action_lifecycle.deleted" "action still visible after delete: $show_deleted"

    # Non-owner (regular user @bob) cannot delete @alice's action
    create_out=$(jj "$db" "$home_alice" action create hello --kind http \
        --source "http://127.0.0.1:1/hello")
    local alice_action_id
    alice_action_id=$(strfield "$create_out" "id")
    local bob_delete
    bob_delete=$(j "$db" "$home_bob" action delete "$alice_action_id")
    echo "$bob_delete" | grep -qi "unauthorized\|not found\|error" \
        && ok "action_lifecycle.owner_enforced" \
        || fail "action_lifecycle.owner_enforced" "non-owner delete succeeded: $bob_delete"

    # Name reuse: after deleting an action, the same name can be registered again
    j "$db" "$home_alice" action delete "$alice_action_id" >/dev/null 2>&1
    local reuse_out
    reuse_out=$(jj "$db" "$home_alice" action create hello --kind http \
        --source "http://127.0.0.1:1/hello2" --description "reused name")
    local reuse_id
    reuse_id=$(strfield "$reuse_out" "id")
    [ -n "$reuse_id" ] \
        && ok "action_lifecycle.name_reuse_after_delete" \
        || fail "action_lifecycle.name_reuse_after_delete" "name reuse failed: $reuse_out"

    # action_name is captured in transactions and survives action deletion
    start_backend "$backend_port" 200 '{"answer":42}'
    local backend_pid=$BACKEND_PID
    local addr="127.0.0.1:$port"
    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local tx_action_id
    create_out=$(jj "$db" "$home_alice" action create callable \
        --kind http --source "http://127.0.0.1:${backend_port}/call" \
        --description "for tx test" --price 0)
    tx_action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable "$tx_action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update "$tx_action_id" --public >/dev/null 2>&1

    j "$db" "$home_bob"   auth login @bob --password bobpass >/dev/null 2>&1
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run "@alice/callable" '{}')
    tx_id=$(strfield "$call_out" "tx_id")

    # Delete the action — transaction must still carry the name
    j "$db" "$home_alice" action delete "$tx_action_id" >/dev/null 2>&1
    local tx_show action_name_in_tx
    tx_show=$(jj "$db" "$home_bob" tx show "$tx_id")
    action_name_in_tx=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('action_name',''))" "$tx_show" 2>/dev/null)
    [ "$action_name_in_tx" = "callable" ] \
        && ok "action_lifecycle.action_name_in_tx_after_delete" \
        || fail "action_lifecycle.action_name_in_tx_after_delete" "action_name='$action_name_in_tx', want 'callable'; tx: $tx_show"

    # HTTP surface: full action CRUD via REST API.
    local tok_resp alice_tok bob_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@bob","password":"bobpass"}' 2>/dev/null)
    bob_tok=$(strfield "$tok_resp" "token")

    # POST /v1/actions — create inactive action.
    local cr_resp http_id
    cr_resp=$(curl -sf -X POST "http://$addr/v1/actions" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $alice_tok" \
        -d '{"name":"http-crud","kind":"http","source":"http://127.0.0.1:1/x","price":0,"description":"http test","input_schema":{"type":"object"},"output_schema":{"type":"object"}}' 2>/dev/null)
    http_id=$(strfield "$cr_resp" "id")
    [ -n "$http_id" ] \
        && ok "action_lifecycle.http_create" \
        || fail "action_lifecycle.http_create" "POST /v1/actions returned: $cr_resp"

    # GET /v1/actions/{id} — inactive by default.
    local show_resp http_active
    show_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id" 2>/dev/null)
    http_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('active',True))" "$show_resp" 2>/dev/null)
    [ "$http_active" = "False" ] \
        && ok "action_lifecycle.http_inactive_by_default" \
        || fail "action_lifecycle.http_inactive_by_default" "expected False, got: $show_resp"

    # POST /v1/actions/{id}/enable.
    curl -sf -X POST -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id/enable" >/dev/null 2>&1
    show_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id" 2>/dev/null)
    http_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('active',False))" "$show_resp" 2>/dev/null)
    [ "$http_active" = "True" ] \
        && ok "action_lifecycle.http_enabled" \
        || fail "action_lifecycle.http_enabled" "expected True after enable: $show_resp"

    # PUT /v1/actions/{id} — update description.
    curl -sf -X PUT "http://$addr/v1/actions/$http_id" \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $alice_tok" \
        -d '{"description":"http updated"}' >/dev/null 2>&1
    show_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id" 2>/dev/null)
    echo "$show_resp" | grep -q "http updated" \
        && ok "action_lifecycle.http_update_persisted" \
        || fail "action_lifecycle.http_update_persisted" "description not updated: $show_resp"

    # Non-owner cannot delete via HTTP.
    local del_resp
    del_resp=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE \
        -H "Authorization: Bearer $bob_tok" "http://$addr/v1/actions/$http_id" 2>/dev/null)
    [ "$del_resp" != "200" ] \
        && ok "action_lifecycle.http_owner_enforced" \
        || fail "action_lifecycle.http_owner_enforced" "non-owner delete returned $del_resp"

    # DELETE /v1/actions/{id} — owner can delete.
    curl -sf -X DELETE -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id" >/dev/null 2>&1
    local gone_code
    gone_code=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer $alice_tok" "http://$addr/v1/actions/$http_id" 2>/dev/null)
    [ "$gone_code" = "404" ] \
        && ok "action_lifecycle.http_deleted" \
        || fail "action_lifecycle.http_deleted" "expected 404 after delete, got $gone_code"
}

flow_action_owner_visibility() {
    echo "=== FLOW action_owner_visibility ==="
    local dir db home_sys home_alice port addr
    dir=$(mktemp -d)
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    addr="127.0.0.1:$port"

    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "action_owner_visibility.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create @alice alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login @alice --password alicepass >/dev/null 2>&1

    # Create an inactive (private) action — no --enable or --public flags.
    j "$db" "$home_alice" action create secret-op --kind http \
        --source "http://127.0.0.1:1/secret" --description "private" >/dev/null 2>&1

    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Unauthenticated: owner's inactive/private action must not appear.
    local unauth_resp unauth_count
    unauth_resp=$(curl -sf "http://$addr/v1/actions?owner=@alice" 2>/dev/null)
    unauth_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$unauth_resp" 2>/dev/null)
    [ "$unauth_count" = "0" ] \
        && ok "action_owner_visibility.unauthenticated_zero" \
        || fail "action_owner_visibility.unauthenticated_zero" "expected 0, got $unauth_count; resp: $unauth_resp"

    # Obtain owner's Bearer token via HTTP (password grant).
    local tok_resp alice_tok
    tok_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@alice","password":"alicepass"}' 2>/dev/null)
    alice_tok=$(strfield "$tok_resp" "token")
    [ -n "$alice_tok" ] \
        && ok "action_owner_visibility.token_obtained" \
        || fail "action_owner_visibility.token_obtained" "no token in: $tok_resp"

    # Authenticated owner: inactive/private action must appear.
    local auth_resp auth_count
    auth_resp=$(curl -sf -H "Authorization: Bearer $alice_tok" \
        "http://$addr/v1/actions?owner=@alice" 2>/dev/null)
    auth_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$auth_resp" 2>/dev/null)
    [ "${auth_count:-0}" -ge 1 ] \
        && ok "action_owner_visibility.owner_sees_private" \
        || fail "action_owner_visibility.owner_sees_private" "expected >=1, got $auth_count; resp: $auth_resp"

    stop_serve "$serve_pid"
}
