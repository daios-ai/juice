#!/usr/bin/env bash
# End-to-end flow tests for the juice CLI.
# Covers 34 independent user-story flows, each with its own SQLite database.
#
# Usage (manual):
#   go build -o /tmp/juice ./cmd/juice/
#   JUICE=/tmp/juice JUICE_SECRET_KEY=test bash scripts/flows_test.sh
#
# Usage (via Go test suite):
#   go test ./cmd/juice/ -run TestFlowsIntegration -v -timeout 300s
#
# Requires: bash >=4, python3, curl
set -uo pipefail

JUICE="${JUICE:-$(command -v juice 2>/dev/null || true)}"
if [ ! -x "$JUICE" ]; then
    echo "JUICE binary not found. Set JUICE=/path/to/binary."
    exit 1
fi

# ---------------------------------------------------------------------------
# Global counters and port allocator
# ---------------------------------------------------------------------------
PASS=0
FAIL=0
ERRS=""

_NEXT_PORT=39000
alloc_port() { local p=$_NEXT_PORT; _NEXT_PORT=$((_NEXT_PORT + 1)); echo "$p"; }

# ---------------------------------------------------------------------------
# Assertion helpers
# ---------------------------------------------------------------------------
ok()   { echo "  PASS: $1"; ((PASS++)); }
fail() { echo "  FAIL: $1 — $2"; ((FAIL++)); ERRS="${ERRS}\n  [$1] $2"; }

# ---------------------------------------------------------------------------
# CLI wrappers
# j  db home [args...] — run juice against db with the given HOME
# jj db home [args...] — same with --output json
# ---------------------------------------------------------------------------
j() {
    local db="$1" home="$2"; shift 2
    JUICE_LOG_LEVEL=error HOME="$home" JUICE_ALLOW_LOCAL_SOURCES=true \
        "$JUICE" --db "$db" "$@" 2>&1
}

# jj — JSON output; stderr suppressed so log lines don't corrupt JSON parsing.
jj() {
    local db="$1" home="$2"; shift 2
    JUICE_LOG_LEVEL=error HOME="$home" JUICE_ALLOW_LOCAL_SOURCES=true \
        "$JUICE" --db "$db" --output json "$@" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------

# bootstrap_kernel db pass home port
# Starts juice serve briefly to trigger first-boot initialisation, then stops it.
bootstrap_kernel() {
    local db="$1" pass="$2" home="$3" port="$4"
    JUICE_LOG_LEVEL=error JUICE_BOOTSTRAP_PASSWORD="$pass" JUICE_ALLOW_LOCAL_SOURCES=true \
        HOME="$home" "$JUICE" --db "$db" serve --addr "127.0.0.1:$port" &
    local pid=$!
    local deadline=$(( $(date +%s) + 15 ))
    until curl -sf "http://127.0.0.1:${port}/health" >/dev/null 2>&1; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 1
        fi
        sleep 0.2
    done
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 0
}

# start_serve db addr pass home
# Starts juice serve in the background, polls /health. Sets SERVE_PID.
SERVE_PID=""
start_serve() {
    local db="$1" addr="$2" pass="$3" home="$4"
    JUICE_LOG_LEVEL=error JUICE_BOOTSTRAP_PASSWORD="$pass" JUICE_ALLOW_LOCAL_SOURCES=true \
        HOME="$home" "$JUICE" --db "$db" serve --addr "$addr" &
    SERVE_PID=$!
    local deadline=$(( $(date +%s) + 15 ))
    until curl -sf "http://${addr}/health" >/dev/null 2>&1; do
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kill "$SERVE_PID" 2>/dev/null; return 1
        fi
        sleep 0.2
    done
}

stop_serve() {
    local pid="$1"
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 0
}

# ---------------------------------------------------------------------------
# JSON field extractors (Python-backed for robustness)
# ---------------------------------------------------------------------------
strfield() {
    python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d.get(sys.argv[2],''))" \
        "$1" "$2" 2>/dev/null
}
numfield() {
    python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(int(d.get(sys.argv[2],0)))" \
        "$1" "$2" 2>/dev/null
}

# ---------------------------------------------------------------------------
# WASM generators
# ---------------------------------------------------------------------------

# make_echo_wasm outfile
# Writes a WASM module: alloc(n)->ptr bump-allocator; run(ptr,len)->(ptr,len) echoes input.
# Binary taken from script/wasm_test.go echoWASM.
make_echo_wasm() {
    python3 -c "
import sys
sys.stdout.buffer.write(bytes([
    0x00,0x61,0x73,0x6d,0x01,0x00,0x00,0x00,
    0x01,0x0d,0x02,
    0x60,0x01,0x7f,0x01,0x7f,
    0x60,0x02,0x7f,0x7f,0x02,0x7f,0x7f,
    0x03,0x03,0x02,0x00,0x01,
    0x05,0x03,0x01,0x00,0x01,
    0x06,0x06,0x01,0x7f,0x01,0x41,0x00,0x0b,
    0x07,0x18,0x03,
    0x06,0x6d,0x65,0x6d,0x6f,0x72,0x79,0x02,0x00,
    0x05,0x61,0x6c,0x6c,0x6f,0x63,0x00,0x00,
    0x03,0x72,0x75,0x6e,0x00,0x01,
    0x0a,0x1a,0x02,
    0x11,0x01,0x01,0x7f,
    0x23,0x00,0x21,0x01,0x23,0x00,0x20,0x00,0x6a,0x24,0x00,0x20,0x01,0x0b,
    0x06,0x00,0x20,0x00,0x20,0x01,0x0b,
]))
" > "$1"
}

# make_infinite_loop_wasm outfile
# Writes a WASM module whose run() loops forever (for timeout/cancellation tests).
# Binary taken from script/wasm_test.go infiniteLoopWASM.
make_infinite_loop_wasm() {
    python3 -c "
import sys
sys.stdout.buffer.write(bytes([
    0x00,0x61,0x73,0x6d,0x01,0x00,0x00,0x00,
    0x01,0x0d,0x02,
    0x60,0x01,0x7f,0x01,0x7f,
    0x60,0x02,0x7f,0x7f,0x02,0x7f,0x7f,
    0x03,0x03,0x02,0x00,0x01,
    0x05,0x03,0x01,0x00,0x01,
    0x07,0x18,0x03,
    0x06,0x6d,0x65,0x6d,0x6f,0x72,0x79,0x02,0x00,
    0x05,0x61,0x6c,0x6c,0x6f,0x63,0x00,0x00,
    0x03,0x72,0x75,0x6e,0x00,0x01,
    0x0a,0x12,0x02,
    0x04,0x00,0x41,0x00,0x0b,
    0x0b,0x00,0x03,0x40,0x0c,0x00,0x0b,0x20,0x00,0x20,0x01,0x0b,
]))
" > "$1"
}

# make_contractor_wasm outfile action_name
# Writes a WASM module that calls juice.call(action_name, "{}") and returns the result.
# action_name format: "handle/action-name" e.g. "@bob/target"
make_contractor_wasm() {
    python3 - "$1" "$2" <<'PYEOF'
import sys

outfile, action_name = sys.argv[1], sys.argv[2]
name_b = action_name.encode()
name_len = len(name_b)

def leb128u(n):
    r = []
    while True:
        b = n & 0x7f; n >>= 7
        r.append(b | (0x80 if n else 0))
        if not n: break
    return bytes(r)

def vec(d): return leb128u(len(d)) + bytes(d)
def sec(id_, p): return bytes([id_]) + vec(p)

types   = leb128u(3) + b'\x60\x04\x7f\x7f\x7f\x7f\x02\x7f\x7f' + b'\x60\x01\x7f\x01\x7f' + b'\x60\x02\x7f\x7f\x02\x7f\x7f'
imports = leb128u(1) + vec(b'juice') + vec(b'call') + b'\x00\x00'
funcs   = leb128u(2) + b'\x01\x02'
mems    = leb128u(1) + b'\x00\x01'
globs   = leb128u(1) + b'\x7f\x01\x41' + leb128u(512) + b'\x0b'
exports = leb128u(3) + vec(b'memory') + b'\x02\x00' + vec(b'alloc') + b'\x00\x01' + vec(b'run') + b'\x00\x02'

alloc_body = bytes([0x01,0x01,0x7f,0x23,0x00,0x21,0x01,0x23,0x00,0x20,0x00,0x6a,0x24,0x00,0x20,0x01,0x0b])
run_body   = b'\x00\x41\x00' + b'\x41' + leb128u(name_len) + b'\x41\x20\x41\x02\x10\x00\x0b'
codes      = leb128u(2) + vec(alloc_body) + vec(run_body)

def dseg(off, d): return b'\x00\x41' + leb128u(off) + b'\x0b' + vec(d)
data = leb128u(2) + dseg(0, name_b) + dseg(32, b'{}')

wasm = (b'\x00asm\x01\x00\x00\x00'
    + sec(1,types) + sec(2,imports) + sec(3,funcs)
    + sec(5,mems)  + sec(6,globs)   + sec(7,exports)
    + sec(10,codes)+ sec(11,data))

open(outfile,'wb').write(wasm)
PYEOF
}

# ===========================================================================
# BATCH 1 — Foundation
# ===========================================================================

flow_bootstrap() {
    echo "=== FLOW bootstrap ==="
    local dir db home_sys port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys"; mkdir -p "$home_sys/.juice"
    port=$(alloc_port)

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
names = [a['Name'] for a in acts]
assert '/lookup' in names, f'/lookup missing; got {names}'
assert '/llm/chat' in names, f'/llm/chat missing; got {names}'
" 2>/dev/null \
        && ok "bootstrap.native_actions_registered" \
        || fail "bootstrap.native_actions_registered" "lookup or llm/chat not in action list"

    # Second boot is idempotent — starts cleanly without re-creating @sys
    local port2
    port2=$(alloc_port)
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
    port=$(alloc_port)
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
    port=$(alloc_port)
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
    [ "$(strfield "$alice_show" "Handle")" = "@alice" ] \
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
    port=$(alloc_port)
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
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";   mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    local home_bob="$dir/bob"; mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "action_lifecycle.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # Create action — inactive by default
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /greet --kind http \
        --source "http://127.0.0.1:1/greet" --description "hello world" --price 5)
    action_id=$(strfield "$create_out" "ID")
    [ -n "$action_id" ] \
        && ok "action_lifecycle.created" \
        || fail "action_lifecycle.created" "action add returned no ID; output: $create_out"

    # Active is a Go bool — serialises as JSON false (Python False)
    local active
    active=$(python3 -c "import sys,json; d=json.loads(sys.argv[1]); print(d['Active'])" \
        "$create_out" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "action_lifecycle.inactive_by_default" \
        || fail "action_lifecycle.inactive_by_default" "expected False, got Active=$active"

    # Enable → active
    j "$db" "$home_alice" action enable --id "$action_id" >/dev/null 2>&1
    local show
    show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['Active'])" "$show" 2>/dev/null)
    [ "$active" = "True" ] \
        && ok "action_lifecycle.enabled" \
        || fail "action_lifecycle.enabled" "expected True, got $active; show: $show"

    # Disable → inactive
    j "$db" "$home_alice" action disable --id "$action_id" >/dev/null 2>&1
    show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['Active'])" "$show" 2>/dev/null)
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
    create_out=$(jj "$db" "$home_alice" action add --name /hello --kind http \
        --source "http://127.0.0.1:1/hello")
    local alice_action_id
    alice_action_id=$(strfield "$create_out" "ID")
    local bob_delete
    bob_delete=$(j "$db" "$home_bob" action delete --id "$alice_action_id")
    echo "$bob_delete" | grep -qi "unauthorized\|not found\|error" \
        && ok "action_lifecycle.owner_enforced" \
        || fail "action_lifecycle.owner_enforced" "non-owner delete succeeded: $bob_delete"
}

# ===========================================================================
# Main runner
# ===========================================================================
main() {
    flow_bootstrap
    flow_local_auth
    flow_suspension
    flow_deposits
    flow_action_lifecycle

    echo ""
    echo "Results: ${PASS} passed, ${FAIL} failed"
    if [ "$FAIL" -gt 0 ]; then
        printf "Failures:%b\n" "$ERRS"
        exit 1
    fi
}

main
