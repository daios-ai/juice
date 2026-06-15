# Federation setup helpers and flows.
# Sourced by flows_test.sh; relies on j, jj, ok, fail, alloc_port, bootstrap_kernel,
# start_serve, stop_serve, start_backend, write_test_config, etc.

# _fed_setup dir port_l port_r port_b
# Common federation setup: two bootstrapped kernels, backend, serves started,
# both registered as peers, /greet imported and enabled on LOCAL.
# Each kernel gets its own subdirectory so server_url is configured correctly.
# Saves db_l, db_r, home_l, home_r, proxy_id to files in $dir.
_fed_setup() {
    local dir="$1" port_l="$2" port_r="$3" port_b="$4"

    # Each kernel lives in its own subdir so juice.json configs don't clash.
    local db_l="$dir/l/juice.db" db_r="$dir/r/juice.db"
    local home_l="$dir/lsys"     home_r="$dir/rsys"
    mkdir -p "$dir/l" "$dir/r" "$home_l/.juice" "$home_r/.juice"

    # Save paths for callers.
    echo "$db_l"   > "$dir/db_l"
    echo "$db_r"   > "$dir/db_r"
    echo "$home_l" > "$dir/home_l"
    echo "$home_r" > "$dir/home_r"

    local boot_l boot_r
    alloc_port; boot_l=$_ALLOC_PORT; alloc_port; boot_r=$_ALLOC_PORT
    # Bootstrap each kernel with its own peer_handle and server_url.
    bootstrap_kernel "$db_l" syspass "$home_l" "$boot_l" \
        "server_url=http://127.0.0.1:$port_l" "peer_handle=@kernel-l" || return 1
    bootstrap_kernel "$db_r" syspass "$home_r" "$boot_r" \
        "server_url=http://127.0.0.1:$port_r" "peer_handle=@kernel-r" || return 1

    j "$db_l" "$home_l" auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db_r" "$home_r" auth login --handle @sys --password syspass >/dev/null 2>&1

    # Backend for remote action
    start_backend "$port_b" 200 '{"greeting":"hello"}'
    echo "$BACKEND_PID" > "$dir/bpid"

    # Create /greet on REMOTE, enable, grant-all
    local action_id_r
    action_id_r=$(jj "$db_r" "$home_r" action create \
        --name greet --kind http \
        --source "http://127.0.0.1:$port_b" \
        --description "greet endpoint" \
        --price 0 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$action_id_r" ] || { echo "_fed_setup: action_id_r empty (port_b=$port_b)" >&2; return 1; }
    echo "$action_id_r" > "$dir/remote_action_id"
    j "$db_r" "$home_r" action enable --id "$action_id_r" >/dev/null 2>&1
    j "$db_r" "$home_r" action update --id "$action_id_r" --public >/dev/null 2>&1

    # Start R's serve first. L's serve starts AFTER peer friend to avoid a race:
    # R's async sendReciprocal goroutine would otherwise hit L's serve and call
    # bulkImportPeerActions concurrently with the CLI, causing SQLite contention.
    start_serve "$db_r" "127.0.0.1:$port_r" syspass "$home_r" \
        || { echo "_fed_setup: start_serve remote ($port_r) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_r"

    # peer friend: R is up, L serve is not yet started. R creates @kernel-l, fires
    # sendReciprocal to L (which fails silently — L isn't listening). CLI's own
    # bulkImportPeerActions runs without competition and imports greet.
    j "$db_l" "$home_l" peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        || { echo "_fed_setup: peer friend L->R failed" >&2; return 1; }

    local remote_handle="@kernel-r"
    echo "$remote_handle" > "$dir/remote_handle"

    # Resolve proxy action ID by name reference.
    local proxy_id
    proxy_id=$(jj "$db_l" "$home_l" action show --action "@kernel-r/greet" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$proxy_id" ] || { echo "_fed_setup: proxy action not found after peer friend" >&2; return 1; }
    echo "$proxy_id" > "$dir/proxy_id"

    # Now start L's serve (peer already established, no more race).
    start_serve "$db_l" "127.0.0.1:$port_l" syspass "$home_l" \
        || { echo "_fed_setup: start_serve local ($port_l) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_l"
}

_fed_teardown() {
    local dir="$1"
    local pid_l pid_r bpid
    pid_l=$(cat "$dir/pid_l" 2>/dev/null); pid_r=$(cat "$dir/pid_r" 2>/dev/null)
    bpid=$(cat "$dir/bpid" 2>/dev/null)
    [ -n "$pid_l" ] && { kill "$pid_l" 2>/dev/null; wait "$pid_l" 2>/dev/null; }
    [ -n "$pid_r" ] && { kill "$pid_r" 2>/dev/null; wait "$pid_r" 2>/dev/null; }
    [ -n "$bpid" ] && { kill "$bpid" 2>/dev/null; wait "$bpid" 2>/dev/null; }
}

flow_federation_import_execute() {
    echo "=== FLOW federation_import_execute ==="
    local dir port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_import.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)
    [ -n "$proxy_id" ] || { fail "fed_import.setup" "no proxy_id"; return; }

    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Call the proxy (price=0, no funds needed)
    local call_out tx_id
    call_out=$(jj "$db_l" "$home_l" run \
        --action "$remote_handle/greet" \
        --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "fed_import.call_succeeds" \
        || fail "fed_import.call_succeeds" "no tx_id in: $call_out"

    # Verify remote_receipt_hash and remote_receipt_json stored via tx show
    local tx_show rrh rrj
    tx_show=$(jj "$db_l" "$home_l" tx show --id "$tx_id")
    rrh=$(strfield "$tx_show" "remote_receipt_hash")
    rrj=$(strfield "$tx_show" "remote_receipt_json")
    [ -n "$rrh" ] \
        && ok "fed_import.remote_receipt_hash" \
        || fail "fed_import.remote_receipt_hash" "remote_receipt_hash empty for tx $tx_id"
    [ -n "$rrj" ] \
        && ok "fed_import.remote_receipt_json" \
        || fail "fed_import.remote_receipt_json" "remote_receipt_json empty for tx $tx_id"

    # Local stats: uses=1
    local stats_out uses
    stats_out=$(jj "$db_l" "$home_l" action stats --id "$proxy_id")
    uses=$(numfield "$stats_out" "uses")
    [ "$uses" -eq 1 ] \
        && ok "fed_import.local_stats_updated" \
        || fail "fed_import.local_stats_updated" "expected uses=1, got $uses"

    # HTTP: verify via local serve (already running on port_l)
    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://127.0.0.1:$port_l/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "fed_import.http_token" \
        || fail "fed_import.http_token" "no sys token via HTTP: $tok_resp"

    local http_tx_show
    http_tx_show=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/transactions/$tx_id" 2>/dev/null)
    [ -n "$(strfield "$http_tx_show" "remote_receipt_hash")" ] \
        && ok "fed_import.http_remote_receipt_hash" \
        || fail "fed_import.http_remote_receipt_hash" "no remote_receipt_hash via HTTP: $http_tx_show"

    local http_stats
    http_stats=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/stats/$proxy_id" 2>/dev/null)
    [ "$(numfield "$http_stats" "uses")" -eq 1 ] \
        && ok "fed_import.http_stats_uses" \
        || fail "fed_import.http_stats_uses" "expected uses=1 via HTTP, got: $http_stats"
}

flow_federation_changed_reimport() {
    echo "=== FLOW federation_changed_reimport ==="
    local dir port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_reimport.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Add a NEW action to R after the initial peer friend.  Re-friending must pick
    # it up (incremental import).  The original greet proxy must remain active (unchanged).
    local wave_id
    wave_id=$(jj "$db_r" "$home_r" action create \
        --name wave --kind http \
        --source "http://127.0.0.1:$port_b" \
        --description "wave endpoint" \
        --price 0 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$wave_id" ] || { fail "fed_reimport.create_wave" "wave action_id empty"; return; }
    j "$db_r" "$home_r" action enable --id "$wave_id" >/dev/null 2>&1
    j "$db_r" "$home_r" action update --id "$wave_id" --public >/dev/null 2>&1

    # Re-friend: L re-syncs with R; wave must appear on L.
    j "$db_l" "$home_l" peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        && ok "fed_reimport.refriend" \
        || fail "fed_reimport.refriend" "peer friend re-sync failed"

    # wave proxy now exists on L and is active.
    local wave_proxy_show wave_proxy_active
    wave_proxy_show=$(jj "$db_l" "$home_l" action show --action "@kernel-r/wave" 2>/dev/null)
    wave_proxy_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$wave_proxy_show" 2>/dev/null)
    [ "$wave_proxy_active" = "1" ] \
        && ok "fed_reimport.wave_proxy_created" \
        || fail "fed_reimport.wave_proxy_created" "expected wave proxy active=1, got $wave_proxy_active"

    # Original greet proxy still active (unchanged → left as-is by bulkImportPeerActions).
    local greet_show greet_active
    greet_show=$(jj "$db_l" "$home_l" action show --id "$proxy_id")
    greet_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$greet_show" 2>/dev/null)
    [ "$greet_active" = "1" ] \
        && ok "fed_reimport.greet_still_active" \
        || fail "fed_reimport.greet_still_active" "expected greet proxy active=1, got $greet_active"

    # Proxy ID for greet unchanged.
    local current_proxy_id
    current_proxy_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['id'])" "$greet_show" 2>/dev/null)
    [ "$current_proxy_id" = "$proxy_id" ] \
        && ok "fed_reimport.id_preserved" \
        || fail "fed_reimport.id_preserved" "expected $proxy_id, got $current_proxy_id"

    # Can call wave through the new proxy.
    local wave_call_out
    wave_call_out=$(jj "$db_l" "$home_l" run --action "$remote_handle/wave" --args '{}' 2>/dev/null)
    [ -n "$(strfield "$wave_call_out" "tx_id")" ] \
        && ok "fed_reimport.wave_callable" \
        || fail "fed_reimport.wave_callable" "wave call failed: $wave_call_out"

    # HTTP: wave proxy active and accessible via REST.
    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://127.0.0.1:$port_l/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "fed_reimport.http_token" \
        || fail "fed_reimport.http_token" "no sys token: $tok_resp"

    local wave_proxy_id
    wave_proxy_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['id'])" "$wave_proxy_show" 2>/dev/null)
    local http_wave_show
    http_wave_show=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/actions/$wave_proxy_id" 2>/dev/null)
    [ "$(strfield "$http_wave_show" "active")" = "True" ] \
        && ok "fed_reimport.http_wave_active" \
        || fail "fed_reimport.http_wave_active" "expected active=True: $http_wave_show"
}

flow_federation_unfriend() {
    echo "=== FLOW federation_unfriend ==="
    local dir port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_unfriend.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)

    # L unfriends R: deactivates all proxy actions from R and deny-lists R's key.
    local unfriend_out
    unfriend_out=$(j "$db_l" "$home_l" peer unfriend --handle @kernel-r 2>&1)
    echo "$unfriend_out" | grep -qi "unfriended\|denied\|ok" \
        && ok "fed_unfriend.unfriended" \
        || fail "fed_unfriend.unfriended" "unexpected output: $unfriend_out"

    # Proxy must be inactive on L.
    local proxy_show proxy_active
    proxy_show=$(jj "$db_l" "$home_l" action show --id "$proxy_id")
    proxy_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$proxy_show" 2>/dev/null)
    [ "$proxy_active" = "0" ] \
        && ok "fed_unfriend.proxy_inactive" \
        || fail "fed_unfriend.proxy_inactive" "expected active=0, got $proxy_active"

    # R's action must still be active on R (L's unfriend only affects L).
    local remote_action_id remote_show remote_active
    remote_action_id=$(cat "$dir/remote_action_id" 2>/dev/null)
    remote_show=$(jj "$db_r" "$home_r" action show --id "$remote_action_id")
    remote_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$remote_show" 2>/dev/null)
    [ "$remote_active" = "1" ] \
        && ok "fed_unfriend.remote_still_active" \
        || fail "fed_unfriend.remote_still_active" "expected remote active=1, got $remote_active"

    # L's call through the proxy must now be rejected.
    j "$db_l" "$home_l" run --action "@kernel-r/greet" --args '{}' >/dev/null 2>&1 \
        && fail "fed_unfriend.call_rejected" "expected call to fail after unfriend" \
        || ok "fed_unfriend.call_rejected"

    # HTTP: proxy inactive on L.
    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://127.0.0.1:$port_l/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "fed_unfriend.http_token" \
        || fail "fed_unfriend.http_token" "no sys token: $tok_resp"

    local http_proxy_show
    http_proxy_show=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/actions/$proxy_id" 2>/dev/null)
    [ "$(strfield "$http_proxy_show" "active")" = "False" ] \
        && ok "fed_unfriend.http_proxy_inactive" \
        || fail "fed_unfriend.http_proxy_inactive" "expected active=False: $http_proxy_show"

    # R's action still active via R's serve.
    local tok_resp_r sys_tok_r
    tok_resp_r=$(curl -sf -X POST "http://127.0.0.1:$port_r/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok_r=$(strfield "$tok_resp_r" "token")
    local http_remote_show
    http_remote_show=$(curl -sf -H "Authorization: Bearer $sys_tok_r" \
        "http://127.0.0.1:$port_r/v1/actions/$remote_action_id" 2>/dev/null)
    [ "$(strfield "$http_remote_show" "active")" = "True" ] \
        && ok "fed_unfriend.http_remote_still_active" \
        || fail "fed_unfriend.http_remote_still_active" "expected remote active=True: $http_remote_show"
}

# flow_federation_replay has been moved to TestFederationReplay in cmd/juice/cmd_remote_test.go
# using crypto/ed25519 — the previous implementation required Python nacl.signing.

flow_fed_verify_receipt() {
    echo "=== FLOW fed_verify_receipt ==="
    local dir port_l port_r port_b
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_verify.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")

    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Make a call through the remote proxy.
    local tx_id
    tx_id=$(strfield "$(jj "$db_l" "$home_l" run \
        --action "$remote_handle/greet" \
        --args '{}')" "tx_id")
    [ -n "$tx_id" ] || { fail "fed_verify.call" "call failed, no tx_id"; return; }

    # Buyer: verify as @sys on local kernel.
    local vr_out
    vr_out=$(jj "$db_l" "$home_l" tx verify-receipt --id "$tx_id" 2>&1)

    # top-level valid must be true
    local valid
    valid=$(echo "$vr_out" | python3 -c "import json,sys; d=json.load(sys.stdin); print(str(d.get('valid',False)).lower())" 2>/dev/null)
    [ "$valid" = "true" ] \
        && ok "fed_verify.buyer_valid" \
        || fail "fed_verify.buyer_valid" "expected valid=true, checks=$(echo "$vr_out" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('checks',{}))" 2>/dev/null)"

    # checks.signature must be true
    local sig_check
    sig_check=$(echo "$vr_out" | python3 -c "import json,sys; d=json.load(sys.stdin); print(str(d.get('checks',{}).get('signature',False)).lower())" 2>/dev/null)
    [ "$sig_check" = "true" ] \
        && ok "fed_verify.signature_check" \
        || fail "fed_verify.signature_check" "signature check not true"

    # checks.receipt_hash must be true
    local receipt_hash_check
    receipt_hash_check=$(echo "$vr_out" | python3 -c "import json,sys; d=json.load(sys.stdin); print(str(d.get('checks',{}).get('receipt_hash',False)).lower())" 2>/dev/null)
    [ "$receipt_hash_check" = "true" ] \
        && ok "fed_verify.receipt_hash_check" \
        || fail "fed_verify.receipt_hash_check" "receipt_hash check not true"

    # Non-remote-proxy transaction returns an error (ErrInvalidState → exit non-zero).
    # Use @sys/time (price=0, native, always succeeds without Ollama).
    local local_tx_id
    local_tx_id=$(strfield "$(jj "$db_l" "$home_l" run \
        --action "@sys/time" \
        --args '{}' 2>/dev/null)" "tx_id")
    [ -n "$local_tx_id" ] || { fail "fed_verify.local_call" "local call failed"; return; }
    j "$db_l" "$home_l" tx verify-receipt --id "$local_tx_id" >/dev/null 2>&1 \
        && fail "fed_verify.local_tx_rejected" "expected error for non-remote-proxy tx, got success" \
        || ok "fed_verify.local_tx_rejected"

    # HTTP: verify receipt via HTTP (local serve running on port_l)
    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://127.0.0.1:$port_l/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "fed_verify.http_token" \
        || fail "fed_verify.http_token" "no sys token via HTTP: $tok_resp"

    local http_vr_resp
    http_vr_resp=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/transactions/$tx_id/receipt-verification" 2>/dev/null)
    python3 -c "
import sys,json
d=json.loads(sys.argv[1])
assert d.get('valid') == True, f'valid={d.get(\"valid\")}'
assert d.get('checks',{}).get('signature') == True, 'signature not True'
assert d.get('checks',{}).get('receipt_hash') == True, 'receipt_hash not True'
" "$http_vr_resp" 2>/dev/null \
        && ok "fed_verify.http_receipt_valid" \
        || fail "fed_verify.http_receipt_valid" "expected valid receipt via HTTP, got: $http_vr_resp"
}

# _assert_all_receipt_checks prefix vr_json
# Asserts valid=true and all 9 individual checks are true in a verify-receipt JSON response.
_assert_all_receipt_checks() {
    local pfx="$1" vr_out="$2"
    python3 -c "
import sys, json
vr = json.loads(sys.argv[1])
c = vr.get('checks', {})
ok_list = []
fail_list = []
if vr.get('valid') is not True:
    fail_list.append('valid=False')
for k in ['receipt_hash','signature','action_id','status','charge',
          'settlement_arith','refund_conservation','args_hash','reply_hash']:
    if c.get(k) is True:
        ok_list.append(k)
    else:
        fail_list.append(f'{k}={c.get(k)}')
if fail_list:
    print('FAIL:' + ','.join(fail_list))
else:
    print('OK')
" "$vr_out" 2>/dev/null
}

flow_fed_all_receipt_checks() {
    echo "=== FLOW fed_all_receipt_checks ==="
    local dir port_l port_r port_b
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT; alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_all_receipt.setup" "setup failed"; return; }
    local db_l home_l
    db_l=$(cat "$dir/db_l"); home_l=$(cat "$dir/home_l")
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Make one call through the proxy.
    local tx_id
    tx_id=$(strfield "$(jj "$db_l" "$home_l" run \
        --action "$remote_handle/greet" --args '{}')" "tx_id")
    [ -n "$tx_id" ] || { fail "fed_all_receipt.call" "call failed, no tx_id"; return; }

    # CLI: all 9 checks must be true.
    local vr_out result
    vr_out=$(jj "$db_l" "$home_l" tx verify-receipt --id "$tx_id" 2>/dev/null)
    result=$(_assert_all_receipt_checks "fed_all_receipt" "$vr_out")
    [ "$result" = "OK" ] \
        && ok "fed_all_receipt.all_9_checks_cli" \
        || fail "fed_all_receipt.all_9_checks_cli" "$result; vr=$vr_out"

    # HTTP: same via GET /v1/transactions/{txID}/receipt-verification.
    local tok_resp sys_tok
    tok_resp=$(curl -sf -X POST "http://127.0.0.1:$port_l/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d '{"handle":"@sys","password":"syspass"}' 2>/dev/null)
    sys_tok=$(strfield "$tok_resp" "token")
    [ -n "$sys_tok" ] \
        && ok "fed_all_receipt.http_token" \
        || fail "fed_all_receipt.http_token" "no sys token: $tok_resp"

    local http_vr_out
    http_vr_out=$(curl -sf -H "Authorization: Bearer $sys_tok" \
        "http://127.0.0.1:$port_l/v1/transactions/$tx_id/receipt-verification" 2>/dev/null)
    result=$(_assert_all_receipt_checks "fed_all_receipt" "$http_vr_out")
    [ "$result" = "OK" ] \
        && ok "fed_all_receipt.all_9_checks_http" \
        || fail "fed_all_receipt.all_9_checks_http" "$result; vr=$http_vr_out"
}

flow_fed_denial_unfriended() {
    echo "=== FLOW fed_denial_unfriended ==="
    local dir port_l port_r port_b
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT; alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_denial_unfriended.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # R unfriends L — R will now deny L's inbound federation calls.
    local unfriend_out
    unfriend_out=$(j "$db_r" "$home_r" peer unfriend --handle @kernel-l 2>&1)
    echo "$unfriend_out" | grep -qi "unfriended\|denied\|ok" \
        && ok "fed_denial_unfriended.unfriend" \
        || fail "fed_denial_unfriended.unfriend" "unfriend failed: $unfriend_out"

    # L calls its proxy (still active on L) — R returns 403 denial receipt.
    j "$db_l" "$home_l" run --action "$remote_handle/greet" --args '{}' >/dev/null 2>&1 || true

    # Get the failed tx (first in list).
    local tx_list tx_id
    tx_list=$(jj "$db_l" "$home_l" tx list 2>/dev/null)
    tx_id=$(python3 -c "import sys,json; txs=json.load(sys.stdin); print(txs[0]['id'] if txs else '')" \
        <<< "$tx_list" 2>/dev/null)
    [ -n "$tx_id" ] \
        && ok "fed_denial_unfriended.tx_recorded" \
        || fail "fed_denial_unfriended.tx_recorded" "no tx recorded after denied call"
    [ -n "$tx_id" ] || return

    # tx.status must be failure.
    local tx_show
    tx_show=$(jj "$db_l" "$home_l" tx show --id "$tx_id" 2>/dev/null)
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "fed_denial_unfriended.tx_status_failure" \
        || fail "fed_denial_unfriended.tx_status_failure" "expected failure: $tx_show"

    # All 9 receipt checks must pass — action_id in particular guards the service.go fix.
    local vr_out result
    vr_out=$(jj "$db_l" "$home_l" tx verify-receipt --id "$tx_id" 2>/dev/null)
    result=$(_assert_all_receipt_checks "fed_denial_unfriended" "$vr_out")
    [ "$result" = "OK" ] \
        && ok "fed_denial_unfriended.all_9_checks" \
        || fail "fed_denial_unfriended.all_9_checks" "$result; vr=$vr_out"
}

flow_fed_denial_underfunded() {
    echo "=== FLOW fed_denial_underfunded ==="
    local dir port_l port_r port_b
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT; alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_denial_underfunded.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Create a paid action on R pointing to the same backend (already running on port_b).
    local paid_action_id
    paid_action_id=$(jj "$db_r" "$home_r" action create \
        --name paid-svc --kind http \
        --source "http://127.0.0.1:$port_b" \
        --description "paid service" \
        --price 100 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$paid_action_id" ] || { fail "fed_denial_underfunded.create_paid_action" "action_id empty"; return; }
    j "$db_r" "$home_r" action enable --id "$paid_action_id" >/dev/null 2>&1
    j "$db_r" "$home_r" action update --id "$paid_action_id" --public >/dev/null 2>&1

    # Re-friend to pick up paid-svc (do NOT fund @kernel-l on R — it has 0 credits there).
    j "$db_l" "$home_l" peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        || { fail "fed_denial_underfunded.import" "peer re-friend failed"; return; }
    local paid_proxy_id
    paid_proxy_id=$(jj "$db_l" "$home_l" action show --action "@kernel-r/paid-svc" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$paid_proxy_id" ] || { fail "fed_denial_underfunded.import" "proxy not found after peer friend"; return; }

    # Give L's @sys user enough credits on L.
    j "$db_l" "$home_l" admin user deposit --handle @sys --amount 1000 >/dev/null 2>&1

    # L calls the paid proxy — R rejects (L has 0 credits on R) → 402 denial receipt.
    j "$db_l" "$home_l" run --action "$remote_handle/paid-svc" --args '{}' >/dev/null 2>&1 || true

    # Get the failed tx.
    local tx_list tx_id
    tx_list=$(jj "$db_l" "$home_l" tx list 2>/dev/null)
    tx_id=$(python3 -c "
import sys,json
txs=json.load(sys.stdin)
# Find the paid-svc tx (not the free greet tx if any)
for tx in txs:
    if tx.get('action_name') == 'paid-svc':
        print(tx['id']); break
" <<< "$tx_list" 2>/dev/null)
    [ -n "$tx_id" ] \
        && ok "fed_denial_underfunded.tx_recorded" \
        || fail "fed_denial_underfunded.tx_recorded" "no paid-svc tx found; list=$tx_list"
    [ -n "$tx_id" ] || return

    local tx_show
    tx_show=$(jj "$db_l" "$home_l" tx show --id "$tx_id" 2>/dev/null)
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "fed_denial_underfunded.tx_status_failure" \
        || fail "fed_denial_underfunded.tx_status_failure" "expected failure: $tx_show"

    # All 9 receipt checks must pass.
    local vr_out result
    vr_out=$(jj "$db_l" "$home_l" tx verify-receipt --id "$tx_id" 2>/dev/null)
    result=$(_assert_all_receipt_checks "fed_denial_underfunded" "$vr_out")
    [ "$result" = "OK" ] \
        && ok "fed_denial_underfunded.all_9_checks" \
        || fail "fed_denial_underfunded.all_9_checks" "$result; vr=$vr_out"
}

flow_fed_import_duty() {
    echo "=== FLOW fed_import_duty ==="
    local dir port_l port_r port_b
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT; alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_import_duty.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Create a paid action on R (price=1000) backed by the existing backend.
    local paid_action_id
    paid_action_id=$(jj "$db_r" "$home_r" action create \
        --name duty-svc --kind http \
        --source "http://127.0.0.1:$port_b" \
        --description "duty service" \
        --price 1000 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$paid_action_id" ] || { fail "fed_import_duty.create_paid_action" "action_id empty"; return; }
    j "$db_r" "$home_r" action enable --id "$paid_action_id" >/dev/null 2>&1
    j "$db_r" "$home_r" action update --id "$paid_action_id" --public >/dev/null 2>&1

    # Re-friend to pick up duty-svc; proxy price = 1000 + ceil(1000*500/10000) = 1050.
    j "$db_l" "$home_l" peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        || { fail "fed_import_duty.import" "peer re-friend failed"; return; }
    local duty_proxy_id
    duty_proxy_id=$(jj "$db_l" "$home_l" action show --action "@kernel-r/duty-svc" 2>/dev/null \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$duty_proxy_id" ] || { fail "fed_import_duty.import" "proxy not found after peer friend"; return; }

    # Verify the proxy price is exactly 1050.
    local proxy_show proxy_price
    proxy_show=$(jj "$db_l" "$home_l" action show --action "@kernel-r/duty-svc" 2>/dev/null)
    proxy_price=$(numfield "$proxy_show" "price")
    [ "$proxy_price" -eq 1050 ] \
        && ok "fed_import_duty.proxy_price" \
        || fail "fed_import_duty.proxy_price" "expected 1050, got $proxy_price"

    # Fund @kernel-l on R (so the remote call can proceed).
    j "$db_r" "$home_r" admin user deposit --handle @kernel-l --amount 5000 >/dev/null 2>&1
    # Fund L's @sys on L.
    j "$db_l" "$home_l" admin user deposit --handle @sys --amount 5000 >/dev/null 2>&1

    # Capture balances before the call.
    local user_bal_before peer_bal_before
    user_bal_before=$(numfield "$(jj "$db_l" "$home_l" user me 2>/dev/null)" "available")
    peer_bal_before=$(numfield "$(jj "$db_r" "$home_r" admin user show --handle @kernel-l 2>/dev/null)" "available")

    # Make the call.
    local call_out tx_id
    call_out=$(jj "$db_l" "$home_l" run --action "$remote_handle/duty-svc" --args '{}' 2>/dev/null)
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "fed_import_duty.call_succeeded" \
        || fail "fed_import_duty.call_succeeded" "no tx_id: $call_out"
    [ -n "$tx_id" ] || return

    # Capture balances after.
    local user_bal_after peer_bal_after
    user_bal_after=$(numfield "$(jj "$db_l" "$home_l" user me 2>/dev/null)" "available")
    peer_bal_after=$(numfield "$(jj "$db_r" "$home_r" admin user show --handle @kernel-l 2>/dev/null)" "available")

    local user_charged peer_charged
    user_charged=$(( user_bal_before - user_bal_after ))
    peer_charged=$(( peer_bal_before - peer_bal_after ))

    # L's @sys is both caller and fee recipient: gross=1050 deducted, fee=50 credited back,
    # so net balance change is 1000.
    [ "$user_charged" -eq 1000 ] \
        && ok "fed_import_duty.user_charged_proxy_price" \
        || fail "fed_import_duty.user_charged_proxy_price" "expected 1000 (gross 1050 minus fee 50 rebate), got $user_charged"

    # R charged L's peer exactly the base price (1000).
    [ "$peer_charged" -eq 1000 ] \
        && ok "fed_import_duty.peer_charged_base_price" \
        || fail "fed_import_duty.peer_charged_base_price" "expected 1000, got $peer_charged"

    # tx fields: gross=1050, net=1000, fee=50, status=success.
    local tx_show
    tx_show=$(jj "$db_l" "$home_l" tx show --id "$tx_id" 2>/dev/null)
    [ "$(numfield "$tx_show" "gross")" -eq 1050 ] \
        && ok "fed_import_duty.tx_gross" \
        || fail "fed_import_duty.tx_gross" "expected 1050: $tx_show"
    [ "$(numfield "$tx_show" "net")" -eq 1000 ] \
        && ok "fed_import_duty.tx_net" \
        || fail "fed_import_duty.tx_net" "expected 1000: $tx_show"
    [ "$(numfield "$tx_show" "fee")" -eq 50 ] \
        && ok "fed_import_duty.tx_fee" \
        || fail "fed_import_duty.tx_fee" "expected 50 (5% duty): $tx_show"
    [ "$(strfield "$tx_show" "status")" = "success" ] \
        && ok "fed_import_duty.tx_status" \
        || fail "fed_import_duty.tx_status" "expected success: $tx_show"
}

flow_fed_failed_action_refund() {
    echo "=== FLOW fed_failed_action_refund ==="
    local dir port_l port_r port_b port_fail
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT; alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_failed_refund.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Start a second backend that always returns 500.
    alloc_port; port_fail=$_ALLOC_PORT
    start_backend "$port_fail" 500 '{"error":"backend failure"}'
    local fail_backend_pid=$BACKEND_PID
    trap "_fed_teardown '$dir'; kill '$fail_backend_pid' 2>/dev/null; wait '$fail_backend_pid' 2>/dev/null; rm -rf '$dir'" RETURN

    # Create paid action on R pointing to the 500 backend (price=100).
    local fail_action_id
    fail_action_id=$(jj "$db_r" "$home_r" action create \
        --name fail-svc --kind http \
        --source "http://127.0.0.1:$port_fail" \
        --description "always fails" \
        --price 100 | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])" 2>/dev/null)
    [ -n "$fail_action_id" ] || { fail "fed_failed_refund.create_action" "action_id empty"; return; }
    j "$db_r" "$home_r" action enable --id "$fail_action_id" >/dev/null 2>&1
    j "$db_r" "$home_r" action update --id "$fail_action_id" --public >/dev/null 2>&1

    # Re-friend to pick up fail-svc; proxy price = 100 + ceil(100*500/10000) = 105.
    j "$db_l" "$home_l" peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        || { fail "fed_failed_refund.import" "peer re-friend failed"; return; }

    # Fund @kernel-l on R and L's @sys on L.
    j "$db_r" "$home_r" admin user deposit --handle @kernel-l --amount 5000 >/dev/null 2>&1
    j "$db_l" "$home_l" admin user deposit --handle @sys --amount 1000 >/dev/null 2>&1

    local user_bal_before
    user_bal_before=$(numfield "$(jj "$db_l" "$home_l" user me 2>/dev/null)" "available")

    # Call — backend returns 500 → remote failure receipt → full refund to L's user.
    j "$db_l" "$home_l" run --action "$remote_handle/fail-svc" --args '{}' >/dev/null 2>&1 || true

    local user_bal_after
    user_bal_after=$(numfield "$(jj "$db_l" "$home_l" user me 2>/dev/null)" "available")

    # L's user balance must be unchanged (full refund).
    [ "$user_bal_after" -eq "$user_bal_before" ] \
        && ok "fed_failed_refund.balance_unchanged" \
        || fail "fed_failed_refund.balance_unchanged" "expected $user_bal_before, got $user_bal_after"

    # Find the fail-svc tx.
    local tx_list tx_id
    tx_list=$(jj "$db_l" "$home_l" tx list 2>/dev/null)
    tx_id=$(python3 -c "
import sys,json
txs=json.load(sys.stdin)
for tx in txs:
    if tx.get('action_name') == 'fail-svc':
        print(tx['id']); break
" <<< "$tx_list" 2>/dev/null)
    [ -n "$tx_id" ] \
        && ok "fed_failed_refund.tx_recorded" \
        || fail "fed_failed_refund.tx_recorded" "no fail-svc tx found"
    [ -n "$tx_id" ] || return

    # tx: status=failure, gross=105, net=0, fee=0.
    local tx_show
    tx_show=$(jj "$db_l" "$home_l" tx show --id "$tx_id" 2>/dev/null)
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
        && ok "fed_failed_refund.tx_status_failure" \
        || fail "fed_failed_refund.tx_status_failure" "expected failure: $tx_show"
    [ "$(numfield "$tx_show" "gross")" -eq 105 ] \
        && ok "fed_failed_refund.tx_gross" \
        || fail "fed_failed_refund.tx_gross" "expected 105: $tx_show"
    [ "$(numfield "$tx_show" "net")" -eq 0 ] \
        && ok "fed_failed_refund.tx_net_zero" \
        || fail "fed_failed_refund.tx_net_zero" "expected 0: $tx_show"
    [ "$(numfield "$tx_show" "fee")" -eq 0 ] \
        && ok "fed_failed_refund.tx_fee_zero" \
        || fail "fed_failed_refund.tx_fee_zero" "expected 0: $tx_show"

    stop_backend "$fail_backend_pid"
}
