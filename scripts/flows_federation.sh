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

    # Start both serves (each serve reads its own subdir's juice.json with server_url set)
    start_serve "$db_l" "127.0.0.1:$port_l" syspass "$home_l" \
        || { echo "_fed_setup: start_serve local ($port_l) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_l"
    start_serve "$db_r" "127.0.0.1:$port_r" syspass "$home_r" \
        || { echo "_fed_setup: start_serve remote ($port_r) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_r"

    # One admin peer friend call is sufficient: L registers R locally AND R registers L in
    # its own postPeer handler (synchronous, completes before the HTTP 200 returns).
    # The async sendReciprocal chain adds only redundant re-registrations; we don't need it.
    j "$db_l" "$home_l" admin peer friend --url "http://127.0.0.1:$port_r" >/dev/null 2>&1 \
        || { echo "_fed_setup: peer friend L->R failed" >&2; return 1; }

    local remote_handle="@kernel-r"
    echo "$remote_handle" > "$dir/remote_handle"

    # Import /greet from REMOTE into LOCAL
    # Capture stdout only (stderr has log lines with action_id= that break the sed UUID parse)
    local ri_out
    ri_out=$(j "$db_l" "$home_l" remote import --remote "$remote_handle" --action greet 2>/dev/null)
    if ! echo "$ri_out" | grep -qi "imported\|unchanged"; then
        j "$db_l" "$home_l" remote import --remote "$remote_handle" --action greet 2>&1 | head -5 >&2
        echo "_fed_setup: remote import failed" >&2; return 1
    fi

    # Extract proxy action ID from import output (format: "Imported action greet (id=<uuid>)")
    local proxy_id
    proxy_id=$(echo "$ri_out" | sed 's/.*id=\([^,)]*\).*/\1/')
    # Enable proxy and grant-all (superuser can admin remote proxy)
    j "$db_l" "$home_l" action enable --id "$proxy_id" >/dev/null 2>&1
    j "$db_l" "$home_l" action update --id "$proxy_id" --public >/dev/null 2>&1

    echo "$proxy_id" > "$dir/proxy_id"
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

    # Make one call so there's a tx in history
    local tx_id
    tx_id=$(strfield "$(jj "$db_l" "$home_l" run \
        --action "$remote_handle/greet" --args '{}')" "tx_id")

    # Update action description on REMOTE (stop serve, update db, restart)
    local pid_r
    pid_r=$(cat "$dir/pid_r")
    kill "$pid_r" 2>/dev/null; wait "$pid_r" 2>/dev/null

    # Update description on REMOTE via CLI
    local remote_action_id
    remote_action_id=$(cat "$dir/remote_action_id" 2>/dev/null)
    j "$db_r" "$home_r" action update --id "$remote_action_id" --description "v2 greeting" >/dev/null 2>&1

    # Restart REMOTE serve
    start_serve "$db_r" "127.0.0.1:$port_r" syspass "$home_r"
    echo "$SERVE_PID" > "$dir/pid_r"

    # Re-import
    local reimport_out
    reimport_out=$(j "$db_l" "$home_l" remote import --remote "$remote_handle" --action greet 2>&1)
    echo "$reimport_out" | grep -qi "updated\|deactivated" \
        && ok "fed_reimport.updated" \
        || fail "fed_reimport.updated" "expected Updated, got: $reimport_out"

    # Proxy should now be inactive
    local proxy_show proxy_active
    proxy_show=$(jj "$db_l" "$home_l" action show --id "$proxy_id")
    proxy_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$proxy_show" 2>/dev/null)
    [ "$proxy_active" = "0" ] \
        && ok "fed_reimport.proxy_deactivated" \
        || fail "fed_reimport.proxy_deactivated" "expected active=0, got $proxy_active"

    # Proxy ID preserved (id= is in reimport_out: "Updated action greet (id=<uuid>, ...)")
    local new_proxy_id
    new_proxy_id=$(echo "$reimport_out" | sed 's/.*id=\([^,)]*\).*/\1/')
    [ "$new_proxy_id" = "$proxy_id" ] \
        && ok "fed_reimport.id_preserved" \
        || fail "fed_reimport.id_preserved" "expected $proxy_id, got $new_proxy_id"

    # Prior tx still in history
    local tx_check
    tx_check=$(jj "$db_l" "$home_l" tx show --id "$tx_id" 2>/dev/null)
    [ "$(strfield "$tx_check" "id")" = "$tx_id" ] \
        && ok "fed_reimport.tx_history_intact" \
        || fail "fed_reimport.tx_history_intact" "prior tx $tx_id missing from local db"
}

flow_federation_unimport() {
    echo "=== FLOW federation_unimport ==="
    local dir port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_unimport.setup" "setup failed"; return; }
    local db_l db_r home_l home_r
    db_l=$(cat "$dir/db_l"); db_r=$(cat "$dir/db_r")
    home_l=$(cat "$dir/home_l"); home_r=$(cat "$dir/home_r")
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)

    local remote_handle
    remote_handle=$(cat "$dir/remote_handle")

    # Unimport
    local unimport_out
    unimport_out=$(j "$db_l" "$home_l" remote unimport --remote "$remote_handle" --action greet 2>&1)
    echo "$unimport_out" | grep -qi "deactivated" \
        && ok "fed_unimport.deactivated" \
        || fail "fed_unimport.deactivated" "expected deactivated, got: $unimport_out"

    # Proxy inactive on LOCAL
    local proxy_show proxy_active
    proxy_show=$(jj "$db_l" "$home_l" action show --id "$proxy_id")
    proxy_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$proxy_show" 2>/dev/null)
    [ "$proxy_active" = "0" ] \
        && ok "fed_unimport.proxy_inactive" \
        || fail "fed_unimport.proxy_inactive" "expected active=0, got $proxy_active"

    # Remote action still active
    local remote_action_id remote_show remote_active
    remote_action_id=$(cat "$dir/remote_action_id" 2>/dev/null)
    remote_show=$(jj "$db_r" "$home_r" action show --id "$remote_action_id")
    remote_active=$(python3 -c "import sys,json; print(1 if json.loads(sys.argv[1])['active'] else 0)" "$remote_show" 2>/dev/null)
    [ "$remote_active" = "1" ] \
        && ok "fed_unimport.remote_still_active" \
        || fail "fed_unimport.remote_still_active" "expected remote active=1, got $remote_active"
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
}
