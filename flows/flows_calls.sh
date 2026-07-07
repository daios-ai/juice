# Process, ACL, and call-semantics flows. Built on flows/lib.sh.
# Dedupe: the CLI is the HTTP client, so `run`/`tx show`/`user me` already exercise
# /v1/run, /v1/transactions, /v1/me — the old per-flow curl mirrors are dropped.

flow_process_lifecycle() {
    echo "=== FLOW process_lifecycle ==="
    local dir db hs ha
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    start_server "$db" "$hs" || { fail "process_lifecycle.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    deposit "$db" "$hs" @alice 1000

    # @sys/message (price 0) creates a process + a waiting step addressed to @alice.
    local msg tx_id step_id proc
    msg=$(jj "$db" "$ha" run @sys/message '{"to":"@alice","message":"lifecycle"}')
    tx_id=$(strfield "$msg" tx_id)
    step_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get('step_id',''))" "$msg" 2>/dev/null)
    proc=$(strfield "$(jj "$db" "$ha" tx show "$tx_id")" process_id)
    assert_nonempty "process_lifecycle.started" "$proc"
    assert_nonempty "process_lifecycle.root_trace" "$(strfield "$msg" trace_id)"
    assert_jnum "process_lifecycle.balance_unchanged" "$(jj "$db" "$ha" user me)" available 1000

    # Process open with an outstanding step, funds parked (available 0).
    local ps; ps=$(jj "$db" "$ha" process show "$proc")
    assert_jnum "process_lifecycle.process_available" "$ps" available 0
    assert_json "process_lifecycle.status_open" "$ps" status open

    # The step's meaning comes from its creating action (@sys/message), not its sink target.
    assert_eq "process_lifecycle.step_created_by" "@sys/message" \
        "$(jj "$db" "$ha" step list | python3 -c "import sys,json;print(next((s.get('created_by','') for s in json.load(sys.stdin) if s.get('id')=='$step_id'),''))" 2>/dev/null)"

    # End the process: waiting step cancelled, funds returned, process closed.
    j "$db" "$ha" process end "$proc" >/dev/null 2>&1
    assert_jnum "process_lifecycle.funds_restored" "$(jj "$db" "$ha" user me)" available 1000
    assert_json "process_lifecycle.status_closed" "$(jj "$db" "$ha" process show "$proc")" status closed
    local st; st=$(jj "$db" "$ha" step list | python3 -c "import sys,json;print(next((s['status'] for s in json.load(sys.stdin) if s.get('id')=='$step_id'),''))" 2>/dev/null)
    assert_eq "process_lifecycle.step_cancelled" cancelled "$st"
}

flow_acl_public() {
    echo "=== FLOW acl_public ==="
    local dir db hs ha hb aid
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    start_server "$db" "$hs" || { fail "acl_public.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob

    # Private action pointing at an unreachable backend (port 1): permission is checked
    # before any process/backend work.
    aid=$(strfield "$(jj "$db" "$ha" action create target --kind http --source "http://127.0.0.1:1/target" --price 0 --description "acl")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1

    assert_fails "acl_public.private_denied" "unauthorized\|permission\|error" -- j "$db" "$hb" run @alice/target '{}'
    assert_fails "acl_public.update_public_owner_only" "unauthorized\|error" -- j "$db" "$hb" action update "$aid" --public

    # Public: @bob passes the permission check (then fails at the unreachable backend, NOT on permission).
    j "$db" "$ha" action update "$aid" --public >/dev/null 2>&1
    assert_not_contains "acl_public.public_passes" "permission" "$(j "$db" "$hb" run @alice/target '{}')"

    # Private again: permission enforced.
    j "$db" "$ha" action update "$aid" --public=false >/dev/null 2>&1
    assert_fails "acl_public.private_enforced" "unauthorized\|permission\|error" -- j "$db" "$hb" run @alice/target '{}'
}

flow_successful_paid_call() {
    echo "=== FLOW successful_paid_call ==="
    local dir db hs ha hb bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 200 '{"result":"ok"}'
    start_server "$db" "$hs" fee_bps=2000 || { fail "successful_paid_call.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 500

    local aid; aid=$(strfield "$(jj "$db" "$ha" action create pay --kind http --source "http://127.0.0.1:${bport}/pay" --price 100 --description "paid")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --public >/dev/null 2>&1
    local sys_start; sys_start=$(numfield "$(jj "$db" "$hs" admin show @sys)" available)

    # fee_bps=2000 → on gross=100: fee=20, net=80.
    local tx_id; tx_id=$(strfield "$(jj "$db" "$hb" run @alice/pay '{}')" tx_id)
    assert_nonempty "successful_paid_call.call_succeeded" "$tx_id"
    local tx; tx=$(jj "$db" "$hb" tx show "$tx_id")
    assert_jnum "successful_paid_call.tx_gross" "$tx" gross 100
    assert_jnum "successful_paid_call.tx_fee"   "$tx" fee 20
    assert_jnum "successful_paid_call.tx_net"   "$tx" net 80
    assert_json "successful_paid_call.tx_status" "$tx" status success
    assert_jnum "successful_paid_call.bob_debited"     "$(jj "$db" "$hb" user me)" available 400
    assert_jnum "successful_paid_call.target_credited" "$(jj "$db" "$ha" user me)" available 80
    assert_eq   "successful_paid_call.fee_credited" "$(( sys_start + 20 ))" "$(numfield "$(jj "$db" "$hs" admin show @sys)" available)"
}

flow_http_verbs() {
    echo "=== FLOW http_verbs ==="
    local dir db hs ha bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    bport=$(backend_port); start_echo_backend "$bport" || { fail "http_verbs.backend" "echo backend failed"; return; }
    start_server "$db" "$hs" || { fail "http_verbs.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice

    # A kind=http action fires the verb it was created with; the input reaches upstream
    # (query for GET, body otherwise).
    local verb lname aid out
    for verb in GET PUT PATCH DELETE; do
        lname=$(printf '%s' "$verb" | tr 'A-Z' 'a-z')
        aid=$(strfield "$(jj "$db" "$ha" action create "v-$lname" --kind http --method "$verb" --source "http://127.0.0.1:${bport}/echo" --price 0 --description "verb $verb")" id)
        j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
        out=$(jj "$db" "$ha" run "@alice/v-$lname" '{"v":"x"}')
        local m v
        m=$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('result',{}).get('method',''))" "$out" 2>/dev/null)
        v=$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('result',{}).get('v',''))" "$out" 2>/dev/null)
        assert_eq "http_verbs.$lname" "$verb:x" "$m:$v"
    done
    # Decomposed http view round-trips on read.
    local m; m=$(python3 -c "import sys,json;print(json.loads(sys.argv[1]).get('http',{}).get('method',''))" "$(jj "$db" "$ha" action show @alice/v-get)" 2>/dev/null)
    assert_eq "http_verbs.read_view" GET "$m"
}

flow_failed_call_refund() {
    echo "=== FLOW failed_call_refund ==="
    local dir db hs ha hb bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 500 '{"error":"backend error"}'
    start_server "$db" "$hs" || { fail "failed_call_refund.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 500

    local aid; aid=$(strfield "$(jj "$db" "$ha" action create fail --kind http --source "http://127.0.0.1:${bport}/fail" --price 100 --description "failing")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --public >/dev/null 2>&1

    # Backend 500 → execution failure; full refund, @alice credited nothing, failure tx recorded.
    j "$db" "$hb" run @alice/fail '{}' >/dev/null 2>&1 || true
    assert_jnum "failed_call_refund.process_unchanged"    "$(jj "$db" "$hb" user me)" available 500
    assert_jnum "failed_call_refund.target_not_credited"  "$(jj "$db" "$ha" user me)" available 0
    local txs; txs=$(jj "$db" "$hb" tx list)
    assert_eq "failed_call_refund.failure_tx_recorded" yes "$([ "$(list_len "$txs")" -ge 1 ] && echo yes || echo no)"
    local tx_id; tx_id=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])[0]['id'])" "$txs" 2>/dev/null)
    assert_json "failed_call_refund.tx_status_failure" "$(jj "$db" "$hb" tx show "$tx_id")" status failure
}

flow_input_schema_failure() {
    echo "=== FLOW input_schema_failure ==="
    local dir db hs ha hb
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    start_server "$db" "$hs" || { fail "input_schema_failure.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 300

    local aid; aid=$(strfield "$(jj "$db" "$ha" action create schema-in --kind http --source "http://127.0.0.1:1/x" --price 0 --description "schema" \
        --input-schema '{"type":"object","properties":{"x":{"type":"string","description":"the x parameter"}},"required":["x"]}')" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --public >/dev/null 2>&1

    # Missing required "x" → schema error BEFORE any trace/charge.
    assert_fails "input_schema_failure.error_returned" "schema\|invalid\|required\|error" -- j "$db" "$hb" run @alice/schema-in '{}'
    assert_jnum "input_schema_failure.balance_unchanged" "$(jj "$db" "$hb" user me)" available 300
    assert_eq   "input_schema_failure.no_tx_created" 0 "$(list_len "$(jj "$db" "$hb" tx list)")"
}

flow_output_schema_failure() {
    echo "=== FLOW output_schema_failure ==="
    local dir db hs ha hb bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'   # missing required output "id"
    start_server "$db" "$hs" || { fail "output_schema_failure.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 300

    local aid; aid=$(strfield "$(jj "$db" "$ha" action create schema-out --kind http --source "http://127.0.0.1:${bport}/schema-out" --price 50 --description "schema out" \
        --output-schema '{"type":"object","properties":{"id":{"type":"string","description":"the record id"}},"required":["id"]}')" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --public >/dev/null 2>&1

    # Execution succeeds but output fails validation → CommitFailedCall: refund + failure tx.
    assert_fails "output_schema_failure.error_returned" "schema\|invalid\|error" -- j "$db" "$hb" run @alice/schema-out '{}'
    assert_jnum "output_schema_failure.process_refunded"   "$(jj "$db" "$hb" user me)" available 300
    assert_jnum "output_schema_failure.target_not_credited" "$(jj "$db" "$ha" user me)" available 0
    local txs; txs=$(jj "$db" "$hb" tx list)
    assert_eq "output_schema_failure.failure_tx_recorded" yes "$([ "$(list_len "$txs")" -ge 1 ] && echo yes || echo no)"
    local tx_id; tx_id=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])[0]['id'])" "$txs" 2>/dev/null)
    assert_json "output_schema_failure.tx_status_failure" "$(jj "$db" "$hb" tx show "$tx_id")" status failure
}

# flow_grant — delegated-OAuth consent gating (§8), CLI surface.
# Covers action-create with an oauth_delegated auth config, reject-before-lock when no grant
# exists, and user disconnect. The full consent+run happy path needs a live provider and is
# covered by the Go flow suite (TestFlow_OAuthDelegated); here we assert the CLI gating.
flow_grant() {
    echo "=== FLOW grant ==="
    local dir db hs ha aid
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    start_server "$db" "$hs" || { fail "grant.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password syspass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    deposit "$db" "$hs" @alice 1000

    # A delegated-OAuth action; provider endpoints are loopback stubs (never dialed on the
    # reject path — consent is required before any funds lock).
    aid=$(strfield "$(jj "$db" "$ha" action create inbox --kind http --source "http://127.0.0.1:9/api" --price 100 --description "delegated inbox" --auth '{"scheme":"oauth_delegated","config":{"auth_url":"http://127.0.0.1:9/auth","token_url":"http://127.0.0.1:9/token","client_id":"cid","scopes":"read"}}')" id)
    assert_nonempty "grant.action_created" "$aid"
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1

    # Running without a grant is rejected before any charge, with the consent hint surfaced.
    assert_fails "grant.reject_before_consent" "grant" -- j "$db" "$ha" run @alice/inbox '{}'
    assert_jnum "grant.no_charge" "$(jj "$db" "$ha" user me)" available 1000

    # No connection exists yet, so disconnect reports not-found.
    assert_fails "grant.revoke_absent" "not found\|error" -- j "$db" "$ha" user disconnect @alice/inbox
}
