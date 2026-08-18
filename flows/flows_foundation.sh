# Foundation flows: bootstrap, auth, users, deposits, action lifecycle.
# Sourced by flows_test.sh; built on flows/lib.sh (start_server, j/jj, make_user, assert_*).
#
# Dedupe note: user-facing CLI commands are HTTP clients, so `user me` IS GET /v1/me,
# `action list` IS GET /v1/actions, etc. We keep CLI assertions and only add curl where it
# tests something the CLI can't reach (password-grant, raw bad-token, unauthenticated list).

flow_bootstrap() {
    echo "=== FLOW bootstrap ==="
    local dir db hs; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys)

    start_server "$db" "$hs" || { fail "bootstrap.first_boot" "server did not start"; return; }
    ok "bootstrap.first_boot"
    j "$db" "$hs" auth login sys --password sys-pass >/dev/null 2>&1

    assert_json "bootstrap.sys_user" "$(jj "$db" "$hs" user me)" handle sys

    local acts; acts=$(jj "$db" "$hs" action list)
    assert_eq "bootstrap.lookup_registered"  yes "$(has_action "$acts" lookup)"
    assert_eq "bootstrap.llm_chat_registered" yes "$(has_action "$acts" llm/chat)"

    # Restart on the same DB: bootstrap is idempotent (re-reconciles natives, keeps sys).
    stop_server "$db"
    start_server "$db" "$hs" || { fail "bootstrap.idempotent" "second boot failed"; return; }
    ok "bootstrap.idempotent"
    assert_json "bootstrap.state_preserved" "$(jj "$db" "$hs" user me)" handle sys

    # HTTP-only: the authorize→exchange token path (§12), driven raw rather than through the CLI.
    local tok; tok=$(token "$(url "$db")" sys sys-pass)
    assert_nonempty "bootstrap.http_token_exchange" "$tok"
}

# The error contract on the wire. Until assert_status existed, no flow could assert a status at
# all, so a taken handle returning 500 with raw SQL went unnoticed: the CLI reported a generic
# failure and every flow only ever checked success paths.
flow_signup_errors() {
    echo "=== FLOW signup_errors ==="
    local dir db hs ha base; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "signup_errors.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    base=$(url "$db")

    # A taken handle is the caller's conflict, not an internal fault, and the body must not carry
    # the schema: the UI keys on `code`, never on message text.
    local dup='{"handle":"alice","password":"userpass"}' body
    assert_status "signup_errors.duplicate_handle_status" 422 POST "$base/v1/users" "$dup"
    body=$(http_body POST "$base/v1/users" "$dup")
    assert_json         "signup_errors.duplicate_handle_code" "$body" code invalid_input
    assert_not_contains "signup_errors.no_constraint_leak" "constraint" "$body"
    assert_not_contains "signup_errors.no_table_leak"      "accounts."  "$body"

    assert_status "signup_errors.bad_handle"      422 POST "$base/v1/users" '{"handle":"a/b","password":"userpass"}'
    assert_status "signup_errors.short_password"  422 POST "$base/v1/users" '{"handle":"carol","password":"x"}'

    # The same contract on a second caller-supplied unique key: (owner, name) on actions.
    local tok; tok=$(token "$base" alice userpass)
    assert_nonempty "signup_errors.token" "$tok"
    local act='{"name":"dup","kind":"http","price":0,"description":"d","source":"http://127.0.0.1:9/x"}'
    assert_status "signup_errors.first_action"     201 POST "$base/v1/actions" "$act" "$tok"
    assert_status "signup_errors.duplicate_action" 422 POST "$base/v1/actions" "$act" "$tok"
    assert_not_contains "signup_errors.action_no_sql_leak" "constraint" \
        "$(http_body POST "$base/v1/actions" "$act" "$tok")"
}

flow_local_auth() {
    echo "=== FLOW local_auth ==="
    local dir db hs tdir; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "local_auth.boot" "server did not start"; return; }
    tdir=$(juice_token_dir "$hs" "$db")

    assert_eq "local_auth.token_stored" yes "$([ -f "$tdir/token" ] && echo yes || echo no)"
    assert_json "local_auth.me_succeeds" "$(jj "$db" "$hs" user me)" handle sys

    # Refresh is automatic on a 401 (no standalone command): drop the access token, and the next
    # authenticated call transparently refreshes and rotates the refresh token.
    local old_rt=""; [ -f "$tdir/refresh_token" ] && old_rt=$(cat "$tdir/refresh_token")
    rm -f "$tdir/token"
    assert_json "local_auth.me_after_refresh" "$(jj "$db" "$hs" user me)" handle sys
    local new_rt=""; [ -f "$tdir/refresh_token" ] && new_rt=$(cat "$tdir/refresh_token")
    assert_ne "local_auth.refresh_rotates_token" "$old_rt" "$new_rt"

    # A reused (rotated-away) refresh token must be rejected: a stale access token 401s and its
    # auto-refresh with the old refresh token is refused.
    if [ -n "$old_rt" ]; then
        printf 'stale.access.token' > "$tdir/token"; echo "$old_rt" > "$tdir/refresh_token"
        assert_fails "local_auth.refresh_rotation_enforced" "expired\|invalid\|unauthenticated" -- j "$db" "$hs" user me
        echo "$new_rt" > "$tdir/refresh_token"
    fi

    j "$db" "$hs" auth logout >/dev/null 2>&1
    assert_eq "local_auth.logout_removes_token" no "$([ -f "$tdir/token" ] && echo yes || echo no)"
    assert_fails "local_auth.post_logout_rejected" "login\|not logged in\|error" -- j "$db" "$hs" user me

    # HTTP-only: the auth middleware rejects a malformed bearer token (body carries the error).
    assert_contains "local_auth.http_invalid_rejected" "unauthenticated" \
        "$(curl -s -H "Authorization: Bearer bad.token.here" "$(url "$db")/v1/me" 2>/dev/null)"
}

flow_suspension() {
    echo "=== FLOW suspension ==="
    local dir db hs ha; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "suspension.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    assert_json "suspension.alice_active" "$(jj "$db" "$ha" user me)" handle alice

    j "$db" "$hs" admin suspend alice >/dev/null 2>&1
    assert_fails "suspension.suspended_rejected" "suspended\|unauthenticated\|error" -- j "$db" "$ha" user me
    # Data preserved: sys can still see alice.
    assert_json "suspension.data_preserved" "$(jj "$db" "$hs" admin show alice)" handle alice

    j "$db" "$hs" admin unsuspend alice >/dev/null 2>&1
    assert_json "suspension.unsuspend_restores" "$(jj "$db" "$ha" user me)" handle alice
}

flow_deposits() {
    echo "=== FLOW deposits ==="
    local dir db hs ha hb; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    make_admin "$db" "$hs" || { fail "deposits.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob

    assert_jnum "deposits.initial_zero" "$(jj "$db" "$ha" user me)" available 0
    j "$db" "$hs" admin deposit alice 500 >/dev/null 2>&1
    assert_jnum "deposits.balance_updated" "$(jj "$db" "$ha" user me)" available 500
    j "$db" "$hs" admin deposit alice 200 >/dev/null 2>&1
    assert_jnum "deposits.accumulates" "$(jj "$db" "$ha" user me)" available 700

    assert_fails "deposits.non_sys_rejected" "unauthorized\|superuser\|error" -- j "$db" "$hb" admin deposit alice 10
    assert_jnum "deposits.other_user_unaffected" "$(jj "$db" "$hb" user me)" available 0
}

flow_transfers() {
    echo "=== FLOW transfers ==="
    local dir db hs ha hb; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    make_admin "$db" "$hs" || { fail "transfers.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    j "$db" "$hs" admin deposit alice 500 >/dev/null 2>&1

    # Alice transfers 200 to bob by handle; balances move by exactly the amount.
    j "$db" "$ha" user transfer bob 200 --reason gift >/dev/null 2>&1
    assert_jnum "transfers.sender_debited" "$(jj "$db" "$ha" user me)" available 300
    assert_jnum "transfers.recipient_credited" "$(jj "$db" "$hb" user me)" available 200

    # Both parties see the transfer in their ledger (alice also sees her deposit).
    assert_contains "transfers.sender_ledger" "bob" "$(jj "$db" "$ha" user ledger)"
    assert_contains "transfers.recipient_ledger" "alice" "$(jj "$db" "$hb" user ledger)"

    # Pagination: alice has 2 ledger entries (deposit + transfer); --limit 1 returns one.
    assert_eq "transfers.ledger_paginated" 1 \
        "$(jj "$db" "$ha" user ledger --limit 1 | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))')"

    # Over-balance and self transfers are rejected; balance unchanged.
    assert_fails "transfers.overdraw_rejected" "insufficient\|error" -- j "$db" "$ha" user transfer bob 100000
    assert_fails "transfers.self_rejected" "yourself\|invalid\|error" -- j "$db" "$ha" user transfer alice 10
    assert_jnum "transfers.balance_unchanged" "$(jj "$db" "$ha" user me)" available 300
}

flow_action_lifecycle() {
    echo "=== FLOW action_lifecycle ==="
    local dir db hs ha hb bport; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    make_admin "$db" "$hs" || { fail "action_lifecycle.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob

    # Create — inactive by default.
    local cr aid
    cr=$(jj "$db" "$ha" action create greet --kind http --source "http://127.0.0.1:1/greet" --description "hello world" --price 5)
    aid=$(strfield "$cr" id)
    assert_nonempty "action_lifecycle.created" "$aid"
    assert_json "action_lifecycle.inactive_by_default" "$cr" active False

    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    assert_json "action_lifecycle.enabled" "$(jj "$db" "$ha" action show "$aid")" active True
    j "$db" "$ha" action disable "$aid" >/dev/null 2>&1
    assert_json "action_lifecycle.disabled" "$(jj "$db" "$ha" action show "$aid")" active False

    j "$db" "$ha" action update "$aid" --description "updated desc" >/dev/null 2>&1
    assert_contains "action_lifecycle.update_persisted" "updated desc" "$(jj "$db" "$ha" action show "$aid")"

    j "$db" "$ha" action delete "$aid" >/dev/null 2>&1
    assert_fails "action_lifecycle.deleted" "not found\|error" -- j "$db" "$ha" action show "$aid"

    # Non-owner cannot delete someone else's action.
    local hello_id
    hello_id=$(strfield "$(jj "$db" "$ha" action create hello --kind http --source "http://127.0.0.1:1/hello")" id)
    assert_fails "action_lifecycle.owner_enforced" "unauthorized\|not found\|error" -- j "$db" "$hb" action delete "$hello_id"

    # Name reuse after delete.
    j "$db" "$ha" action delete "$hello_id" >/dev/null 2>&1
    local reuse_id
    reuse_id=$(strfield "$(jj "$db" "$ha" action create hello --kind http --source "http://127.0.0.1:1/hello2" --description "reused")" id)
    assert_nonempty "action_lifecycle.name_reuse_after_delete" "$reuse_id"

    # action_name is captured in a transaction and survives action deletion.
    bport=$(backend_port); start_backend "$bport" 200 '{"answer":42}'
    local tid tx_id
    tid=$(publish "$db" "$ha" callable --kind http --source "http://127.0.0.1:${bport}/call" --description "tx test" --price 0)
    tx_id=$(strfield "$(jj "$db" "$hb" run alice/callable '{}')" tx_id)
    j "$db" "$ha" action delete "$tid" >/dev/null 2>&1
    assert_json "action_lifecycle.action_name_in_tx_after_delete" "$(jj "$db" "$hb" tx show "$tx_id")" action_name callable
}

flow_action_owner_visibility() {
    echo "=== FLOW action_owner_visibility ==="
    local dir db hs ha; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "action_owner_visibility.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    # A private, inactive action (no enable, no --visibility public).
    j "$db" "$ha" action create secret-op --kind http --source "http://127.0.0.1:1/secret" --description "private" >/dev/null 2>&1

    # Unauthenticated listing must NOT include the owner's private action; the owner's does.
    # (Raw HTTP: exercises the auth-conditional ?owner= visibility the CLI abstracts over.)
    local base; base=$(url "$db")
    assert_eq "action_owner_visibility.unauthenticated_zero" 0 \
        "$(list_len "$(curl -sf "$base/v1/actions?owner=alice" 2>/dev/null)")"
    local tok; tok=$(token "$base" alice userpass)
    local n; n=$(list_len "$(curl -sf -H "Authorization: Bearer $tok" "$base/v1/actions?owner=alice" 2>/dev/null)")
    assert_eq "action_owner_visibility.owner_sees_private" yes "$([ "${n:-0}" -ge 1 ] && echo yes || echo no)"
}

# flow_recovery: seed-phrase password recovery (§12), plus user/kernel descriptions (§13).
# A created account prints a one-time recovery phrase; losing the password, the user recovers it by
# signing the server challenge with that phrase. Also: a user sets its own description, and sys's
# description is the kernel "about" surfaced by admin identity.
flow_recovery() {
    echo "=== FLOW recovery ==="
    local dir db hs uh phrase; dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); uh=$(home "$dir" rec)
    make_admin "$db" "$hs" || { fail "recovery.boot" "server did not start"; return; }

    # sys's description is the kernel "about" (surfaced by admin identity).
    j "$db" "$hs" user update --description "the neighbourhood kernel" >/dev/null 2>&1
    assert_contains "recovery.kernel_about" "neighbourhood" "$(j "$db" "$hs" admin identity)"

    # Create a user and capture the one-time recovery phrase (printed to stderr, merged by j()).
    phrase=$(j "$db" "$uh" user create recuser --password origpass | grep -oE '([a-z]+ ){11}[a-z]+' | head -1)
    assert_ne "recovery.phrase_printed" "" "$phrase"

    # A user sets and reads back its own description.
    j "$db" "$uh" auth login recuser --password origpass >/dev/null 2>&1
    j "$db" "$uh" user update --description "weather tools" >/dev/null 2>&1
    assert_json "recovery.user_description" "$(jj "$db" "$uh" user me)" description "weather tools"
    j "$db" "$uh" auth logout >/dev/null 2>&1

    # Recover a lost password with the phrase; the old password is then rejected and the new works.
    j "$db" "$uh" auth recover recuser --phrase "$phrase" --password newpass1 >/dev/null 2>&1
    assert_fails "recovery.old_password_rejected" "invalid\|error\|unauth" -- j "$db" "$uh" auth login recuser --password origpass
    j "$db" "$uh" auth login recuser --password newpass1 >/dev/null 2>&1
    assert_json "recovery.new_password_works" "$(jj "$db" "$uh" user me)" handle recuser
}
