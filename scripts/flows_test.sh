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
_ALLOC_PORT=0
alloc_port() { _ALLOC_PORT=$_NEXT_PORT; _NEXT_PORT=$((_NEXT_PORT + 1)); }

# ---------------------------------------------------------------------------
# Assertion helpers
# ---------------------------------------------------------------------------
ok()   { echo "  PASS: $1"; ((PASS++)); }
fail() { echo "  FAIL: $1 — $2"; ((FAIL++)); ERRS="${ERRS}\n  [$1] $2"; }

# ---------------------------------------------------------------------------
# Config file helpers
# ---------------------------------------------------------------------------

# write_test_config db [key=value ...]
# Writes juice.json next to the db file with test defaults and optional overrides.
# Keys: fee_bps fee_recipient script_timeout_ms (all others use defaults).
write_test_config() {
    local db="$1"; shift
    local fee_bps=0 fee_recipient="" script_timeout_ms=10000
    for arg in "$@"; do
        case "$arg" in
            fee_bps=*)           fee_bps="${arg#*=}" ;;
            fee_recipient=*)     fee_recipient="${arg#*=}" ;;
            script_timeout_ms=*) script_timeout_ms="${arg#*=}" ;;
        esac
    done
    cat > "$(dirname "$db")/juice.json" << EOF
{
  "ollama_url": "http://localhost:11434",
  "ollama_chat_model": "gemma4:26b",
  "ollama_embed_model": "nomic-embed-text",
  "script_timeout_ms": $script_timeout_ms,
  "script_memory_bytes": 67108864,
  "fee_bps": $fee_bps,
  "fee_recipient": "$fee_recipient",
  "token_ttl": "15m",
  "auth_issuer": "",
  "auth_audience": "",
  "log_level": "error",
  "log_file": "",
  "log_format": "text",
  "make_max_steps": 5,
  "allow_local_sources": true,
  "server_url": ""
}
EOF
}

# ---------------------------------------------------------------------------
# CLI wrappers
# j  db home [args...] — run juice against db with the given HOME
# jj db home [args...] — same with --output json
# ---------------------------------------------------------------------------
j() {
    local db="$1" home="$2"; shift 2
    HOME="$home" "$JUICE" --db "$db" "$@" 2>&1
}

# jj — JSON output; stderr suppressed so log lines don't corrupt JSON parsing.
jj() {
    local db="$1" home="$2"; shift 2
    HOME="$home" "$JUICE" --db "$db" --output json "$@" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------

# bootstrap_kernel db pass home port
# Writes the default config file, then starts juice serve briefly to trigger
# first-boot initialisation and stops it.
bootstrap_kernel() {
    local db="$1" pass="$2" home="$3" port="$4"
    write_test_config "$db"
    JUICE_BOOTSTRAP_PASSWORD="$pass" \
        HOME="$home" "$JUICE" --db "$db" serve --addr "127.0.0.1:$port" >/dev/null 2>&1 &
    local pid=$!
    local deadline=$(( $(date +%s) + 15 ))
    until curl -sf "http://127.0.0.1:${port}/health" >/dev/null 2>&1; do
        if ! kill -0 "$pid" 2>/dev/null; then
            wait "$pid" 2>/dev/null; return 1
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 1
        fi
        sleep 0.2
    done
    if ! kill -0 "$pid" 2>/dev/null; then
        return 1
    fi
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 0
}

# start_serve db addr pass home
# Starts juice serve in the background, polls /health. Sets SERVE_PID.
SERVE_PID=""
start_serve() {
    local db="$1" addr="$2" pass="$3" home="$4"
    JUICE_BOOTSTRAP_PASSWORD="$pass" \
        HOME="$home" "$JUICE" --db "$db" serve --addr "$addr" >/dev/null 2>&1 &
    SERVE_PID=$!
    local deadline=$(( $(date +%s) + 15 ))
    until curl -sf "http://${addr}/health" >/dev/null 2>&1; do
        if ! kill -0 "$SERVE_PID" 2>/dev/null; then
            wait "$SERVE_PID" 2>/dev/null; return 1
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            kill "$SERVE_PID" 2>/dev/null; return 1
        fi
        sleep 0.2
    done
    if ! kill -0 "$SERVE_PID" 2>/dev/null; then
        return 1
    fi
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
import sys, http.server, socket
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
srv = http.server.HTTPServer(('127.0.0.1', port), H)
srv.serve_forever()
PYEOF
    BACKEND_PID=$!
    local deadline=$(( $(date +%s) + 5 ))
    until curl -sf -X POST "http://127.0.0.1:${port}/" -d '{}' -H 'Content-Type: application/json' >/dev/null 2>&1; do
        if ! kill -0 "$BACKEND_PID" 2>/dev/null; then return 1; fi
        if [ "$(date +%s)" -ge "$deadline" ]; then kill "$BACKEND_PID" 2>/dev/null; return 1; fi
        sleep 0.05
    done
}

stop_backend() {
    local pid="${1:-$BACKEND_PID}"
    kill "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; return 0
}

# start_api_server port spec_file
# Serves spec_file on GET and '{"message":"ok"}' on POST. Sets API_SERVER_PID.
API_SERVER_PID=""
start_api_server() {
    local port="$1" spec_file="$2"
    python3 - "$port" "$spec_file" <<'PYEOF' &
import sys, http.server
port, spec_file = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(spec_file, 'rb') as f: body = f.read()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(body)
    def do_POST(self):
        n = int(self.headers.get('Content-Length', 0))
        self.rfile.read(n)
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(b'{"message":"ok"}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    API_SERVER_PID=$!
    local deadline=$(( $(date +%s) + 5 ))
    until curl -sf "http://127.0.0.1:${port}/" >/dev/null 2>&1; do
        if ! kill -0 "$API_SERVER_PID" 2>/dev/null; then return 1; fi
        if [ "$(date +%s)" -ge "$deadline" ]; then kill "$API_SERVER_PID" 2>/dev/null; return 1; fi
        sleep 0.05
    done
}

stop_api_server() {
    local pid="${1:-$API_SERVER_PID}"
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

types   = leb128u(3) + b'\x60\x04\x7f\x7f\x7f\x7f\x01\x7e' + b'\x60\x01\x7f\x01\x7f' + b'\x60\x02\x7f\x7f\x01\x7e'
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
    j "$db" "$home_alice" action grant-all --id "$tx_action_id" >/dev/null 2>&1

    j "$db" "$home_bob"   auth login --handle @bob --password bobpass >/dev/null 2>&1
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start)
    proc_id=$(strfield "$proc_out" "process_id")
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" call --process "$proc_id" --action "@alice/callable" --args '{}')
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
    alloc_port; port=$_ALLOC_PORT
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
    [ "$(numfield "$proc_show" "available")" -eq 300 ] \
        && ok "process_lifecycle.process_available" \
        || fail "process_lifecycle.process_available" "expected 300, got: $proc_show"
    [ "$(strfield "$proc_show" "status")" = "open" ] \
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
    [ "$(strfield "$proc_show" "status")" = "closed" ] \
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
    alloc_port; port=$_ALLOC_PORT
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
    [ "$(numfield "$proc_show" "available")" -eq 200 ] \
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
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "acl_public.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1

    # @alice creates and enables an action pointing to unreachable backend (port 1)
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name target --kind http \
        --source "http://127.0.0.1:1/target" --price 0 --description "acl test")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable --id "$action_id" >/dev/null 2>&1

    # @bob starts a zero-funded process
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")

    # Without ACL: permission error
    local out
    out=$(j "$db" "$home_bob" call --process "$proc_id" --action @alice/target)
    echo "$out" | grep -qi "unauthorized\|permission\|error" \
        && ok "acl_public.no_acl_rejected" \
        || fail "acl_public.no_acl_rejected" "call without ACL succeeded: $out"

    # Grant call ACL — now @bob passes the permission check (fails at backend instead)
    j "$db" "$home_alice" action acl grant --id "$action_id" --user @bob --perm call >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --action @alice/target)
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.with_acl_passes_permission" \
        || fail "acl_public.with_acl_passes_permission" "permission check still failed after grant: $out"

    # Revoke — permission check enforced again
    j "$db" "$home_alice" action acl revoke --id "$action_id" --user @bob --perm call >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --action @alice/target)
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
    out=$(j "$db" "$home_bob" call --process "$proc_id" --action @alice/target)
    echo "$out" | grep -qiv "unauthorized\|permission denied" \
        && ok "acl_public.grant_all_passes_permission" \
        || fail "acl_public.grant_all_passes_permission" "grant-all still hit permission error: $out"

    # revoke-all: permission check enforced again
    j "$db" "$home_alice" action revoke-all --id "$action_id" >/dev/null 2>&1
    out=$(j "$db" "$home_bob" call --process "$proc_id" --action @alice/target)
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name pay --kind http \
        --source "http://127.0.0.1:${backend_port}/pay" --price 100 --description "paid action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # Get @sys ID and starting balance for fee accounting
    local sys_show sys_id sys_start
    sys_show=$(jj "$db" "$home_sys" admin user show --handle @sys)
    sys_id=$(strfield "$sys_show" "id")
    sys_start=$(numfield "$sys_show" "available")

    # @bob starts process with 300 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call with fee_bps=2000 → fee=20, net=80, gross=100
    write_test_config "$db" "fee_bps=2000" "fee_recipient=$sys_id"
    local call_out tx_id
    call_out=$(HOME="$home_bob" \
        "$JUICE" --db "$db" --output json call \
        --process "$proc_id" --action @alice/pay --args '{}' 2>/dev/null)
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_paid_call.call_succeeded" \
        || fail "successful_paid_call.call_succeeded" "call returned no tx_id: $call_out"

    # tx fields
    local tx_show
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(numfield "$tx_show" "gross")" -eq 100 ] \
        && ok "successful_paid_call.tx_gross" \
        || fail "successful_paid_call.tx_gross" "expected Gross=100, got: $tx_show"
    [ "$(numfield "$tx_show" "fee")" -eq 20 ] \
        && ok "successful_paid_call.tx_fee" \
        || fail "successful_paid_call.tx_fee" "expected Fee=20, got: $tx_show"
    [ "$(numfield "$tx_show" "net")" -eq 80 ] \
        && ok "successful_paid_call.tx_net" \
        || fail "successful_paid_call.tx_net" "expected Net=80, got: $tx_show"
    [ "$(strfield "$tx_show" "status")" = "success" ] \
        && ok "successful_paid_call.tx_status" \
        || fail "successful_paid_call.tx_status" "expected success, got: $tx_show"

    # Process debited by 100 (300 - 100 = 200)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 200 ] \
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
    sys_end=$(numfield "$(jj "$db" "$home_sys" admin user show --handle @sys)" "available")
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name fail --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 100 --description "failing action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 300 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call — backend returns 500 → execution failure
    j "$db" "$home_bob" call --process "$proc_id" --action @alice/fail --args '{}' \
        >/dev/null 2>&1

    # Process available unchanged (full refund)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 300 ] \
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
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "failed_call_refund.failure_tx_recorded" \
        || fail "failed_call_refund.failure_tx_recorded" "expected >=1 tx, count=$tx_count list=$tx_list"

    # tx.Status = failure
    local tx_id tx_show
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
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
    alloc_port; port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name schema-in --kind http \
        --source "http://127.0.0.1:1/schema-in" --price 50 --description "schema test" \
        --input-schema '{"type":"object","properties":{"x":{"type":"string","description":"the x parameter"}},"required":["x"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 200 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 200)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call without required field "x" → schema error before any fund lock
    local call_out
    call_out=$(j "$db" "$home_bob" call \
        --process "$proc_id" --action @alice/schema-in --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|required\|error" \
        && ok "input_schema_failure.error_returned" \
        || fail "input_schema_failure.error_returned" "expected schema error, got: $call_out"

    # Process available unchanged (no debit happened)
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 200 ] \
        && ok "input_schema_failure.process_unchanged" \
        || fail "input_schema_failure.process_unchanged" "expected 200, got: $proc_show"

    # No tx created
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name schema-out --kind http \
        --source "http://127.0.0.1:${backend_port}/schema-out" --price 50 --description "schema out test" \
        --output-schema '{"type":"object","properties":{"id":{"type":"string","description":"the record id"}},"required":["id"]}')
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob starts process with 200 funds
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 200)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call — execution runs, output schema check fails → CommitFailedCall
    local call_out
    call_out=$(j "$db" "$home_bob" call \
        --process "$proc_id" --action @alice/schema-out --args '{}')
    echo "$call_out" | grep -qi "schema\|invalid\|error" \
        && ok "output_schema_failure.error_returned" \
        || fail "output_schema_failure.error_returned" "expected schema error, got: $call_out"

    # Full refund — process.available unchanged
    local proc_show
    proc_show=$(jj "$db" "$home_bob" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 200 ] \
        && ok "output_schema_failure.process_refunded" \
        || fail "output_schema_failure.process_refunded" "expected 200, got: $proc_show"

    # Failure tx IS recorded (unlike input schema failure)
    local tx_list tx_count
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$tx_list" 2>/dev/null || echo 0)
    [ "$tx_count" -ge 1 ] \
        && ok "output_schema_failure.failure_tx_recorded" \
        || fail "output_schema_failure.failure_tx_recorded" "expected >=1 tx, count=$tx_count"

    # tx.Status = failure
    local tx_id tx_show
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_show=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    [ "$(strfield "$tx_show" "status")" = "failure" ] \
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
    alloc_port; port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name echo --kind wasm \
        --source "$echo_wasm" --price 10 --description "echo wasm")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # ArtifactHash is set after enable (WASM compiled on activation)
    local action_show artifact_hash
    action_show=$(jj "$db" "$home_alice" action show --id "$action_id")
    artifact_hash=$(strfield "$action_show" "artifact_hash")
    [ -n "$artifact_hash" ] \
        && ok "wasm_execution.artifact_hash" \
        || fail "wasm_execution.artifact_hash" "expected non-empty ArtifactHash: $action_show"

    # @bob calls echo WASM
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc_id=$(strfield "$proc_out" "process_id")
    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" call --process "$proc_id" \
        --action @alice/echo --args '{"msg":"hello"}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "wasm_execution.echo_call_succeeds" \
        || fail "wasm_execution.echo_call_succeeds" "echo call returned no tx_id: $call_out"

    # Infinite-loop WASM → timeout error
    local loop_wasm="$dir/loop.wasm"
    make_infinite_loop_wasm "$loop_wasm"
    local loop_out loop_id
    loop_out=$(jj "$db" "$home_alice" action create --name loop --kind wasm \
        --source "$loop_wasm" --price 10 --description "infinite loop")
    loop_id=$(strfield "$loop_out" "id")
    j "$db" "$home_alice" action enable   --id "$loop_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$loop_id" >/dev/null 2>&1

    local proc2_out proc2_id timeout_out
    proc2_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc2_id=$(strfield "$proc2_out" "process_id")
    write_test_config "$db" "script_timeout_ms=200"
    timeout_out=$(HOME="$home_bob" "$JUICE" --db "$db" call \
        --process "$proc2_id" --action @alice/loop --args '{}' 2>&1)
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    sub_out=$(jj "$db" "$home_bob" action create --name sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "id")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action grant-all --id "$sub_id" >/dev/null 2>&1

    # @alice creates contractor WASM (price=0) that calls @bob/sub-target
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action create --name contractor --kind wasm \
        --source "$contractor_wasm" --price 0 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "id")
    j "$db" "$home_alice" action enable   --id "$cont_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$cont_id" >/dev/null 2>&1

    # @carol calls contractor (price=0 → process needs 0 funds)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_carol" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    local call_out tx_id
    call_out=$(jj "$db" "$home_carol" call \
        --process "$proc_id" --action @alice/contractor --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "contractor_subcall.call_succeeds" \
        || fail "contractor_subcall.call_succeeds" "contractor call returned no tx_id: $call_out"

    # @carol process.available unchanged (contractor price=0)
    local proc_show
    proc_show=$(jj "$db" "$home_carol" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 0 ] \
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    sub_out=$(jj "$db" "$home_bob" action create --name sub-target --kind http \
        --source "http://127.0.0.1:${backend_port}/sub" --price 50 --description "sub target")
    sub_id=$(strfield "$sub_out" "id")
    j "$db" "$home_bob" action enable   --id "$sub_id" >/dev/null 2>&1
    j "$db" "$home_bob" action grant-all --id "$sub_id" >/dev/null 2>&1

    # @alice creates contractor WASM (price=0)
    local contractor_wasm="$dir/contractor.wasm"
    make_contractor_wasm "$contractor_wasm" "@bob/sub-target"
    local cont_out cont_id
    cont_out=$(jj "$db" "$home_alice" action create --name contractor --kind wasm \
        --source "$contractor_wasm" --price 0 --description "contractor wasm")
    cont_id=$(strfield "$cont_out" "id")
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
        --process "$proc_id" --action @alice/contractor --args '{}' 2>&1)
    echo "$call_out" | grep -qi "insufficient\|balance\|funds" \
        && ok "contractor_failure.error_returned" \
        || fail "contractor_failure.error_returned" "expected insufficient-funds error, got: $call_out"

    # @carol process.available unchanged (nothing was locked for price=0 contractor)
    local proc_show
    proc_show=$(jj "$db" "$home_carol" process show --id "$proc_id")
    [ "$(numfield "$proc_show" "available")" -eq 100 ] \
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    handler_out=$(jj "$db" "$home_alice" action create --name handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "id")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" listener create \
        --source-user @bob --event test.evt --action @alice/handler)
    listener_id=$(strfield "$listen_out" "id")

    # @bob emits the event
    local emit_out event_id
    emit_out=$(jj "$db" "$home_bob" event emit --event test.evt --args '{}')
    event_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit_out" 2>/dev/null)
    [ -n "$event_id" ] \
        && ok "event_queue_success.emit_returns_id" \
        || fail "event_queue_success.emit_returns_id" "emit returned no event_id: $emit_out"

    # @alice polls → 1 pending event
    local poll_out event_count
    poll_out=$(jj "$db" "$home_alice" event list --listener "$listener_id")
    event_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" \
        "$poll_out" 2>/dev/null || echo 0)
    [ "$event_count" -eq 1 ] \
        && ok "event_queue_success.poll_returns_event" \
        || fail "event_queue_success.poll_returns_event" "expected 1, got: $poll_out"

    # @alice consumes the event (action price=0, no funds needed)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    local consume_out consume_tx
    consume_out=$(jj "$db" "$home_alice" event consume --id "$event_id" --process "$proc_id")
    consume_tx=$(strfield "$consume_out" "tx_id")
    [ -n "$consume_tx" ] \
        && ok "event_queue_success.consume_returns_tx" \
        || fail "event_queue_success.consume_returns_tx" "consume returned no tx_id: $consume_out"

    # @alice polls again → 0 pending events
    local poll2_out event_count2
    poll2_out=$(jj "$db" "$home_alice" event list --listener "$listener_id")
    event_count2=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" \
        "$poll2_out" 2>/dev/null || echo 0)
    [ "$event_count2" -eq 0 ] \
        && ok "event_queue_success.poll_empty_after_consume" \
        || fail "event_queue_success.poll_empty_after_consume" "expected 0, got: $poll2_out"

    # @alice deletes the listener
    local unlisten_out
    unlisten_out=$(j "$db" "$home_alice" listener delete --id "$listener_id")
    echo "$unlisten_out" | grep -q "deleted" \
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    handler_out=$(jj "$db" "$home_alice" action create --name handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "id")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" listener create \
        --source-user @bob --event fail.evt --action @alice/handler)
    listener_id=$(strfield "$listen_out" "id")

    # @bob emits event_1; @alice consumes it
    local emit1_out event1_id
    emit1_out=$(jj "$db" "$home_bob" event emit --event fail.evt --args '{}')
    event1_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit1_out" 2>/dev/null)
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")
    jj "$db" "$home_alice" event consume --id "$event1_id" --process "$proc_id" >/dev/null 2>&1

    # Consume event_1 again → ErrInvalidState (already consumed)
    local consume2_out
    consume2_out=$(j "$db" "$home_alice" event consume --id "$event1_id" --process "$proc_id" 2>&1)
    echo "$consume2_out" | grep -qi "invalid.state\|already.consumed\|in-flight" \
        && ok "event_queue_failure.double_consume_rejected" \
        || fail "event_queue_failure.double_consume_rejected" "expected invalid state, got: $consume2_out"

    # @bob emits event_2; @alice polls to get id
    local emit2_out event2_id
    emit2_out=$(jj "$db" "$home_bob" event emit --event fail.evt --args '{}')
    event2_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['event_ids'][0])" \
        "$emit2_out" 2>/dev/null)

    # @carol (non-owner) tries to consume event_2 → ErrUnauthorized
    local carol_proc_out carol_proc_id
    carol_proc_out=$(jj "$db" "$home_carol" process start --funds 0)
    carol_proc_id=$(strfield "$carol_proc_out" "process_id")
    local consume3_out
    consume3_out=$(j "$db" "$home_carol" event consume --id "$event2_id" --process "$carol_proc_id" 2>&1)
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    handler_out=$(jj "$db" "$home_alice" action create --name handler --kind http \
        --source "http://127.0.0.1:${backend_port}/handler" --price 0 --description "event handler")
    handler_id=$(strfield "$handler_out" "id")
    j "$db" "$home_alice" action enable   --id "$handler_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$handler_id" >/dev/null 2>&1

    local listen_out listener_id
    listen_out=$(jj "$db" "$home_alice" listener create \
        --source-user @bob --event restart.evt --action @alice/handler)
    listener_id=$(strfield "$listen_out" "id")

    # @bob emits; @alice polls → 1 event
    jj "$db" "$home_bob" event emit --event restart.evt --args '{}' >/dev/null 2>&1
    local poll_out event_id event_count
    poll_out=$(jj "$db" "$home_alice" event list --listener "$listener_id")
    event_id=$(python3 -c "import sys,json; evs=json.loads(sys.argv[1]); print(evs[0]['id'] if evs else '')" \
        "$poll_out" 2>/dev/null)
    event_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" \
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
    alloc_port; port2=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port2" >/dev/null 2>&1

    # @alice polls again → event restored
    local poll2_out event_count2
    poll2_out=$(jj "$db" "$home_alice" event list --listener "$listener_id")
    event_count2=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" \
        "$poll2_out" 2>/dev/null || echo 0)
    [ "$event_count2" -eq 1 ] \
        && ok "event_deletion_restart.inflight_reset" \
        || fail "event_deletion_restart.inflight_reset" "expected 1 after reset, got: $poll2_out"

    # Delete listener → pending events purged; poll returns empty
    j "$db" "$home_alice" listener delete --id "$listener_id" >/dev/null 2>&1
    local poll3_out event_count3
    poll3_out=$(jj "$db" "$home_alice" event list --listener "$listener_id")
    event_count3=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" \
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
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
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
    create_out=$(jj "$db" "$home_alice" action create --name rate-me --kind http \
        --source "http://127.0.0.1:${backend_port}/rate" --price 10 --description "rateable action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob calls @alice's action → tx_id
    local proc_out proc_id call_out tx_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 100)
    proc_id=$(strfield "$proc_out" "process_id")
    call_out=$(jj "$db" "$home_bob" call \
        --process "$proc_id" --action @alice/rate-me --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")

    # Before rating: detail response has null rating field.
    local show_before
    show_before=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    python3 -c "import sys,json; d=json.loads(sys.argv[1]); assert d.get('rating') is None, d" \
        "$show_before" 2>/dev/null \
        && ok "rating.unrated_null" \
        || fail "rating.unrated_null" "expected null rating before rating, got: $show_before"

    # @bob rates tx with a note → success
    local rate_out
    rate_out=$(j "$db" "$home_bob" tx rate --id "$tx_id" --rating 1 --note "great service" 2>&1)
    echo "$rate_out" | grep -q "rated" \
        && ok "rating.rate_succeeds" \
        || fail "rating.rate_succeeds" "unexpected rate output: $rate_out"

    # stats.rating_count = 1
    local stats_out
    stats_out=$(jj "$db" "$home_bob" action stats --id "$action_id")
    [ "$(numfield "$stats_out" "rating_count")" -eq 1 ] \
        && ok "rating.stats_updated" \
        || fail "rating.stats_updated" "expected rating_count=1, got: $stats_out"

    # Buyer (@bob) sees embedded rating with value and note in detail.
    local show_buyer
    show_buyer=$(jj "$db" "$home_bob" tx show --id "$tx_id")
    python3 -c "
import sys, json
d = json.loads(sys.argv[1])
r = d.get('rating')
assert r is not None, 'rating is null'
assert r['value'] == 1, f'value={r[\"value\"]}'
assert r['note'] == 'great service', f'note={r[\"note\"]}'
" "$show_buyer" 2>/dev/null \
        && ok "rating.buyer_sees_embedded_rating" \
        || fail "rating.buyer_sees_embedded_rating" "buyer detail wrong: $show_buyer"

    # Seller (@alice) also sees embedded rating with note in detail.
    local show_seller
    show_seller=$(jj "$db" "$home_alice" tx show --id "$tx_id")
    python3 -c "
import sys, json
d = json.loads(sys.argv[1])
r = d.get('rating')
assert r is not None, 'rating is null'
assert r['value'] == 1, f'value={r[\"value\"]}'
assert r['note'] == 'great service', f'note={r[\"note\"]}'
" "$show_seller" 2>/dev/null \
        && ok "rating.seller_sees_embedded_rating" \
        || fail "rating.seller_sees_embedded_rating" "seller detail wrong: $show_seller"

    # Embedded rating also appears in the list response.
    local list_out
    list_out=$(jj "$db" "$home_bob" tx list)
    python3 -c "
import sys, json
txs = json.loads(sys.argv[1])
match = next((t for t in txs if t['id'] == sys.argv[2]), None)
assert match is not None, 'tx not in list'
r = match.get('rating')
assert r is not None, 'rating is null in list'
assert r['value'] == 1, f'value={r[\"value\"]}'
" "$list_out" "$tx_id" 2>/dev/null \
        && ok "rating.embedded_in_list" \
        || fail "rating.embedded_in_list" "list view wrong: $list_out"

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
# BATCH 4 — PKCE, Refresh, Receipts, LLM, OpenAPI
# ===========================================================================

flow_pkce_auth() {
    echo "=== FLOW pkce_auth ==="
    local dir db home_sys home_alice port serve_port addr
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; serve_port=$_ALLOC_PORT
    addr="127.0.0.1:$serve_port"
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "pkce_auth.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1

    start_serve "$db" "$addr" syspass "$home_sys"
    local serve_pid=$SERVE_PID
    trap "rm -rf '$dir'; kill '$serve_pid' 2>/dev/null; wait '$serve_pid' 2>/dev/null" RETURN

    # Generate PKCE verifier + challenge
    local verifier challenge
    verifier=$(python3 -c "import secrets; print(secrets.token_urlsafe(32))")
    challenge=$(python3 -c "
import sys, hashlib, base64
v = sys.argv[1].encode()
print(base64.urlsafe_b64encode(hashlib.sha256(v).digest()).rstrip(b'=').decode())
" "$verifier")

    # Authorize → get code
    local auth_resp code
    auth_resp=$(curl -sf -X POST "http://$addr/v1/auth/authorize" \
        -H "Content-Type: application/json" \
        -d "{\"handle\":\"@alice\",\"password\":\"alicepass\",\"code_challenge\":\"$challenge\"}" 2>/dev/null)
    code=$(python3 -c "
import sys, urllib.parse, json
d = json.loads(sys.argv[1])
qs = urllib.parse.urlparse(d['redirect']).query
print(urllib.parse.parse_qs(qs)['code'][0])
" "$auth_resp" 2>/dev/null)

    # Exchange code → access_token
    local token_resp access_token
    token_resp=$(curl -sf -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"code_verifier\":\"$verifier\"}" 2>/dev/null)
    access_token=$(strfield "$token_resp" "access_token")
    [ -n "$access_token" ] \
        && ok "pkce_auth.token_obtained" \
        || fail "pkce_auth.token_obtained" "no access_token in: $token_resp"

    # Code reuse → rejected (code already marked used)
    local reuse_resp
    reuse_resp=$(curl -s -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code\",\"code_verifier\":\"$verifier\"}" 2>/dev/null)
    echo "$reuse_resp" | grep -qi "invalid\|error\|unauthenticated" \
        && ok "pkce_auth.code_reuse_rejected" \
        || fail "pkce_auth.code_reuse_rejected" "expected error on reuse, got: $reuse_resp"

    # Wrong verifier → rejected
    local auth_resp2 code2 bad_resp
    auth_resp2=$(curl -sf -X POST "http://$addr/v1/auth/authorize" \
        -H "Content-Type: application/json" \
        -d "{\"handle\":\"@alice\",\"password\":\"alicepass\",\"code_challenge\":\"$challenge\"}" 2>/dev/null)
    code2=$(python3 -c "
import sys, urllib.parse, json
d = json.loads(sys.argv[1])
qs = urllib.parse.urlparse(d['redirect']).query
print(urllib.parse.parse_qs(qs)['code'][0])
" "$auth_resp2" 2>/dev/null)
    bad_resp=$(curl -s -X POST "http://$addr/v1/auth/token" \
        -H "Content-Type: application/json" \
        -d "{\"grant_type\":\"authorization_code\",\"code\":\"$code2\",\"code_verifier\":\"wrong-verifier-value\"}" 2>/dev/null)
    echo "$bad_resp" | grep -qi "invalid\|error\|unauthenticated" \
        && ok "pkce_auth.wrong_verifier_rejected" \
        || fail "pkce_auth.wrong_verifier_rejected" "expected error for wrong verifier, got: $bad_resp"

    stop_serve "$serve_pid"
}

flow_refresh_rotation() {
    echo "=== FLOW refresh_rotation ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "refresh_rotation.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Capture RT1 before refresh
    local rt1
    rt1=$(cat "$home_alice/.juice/refresh_token" 2>/dev/null)

    # Refresh → RT1 rotated to RT2
    local refresh_out
    refresh_out=$(j "$db" "$home_alice" auth refresh 2>&1)
    echo "$refresh_out" | grep -q "refreshed" \
        && ok "refresh_rotation.refresh_succeeds" \
        || fail "refresh_rotation.refresh_succeeds" "unexpected output: $refresh_out"

    local rt2
    rt2=$(cat "$home_alice/.juice/refresh_token" 2>/dev/null)

    # Old RT1 rejected
    local home_old="$dir/old"; mkdir -p "$home_old/.juice"
    echo "$rt1" > "$home_old/.juice/refresh_token"
    local bad_refresh
    bad_refresh=$(j "$db" "$home_old" auth refresh 2>&1)
    echo "$bad_refresh" | grep -qi "invalid\|expired\|unauthenticated" \
        && ok "refresh_rotation.old_rt_rejected" \
        || fail "refresh_rotation.old_rt_rejected" "expected invalid/expired, got: $bad_refresh"

    # Logout (revokes RT2) → RT2 rejected
    j "$db" "$home_alice" auth logout >/dev/null 2>&1
    local home_rt2="$dir/rt2"; mkdir -p "$home_rt2/.juice"
    echo "$rt2" > "$home_rt2/.juice/refresh_token"
    local after_logout
    after_logout=$(j "$db" "$home_rt2" auth refresh 2>&1)
    echo "$after_logout" | grep -qi "invalid\|expired\|unauthenticated" \
        && ok "refresh_rotation.revoked_rt_rejected" \
        || fail "refresh_rotation.revoked_rt_rejected" "expected invalid after logout, got: $after_logout"
}

flow_successful_receipt() {
    echo "=== FLOW successful_receipt ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "successful_receipt.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 100 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name receipt-action --kind http \
        --source "http://127.0.0.1:${backend_port}/act" --price 10 --description "receipt test action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 50)
    proc_id=$(strfield "$proc_out" "process_id")

    local call_out tx_id
    call_out=$(jj "$db" "$home_bob" call \
        --process "$proc_id" --action @alice/receipt-action --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "successful_receipt.call_succeeded" \
        || fail "successful_receipt.call_succeeded" "no tx_id: $call_out"

    # Verify receipt record was created in DB
    local receipt_id
    receipt_id=$(python3 - "$db" "$tx_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT id FROM receipts WHERE tx_id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row else "")
PYEOF
)
    [ -n "$receipt_id" ] \
        && ok "successful_receipt.receipt_created" \
        || fail "successful_receipt.receipt_created" "no receipt for tx_id=$tx_id"

    stop_backend "$backend_pid"
}

flow_failed_receipt() {
    echo "=== FLOW failed_receipt ==="
    local dir db home_sys home_alice home_bob port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "failed_receipt.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 100 >/dev/null 2>&1

    start_backend "$backend_port" 500 '{"error":"backend error"}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name fail-action --kind http \
        --source "http://127.0.0.1:${backend_port}/fail" --price 10 --description "fail action")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    local proc_out proc_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 50)
    proc_id=$(strfield "$proc_out" "process_id")

    # Failing call — ignore error, tx is recorded in DB
    j "$db" "$home_bob" call --process "$proc_id" --action @alice/fail-action --args '{}' \
        >/dev/null 2>&1 || true

    # Find the failed tx
    local tx_list tx_id tx_status
    tx_list=$(jj "$db" "$home_bob" tx list --process "$proc_id")
    tx_id=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['id'])" "$tx_list" 2>/dev/null)
    tx_status=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])[0]['status'])" "$tx_list" 2>/dev/null)
    [ "$tx_status" = "failure" ] \
        && ok "failed_receipt.failure_tx_recorded" \
        || fail "failed_receipt.failure_tx_recorded" "expected failure status, got: $tx_list"

    # Verify receipt was also created for the failed tx
    local receipt_id
    receipt_id=$(python3 - "$db" "$tx_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT id FROM receipts WHERE tx_id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row else "")
PYEOF
)
    [ -n "$receipt_id" ] \
        && ok "failed_receipt.receipt_created_for_failure" \
        || fail "failed_receipt.receipt_created_for_failure" "no receipt for failed tx_id=$tx_id"

    stop_backend "$backend_pid"
}

flow_lookup() {
    echo "=== FLOW lookup ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "lookup.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # @sys/lookup price=0; start a zero-fund process
    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")

    # Missing required 'query' field → schema violation
    local schema_out
    schema_out=$(j "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/lookup \
        --args '{}' 2>&1)
    echo "$schema_out" | grep -qi "query\|required\|schema" \
        && ok "lookup.missing_query_rejected" \
        || fail "lookup.missing_query_rejected" "expected schema/query error, got: $schema_out"
}

flow_chat() {
    echo "=== FLOW chat ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "chat.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call without chatter → ErrInvalidState
    local chat_out
    chat_out=$(j "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/llm-chat \
        --args '{"messages":[{"role":"user","content":"hello"}]}' 2>&1)
    echo "$chat_out" | grep -qi "chat\|invalid.state\|invalid_state" \
        && ok "chat.no_chatter_error" \
        || fail "chat.no_chatter_error" "expected ErrInvalidState, got: $chat_out"

    # Missing required 'messages' field → schema violation
    local schema_out
    schema_out=$(j "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/llm-chat \
        --args '{}' 2>&1)
    echo "$schema_out" | grep -qi "messages\|required\|schema" \
        && ok "chat.missing_messages_rejected" \
        || fail "chat.missing_messages_rejected" "expected schema/messages error, got: $schema_out"
}

flow_openapi_import_execute() {
    echo "=== FLOW openapi_import_execute ==="
    local dir db home_sys home_alice home_bob port api_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_import_execute.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 50 >/dev/null 2>&1

    # Write spec to file; start combined spec+backend server
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" <<'PYEOF'
import json, sys
api_port, spec_file = sys.argv[1], sys.argv[2]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": "Say hello",
                "x-juice-price": 5,
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # Import spec → created=1
    local import_out created_count action_id action_name
    import_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    created_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('Created',[])))" \
        "$import_out" 2>/dev/null || echo 0)
    [ "$created_count" -eq 1 ] \
        && ok "openapi_import_execute.import_created_1" \
        || fail "openapi_import_execute.import_created_1" "expected 1 created, got: $import_out"

    action_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['id'])" \
        "$import_out" 2>/dev/null)
    action_name=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['name'])" \
        "$import_out" 2>/dev/null)

    # Enable + grant-all (requires x-juice-owner for public access)
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob calls the imported action
    local proc_out proc_id call_out tx_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 20)
    proc_id=$(strfield "$proc_out" "process_id")
    call_out=$(jj "$db" "$home_bob" call \
        --process "$proc_id" --action "@alice/$action_name" --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "openapi_import_execute.call_succeeds" \
        || fail "openapi_import_execute.call_succeeds" "no tx_id: $call_out"

    # Action name is the operation_key; owner is encoded in owner_user_id only.
    [ "$action_name" = "greet" ] \
        && ok "openapi_import_execute.action_name_correct" \
        || fail "openapi_import_execute.action_name_correct" "expected greet, got: $action_name"

    stop_api_server "$api_pid"
}

flow_openapi_changed_reimport() {
    echo "=== FLOW openapi_changed_reimport ==="
    local dir db home_sys home_alice port api_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_changed_reimport.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Write v1 spec; start server
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" "Say hello v1" <<'PYEOF'
import json, sys
api_port, spec_file, desc = sys.argv[1], sys.argv[2], sys.argv[3]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": desc,
                "x-juice-price": 5,
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # First import
    local import1_out action_id
    import1_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    action_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Created'][0]['id'])" \
        "$import1_out" 2>/dev/null)

    # Update spec on disk (change description → hash changes)
    python3 - "$api_port" "$spec_file" "Say hello v2" <<'PYEOF'
import json, sys
api_port, spec_file, desc = sys.argv[1], sys.argv[2], sys.argv[3]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": desc,
                "x-juice-price": 5,
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {
                                "schema": {"type": "object", "properties": {"message": {"type": "string", "description": "the response message"}}}
                            }
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF

    # Reimport → updated=1 (description changed → hash changed → action deactivated)
    local import2_out updated_count
    import2_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    updated_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]).get('Updated',[])))" \
        "$import2_out" 2>/dev/null || echo 0)
    [ "$updated_count" -eq 1 ] \
        && ok "openapi_changed_reimport.reimport_updated_1" \
        || fail "openapi_changed_reimport.reimport_updated_1" "expected 1 updated, got: $import2_out"

    # Action is now inactive
    local action_show active
    action_show=$(jj "$db" "$home_alice" action show --id "$action_id")
    active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$action_show" 2>/dev/null)
    [ "$active" = "False" ] \
        && ok "openapi_changed_reimport.action_deactivated" \
        || fail "openapi_changed_reimport.action_deactivated" "expected False, got: $action_show"

    # Action ID unchanged
    local updated_id
    updated_id=$(python3 -c "import sys,json; r=json.loads(sys.argv[1]); print(r['Updated'][0]['id'])" \
        "$import2_out" 2>/dev/null)
    [ "$updated_id" = "$action_id" ] \
        && ok "openapi_changed_reimport.action_id_preserved" \
        || fail "openapi_changed_reimport.action_id_preserved" "expected $action_id, got: $updated_id"

    stop_api_server "$api_pid"
}

flow_openapi_unimport() {
    echo "=== FLOW openapi_unimport ==="
    local dir db home_sys home_alice port api_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; api_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "openapi_unimport.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    # Write spec with 2 operations
    local spec_file="$dir/spec.json"
    python3 - "$api_port" "$spec_file" <<'PYEOF'
import json, sys
api_port, spec_file = sys.argv[1], sys.argv[2]
spec = {
    "openapi": "3.0.0",
    "info": {"title": "Test API", "version": "1.0.0"},
    "x-juice-owner": "@alice",
    "servers": [{"url": f"http://127.0.0.1:{api_port}"}],
    "paths": {
        "/greet": {
            "post": {
                "operationId": "greet",
                "description": "Say hello",
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {"schema": {"type": "object"}}
                        }
                    }
                }
            }
        },
        "/farewell": {
            "post": {
                "operationId": "farewell",
                "description": "Say goodbye",
                "responses": {
                    "200": {
                        "content": {
                            "application/json": {"schema": {"type": "object"}}
                        }
                    }
                }
            }
        }
    }
}
with open(spec_file, 'w') as f:
    json.dump(spec, f)
PYEOF
    start_api_server "$api_port" "$spec_file"
    local api_pid=$API_SERVER_PID
    trap "rm -rf '$dir'; kill '$api_pid' 2>/dev/null; wait '$api_pid' 2>/dev/null" RETURN

    # Import 2 operations
    local import_out greet_id
    import_out=$(JUICE_ALLOW_LOCAL_SOURCES=true jj "$db" "$home_alice" action import --openapi "http://127.0.0.1:${api_port}/")
    greet_id=$(python3 -c "
import sys,json
r=json.loads(sys.argv[1])
for a in r.get('Created',[]):
    if 'greet' in a['name']: print(a['id']); break
" "$import_out" 2>/dev/null)

    # Create a manual (non-OpenAPI) action
    local manual_out manual_id
    manual_out=$(jj "$db" "$home_alice" action create --name manual --kind http \
        --source "http://127.0.0.1:${api_port}/manual" --price 0 --description "manual action")
    manual_id=$(strfield "$manual_out" "id")
    j "$db" "$home_alice" action enable --id "$manual_id" >/dev/null 2>&1

    # Unimport → both OpenAPI actions deactivated
    local unimport_out
    unimport_out=$(j "$db" "$home_alice" action unimport \
        --openapi "http://127.0.0.1:${api_port}/" 2>&1)
    echo "$unimport_out" | grep -q "deactivated 2" \
        && ok "openapi_unimport.two_deactivated" \
        || fail "openapi_unimport.two_deactivated" "expected 'deactivated 2', got: $unimport_out"

    # Greet action is now inactive
    local greet_show greet_active
    greet_show=$(jj "$db" "$home_alice" action show --id "$greet_id")
    greet_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$greet_show" 2>/dev/null)
    [ "$greet_active" = "False" ] \
        && ok "openapi_unimport.openapi_action_deactivated" \
        || fail "openapi_unimport.openapi_action_deactivated" "expected False, got: $greet_show"

    # Manual action still active
    local manual_show manual_active
    manual_show=$(jj "$db" "$home_alice" action show --id "$manual_id")
    manual_active=$(python3 -c "import sys,json; print(json.loads(sys.argv[1])['active'])" "$manual_show" 2>/dev/null)
    [ "$manual_active" = "True" ] \
        && ok "openapi_unimport.manual_action_unaffected" \
        || fail "openapi_unimport.manual_action_unaffected" "expected True, got: $manual_show"

    stop_api_server "$api_pid"
}

# ---------------------------------------------------------------------------
# Batch 5: Federation and Admin
# ---------------------------------------------------------------------------

# _fed_setup dir_var db_l db_r home_l home_r port_l port_r port_b
# Common federation setup: two bootstrapped kernels, backend, serves started,
# both registered as peers, /greet imported and enabled on LOCAL.
# Returns proxy_id via stdout (last line of output).
_fed_setup() {
    local dir="$1" db_l="$2" db_r="$3" home_l="$4" home_r="$5"
    local port_l="$6" port_r="$7" port_b="$8"

    mkdir -p "$home_l/.juice" "$home_r/.juice"

    local boot_l boot_r
    alloc_port; boot_l=$_ALLOC_PORT; alloc_port; boot_r=$_ALLOC_PORT
    bootstrap_kernel "$db_l" syspass "$home_l" "$boot_l" || return 1
    bootstrap_kernel "$db_r" syspass "$home_r" "$boot_r" || return 1

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
    j "$db_r" "$home_r" action enable --id "$action_id_r" >/dev/null 2>&1
    j "$db_r" "$home_r" action grant-all --id "$action_id_r" >/dev/null 2>&1

    # Start both serves
    start_serve "$db_l" "127.0.0.1:$port_l" syspass "$home_l" \
        || { echo "_fed_setup: start_serve local ($port_l) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_l"
    start_serve "$db_r" "127.0.0.1:$port_r" syspass "$home_r" \
        || { echo "_fed_setup: start_serve remote ($port_r) failed" >&2; return 1; }
    echo "$SERVE_PID" > "$dir/pid_r"

    # Register each as peer of the other
    local ra_out
    ra_out=$(j "$db_l" "$home_l" remote add --url "http://127.0.0.1:$port_r" 2>&1)
    echo "$ra_out" | grep -q "Registered" || { echo "_fed_setup: remote add l->r failed: $ra_out" >&2; return 1; }
    j "$db_r" "$home_r" remote add --url "http://127.0.0.1:$port_l" >/dev/null 2>&1

    # Import /greet from REMOTE into LOCAL
    local remote_handle="@127.0.0.1:$port_r"
    local ri_out
    ri_out=$(j "$db_l" "$home_l" remote import --remote "$remote_handle" --action greet 2>&1)
    echo "$ri_out" | grep -qi "imported\|unchanged" || { echo "_fed_setup: remote import failed: $ri_out" >&2; return 1; }

    # Find proxy action ID on LOCAL
    local proxy_id
    proxy_id=$(python3 - "$db_l" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT id FROM actions WHERE name='greet' AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1").fetchone()
conn.close()
print(row[0] if row else "")
PYEOF
)
    # Enable proxy and grant-all (superuser can admin remote proxy)
    j "$db_l" "$home_l" action enable --id "$proxy_id" >/dev/null 2>&1
    j "$db_l" "$home_l" action grant-all --id "$proxy_id" >/dev/null 2>&1

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
    local dir db_l db_r home_l home_r port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    db_l="$dir/local.db"; db_r="$dir/remote.db"
    home_l="$dir/lsys";   home_r="$dir/rsys"
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$db_l" "$db_r" "$home_l" "$home_r" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_import.setup" "setup failed"; return; }
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)
    [ -n "$proxy_id" ] || { fail "fed_import.setup" "no proxy_id"; return; }

    local remote_handle="@127.0.0.1:$port_r"

    # Start process for @sys on LOCAL (price=0, no funds needed)
    local proc_out proc_id
    proc_out=$(jj "$db_l" "$home_l" process start --funds 0)
    proc_id=$(strfield "$proc_out" "process_id")

    # Call the proxy
    local call_out tx_id
    call_out=$(jj "$db_l" "$home_l" call \
        --process "$proc_id" \
        --action "$remote_handle/greet" \
        --args '{}')
    tx_id=$(strfield "$call_out" "tx_id")
    [ -n "$tx_id" ] \
        && ok "fed_import.call_succeeds" \
        || fail "fed_import.call_succeeds" "no tx_id in: $call_out"

    # Verify remote_receipt_hash and remote_receipt_json stored in local db
    local rrh rrj
    rrh=$(python3 - "$db_l" "$tx_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT remote_receipt_hash FROM transactions WHERE id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row and row[0] else "")
PYEOF
)
    [ -n "$rrh" ] \
        && ok "fed_import.remote_receipt_hash" \
        || fail "fed_import.remote_receipt_hash" "remote_receipt_hash empty for tx $tx_id"

    rrj=$(python3 - "$db_l" "$tx_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT remote_receipt_json FROM transactions WHERE id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row and row[0] else "")
PYEOF
)
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
    local dir db_l db_r home_l home_r port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    db_l="$dir/local.db"; db_r="$dir/remote.db"
    home_l="$dir/lsys";   home_r="$dir/rsys"
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$db_l" "$db_r" "$home_l" "$home_r" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_reimport.setup" "setup failed"; return; }
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)

    local remote_handle="@127.0.0.1:$port_r"

    # Make one call so there's a tx in history
    local proc_id tx_id
    proc_id=$(strfield "$(jj "$db_l" "$home_l" process start --funds 0)" "process_id")
    tx_id=$(strfield "$(jj "$db_l" "$home_l" call \
        --process "$proc_id" \
        --action "$remote_handle/greet" --args '{}')" "tx_id")

    # Update action description on REMOTE (stop serve, update db, restart)
    local pid_r
    pid_r=$(cat "$dir/pid_r")
    kill "$pid_r" 2>/dev/null; wait "$pid_r" 2>/dev/null

    # Change description directly in REMOTE db
    python3 - "$db_r" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
conn.execute("UPDATE actions SET description='v2 greeting' WHERE name='greet'")
conn.commit()
conn.close()
PYEOF

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
    local proxy_active
    proxy_active=$(python3 - "$db_l" "$proxy_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT active FROM actions WHERE id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row else -1)
PYEOF
)
    [ "$proxy_active" = "0" ] \
        && ok "fed_reimport.proxy_deactivated" \
        || fail "fed_reimport.proxy_deactivated" "expected active=0, got $proxy_active"

    # Proxy ID preserved
    local new_proxy_id
    new_proxy_id=$(python3 - "$db_l" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT id FROM actions WHERE name='greet' AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1").fetchone()
conn.close()
print(row[0] if row else "")
PYEOF
)
    [ "$new_proxy_id" = "$proxy_id" ] \
        && ok "fed_reimport.id_preserved" \
        || fail "fed_reimport.id_preserved" "expected $proxy_id, got $new_proxy_id"

    # Prior tx still in history
    local tx_check
    tx_check=$(python3 - "$db_l" "$tx_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT id FROM transactions WHERE id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row else "")
PYEOF
)
    [ -n "$tx_check" ] \
        && ok "fed_reimport.tx_history_intact" \
        || fail "fed_reimport.tx_history_intact" "prior tx $tx_id missing from local db"
}

flow_federation_unimport() {
    echo "=== FLOW federation_unimport ==="
    local dir db_l db_r home_l home_r port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    db_l="$dir/local.db"; db_r="$dir/remote.db"
    home_l="$dir/lsys";   home_r="$dir/rsys"
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$db_l" "$db_r" "$home_l" "$home_r" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_unimport.setup" "setup failed"; return; }
    proxy_id=$(cat "$dir/proxy_id" 2>/dev/null)

    local remote_handle="@127.0.0.1:$port_r"

    # Unimport
    local unimport_out
    unimport_out=$(j "$db_l" "$home_l" remote unimport --remote "$remote_handle" --action greet 2>&1)
    echo "$unimport_out" | grep -qi "deactivated" \
        && ok "fed_unimport.deactivated" \
        || fail "fed_unimport.deactivated" "expected deactivated, got: $unimport_out"

    # Proxy inactive on LOCAL
    local proxy_active
    proxy_active=$(python3 - "$db_l" "$proxy_id" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT active FROM actions WHERE id=?", [sys.argv[2]]).fetchone()
conn.close()
print(row[0] if row else -1)
PYEOF
)
    [ "$proxy_active" = "0" ] \
        && ok "fed_unimport.proxy_inactive" \
        || fail "fed_unimport.proxy_inactive" "expected active=0, got $proxy_active"

    # Remote action still active
    local remote_active
    remote_active=$(python3 - "$db_r" <<'PYEOF'
import sqlite3, sys
conn = sqlite3.connect(sys.argv[1])
row = conn.execute("SELECT active FROM actions WHERE name='greet' AND deleted_at IS NULL LIMIT 1").fetchone()
conn.close()
print(row[0] if row else -1)
PYEOF
)
    [ "$remote_active" = "1" ] \
        && ok "fed_unimport.remote_still_active" \
        || fail "fed_unimport.remote_still_active" "expected remote active=1, got $remote_active"
}

# flow_federation_replay has been moved to TestFederationReplay in cmd/juice/cmd_remote_test.go
# using crypto/ed25519 — the previous implementation required Python nacl.signing.

flow_fed_verify_receipt() {
    echo "=== FLOW fed_verify_receipt ==="
    local dir db_l db_r home_l home_r port_l port_r port_b proxy_id
    dir=$(mktemp -d); trap "_fed_teardown '$dir'; rm -rf '$dir'" RETURN
    db_l="$dir/local.db"; db_r="$dir/remote.db"
    home_l="$dir/lsys";   home_r="$dir/rsys"
    alloc_port; port_l=$_ALLOC_PORT;  alloc_port; port_r=$_ALLOC_PORT; alloc_port; port_b=$_ALLOC_PORT

    _fed_setup "$dir" "$db_l" "$db_r" "$home_l" "$home_r" "$port_l" "$port_r" "$port_b" \
        || { fail "fed_verify.setup" "setup failed"; return; }

    local remote_handle="@127.0.0.1:$port_r"

    # Make a call through the remote proxy.
    local proc_id tx_id
    proc_id=$(strfield "$(jj "$db_l" "$home_l" process start --funds 0)" "process_id")
    tx_id=$(strfield "$(jj "$db_l" "$home_l" call \
        --process "$proc_id" \
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
    local local_proc_id local_tx_id
    local_proc_id=$(strfield "$(jj "$db_l" "$home_l" process start --funds 0)" "process_id")
    local_tx_id=$(strfield "$(jj "$db_l" "$home_l" call \
        --process "$local_proc_id" \
        --action "@sys/lookup" \
        --args '{"query":"test"}' 2>/dev/null)" "tx_id")
    [ -n "$local_tx_id" ] || { fail "fed_verify.local_call" "local call failed"; return; }
    j "$db_l" "$home_l" tx verify-receipt --id "$local_tx_id" >/dev/null 2>&1 \
        && fail "fed_verify.local_tx_rejected" "expected error for non-remote-proxy tx, got success" \
        || ok "fed_verify.local_tx_rejected"
}

flow_transaction_access() {
    echo "=== FLOW transaction_access ==="
    local dir db home_sys home_alice home_bob home_carol port backend_port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    home_bob="$dir/bob";     mkdir -p "$home_bob/.juice"
    home_carol="$dir/carol"; mkdir -p "$home_carol/.juice"
    alloc_port; port=$_ALLOC_PORT
    alloc_port; backend_port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "tx_access.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys"   auth login --handle @sys   --password syspass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @bob   --email bob@test.com   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @carol --email carol@test.com --password carolpass >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_bob"   auth login --handle @bob   --password bobpass   >/dev/null 2>&1
    j "$db" "$home_carol" auth login --handle @carol --password carolpass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @bob --amount 300 >/dev/null 2>&1

    start_backend "$backend_port" 200 '{"ok":true}'
    local backend_pid=$BACKEND_PID
    trap "rm -rf '$dir'; kill '$backend_pid' 2>/dev/null; wait '$backend_pid' 2>/dev/null" RETURN

    # @alice (seller) creates a paid action (price=10) callable by anyone.
    local create_out action_id
    create_out=$(jj "$db" "$home_alice" action create --name pvd-action --kind http \
        --source "http://127.0.0.1:${backend_port}/pvd" --price 10 --description "tx access test")
    action_id=$(strfield "$create_out" "id")
    j "$db" "$home_alice" action enable   --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" action grant-all --id "$action_id" >/dev/null 2>&1

    # @bob (buyer) calls @alice's action 3 times.
    local proc_out proc_id i call_out a_tx_id
    proc_out=$(jj "$db" "$home_bob" process start --funds 200)
    proc_id=$(strfield "$proc_out" "process_id")
    for i in 1 2 3; do
        call_out=$(jj "$db" "$home_bob" call \
            --process "$proc_id" --action @alice/pvd-action --args '{}')
        a_tx_id=$(strfield "$call_out" "tx_id")
    done

    # @alice (seller) lists transactions for calls to her action — sees all 3.
    local alice_txs alice_count
    alice_txs=$(jj "$db" "$home_alice" tx list)
    alice_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$alice_txs" 2>/dev/null || echo 0)
    [ "$alice_count" -eq 3 ] \
        && ok "tx_access.seller_sees_all" \
        || fail "tx_access.seller_sees_all" "expected 3 txs for seller, got $alice_count: $alice_txs"

    # Sum of transaction net amounts equals @alice's credited balance.
    local alice_out alice_balance net_sum
    alice_out=$(jj "$db" "$home_alice" user me)
    alice_balance=$(numfield "$alice_out" "available")
    net_sum=$(python3 -c "import sys,json; rs=json.loads(sys.argv[1]); print(sum(t['net'] for t in rs))" \
        "$alice_txs" 2>/dev/null || echo -1)
    [ "$net_sum" -eq "$alice_balance" ] \
        && ok "tx_access.reconstructibility" \
        || fail "tx_access.reconstructibility" "tx net sum=$net_sum != alice balance=$alice_balance"

    # @bob (buyer) also sees the same 3 transactions.
    local bob_txs bob_count
    bob_txs=$(jj "$db" "$home_bob" tx list)
    bob_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$bob_txs" 2>/dev/null || echo 0)
    [ "$bob_count" -eq 3 ] \
        && ok "tx_access.buyer_sees_all" \
        || fail "tx_access.buyer_sees_all" "expected 3 txs for buyer, got $bob_count: $bob_txs"

    # @carol is not a party — sees none, and tx show returns not found.
    local carol_txs carol_count carol_show
    carol_txs=$(jj "$db" "$home_carol" tx list)
    carol_count=$(python3 -c "import sys,json; print(len(json.loads(sys.argv[1]) or []))" "$carol_txs" 2>/dev/null || echo -1)
    [ "$carol_count" -eq 0 ] \
        && ok "tx_access.non_party_sees_none" \
        || fail "tx_access.non_party_sees_none" "expected 0 txs for non-party, got $carol_count: $carol_txs"

    carol_show=$(j "$db" "$home_carol" tx show --id "$a_tx_id" 2>&1)
    echo "$carol_show" | grep -qi "not found" \
        && ok "tx_access.non_party_denied" \
        || fail "tx_access.non_party_denied" "expected not found, got: $carol_show"

    stop_backend "$backend_pid"
}

flow_admin_supervision() {
    echo "=== FLOW admin_supervision ==="
    local dir db home_sys home_alice
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    local port
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "admin.boot" "bootstrap failed"; return; }

    j "$db" "$home_sys" auth login --handle @sys --password syspass >/dev/null 2>&1

    # Create alice
    local alice_id
    j "$db" "$home_sys" user create \
        --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    alice_id=$(strfield "$(jj "$db" "$home_sys" admin user show --handle @alice)" "id")

    # admin user list shows both
    local user_list
    user_list=$(jj "$db" "$home_sys" admin user list)
    echo "$user_list" | python3 -c "
import sys,json
users = json.load(sys.stdin)
handles = [u.get('handle','') for u in users]
assert '@sys' in handles and '@alice' in handles, f'missing user in {handles}'
" 2>/dev/null \
        && ok "admin.user_list" \
        || fail "admin.user_list" "expected @sys and @alice in list"

    # admin user show --handle @alice
    local show_out
    show_out=$(jj "$db" "$home_sys" admin user show --handle @alice)
    echo "$show_out" | python3 -c "
import sys,json; d=json.load(sys.stdin); assert d.get('handle')=='@alice'
" 2>/dev/null \
        && ok "admin.user_show" \
        || fail "admin.user_show" "expected Handle=@alice, got: $show_out"

    # Suspend alice
    j "$db" "$home_sys" admin user suspend --id "$alice_id" >/dev/null 2>&1
    local me_out
    me_out=$(j "$db" "$home_alice" user me)
    echo "$me_out" | grep -qi "suspended\|unauthenticated\|invalid\|error" \
        && ok "admin.suspend_blocks_alice" \
        || fail "admin.suspend_blocks_alice" "expected auth failure, got: $me_out"

    # Unsuspend alice
    j "$db" "$home_sys" admin user unsuspend --id "$alice_id" >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    local me2_out
    me2_out=$(jj "$db" "$home_alice" user me)
    [ "$(strfield "$me2_out" "handle")" = "@alice" ] \
        && ok "admin.unsuspend_restores_alice" \
        || fail "admin.unsuspend_restores_alice" "expected alice me, got: $me2_out"

    # Create and enable an action for admin action tests
    local action_id
    local bport; alloc_port; bport=$_ALLOC_PORT
    start_backend "$bport" 200 '{"ok":true}' \
        || { fail "admin.backend" "backend failed to start"; return; }
    local bpid=$BACKEND_PID
    trap "kill '$bpid' 2>/dev/null; wait '$bpid' 2>/dev/null; rm -rf '$dir'" RETURN
    action_id=$(strfield "$(jj "$db" "$home_sys" action create \
        --name test --kind http \
        --source "http://127.0.0.1:$bport" \
        --description "admin test action" --price 0)" "id")
    j "$db" "$home_sys" action enable --id "$action_id" >/dev/null 2>&1

    # admin action list shows the action
    local act_list
    act_list=$(jj "$db" "$home_sys" admin action list)
    echo "$act_list" | python3 -c "
import sys,json; ids=[a['id'] for a in json.load(sys.stdin)]
assert sys.argv[1] in ids
" "$action_id" 2>/dev/null \
        && ok "admin.action_list" \
        || fail "admin.action_list" "action $action_id not in list"

    # admin action disable
    j "$db" "$home_sys" admin action disable --id "$action_id" >/dev/null 2>&1
    local show_action
    show_action=$(jj "$db" "$home_sys" action show --id "$action_id")
    echo "$show_action" | python3 -c "
import sys,json; d=json.load(sys.stdin); assert not d.get('active'), f'still active: {d}'
" 2>/dev/null \
        && ok "admin.action_disable" \
        || fail "admin.action_disable" "action still active after disable: $show_action"

    # Re-enable and call to create a process and tx for admin list tests
    j "$db" "$home_sys" action enable --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_sys" action grant-all --id "$action_id" >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys" admin user deposit --handle @alice --amount 100 >/dev/null 2>&1
    local proc_id
    proc_id=$(strfield "$(jj "$db" "$home_alice" process start --funds 50)" "process_id")
    jj "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/test --args '{}' >/dev/null 2>&1

    # admin process list shows the process
    local proc_list
    proc_list=$(jj "$db" "$home_sys" admin process list)
    echo "$proc_list" | python3 -c "
import sys,json; ids=[p['id'] for p in json.load(sys.stdin)]
assert sys.argv[1] in ids
" "$proc_id" 2>/dev/null \
        && ok "admin.process_list" \
        || fail "admin.process_list" "process $proc_id not in list"

    # admin tx list shows transactions
    local tx_list
    tx_list=$(jj "$db" "$home_sys" admin tx list)
    echo "$tx_list" | python3 -c "
import sys,json; txs=json.load(sys.stdin); assert len(txs)>0
" 2>/dev/null \
        && ok "admin.tx_list" \
        || fail "admin.tx_list" "expected at least one tx, got: $tx_list"

    # Non-sys user rejected from admin commands
    local alice_admin_out
    alice_admin_out=$(j "$db" "$home_alice" admin user list 2>&1)
    echo "$alice_admin_out" | grep -qi "unauthorized\|superuser" \
        && ok "admin.non_sys_rejected" \
        || fail "admin.non_sys_rejected" "expected rejection, got: $alice_admin_out"

    stop_backend "$bpid"
}

flow_make() {
    echo "=== FLOW make ==="
    local dir db home_sys home_alice port
    dir=$(mktemp -d); trap "rm -rf '$dir'" RETURN
    db="$dir/juice.db"
    home_sys="$dir/sys";     mkdir -p "$home_sys/.juice"
    home_alice="$dir/alice"; mkdir -p "$home_alice/.juice"
    alloc_port; port=$_ALLOC_PORT
    bootstrap_kernel "$db" syspass "$home_sys" "$port" \
        || { fail "make.boot" "bootstrap failed"; return; }

    # @sys/make must be registered, active, and public after bootstrap.
    local actions_out make_json
    actions_out=$(jj "$db" "$home_sys" action list 2>/dev/null)
    make_json=$(echo "$actions_out" | python3 -c "
import sys,json
actions = json.load(sys.stdin)
m = next((a for a in actions if a.get('name') == 'make'), None)
print(json.dumps(m) if m else 'null')
" 2>/dev/null)
    [ "$make_json" != "null" ] && [ -n "$make_json" ] \
        && ok "make.registered" \
        || fail "make.registered" "@sys/make not found in action list"

    echo "$make_json" | python3 -c "import sys,json; a=json.load(sys.stdin); assert a.get('active') and a.get('public')" 2>/dev/null \
        && ok "make.active_public" \
        || fail "make.active_public" "@sys/make not active+public: $make_json"

    local price
    price=$(echo "$make_json" | python3 -c "import sys,json; print(json.load(sys.stdin).get('price',0))" 2>/dev/null)
    [ "$price" = "20" ] \
        && ok "make.price_20" \
        || fail "make.price_20" "expected price 20, got: $price"

    # Set up alice with credits.
    j "$db" "$home_sys"   auth login --handle @sys --password syspass >/dev/null 2>&1
    j "$db" "$home_sys"   user create --handle @alice --email alice@test.com --password alicepass >/dev/null 2>&1
    j "$db" "$home_sys"   admin user deposit --handle @alice --amount 500 >/dev/null 2>&1
    j "$db" "$home_alice" auth login --handle @alice --password alicepass >/dev/null 2>&1

    local proc_out proc_id
    proc_out=$(jj "$db" "$home_alice" process start --funds 300)
    proc_id=$(strfield "$proc_out" "process_id")

    # Missing description → schema violation before any execution.
    local no_desc_out
    no_desc_out=$(j "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/make \
        --args '{}' 2>&1)
    echo "$no_desc_out" | grep -qi "description\|required\|schema" \
        && ok "make.missing_description_rejected" \
        || fail "make.missing_description_rejected" "expected schema error, got: $no_desc_out"

    # Call with description only (the only accepted input).
    # Succeeds as a kernel call regardless of whether tinygo/Ollama is available.
    # Returns {status: "success"|"failure", diagnostics: [...]} — never a hard kernel error.
    local make_out make_result status
    make_out=$(j "$db" "$home_alice" call \
        --process "$proc_id" --action @sys/make \
        --args '{"description": "Return a fixed greeting message that says Hello followed by the name"}' 2>&1)
    make_result=$(echo "$make_out" | python3 -c "
import sys, json, re
text = sys.stdin.read()
# Extract JSON object from the result: section
m = re.search(r'result:\n(\{.*\})', text, re.DOTALL)
if m:
    try: print(json.dumps(json.loads(m.group(1))))
    except: print('{}')
else:
    print('{}')
" 2>/dev/null)
    status=$(echo "$make_result" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
    [ "$status" = "success" ] || [ "$status" = "failure" ] \
        && ok "make.returns_structured_result" \
        || fail "make.returns_structured_result" "expected success|failure status, got: $make_out"

    # Transactions must have been recorded for the process (make + any sub-calls).
    local tx_out tx_count
    tx_out=$(jj "$db" "$home_alice" tx list 2>/dev/null)
    tx_count=$(echo "$tx_out" | python3 -c "
import sys,json
txs = json.load(sys.stdin)
print(sum(1 for t in txs if t.get('process_id') == sys.argv[1]))
" "$proc_id" 2>/dev/null)
    [ "${tx_count:-0}" -ge 1 ] \
        && ok "make.transaction_recorded" \
        || fail "make.transaction_recorded" "expected >=1 tx for process, got count=$tx_count"
}

# ===========================================================================
# Main runner
# ===========================================================================
main() {
    # Kill all stray juice serves and Python test-backends from interrupted runs.
    pkill -9 -f "juice_b5" 2>/dev/null || true
    pkill -9 -f "python3 - [0-9]" 2>/dev/null || true
    sleep 0.5

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
    flow_pkce_auth
    flow_refresh_rotation
    flow_successful_receipt
    flow_failed_receipt
    flow_lookup
    flow_chat
    flow_openapi_import_execute
    flow_openapi_changed_reimport
    flow_openapi_unimport
    flow_federation_import_execute
    flow_federation_changed_reimport
    flow_federation_unimport
    flow_fed_verify_receipt
    flow_transaction_access
    flow_admin_supervision
    flow_make

    echo ""
    echo "Results: ${PASS} passed, ${FAIL} failed"
    if [ "$FAIL" -gt 0 ]; then
        printf "Failures:%b\n" "$ERRS"
        exit 1
    fi
}

main
