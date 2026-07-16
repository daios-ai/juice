# WASM, contractor, steps, crash-recovery, rating, and TinyGo flows. Built on flows/lib.sh.
# Dedupe: CLI == HTTP client, so step/tx/run CLI commands already exercise the HTTP surface;
# the old per-flow curl mirrors are dropped. Crash-recovery uses a real stop/inject/start.

flow_wasm_execution() {
    echo "=== FLOW wasm_execution ==="
    local dir db hs ha hb
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    # Short script timeout so the infinite-loop action is killed quickly (echo is instant).
    start_server "$db" "$hs" script_timeout_ms=200 || { fail "wasm_execution.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 200

    make_echo_wasm "$dir/echo.wasm"
    local aid; aid=$(strfield "$(jj "$db" "$ha" action create echo --kind wasm --source "$dir/echo.wasm" --price 10 --description "echo")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1
    assert_nonempty "wasm_execution.artifact_hash" "$(strfield "$(jj "$db" "$ha" action show "$aid")" artifact_hash)"

    assert_nonempty "wasm_execution.echo_call_succeeds" "$(strfield "$(jj "$db" "$hb" run @alice/echo '{"msg":"hello"}')" tx_id)"
    assert_jnum "wasm_execution.echo_charged" "$(jj "$db" "$hb" user me)" available 190

    make_infinite_loop_wasm "$dir/loop.wasm"
    local lid; lid=$(strfield "$(jj "$db" "$ha" action create loop --kind wasm --source "$dir/loop.wasm" --price 10 --description "loop")" id)
    j "$db" "$ha" action enable "$lid" >/dev/null 2>&1
    j "$db" "$ha" action update "$lid" --visibility public >/dev/null 2>&1
    assert_fails "wasm_execution.infinite_loop_timeout" "timeout\|timed\|execution" -- j "$db" "$hb" run @alice/loop '{}'
    assert_jnum "wasm_execution.loop_refunded" "$(jj "$db" "$hb" user me)" available 190
}

flow_contractor_subcall() {
    echo "=== FLOW contractor_subcall ==="
    local dir db hs ha hb hc bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob); hc=$(home "$dir" carol)
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'
    start_server "$db" "$hs" || { fail "contractor_subcall.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    make_user "$db" "$hs" "$hc" @carol
    deposit "$db" "$hs" @carol 50

    # @bob's HTTP sub-target (price 50); @alice's WASM contractor (price 50) sub-calls it.
    local sub; sub=$(strfield "$(jj "$db" "$hb" action create sub-target --kind http --source "http://127.0.0.1:${bport}/sub" --price 50 --description "sub")" id)
    j "$db" "$hb" action enable "$sub" >/dev/null 2>&1
    j "$db" "$hb" action update "$sub" --visibility public >/dev/null 2>&1
    make_contractor_wasm "$dir/contractor.wasm" "@bob/sub-target"
    local cid; cid=$(strfield "$(jj "$db" "$ha" action create contractor --kind wasm --source "$dir/contractor.wasm" --price 50 --description "contractor")" id)
    j "$db" "$ha" action enable "$cid" >/dev/null 2>&1
    j "$db" "$ha" action update "$cid" --visibility public >/dev/null 2>&1

    # @carol funds the process with 50; the whole budget flows to @bob via the sub-call.
    assert_nonempty "contractor_subcall.call_succeeds" "$(strfield "$(jj "$db" "$hc" run @alice/contractor '{}')" tx_id)"
    assert_jnum "contractor_subcall.caller_spent"  "$(jj "$db" "$hc" user me)" available 0
    assert_jnum "contractor_subcall.alice_untouched" "$(jj "$db" "$ha" user me)" available 0
    assert_jnum "contractor_subcall.bob_credited"  "$(jj "$db" "$hb" user me)" available 50
}

flow_contractor_failure() {
    echo "=== FLOW contractor_failure ==="
    local dir db hs ha hb hc bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob); hc=$(home "$dir" carol)
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'
    start_server "$db" "$hs" || { fail "contractor_failure.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    make_user "$db" "$hs" "$hc" @carol
    deposit "$db" "$hs" @carol 30   # < sub-call price 50

    local sub; sub=$(strfield "$(jj "$db" "$hb" action create sub-target --kind http --source "http://127.0.0.1:${bport}/sub" --price 50 --description "sub")" id)
    j "$db" "$hb" action enable "$sub" >/dev/null 2>&1
    j "$db" "$hb" action update "$sub" --visibility public >/dev/null 2>&1
    make_contractor_wasm "$dir/contractor.wasm" "@bob/sub-target"
    local cid; cid=$(strfield "$(jj "$db" "$ha" action create contractor --kind wasm --source "$dir/contractor.wasm" --price 50 --description "contractor")" id)
    j "$db" "$ha" action enable "$cid" >/dev/null 2>&1
    j "$db" "$ha" action update "$cid" --visibility public >/dev/null 2>&1

    # @carol (30) < contractor price (50) → rejected at the funds check, nothing charged.
    assert_fails "contractor_failure.error_returned" "insufficient\|balance\|funds\|credits\|costs" -- j "$db" "$hc" run @alice/contractor '{}'
    assert_jnum "contractor_failure.caller_unchanged" "$(jj "$db" "$hc" user me)" available 30
    assert_jnum "contractor_failure.alice_unchanged"  "$(jj "$db" "$ha" user me)" available 0
}

flow_step_success() {
    echo "=== FLOW step_success ==="
    local dir db hs ha hb
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    start_server "$db" "$hs" || { fail "step_success.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob

    # @alice → @bob message creates a waiting step (next_action=@sys/sink, price 0).
    local step_id; step_id=$(resultf "$(jj "$db" "$ha" run @sys/message '{"to":"@bob","message":"review"}')" step_id)
    assert_nonempty "step_success.create_returns_id" "$step_id"
    assert_json "step_success.status_waiting" "$(jj "$db" "$ha" step show "$step_id")" status waiting

    # Owner and required-caller both see it in their step list.
    assert_eq "step_success.owner_sees_step"  1 "$(jj "$db" "$ha" step list | python3 -c "import sys,json;print(sum(1 for s in json.load(sys.stdin) if s.get('id')=='$step_id'))" 2>/dev/null)"
    assert_eq "step_success.caller_sees_step" 1 "$(jj "$db" "$hb" step list | python3 -c "import sys,json;print(sum(1 for s in json.load(sys.stdin) if s.get('id')=='$step_id'))" 2>/dev/null)"
    # The step carries owner_handle (the process owner / payer) — resolvable even to @bob, who is the
    # required caller, not the owner. The step is the continuation that settles into @alice's transaction.
    assert_json "step_success.owner_handle_from_caller" "$(jj "$db" "$hb" step show "$step_id")" owner_handle @alice

    # @bob (required caller) completes it → done.
    local comp; comp=$(jj "$db" "$hb" step complete "$step_id" '{}')
    assert_nonempty "step_success.complete_returns_tx" "$(strfield "$comp" tx_id)"
    assert_eq "step_success.complete_returns_step_id" "$step_id" "$(strfield "$comp" step_id)"
    assert_json "step_success.status_done" "$(jj "$db" "$ha" step show "$step_id")" status done
}

flow_step_failure() {
    echo "=== FLOW step_failure ==="
    local dir db hs ha hb hc
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob); hc=$(home "$dir" carol)
    start_server "$db" "$hs" || { fail "step_failure.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    make_user "$db" "$hs" "$hc" @carol

    # Completing an already-done step → ErrInvalidState.
    local s1; s1=$(resultf "$(jj "$db" "$ha" run @sys/message '{"to":"@bob","message":"first"}')" step_id)
    jj "$db" "$hb" step complete "$s1" '{}' >/dev/null 2>&1
    assert_fails "step_failure.double_complete_rejected" "invalid.state\|already\|not.*waiting" -- j "$db" "$hb" step complete "$s1" '{}'

    # Wrong caller (@carol) completing @bob's step → ErrUnauthorized.
    local s2; s2=$(resultf "$(jj "$db" "$ha" run @sys/message '{"to":"@bob","message":"second"}')" step_id)
    assert_fails "step_failure.wrong_caller_rejected" "unauthorized\|permission\|caller" -- j "$db" "$hc" step complete "$s2" '{}'
}

flow_step_restart() {
    echo "=== FLOW step_restart ==="
    local dir db hs ha hb
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    start_server "$db" "$hs" || { fail "step_restart.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob

    local step_id; step_id=$(resultf "$(jj "$db" "$ha" run @sys/message '{"to":"@bob","message":"restart"}')" step_id)
    assert_json "step_restart.initial_waiting" "$(jj "$db" "$ha" step show "$step_id")" status waiting

    # Inject a crashed 'running' step (ClaimStep succeeded, CompleteStep never did) while the
    # server is stopped, then restart: bootstrap's ResetRunningSteps must revert it to waiting.
    stop_server "$db"
    python3 -c "import sqlite3,sys; c=sqlite3.connect(sys.argv[1]); c.execute(\"UPDATE steps SET status='running' WHERE id=?\",[sys.argv[2]]); c.commit()" "$db" "$step_id"
    start_server "$db" "$hs" || { fail "step_restart.reboot" "server did not restart"; return; }

    assert_json "step_restart.reset_to_waiting" "$(jj "$db" "$ha" step show "$step_id")" status waiting
    assert_nonempty "step_restart.completable_after_reset" "$(strfield "$(jj "$db" "$hb" step complete "$step_id" '{}')" tx_id)"
}

flow_locked_funds_recovery() {
    echo "=== FLOW locked_funds_recovery ==="
    local dir db hs
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys)
    start_server "$db" "$hs" || { fail "locked_funds.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    j "$db" "$hs" admin deposit @sys 200 >/dev/null 2>&1

    # Inject (server stopped) an orphan process+trace: a root call for @sys/tinygo/compile (price 5)
    # that crashed before settling — 5 parked in user.locked and process.locked, trace has
    # no tx (idempotency_key NULL). Not producible through the public API.
    stop_server "$db"
    local proc_id; proc_id=$(python3 - "$db" <<'PYEOF'
import sqlite3, uuid, sys
c = sqlite3.connect(sys.argv[1])
owner = c.execute("SELECT id FROM users WHERE handle='@sys' LIMIT 1").fetchone()[0]
act   = c.execute("SELECT id FROM actions WHERE name='tinygo/compile' LIMIT 1").fetchone()[0]
proc, trace = str(uuid.uuid4()), str(uuid.uuid4())
c.execute("UPDATE users SET available=available-5, locked=locked+5 WHERE id=?", [owner])
c.execute("INSERT INTO processes (id,owner_user_id,available,locked,status,created_at,ended_at) VALUES (?,?,0,5,'open',datetime('now'),NULL)", [proc, owner])
c.execute("""INSERT INTO traces (id,process_id,parent_trace_id,action_owner_id,action_id,caller_user_id,available,locked,idempotency_key,dispatch_json,created_at)
             VALUES (?,?,NULL,?,?,?,5,0,NULL,NULL,datetime('now'))""", [trace, proc, owner, act, owner])
c.commit(); print(proc)
PYEOF
)
    assert_nonempty "locked_funds.injected" "$proc_id"

    # Restart → Recover settles the orphan as an interrupted failure: refund flows up,
    # process closes, and @sys's balance is made whole (200).
    start_server "$db" "$hs" || { fail "locked_funds.reboot" "server did not restart"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    local ps; ps=$(jj "$db" "$hs" process show "$proc_id")
    assert_jnum "locked_funds.locked_cleared" "$ps" locked 0
    assert_json "locked_funds.process_closed" "$ps" status closed
    assert_jnum "locked_funds.user_refunded" "$(jj "$db" "$hs" user me)" available 200
}

flow_rating() {
    echo "=== FLOW rating ==="
    local dir db hs ha hb bport
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'
    start_server "$db" "$hs" || { fail "rating.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    make_user "$db" "$hs" "$hb" @bob
    deposit "$db" "$hs" @bob 200

    local aid; aid=$(strfield "$(jj "$db" "$ha" action create rate-me --kind http --source "http://127.0.0.1:${bport}/rate" --price 10 --description "rateable")" id)
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1

    local tx_id; tx_id=$(strfield "$(jj "$db" "$hb" run @alice/rate-me '{}')" tx_id)
    # Unrated → rating field is null (strfield renders JSON null as Python None).
    assert_eq "rating.unrated_null" None "$(strfield "$(jj "$db" "$hb" tx show "$tx_id")" rating)"

    assert_contains "rating.rate_succeeds" "rated" "$(j "$db" "$hb" tx rate "$tx_id" 1 --note 'great service')"
    assert_jnum "rating.stats_updated" "$(jj "$db" "$hb" action stats "$aid")" rating_count 1

    # Buyer and seller both see the embedded {value, note}; it also appears in the list.
    local chk="import sys,json; r=json.loads(sys.argv[1]).get('rating') or {}; assert r.get('value')==1 and r.get('note')=='great service', r; print('ok')"
    assert_eq "rating.buyer_sees_rating"  ok "$(python3 -c "$chk" "$(jj "$db" "$hb" tx show "$tx_id")" 2>/dev/null)"
    assert_eq "rating.seller_sees_rating" ok "$(python3 -c "$chk" "$(jj "$db" "$ha" tx show "$tx_id")" 2>/dev/null)"
    assert_eq "rating.embedded_in_list" 1 "$(jj "$db" "$hb" tx list | python3 -c "import sys,json;print(sum(1 for t in json.load(sys.stdin) if t['id']=='$tx_id' and (t.get('rating') or {}).get('value')==1))" 2>/dev/null)"

    # Duplicate rating and non-buyer rating are both rejected.
    assert_fails "rating.duplicate_rejected"  "invalid.input\|already" -- j "$db" "$hb" tx rate "$tx_id" 0
    assert_fails "rating.non_buyer_rejected"   "unauthorized\|buyer\|permission" -- j "$db" "$ha" tx rate "$tx_id" 1
}

# flow_tinygo_compile — @sys/tinygo/compile end-to-end with REAL TinyGo. Opt-in via
# JUICE_TINYGO_FLOWS=1 (needs the tinygo toolchain). Large artifacts (~1.4 MB) are parsed
# from FILES, never argv (128 KB MAX_ARG_STRLEN).
flow_tinygo_compile() {
    echo "=== FLOW tinygo_compile ==="
    local dir db hs ha
    dir=$(new_dir); db="$dir/juice.db"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    start_server "$db" "$hs" || { fail "tinygo_compile.boot" "server did not start"; return; }
    j "$db" "$hs" auth login @sys --password sys-pass >/dev/null 2>&1
    make_user "$db" "$hs" "$ha" @alice
    deposit "$db" "$hs" @alice 200

    local src='func Handle(in map[string]any) (map[string]any, error) {
	n, _ := in["n"].(float64)
	return map[string]any{"doubled": n * 2}, nil
}'
    local args; args=$(python3 -c 'import json,sys; print(json.dumps({"source": sys.argv[1]}))' "$src")

    # Compile a valid Handle; save the artifact to a file for registration.
    jj "$db" "$ha" run @sys/tinygo/compile "$args" > "$dir/compile.json"
    python3 -c "import json;open('$dir/doubler.b64','w').write(json.load(open('$dir/compile.json')).get('result',{}).get('artifact',''))" 2>/dev/null
    local status; status=$(python3 -c "import json;print(json.load(open('$dir/compile.json')).get('result',{}).get('status',''))" 2>/dev/null)
    assert_eq "tinygo_compile.compile_success" success "$status"
    assert_eq "tinygo_compile.artifact_bytes" yes "$([ -s "$dir/doubler.b64" ] && echo yes || echo no)"

    # Empty source → ErrInvalidInput (not charged); bad source → charged status=failure.
    assert_fails "tinygo_compile.empty_source_rejected" "invalid.input\|required\|source" -- j "$db" "$ha" run @sys/tinygo/compile '{"source":""}'
    jj "$db" "$ha" run @sys/tinygo/compile '{"source":"func Handle(in map[string]any) (map[string]any, error) { totally not go }"}' > "$dir/bad.json"
    assert_eq "tinygo_compile.bad_source_failure" failure "$(python3 -c "import json;print(json.load(open('$dir/bad.json')).get('result',{}).get('status',''))" 2>/dev/null)"

    # Register the compiled artifact as a wasm action and run it: doubles(21)=42.
    jj "$db" "$ha" action create doubler --kind wasm --artifact "$dir/doubler.b64" --price 5 --description "doubles n" \
        --input-schema '{"type":"object","properties":{"n":{"type":"number","description":"number to double"}}}' \
        --output-schema '{"type":"object","properties":{"doubled":{"type":"number","description":"twice n"}}}' > "$dir/create.json"
    local act_id; act_id=$(python3 -c "import json;print(json.load(open('$dir/create.json')).get('id',''))" 2>/dev/null)
    assert_nonempty "tinygo_compile.register_artifact" "$act_id"
    j "$db" "$ha" action enable "$act_id" >/dev/null 2>&1
    j "$db" "$ha" action update "$act_id" --visibility public >/dev/null 2>&1
    jj "$db" "$ha" run @alice/doubler '{"n":21}' > "$dir/run.json"
    local doubled; doubled=$(python3 -c "import json;print(json.load(open('$dir/run.json')).get('result',{}).get('doubled',''))" 2>/dev/null)
    assert_eq "tinygo_compile.run_compiled_action" 42 "${doubled%.*}"
}
