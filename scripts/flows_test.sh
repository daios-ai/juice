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

# start_backend port [status_code] [body]
# Starts a minimal HTTP server in background. Sets BACKEND_PID.
BACKEND_PID=""
start_backend() {
    local port="$1" code="${2:-200}" body="${3}"
    [ -z "$body" ] && body='{"ok":true}'
    python3 - "$port" "$code" "$body" <<'PYEOF' &
import sys, http.server
port, code, body = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3].encode()
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        self.rfile.read(n)
        self.send_response(code)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    BACKEND_PID=$!
    sleep 0.3
}

stop_backend() {
    local pid="${1:-$BACKEND_PID}"
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
# BATCH 2 — Process, ACL, and call semantics
# ===========================================================================

flow_process_lifecycle() {
    echo "=== FLOW process_lifecycle ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "process_lifecycle.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 1000 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Start process with 300 funds
    local proc_out proc_id root_trace
    proc_out=$(jj "$db" "$home_alice" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")
    root_trace=$(strfield "$proc_out" "trace_id")
    [ -n "$proc_id" ] \
        && ok "process_lifecycle.started" \
        || fail "process_lifecycle.started" "no process_id in: $proc_out"
    [ -n "$root_trace" ] \
        && ok "process_lifecycle.root_trace" \
        || fail "process_lifecycle.root_trace" "no trace_id in: $proc_out"

    # User.available debited by 300 (1000 - 300 = 700)
    local me_after
    me_after=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me_after" "available")" -eq 700 ] \
        && ok "process_lifecycle.funds_debited" \
        || fail "process_lifecycle.funds_debited" "expected 700, got: $me_after"

    # Process fields
    local proc_show
    proc_show=$(jj "$db" "$home_alice" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 300 ] \
        && ok "process_lifecycle.process_available" \
        || fail "process_lifecycle.process_available" "expected 300, got: $proc_show"
    [ "$(strfield "$proc_show" "Status")" = "open" ] \
        && ok "process_lifecycle.status_open" \
        || fail "process_lifecycle.status_open" "expected open, got: $proc_show"

    # End process — returns funds
    j "$db" "$home_alice" process end --id "$proc_id" >/dev/null 2>&1
    local me_restored
    me_restored=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me_restored" "available")" -eq 1000 ] \
        && ok "process_lifecycle.funds_restored" \
        || fail "process_lifecycle.funds_restored" "expected 1000, got: $me_restored"
    proc_show=$(jj "$db" "$home_alice" process show --id "$proc_id")
    [ "$(strfield "$proc_show" "Status")" = "closed" ] \
        && ok "process_lifecycle.status_closed" \
        || fail "process_lifecycle.status_closed" "expected closed, got: $proc_show"
}

flow_process_funding() {
    echo "=== FLOW process_funding ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "process_funding.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 500 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Start process with 0 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    [ -n "$proc_id" ] \
        && ok "process_funding.started" \
        || fail "process_funding.started" "no process_id in: $proc_out"

    # Fund +200
    j "$db" "$home_alice" process fund --id "$proc_id" --funds 200 >/dev/null 2>&1

    # User debited 200 (500 - 200 = 300)
    local me
    me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$me" "available")" -eq 300 ] \
        && ok "process_funding.user_debited" \
        || fail "process_funding.user_debited" "expected 300, got: $me"

    # Process.available = 200
    local proc_show
    proc_show=$(jj "$db" "$home_alice" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 200 ] \
        && ok "process_funding.process_available" \
        || fail "process_funding.process_available" "expected 200, got: $proc_show"

    # Fund rejected after close
    j "$db" "$home_alice" process end --id "$proc_id" >/dev/null 2>&1
    local fund_closed
    fund_closed=$(j "$db" "$home_alice" process fund --id "$proc_id" --funds 100)
    echo "$fund_closed" | grep -qi "closed\|invalid\|error" \
        && ok "process_funding.fund_after_close_rejected" \
        || fail "process_funding.fund_after_close_rejected" "fund after close was accepted: $fund_closed"
}

flow_acl_public() {
    echo "=== FLOW acl_public ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "acl_public.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice creates and enables an action pointing to unreachable backend (port 1)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /target --kind http \
        --source "http://127.0.0.1:1/target" --price 0 --description "acl test")
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable --id "$action_id" >/dev/null 2>&1

    # @bob starts a zero-funded process
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")

    # Without ACL: permission error
    local out
    out=$(j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /target)
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.no_acl_rejected" \
        || fail "acl_public.no_acl_rejected" "call without ACL succeeded: $out"

    # Grant call ACL — now @bob passes the permission check (fails at backend instead)
    j "$db" "$home_alice" action acl grant --action "$action_id" --user @bob --perm call >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /target)
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.with_acl_passes_permission" \
        || fail "acl_public.with_acl_passes_permission" "permission check still failed after grant: $out"

    # Revoke — permission check enforced again
    j "$db" "$home_alice" action acl revoke --action "$action_id" --user @bob --perm call >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /target)
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.revoke_enforced" \
        || fail "acl_public.revoke_enforced" "call after revoke was accepted: $out"

    # Non-owner cannot grant-all
    out=$(j "$db" "$home_bob" action grant-all --id "$action_id")
    echo "$out" | grep -qi "unauthorized\|error" \
        && ok "acl_public.grant_all_owner_only" \
        || fail "acl_public.grant_all_owner_only" "non-owner grant-all succeeded: $out"

    # grant-all: any authenticated user passes permission check
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /target)
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.grant_all_passes_permission" \
        || fail "acl_public.grant_all_passes_permission" "grant-all still hit permission error: $out"

    # revoke-all: permission check enforced again
    j "$db" "$home_alice" action revoke-all --id "$action_id" >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /target)
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.revoke_all_enforced" \
        || fail "acl_public.revoke_all_enforced" "call after revoke-all was accepted: $out"
}

flow_successful_paid_call() {
    echo "=== FLOW successful_paid_call ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "successful_paid_call.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 500 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"result":"ok"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action with grant-all
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /pay --kind http \
        --source "http://127.0.0.1:${backend_port}/pay" --price 100 --description "paid action")
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # Get @sys ID and starting balance for fee accounting
    local sys_show sys_id sys_start
    sys_show=$(jj "$db" "$home_sys" admin user show --handle @sys)
    sys_id=$(strfield "$sys_show" "ID")
    sys_start=$(numfield "$sys_show" "Available")

    # @bob starts process with 300 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call with fee_bps=2000 → fee=20, net=80, gross=100
    local call_out tx_id
    call_out=$(JUICE_FEE_BPS=2000 JUICE_FEE_RECIPIENT="$sys_id" \
        JUICE_LOG_LEVEL=error HOME="$home_bob" JUICE_ALLOW_LOCAL_SOURCES=true \
        "$JUICE" --db "$db" --output json call \
        --process "$proc_id" --target @alice --action /pay --args '{}' 2>/dev/null)
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_paid_call.call_succeeded" \
        || fail "successful_paid_call.call_succeeded" "call returned no tx_id: $call_out"

    # tx fields
    local tx_show
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(numfield "$tx_show" "Gross")" -eq 100 ] \
        && ok "successful_paid_call.tx_gross" \
        || fail "successful_paid_call.tx_gross" "expected Gross=100, got: $tx_show"
    [ "$(numfield "$tx_show" "Fee")" -eq 20 ] \
        && ok "successful_paid_call.tx_fee" \
        || fail "successful_paid_call.tx_fee" "expected Fee=20, got: $tx_show"
    [ "$(numfield "$tx_show" "Net")" -eq 80 ] \
        && ok "successful_paid_call.tx_net" \
        || fail "successful_paid_call.tx_net" "expected Net=80, got: $tx_show"
    [ "$(strfield "$tx_show" "Status")" = "success" ] \
        && ok "successful_paid_call.tx_status" \
        || fail "successful_paid_call.tx_status" "expected success, got: $tx_show"

    # Process debited by 100 (300 - 100 = 200)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 200 ] \
        && ok "successful_paid_call.process_debited" \
        || fail "successful_paid_call.process_debited" "expected 200, got: $proc_show"

    # @alice credited net=80
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 80 ] \
        && ok "successful_paid_call.target_credited" \
        || fail "successful_paid_call.target_credited" "expected 80, got: $alice_me"

    # Fee recipient (@sys) credited fee=20
    local sys_end
    sys_end=$(numfield "$(jj "$db" "$home_sys" admin user show --handle @sys)" "Available")
    [ "$sys_end" -eq $(( sys_start + 20 )) ] \
        && ok "successful_paid_call.fee_credited" \
        || fail "successful_paid_call.fee_credited" "expected +20; start=$sys_start end=$sys_end"

    stop_backend "$backend_pid"
}

flow_failed_call_refund() {
    echo "=== FLOW failed_call_refund ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "failed_call_refund.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 500 >/dev/null 2>&1

    start_backend "$backend_port" 500 '{"error":"backend error"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates price=100 action
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /fail --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 100 --description "failing action")
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 300 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call — backend returns 500 → execution failure
    j "$db" "$home_bob" call --process "$proc_id" --target @alice --action /fail --args '{}' \
        >/dev/null 2>&1

    # Process available unchanged (full refund)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 300 ] \
        && ok "failed_call_refund.process_unchanged" \
        || fail "failed_call_refund.process_unchanged" "expected 300, got: $proc_show"

    # @alice received nothing
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "failed_call_refund.target_not_credited" \
        || fail "failed_call_refund.target_not_credited" "expected 0, got: $alice_me"

    # Failure tx IS recorded
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "failed_call_refund.failure_tx_recorded" \
        || fail "failed_call_refund.failure_tx_recorded" "expected >=1 tx, count=$tx_count list=$tx_list"

    # tx.Status = failure
    local tx_id tx_show
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['ID'])" "$tx_list" 2>/dev/null)
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(strfield "$tx_show" "Status")" = "failure" ] \
        && ok "failed_call_refund.tx_status_failure" \
        || fail "failed_call_refund.tx_status_failure" "expected failure, got: $tx_show"

    stop_backend "$backend_pid"
}

flow_input_schema_failure() {
    echo "=== FLOW input_schema_failure ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "input_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    # @alice creates action with input schema requiring field "x"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /schema-in --kind http \
        --source "http://127.0.0.1:1/schema-in" --price 50 --description "schema test" \
        --input-schema '{"type":"object","properties":{"x":{"type":"string"}},"required":["x"]}')
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 200 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 200)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call without required field "x" → schema error before any fund lock
    local call_out
    call_out=$(j "$db" "$home_bob" call \
        --process "$proc_id" --target @alice --action /schema-in --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|required\|error" \
        && ok "input_schema_failure.error_returned" \
        || fail "input_schema_failure.error_returned" "expected schema error, got: $call_out"

    # Process available unchanged (no debit happened)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 200 ] \
        && ok "input_schema_failure.process_unchanged" \
        || fail "input_schema_failure.process_unchanged" "expected 200, got: $proc_show"

    # No tx created
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -eq 0 ] \
        && ok "input_schema_failure.no_tx_created" \
        || fail "input_schema_failure.no_tx_created" "expected 0 txs, got $tx_count: $tx_list"
}

flow_output_schema_failure() {
    echo "=== FLOW output_schema_failure ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "output_schema_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    # Backend returns {"ok":true} — missing required output field "id"
    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates action with output schema requiring field "id"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /schema-out --kind http \
        --source "http://127.0.0.1:${backend_port}/schema-out" --price 50 --description "schema out test" \
        --output-schema '{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}')
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 200 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 200)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call — execution runs, output schema check fails → CommitFailedCall
    local call_out
    call_out=$(j "$db" "$home_bob" call \
        --process "$proc_id" --target @alice --action /schema-out --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|error" \
        && ok "output_schema_failure.error_returned" \
        || fail "output_schema_failure.error_returned" "expected schema error, got: $call_out"

    # Full refund — process.available unchanged
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 200 ] \
        && ok "output_schema_failure.process_refunded" \
        || fail "output_schema_failure.process_refunded" "expected 200, got: $proc_show"

    # Failure tx IS recorded (unlike input schema failure)
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "output_schema_failure.failure_tx_recorded" \
        || fail "output_schema_failure.failure_tx_recorded" "expected >=1 tx, count=$tx_count"

    # tx.Status = failure
    local tx_id tx_show
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['ID'])" "$tx_list" 2>/dev/null)
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(strfield "$tx_show" "Status")" = "failure" ] \
        && ok "output_schema_failure.tx_status_failure" \
        || fail "output_schema_failure.tx_status_failure" "expected failure, got: $tx_show"

    # @alice received nothing (refund)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "output_schema_failure.target_not_credited" \
        || fail "output_schema_failure.target_not_credited" "expected 0, got: $alice_me"

    stop_backend "$backend_pid"
}

# ===========================================================================
# BATCH 3 — WASM, Events, Rating
# ===========================================================================

flow_wasm_execution() {
    echo "=== FLOW wasm_execution ==="
    local dir db home_sys home_alice home_bob port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "wasm_execution.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 200 >/dev/null 2>&1

    # @alice creates echo WASM action
    local echo_wasm="$dir/echo.wasm"
    make_echo_wasm "$echo_wasm"
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /echo --kind wasm \
        --source "$echo_wasm" --price 10 --description "echo wasm")
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # ArtifactHash is set after enable (WASM compiled on activation)
    local action_show artifact_hash
    action_show=$(jj "$db" "$home_alice" action show --id "$action_id")
    artifact_hash=$(strfield "$action_show" "ArtifactHash")
    [ -n "$artifact_hash" ] \
        && ok "wasm_execution.artifact_hash" \
        || fail "wasm_execution.artifact_hash" "expected non-empty ArtifactHash: $action_show"

    # @bob calls echo WASM
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc_id=$(strfield "$proc_out" "process_id")
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" call --process "$proc_id" \
        --target @alice --action /echo --args '{"msg":"hello"}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "wasm_execution.echo_call_succeeds" \
        || fail "wasm_execution.echo_call_succeeds" "echo call returned no tx_id: $call_out"

    # Infinite-loop WASM → timeout error
    local loop_wasm="$dir/loop.wasm"
    make_infinite_loop_wasm "$loop_wasm"
    local loop_out loop_id
    loop_out=$(jj "$db" "$home_alice" action add --name /loop --kind wasm \
        --source "$loop_wasm" --price 10 --description "infinite loop")
    loop_id=$(strfield "$loop_out" "ID")
    j "$db" "$home_alice" action enable   --id "$loop_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$loop_id" >/dev/null 2>&1

    local proc2_out proc2_id timeout_out
    proc2_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc2_id=$(strfield "$proc2_out" "process_id")
    timeout_out=$(JUICE_SCRIPT_TIMEOUT_MS=200 JUICE_LOG_LEVEL=error \
        HOME="$home_bob" JUICE_ALLOW_LOCAL_SOURCES=true \
        "$JUICE" --db "$db" call \
        --process "$proc2_id" --target @alice --action /loop --args '{}' 2>&1)
    echo "$timeout_out" | grep -qi "timeout\|timed\|execution" \
        && ok "wasm_execution.infinite_loop_timeout" \
        || fail "wasm_execution.infinite_loop_timeout" "expected timeout error, got: $timeout_out"
}

flow_contractor_subcall() {
    echo "=== FLOW contractor_subcall ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "contractor_subcall.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    # @alice needs 50 credits to fund sub-calls on behalf of the contractor
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 50 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @bob creates sub-target HTTP action (price=50)
    local sub_out sub_id
    sub_out=$(jj "$db" "$home_bob" action add --name /sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "ID")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action grant-all --id "$sub_id" >/dev/null 2>&1

    # @alice creates contractor WASM (price=0) that calls @bob/sub-target
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action add --name /contractor --kind wasm \
        --source "$contractor_wasm" --price 0 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "ID")
    j "$db" "$home_alice" action enable   --id "$cont_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$cont_id" >/dev/null 2>&1

    # @carol calls contractor (price=0 → process needs 0 funds)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_carol" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    local call_out tx_id
    call_out=$(jj "$db" "$home_carol" call \
        --process "$proc_id" --target @alice --action /contractor --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "contractor_subcall.call_succeeds" \
        || fail "contractor_subcall.call_succeeds" "contractor call returned no tx_id: $call_out"

    # @carol process.available unchanged (contractor price=0)
    local proc_show
    proc_show=$(jj "$db" "$home_carol" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 0 ] \
        && ok "contractor_subcall.caller_process_unchanged" \
        || fail "contractor_subcall.caller_process_unchanged" "expected 0, got: $proc_show"

    # @alice.available = 0 (started 50, spent 50 on ephemeral sub-call)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "contractor_subcall.alice_debited" \
        || fail "contractor_subcall.alice_debited" "expected 0, got: $alice_me"

    # @bob.available = 50 (received net from sub-call, no fee)
    local bob_me
    bob_me=$(jj "$db" "$home_bob" user me)
    [ "$(numfield "$bob_me" "available")" -eq 50 ] \
        && ok "contractor_subcall.bob_credited" \
        || fail "contractor_subcall.bob_credited" "expected 50, got: $bob_me"

    stop_backend "$backend_pid"
}

flow_contractor_failure() {
    echo "=== FLOW contractor_failure ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "contractor_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    # @alice has 0 credits (no deposit) → sub-call will fail with ErrInsufficientFunds
    j "$db" "$home_sys" admin user deposit --handle @carol --amount 200 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @bob creates sub-target (price=50)
    local sub_out sub_id
    sub_out=$(jj "$db" "$home_bob" action add --name /sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "ID")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action grant-all --id "$sub_id" >/dev/null 2>&1

    # @alice creates contractor WASM (price=0)
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action add --name /contractor --kind wasm \
        --source "$contractor_wasm" --price 0 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "ID")
    j "$db" "$home_alice" action enable   --id "$cont_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$cont_id" >/dev/null 2>&1

    # @carol starts process (100 funds, contractor price=0 so nothing locked)
    local proc_out proc_id
    j "$db" "$home_sys" admin user deposit --handle @carol --amount 0 >/dev/null 2>&1
    proc_out=$(jj "$db" "$home_carol" process start --funds 100)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call contractor → @alice has 0 credits → ephemeral process creation fails
    local call_out
    call_out=$(j "$db" "$home_carol" call \
        --process "$proc_id" --target @alice --action /contractor --args '{}' 2>&1)
    echo "$call_out" | grep -qi "insufficient\|balance\|funds" \
        && ok "contractor_failure.error_returned" \
        || fail "contractor_failure.error_returned" "expected insufficient-funds error, got: $call_out"

    # @carol process.available unchanged (nothing was locked for price=0 contractor)
    local proc_show
    proc_show=$(jj "$db" "$home_carol" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "Available")" -eq 100 ] \
        && ok "contractor_failure.caller_process_unchanged" \
        || fail "contractor_failure.caller_process_unchanged" "expected 100, got: $proc_show"

    # @alice.available = 0 (unchanged, no deposit made)
    local alice_me
    alice_me=$(jj "$db" "$home_alice" user me)
    [ "$(numfield "$alice_me" "available")" -eq 0 ] \
        && ok "contractor_failure.alice_unchanged" \
        || fail "contractor_failure.alice_unchanged" "expected 0, got: $alice_me"

    stop_backend "$backend_pid"
}

flow_event_queue_success() {
    echo "=== FLOW event_queue_success ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "event_queue_success.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates handler action (price=0) and a listener (source=@bob, event=test.evt)
    local handler_out handler_id
    handler_out=$(jj "$db" "$home_alice" action add --name /handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "ID")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" events listen \
        --source @bob --event test.evt --action "$handler_id")
    listener_id=$(strfield "$listen_out" "ID")

    # @bob emits the event
    local emit_out event_id
    emit_out=$(jj "$db" "$home_bob" events emit --event test.evt --args '{}')
    event_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit_out" 2>/dev/null)
    [ -n "$event_id" ] \
        && ok "event_queue_success.emit_returns_id" \
        || fail "event_queue_success.emit_returns_id" "emit returned no event_id: $emit_out"

    # @alice polls → 1 pending event
    local poll_out event_count
    poll_out=$(jj "$db" "$home_alice" events poll --id "$listener_id")
    event_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('events',[])))" \
        "$poll_out" 2>/dev/null || echo 0)
    [ "$event_count" -eq 1 ] \
        && ok "event_queue_success.poll_returns_event" \
        || fail "event_queue_success.poll_returns_event" "expected 1, got: $poll_out"

    # @alice consumes the event (action price=0, no funds needed)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    local consume_out consume_tx
    consume_out=$(jj "$db" "$home_alice" events consume --id "$event_id" --process "$proc_id")
    consume_tx=$(strfield "$consume_out" "tx_id")
    [ -n "$consume_tx" ] \
        && ok "event_queue_success.consume_returns_tx" \
        || fail "event_queue_success.consume_returns_tx" "consume returned no tx_id: $consume_out"

    # @alice polls again → 0 pending events
    local poll2_out event_count2
    poll2_out=$(jj "$db" "$home_alice" events poll --id "$listener_id")
    event_count2=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('events',[])))" \
        "$poll2_out" 2>/dev/null || echo 0)
    [ "$event_count2" -eq 0 ] \
        && ok "event_queue_success.poll_empty_after_consume" \
        || fail "event_queue_success.poll_empty_after_consume" "expected 0, got: $poll2_out"

    # @alice unlistens
    local unlisten_out
    unlisten_out=$(j "$db" "$home_alice" events unlisten --id "$listener_id")
    echo "$unlisten_out" | grep -q "Listener deactivated" \
        && ok "event_queue_success.unlisten" \
        || fail "event_queue_success.unlisten" "unexpected unlisten output: $unlisten_out"

    stop_backend "$backend_pid"
}

flow_event_queue_failure() {
    echo "=== FLOW event_queue_failure ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "event_queue_failure.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @carol --amount 10 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates handler action and listener (source=@bob)
    local handler_out handler_id
    handler_out=$(jj "$db" "$home_alice" action add --name /handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "ID")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" events listen \
        --source @bob --event fail.evt --action "$handler_id")
    listener_id=$(strfield "$listen_out" "ID")

    # @bob emits event_1; @alice consumes it
    local emit1_out event1_id
    emit1_out=$(jj "$db" "$home_bob" events emit --event fail.evt --args '{}')
    event1_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit1_out" 2>/dev/null)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    jj "$db" "$home_alice" events consume --id "$event1_id" --process "$proc_id" >/dev/null 2>&1

    # Consume event_1 again → ErrInvalidState (already consumed)
    local consume2_out
    consume2_out=$(j "$db" "$home_alice" events consume --id "$event1_id" --process "$proc_id" 2>&1)
    echo "$consume2_out" | grep -qi "invalid.state\|already.consumed\|in-flight" \
        && ok "event_queue_failure.double_consume_rejected" \
        || fail "event_queue_failure.double_consume_rejected" "expected invalid state, got: $consume2_out"

    # @bob emits event_2; @alice polls to get id
    local emit2_out event2_id
    emit2_out=$(jj "$db" "$home_bob" events emit --event fail.evt --args '{}')
    event2_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit2_out" 2>/dev/null)

    # @carol (non-owner) tries to consume event_2 → ErrUnauthorized
    local carol_proc_out carol_proc_id
    carol_proc_out=$(jj "$db" "$home_carol" process start --funds 0)
    carol_proc_id=$(strfield "$carol_proc_out" "process_id")
    local consume3_out
    consume3_out=$(j "$db" "$home_carol" events consume --id "$event2_id" --process "$carol_proc_id" 2>&1)
    echo "$consume3_out" | grep -qi "unauthorized\|permission\|owner" \
        && ok "event_queue_failure.non_owner_rejected" \
        || fail "event_queue_failure.non_owner_rejected" "expected unauthorized, got: $consume3_out"

    stop_backend "$backend_pid"
}

flow_event_deletion_restart() {
    echo "=== FLOW event_deletion_restart ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "event_deletion_restart.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates handler + listener (source=@bob)
    local handler_out handler_id
    handler_out=$(jj "$db" "$home_alice" action add --name /handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "ID")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" events listen \
        --source @bob --event restart.evt --action "$handler_id")
    listener_id=$(strfield "$listen_out" "ID")

    # @bob emits; @alice polls → 1 event
    jj "$db" "$home_bob" events emit --event restart.evt --args '{}' >/dev/null 2>&1
    local poll_out event_id event_count
    poll_out=$(jj "$db" "$home_alice" events poll --id "$listener_id")
    event_id=$(python3 -c "import sys,json; evs=json.loads(sys.argv[1]).get('events',[]); print(evs[0]['ID'] if evs else '')" \
        "$poll_out" 2>/dev/null)
    event_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('events',[])))" \
        "$poll_out" 2>/dev/null || echo 0)
    [ "$event_count" -eq 1 ] \
        && ok "event_deletion_restart.initial_poll" \
        || fail "event_deletion_restart.initial_poll" "expected 1, got: $poll_out"

    # Inject in-flight state: set consumed_at (but leave tx_id=NULL)
    python3 - "$db" "$event_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
conn.execute("UPDATE events SET consumed_at = datetime('now') WHERE id = ?", [sys.argv[2]])
conn.commit()
conn.close()
PYEOF

    # bootstrap_kernel on same DB → bootstrap() calls ResetInFlightEvents → consumed_at=NULL
    local port2
    port2=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port2" >/dev/null 2>&1

    # @alice polls again → event restored
    local poll2_out event_count2
    poll2_out=$(jj "$db" "$home_alice" events poll --id "$listener_id")
    event_count2=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('events',[])))" \
        "$poll2_out" 2>/dev/null || echo 0)
    [ "$event_count2" -eq 1 ] \
        && ok "event_deletion_restart.inflight_reset" \
        || fail "event_deletion_restart.inflight_reset" "expected 1 after reset, got: $poll2_out"

    # Unlisten → pending events purged; poll returns empty
    j "$db" "$home_alice" events unlisten --id "$listener_id" >/dev/null 2>&1
    local poll3_out event_count3
    poll3_out=$(jj "$db" "$home_alice" events poll --id "$listener_id")
    event_count3=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('events',[])))" \
        "$poll3_out" 2>/dev/null || echo 0)
    [ "$event_count3" -eq 0 ] \
        && ok "event_deletion_restart.unlisten_purges_events" \
        || fail "event_deletion_restart.unlisten_purges_events" "expected 0 after unlisten, got: $poll3_out"

    stop_backend "$backend_pid"
}

flow_rating() {
    echo "=== FLOW rating ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    port=$(alloc_port)
    backend_port=$(alloc_port)
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "rating.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 200 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice creates action (price=10)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action add --name /rate-me --kind http \
        --source "http://127.0.0.1:${backend_port}/rate" --price 10 --description "rateable action")
    action_id=$(strfield "$create_out" "ID")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob calls @alice's action → tx_id
    local proc_out proc_id call_out tx_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc_id=$(strfield "$proc_out" "process_id")
    call_out=$(jj "$db" "$home_bob" call \
        --process "$proc_id" --target @alice --action /rate-me --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")

    # @bob rates tx → success
    local rate_out
    rate_out=$(j "$db" "$home_bob" tx rate --id "$tx_id" --rating 1 2>&1)
    echo "$rate_out" | grep -q "rated" \
        && ok "rating.rate_succeeds" \
        || fail "rating.rate_succeeds" "unexpected rate output: $rate_out"

    # stats.rating_count = 1
    local stats_out
    stats_out=$(jj "$db" "$home_bob" stats show --action "$action_id")
    [ "$(numfield "$stats_out" "rating_count")" -eq 1 ] \
        && ok "rating.stats_updated" \
        || fail "rating.stats_updated" "expected rating_count=1, got: $stats_out"

    # Duplicate rate → ErrInvalidInput
    local rate2_out
    rate2_out=$(j "$db" "$home_bob" tx rate --id "$tx_id" --rating 0 2>&1)
    echo "$rate2_out" | grep -qi "invalid.input\|already.rated\|already" \
        && ok "rating.duplicate_rejected" \
        || fail "rating.duplicate_rejected" "expected already-rated error, got: $rate2_out"

    # @alice (non-buyer) rates → ErrUnauthorized
    local rate3_out
    rate3_out=$(j "$db" "$home_alice" tx rate --id "$tx_id" --rating 1 2>&1)
    echo "$rate3_out" | grep -qi "unauthorized\|buyer\|permission" \
        && ok "rating.non_buyer_rejected" \
        || fail "rating.non_buyer_rejected" "expected unauthorized, got: $rate3_out"

    stop_backend "$backend_pid"
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
    flow_process_lifecycle
    flow_process_funding
    flow_acl_public
    flow_successful_paid_call
    flow_failed_call_refund
    flow_input_schema_failure
    flow_output_schema_failure
    flow_wasm_execution
    flow_contractor_subcall
    flow_contractor_failure
    flow_event_queue_success
    flow_event_queue_failure
    flow_event_deletion_restart
    flow_rating

    echo ""
    echo "Results: ${PASS} passed, ${FAIL} failed"
    if [ "$FAIL" -gt 0 ]; then
        printf "Failures:%b\n" "$ERRS"
        exit 1
    fi
}

main
