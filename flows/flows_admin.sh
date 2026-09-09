# Admin/supervision, tx access, and native sys/time & sys/message flows.
# Sourced by flows_test.sh; built on flows/lib.sh (start_server, j/jj, make_user, assert_*).
#
# Dedupe note: user-facing CLI commands are HTTP clients, so `tx list` IS GET
# /v1/transactions, `run` IS POST /v1/run, `step complete` IS POST /v1/steps/{id}/complete,
# `user me` IS GET /v1/me. The old curl mirrors re-asserted state the CLI already exercised;
# they are dropped. admin/* run locally against --db by design.

flow_transaction_access() {
    echo "=== FLOW transaction_access ==="
    local dir db hs ha hb hc bport; dir=$(new_dir); db="$(kdb "$dir")"
    hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob); hc=$(home "$dir" carol)
    make_admin "$db" "$hs" || { fail "tx_access.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    make_user "$db" "$hs" "$hc" carol
    deposit "$db" "$hs" bob 300

    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'

    # alice (seller) publishes a paid action (price=10) callable by anyone.
    local aid
    aid=$(strfield "$(jj "$db" "$ha" action create pvd-action --kind http \
        --source "http://127.0.0.1:${bport}/pvd" --price 10 --description "tx access test")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1

    # bob (buyer) calls it 3 times.
    local i last_tx
    for i in 1 2 3; do last_tx=$(strfield "$(jj "$db" "$hb" run alice/pvd-action '{}')" tx_id); done

    # Seller sees all 3; buyer sees all 3; the sum of nets equals the seller's balance.
    local alice_txs
    alice_txs=$(jj "$db" "$ha" tx list)
    assert_eq "tx_access.seller_sees_all" 3 "$(list_len "$alice_txs")"
    assert_eq "tx_access.buyer_sees_all"  3 "$(list_len "$(jj "$db" "$hb" tx list)")"

    local net_sum
    net_sum=$(python3 -c "import sys,json; print(sum(t['net'] for t in json.loads(sys.argv[1])))" "$alice_txs" 2>/dev/null)
    assert_eq "tx_access.reconstructibility" "$(numfield "$(jj "$db" "$ha" user me)" available)" "$net_sum"

    # carol is not a party — sees none, and `tx show` is denied.
    assert_eq "tx_access.non_party_sees_none" 0 "$(list_len "$(jj "$db" "$hc" tx list)")"
    assert_fails "tx_access.non_party_denied" "not found\|error" -- j "$db" "$hc" tx show "$last_tx"

    # A party is a handle everywhere it is rendered (§14): "Outputs render handle/owner-name/petname,
    # never raw user ids", and API.md is explicit that the raw *_user_id UUIDs are not returned. The
    # detail view obeys this; the list must too, or a provider reconciling its earnings (U15) has to
    # fetch every row again just to learn who paid.
    local one_tx list_keys
    one_tx=$(jj "$db" "$ha" tx show "$last_tx")
    assert_contains "tx_access.show_renders_handles" "owner_handle" "$one_tx"
    assert_not_contains "tx_access.show_hides_ids"  "owner_user_id" "$one_tx"

    list_keys=$(python3 -c "import sys,json; print(' '.join(sorted(json.loads(sys.argv[1])[0].keys())))" "$alice_txs" 2>/dev/null)
    assert_contains     "tx_access.list_renders_handles" "owner_handle"  "$list_keys"
    assert_not_contains "tx_access.list_hides_ids"       "owner_user_id" "$list_keys"
}

# A list is a summary (§14: "Detail views expose the full HTTP shape; lists summarize"), and R6 caps
# a page so "no single response is unbounded". Both are about the same thing: a page must stay a
# page. These assert the two places the drive of 2026-09-03 found them not to be — a compiled WASM
# artifact served to anonymous callers, and full argument/reply bodies in a transaction list.
flow_list_projections() {
    echo "=== FLOW list_projections ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"
    hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "listproj.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    deposit "$db" "$hs" alice 500

    # A wasm action carries the largest thing an action owns: its compiled artifact.
    make_echo_wasm "$dir/echo.wasm"
    local aid
    aid=$(strfield "$(jj "$db" "$ha" action create wasm-big --kind wasm --artifact "$dir/echo.wasm" \
        --description "a compiled action" --price 1)" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1

    # An anonymous caller may see that a public action exists — never the provider's compiled
    # implementation. A federation manifest deliberately carries artifact_hash and not the artifact
    # (§13 P6), so the local HTTP surface must not hand out what a peer across the network cannot get.
    local anon
    anon=$(curl -s "$(url "$db")/v1/actions")
    assert_contains     "listproj.anon_sees_the_action" "wasm-big"      "$anon"
    assert_not_contains "listproj.anon_gets_no_artifact" "wasm_artifact" "$anon"

    # The owner's own list is a summary too: the artifact belongs to the detail read.
    assert_not_contains "listproj.owner_list_is_a_summary" "wasm_artifact" "$(jj "$db" "$ha" action list --all)"

    # A transaction list must not carry whole request and reply bodies. Drive a call whose reply is
    # large, then require the list to stay small while the detail read still has everything.
    local bport; bport=$(backend_port)
    start_backend "$bport" 200 "{\"blob\":\"$(head -c 20000 /dev/zero | tr '\0' 'x')\"}"
    local hid
    hid=$(strfield "$(jj "$db" "$ha" action create bulky --kind http --source "http://127.0.0.1:${bport}/b" \
        --description "returns a large reply" --price 1)" id)
    j "$db" "$ha" action enable "$hid" >/dev/null 2>&1
    local i; for i in 1 2 3 4 5; do j "$db" "$ha" run alice/bulky '{}' >/dev/null 2>&1; done

    local list_bytes
    list_bytes=$(jj "$db" "$ha" tx list --limit 50 | wc -c)
    echo "  tx list --limit 50 over 5 large replies: ${list_bytes} bytes"
    assert_eq "listproj.tx_list_is_a_summary" yes "$([ "$list_bytes" -lt 20000 ] && echo yes || echo no)"

    # And the default command must simply work: a user listing their own transactions cannot be
    # asked to guess a smaller --limit.
    assert_nonempty "listproj.default_tx_list_works" "$(jj "$db" "$ha" tx list)"
}

flow_admin_supervision() {
    echo "=== FLOW admin_supervision ==="
    local dir db hs ha bport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "admin.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    # admin users lists sys and alice; admin show returns alice.
    assert_contains "admin.user_list" "alice" "$(jj "$db" "$hs" admin users)"
    assert_contains "admin.user_list_sys" "sys" "$(jj "$db" "$hs" admin users)"
    assert_json "admin.user_show" "$(jj "$db" "$hs" admin show alice)" handle alice

    # Suspend blocks alice's session; unsuspend restores it.
    j "$db" "$hs" admin suspend alice >/dev/null 2>&1
    assert_fails "admin.suspend_blocks_alice" "suspended\|unauthenticated\|error" -- j "$db" "$ha" user me
    j "$db" "$hs" admin unsuspend alice >/dev/null 2>&1
    j "$db" "$ha" auth login alice --password userpass >/dev/null 2>&1
    assert_json "admin.unsuspend_restores_alice" "$(jj "$db" "$ha" user me)" handle alice

    # Superuser rename vacates the old handle; the freed name is reusable by a distinct account,
    # and sys's own handle cannot be renamed.
    local hb; hb=$(home "$dir" bob)
    make_user "$db" "$hs" "$hb" bob
    j "$db" "$hs" admin rename bob bob-retired >/dev/null 2>&1
    assert_json "admin.rename_new_handle" "$(jj "$db" "$hs" admin show bob-retired)" handle bob-retired
    assert_fails "admin.rename_frees_old" "not found\|error" -- j "$db" "$hs" admin show bob
    # The freed handle is reusable by a fresh account.
    make_user "$db" "$hs" "$hb" bob
    assert_json "admin.rename_handle_reused" "$(jj "$db" "$hs" admin show bob)" handle bob
    assert_fails "admin.rename_sys_rejected" "cannot be renamed\|error" -- j "$db" "$hs" admin rename sys root

    # Supervision is scope on the normal commands: sys sees any owner's actions/processes/txs
    # and may disable any action, all over the standard TCP API (no separate admin surface).
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'
    local aid
    aid=$(strfield "$(jj "$db" "$ha" action create test --kind http \
        --source "http://127.0.0.1:$bport" --description "alice's action" --price 0)" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1
    # sys `action list` shows alice's action (system-wide scope).
    assert_contains "admin.action_list_scope" "$aid" "$(jj "$db" "$hs" action list)"

    # sys disables alice's action over TCP (owner-or-superuser); bob cannot.
    j "$db" "$hs" action disable "$aid" >/dev/null 2>&1
    assert_json "admin.action_disable_scope" "$(jj "$db" "$hs" action show "$aid")" active False
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1

    deposit "$db" "$hs" alice 100
    local tx_id proc_id
    tx_id=$(strfield "$(jj "$db" "$ha" run alice/test '{}')" tx_id)
    proc_id=$(strfield "$(jj "$db" "$ha" tx show "$tx_id")" process_id)
    # sys `process list` / `tx list` span all users.
    assert_contains "admin.process_list_scope" "$proc_id" "$(jj "$db" "$hs" process list)"
    assert_eq "admin.tx_list_scope" yes \
        "$([ "$(list_len "$(jj "$db" "$hs" tx list)")" -ge 1 ] && echo yes || echo no)"

    # A non-superuser is rejected from the operator commands.
    assert_fails "admin.non_sys_rejected" "unauthorized\|superuser\|error" -- j "$db" "$ha" admin users
}

flow_time() {
    echo "=== FLOW time ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "time.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    # sys/time is registered active + local + free after bootstrap, so a local user may call it.
    local acts; acts=$(jj "$db" "$hs" action list)
    assert_eq "time.registered" yes "$(has_action "$acts" time)"

    # Call it (price=0): result carries a positive unix ts and an RFC-3339 iso ts.
    local out; out=$(jj "$db" "$ha" run sys/time '{}')
    local unix_val; unix_val=$(resultf "$out" unix)
    assert_eq "time.returns_unix" yes "$([ -n "$unix_val" ] && [ "$unix_val" -gt 0 ] 2>/dev/null && echo yes || echo no)"
    assert_eq "time.returns_iso" ok "$(python3 -c "
import sys; from datetime import datetime
try: datetime.fromisoformat(sys.argv[1].replace('Z','+00:00')); print('ok')
except Exception: print('bad')" "$(resultf "$out" iso)" 2>/dev/null)"
}

flow_message() {
    echo "=== FLOW message ==="
    local dir db hs ha hb; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    make_admin "$db" "$hs" || { fail "message.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob

    # alice sends sys/message to bob (price=0) → a waiting step addressed to bob.
    local step_id
    step_id=$(resultf "$(jj "$db" "$ha" run sys/message '{"to":"bob","message":"Please review doc"}')" step_id)
    assert_nonempty "message.step_created" "$step_id"

    # bob sees the step and completes it.
    assert_contains "message.bob_sees_step" "$step_id" "$(jj "$db" "$hb" step list)"
    assert_json "message.step_waiting" "$(jj "$db" "$hb" step show "$step_id")" status waiting
    assert_contains "message.bob_completes_step" tx_id "$(jj "$db" "$hb" step complete "$step_id" '{}')"
    assert_json "message.step_done" "$(jj "$db" "$ha" step show "$step_id")" status done

    # Missing 'to' and unknown recipient are both rejected.
    assert_fails "message.missing_to_rejected" "to\|required\|invalid" -- j "$db" "$ha" run sys/message '{"message":"hi"}'
    assert_fails "message.unknown_recipient_rejected" "not found\|invalid\|unknown" -- j "$db" "$ha" run sys/message '{"to":"nobody","message":"hi"}'
}

flow_native_orphan_purge() {
    echo "=== FLOW native_orphan_purge ==="
    local dir db hs; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "orphan.boot" "server did not start"; return; }

    # Baseline: a real native is present.
    assert_eq "orphan.time_present" yes "$(has_action "$(jj "$db" "$hs" action list)" time)"

    # Inject (server stopped) a kind=native row whose name no build registers a handler for —
    # simulating a native left over in an existing DB after it was removed from the stdlib.
    stop_server "$db"
    local orphan_id; orphan_id=$(python3 - "$db" <<'PYEOF'
import sqlite3, uuid, sys
c = sqlite3.connect(sys.argv[1])
owner = c.execute("SELECT id FROM accounts WHERE handle='sys' LIMIT 1").fetchone()[0]
aid = str(uuid.uuid4())
c.execute("""INSERT INTO actions
  (id,owner_user_id,name,kind,active,visibility,price,description,input_schema,output_schema,source,artifact_hash,wasm_artifact,remote_action_id,auth_json,created_at,updated_at)
  VALUES (?,?,?,'native',1,'local',0,'obsolete',?,?,'','','','','',datetime('now'),datetime('now'))""",
  [aid, owner, 'obsolete-native', '{"type":"object"}', '{"type":"object"}'])
c.commit(); print(aid)
PYEOF
)
    # The prune assertion below is vacuous if the row never landed, so the injection must fail loudly.
    assert_nonempty "orphan.injected" "$orphan_id"

    # Restart → startup prune soft-deletes the handler-less native; real natives survive.
    make_admin "$db" "$hs" || { fail "orphan.reboot" "server did not restart"; return; }
    assert_eq "orphan.pruned"        no  "$(has_action "$(jj "$db" "$hs" action list --all)" obsolete-native)"
    assert_eq "orphan.real_survives" yes "$(has_action "$(jj "$db" "$hs" action list)" time)"
}
