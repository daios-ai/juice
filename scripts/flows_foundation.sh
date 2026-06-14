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
    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1
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
    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1
    [ -f "$home_sys/.juice/token" ] \
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
    [ -f "$home_sys/.juice/refresh_token" ] && old_rt=$(cat "$home_sys/.juice/refresh_token")

    # Refresh rotates both tokens
    j "$db" "$home_sys" auth refresh >/dev/null 2>&1
    local new_rt=""
    [ -f "$home_sys/.juice/refresh_token" ] && new_rt=$(cat "$home_sys/.juice/refresh_token")
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
        echo "$old_rt" > "$home_sys/.juice/refresh_token"
        local reuse_out
        reuse_out=$(j "$db" "$home_sys" auth refresh 2>&1)
        echo "$reuse_out" | grep -qi "expired\|invalid\|error\|failed" \
            && ok "local_auth.refresh_token_rotation_enforced" \
            || fail "local_auth.refresh_token_rotation_enforced" "reused refresh token was accepted"
        # Restore new token so logout works
        echo "$new_rt" > "$home_sys/.juice/refresh_token"
    fi

    # Logout revokes and removes tokens
    j "$db" "$home_sys" auth logout >/dev/null 2>&1
    [ ! -f "$home_sys/.juice/token" ] \
        && ok "local_auth.logout_removes_token" \
        || fail "local_auth.logout_removes_token" "token file still present after logout"

    # Commands after logout fail — use j so the error message reaches stdout
    local post_logout
    post_logout=$(j "$db" "$home_sys" user me)
    echo "$post_logout" | grep -qi "login\|not logged in\|error" \
        && ok "local_auth.post_logout_rejected" \
        || fail "local_auth.post_logout_rejected" "post-logout command succeeded unexpectedly"
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

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys" user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # @alice works before suspension
    local me
    me=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me" "handle")" = "@alice" ] \
        && ok "suspension.alice_active" \
        || fail "suspension.alice_active" "alice me failed: $me"

    # @sys suspends @alice by ID
    local alice_id
    alice_id=$(strfield "$me" "id")
    j "$db" "$home_sys" admin user suspend --id "$alice_id" >/dev/null 2>&1

    # @alice's existing token is now rejected — use j so the error message is in stdout
    local suspended_out
    suspended_out=$(j "$db" "$home_alice" user me)
    echo "$suspended_out" | grep -qi "suspended\|unauthenticated\|error" \
        && ok "suspension.suspended_token_rejected" \
        || fail "suspension.suspended_token_rejected" "suspended user was not rejected: $suspended_out"

    # Data preserved — @sys can still see @alice
    local alice_show
    alice_show=$(jj "$db" "$home_sys" admin user show --handle @alice)
    [ "$(strfield "$alice_show" "handle")" = "@alice" ] \
        && ok "suspension.data_preserved" \
        || fail "suspension.data_preserved" "admin show failed: $alice_show"

    # @sys unsuspends @alice
    j "$db" "$home_sys" admin user unsuspend --id "$alice_id" >/dev/null 2>&1

    # @alice's existing token works again (no re-login required)
    me=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me" "handle")" = "@alice" ] \
        && ok "suspension.unsuspend_restores_access" \
        || fail "suspension.unsuspend_restores_access" "alice still rejected after unsuspend: $me"
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

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys" user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys" user create --handle @bob --email bob@test.com --password bobpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # Initial balance is zero
    local me
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 0 ] \
        && ok "deposits.initial_zero" \
        || fail "deposits.initial_zero" "initial balance not zero: $me"

    # @sys deposits 500 to @alice
    j "$db" "$home_sys" admin user deposit --handle @alice --amount 500 >/dev/null 2>&1
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 500 ] \
        && ok "deposits.balance_updated" \
        || fail "deposits.balance_updated" "expected 500, got: $me"

    # Second deposit accumulates
    j "$db" "$home_sys" admin user deposit --handle @alice --amount 200 >/dev/null 2>&1
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 700 ] \
        && ok "deposits.accumulates" \
        || fail "deposits.accumulates" "expected 700, got: $me"

    # Non-sys user cannot deposit
    local bob_deposit
    bob_deposit=$(j "$db" "$home_bob" admin user deposit --handle @alice --amount 10 2>&1)
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

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # Create action — inactive by default
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name greet --kind http \
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
    j "$db" "$home_alice" action enable --id "$action_id" >/dev/null 2>&1
    local show
    show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$show" 2>/dev/null)
    [ "$active" = "True" ] \
        && ok "action_lifecycle.enabled" \
        || fail "action_lifecycle.enabled" "expected True, got $active; show: $show"

    # Disable → inactive
    j "$db" "$home_alice" action disable --id "$action_id" >/dev/null 2>&1
    show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$show" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "action_lifecycle.disabled" \
        || fail "action_lifecycle.disabled" "expected False, got $active; show: $show"

    # Update description — persisted
    j "$db" "$home_alice" action update --id "$action_id" --description "updated desc" >/dev/null 2>&1
    show=$(jj "$db" "$home_alice" action show --id "$action_id")
    echo "$show" | grep -q "updated desc" \
        && ok "action_lifecycle.update_persisted" \
        || fail "action_lifecycle.update_persisted" "description not updated; show: $show"

    # Delete — use j (text mode) so the error message is in stdout for grep
    j "$db" "$home_alice" action delete --id "$action_id" >/dev/null 2>&1
    local show_deleted
    show_deleted=$(j "$db" "$home_alice" action show --id "$action_id")
    echo "$show_deleted" | grep -qi "not found\|error" \
        && ok "action_lifecycle.deleted" \
        || fail "action_lifecycle.deleted" "action still visible after delete: $show_deleted"

    # Non-owner (regular user @bob) cannot delete @alice's action
    create_out=$(jj "$db" "$home_alice" action create --name hello --kind http \
        --source "http://127.0.0.1:1/hello")
    local alice_action_id
    alice_action_id=$(strfield "$create_out" "id")
    local bob_delete
    bob_delete=$(j "$db" "$home_bob" action delete --id "$alice_action_id")
    echo "$bob_delete" | grep -qi "unauthorized\|not found\|error" \
        && ok "action_lifecycle.owner_enforced" \
        || fail "action_lifecycle.owner_enforced" "non-owner delete succeeded: $bob_delete"

    # Name reuse: after deleting an action, the same name can be registered again
    j "$db" "$home_alice" action delete --id "$alice_action_id" >/dev/null 2>&1
    local reuse_out
    reuse_out=$(jj "$db" "$home_alice" action create --name hello --kind http \
        --source "http://127.0.0.1:1/hello2" --description "reused name")
    local reuse_id
    reuse_id=$(strfield "$reuse_out" "id")
    [ -n "$reuse_id" ] \
        && ok "action_lifecycle.name_reuse_after_delete" \
        || fail "action_lifecycle.name_reuse_after_delete" "name reuse failed: $reuse_out"

    # action_name is captured in transactions and survives action deletion
    start_backend "$backend_port" 200 '{"answer":42}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local tx_action_id
    create_out=$(jj "$db" "$home_alice" action create --name callable \
        --kind http --source "http://127.0.0.1:${backend_port}/call" \
        --description "for tx test" --price 0)
    tx_action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable    --id "$tx_action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action update --id "$tx_action_id" --public >/dev/null 2>&1

    j "$db" "$home_bob"   auth login --handle @bob --password bobpass >/dev/null 2>&1
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" run --action "@alice/callable" --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")

    # Delete the action — transaction must still carry the name
    j "$db" "$home_alice" action delete --id "$tx_action_id" >/dev/null 2>&1
    local tx_show action_name_in_tx
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    action_name_in_tx=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('action_name',''))" "$tx_show" 2>/dev/null)
    [ "$action_name_in_tx" = "callable" ] \
        && ok "action_lifecycle.action_name_in_tx_after_delete" \
        || fail "action_lifecycle.action_name_in_tx_after_delete" "action_name='$action_name_in_tx', want 'callable'; tx: $tx_show"
}
