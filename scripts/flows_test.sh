#!/usr/bin/env bash
# End-to-end flow tests for the juice CLI.
# Each flow is an independent user story with its own SQLite database.
#
# Usage (manual — default suite, EXCLUDES the slow @sys/make flows):
#   go build -o /tmp/juice ./cmd/juice/
#   JUICE=/tmp/juice JUICE_SECRET_KEY=test bash scripts/flows_test.sh
#
# Usage (the slow @sys/make flows ONLY — opt in when working on @sys/make):
#   JUICE_MAKE_FLOWS=1 JUICE=/tmp/juice JUICE_SECRET_KEY=test bash scripts/flows_test.sh
#   (real TinyGo + live Ollama; minutes per flow; see make_flows() below for why
#    they are kept out of the default run)
#
# Usage (via Go test suite):
#   go test ./cmd/juice/ -run TestFlowsIntegration -v -timeout 300s
#
# Requires: bash >=4, python3, curl
#
# IMPORTANT: These tests must only test the surface!

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
# Keys: fee_bps script_timeout_ms server_url make_max_steps (all others use defaults).
write_test_config() {
    local db="$1"; shift
    local fee_bps=0 script_timeout_ms=10000 server_url="" peer_handle="" make_max_steps=5
    for arg in "$@"; do
        case "$arg" in
            fee_bps=*)           fee_bps="${arg#*=}" ;;
            script_timeout_ms=*) script_timeout_ms="${arg#*=}" ;;
            server_url=*)        server_url="${arg#*=}" ;;
            peer_handle=*)       peer_handle="${arg#*=}" ;;
            make_max_steps=*)    make_max_steps="${arg#*=}" ;;
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
  "token_ttl": "15m",
  "auth_issuer": "",
  "auth_audience": "",
  "log_level": "error",
  "log_file": "",
  "log_format": "text",
  "make_max_steps": $make_max_steps,
  "allow_local_sources": true,
  "server_url": "$server_url",
  "peer_handle": "$peer_handle"
}
EOF
}

# ---------------------------------------------------------------------------
# CLI wrappers
# j  db home [args...] — run juice against db with the given HOME
# jj db home [args...] — same with --json
# ---------------------------------------------------------------------------
j() {
    local db="$1" home="$2"; shift 2
    HOME="$home" "$JUICE" --db "$db" "$@" 2>&1
}

# jj — JSON output; stderr suppressed so log lines don't corrupt JSON parsing.
jj() {
    local db="$1" home="$2"; shift 2
    HOME="$home" "$JUICE" --db "$db" --json "$@" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------

# bootstrap_kernel db pass home port [config_overrides...]
# Writes the default config file (with optional overrides), then starts juice serve briefly to trigger
# first-boot initialisation and stops it.
bootstrap_kernel() {
    local db="$1" pass="$2" home="$3" port="$4"; shift 4
    write_test_config "$db" "$@"
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

# juice_token_dir home db — mirrors tokenDir() in cmd/juice/main.go:
# $HOME/.juice/tokens/{sha256(abs(db))[:6] as 12 hex chars}/
juice_token_dir() {
    python3 -c "
import hashlib, os, sys
h = hashlib.sha256(os.path.abspath(sys.argv[2]).encode()).hexdigest()[:12]
print(os.path.join(sys.argv[1], '.juice', 'tokens', h))
" "$1" "$2"
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

# ---------------------------------------------------------------------------
# Load topic files (function definitions only; no shebang, no set -uo pipefail)
# ---------------------------------------------------------------------------
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$DIR/flows_foundation.sh"
source "$DIR/flows_calls.sh"
source "$DIR/flows_wasm.sh"
source "$DIR/flows_remote.sh"
source "$DIR/flows_federation.sh"
source "$DIR/flows_admin.sh"

# ===========================================================================
# @sys/make flows — ISOLATED FROM THE DEFAULT SUITE ON PURPOSE.
#
# These exercise real TinyGo compilation and a live Ollama model, so each takes
# minutes, and their value-correctness checks (does the synthesized action return
# the right number?) depend on nondeterministic local-LLM codegen. Running them as
# part of every juice change would make the suite slow and flaky.
#
# They are therefore OFF by default. Run them deliberately, ONLY when working on
# @sys/make, by setting JUICE_MAKE_FLOWS=1:
#
#   JUICE_MAKE_FLOWS=1 JUICE=/path/to/juice JUICE_SECRET_KEY=test bash scripts/flows_test.sh
#
# That runs ONLY the make flows (each bootstraps its own kernel, so nothing else is
# needed). DO NOT wire flow_make* back into the default branch of main().
# ===========================================================================
make_flows() {
    flow_make
    flow_make_calculator
    flow_make_translator
    flow_make_natural_language_calc
}

# ===========================================================================
# Main runner
# ===========================================================================
main() {
    # Kill all stray juice serves and Python test-backends from interrupted runs.
    pkill -9 -f "juice_b5" 2>/dev/null || true
    pkill -9 -f "python3 - [0-9]" 2>/dev/null || true
    sleep 0.5

    # Opt-in: run ONLY the slow @sys/make flows (see make_flows above).
    if [ "${JUICE_MAKE_FLOWS:-0}" = "1" ]; then
        echo "=== @sys/make flows ONLY (JUICE_MAKE_FLOWS=1) ==="
        make_flows
        echo ""
        echo "Results: ${PASS} passed, ${FAIL} failed"
        if [ "$FAIL" -gt 0 ]; then
            printf "Failures:%b\n" "$ERRS"
            exit 1
        fi
        return 0
    fi

    flow_bootstrap
    flow_local_auth
    flow_suspension
    flow_deposits
    flow_action_lifecycle
    flow_action_owner_visibility
    flow_process_lifecycle
    flow_acl_public
    flow_successful_paid_call
    flow_failed_call_refund
    flow_input_schema_failure
    flow_output_schema_failure
    flow_wasm_execution
    flow_contractor_subcall
    flow_contractor_failure
    flow_step_success
    flow_step_failure
    flow_step_restart
    flow_locked_funds_recovery
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
    flow_federation_unfriend
    flow_fed_verify_receipt
    flow_fed_all_receipt_checks
    flow_fed_denial_unfriended
    flow_fed_denial_underfunded
    flow_fed_import_duty
    flow_fed_failed_action_refund
    flow_fed_gossip_discovery
    flow_transaction_access
    flow_admin_supervision
    # NOTE: flow_make* are DELIBERATELY EXCLUDED from the default suite — they are
    # slow (real TinyGo + live Ollama) and their value-correctness checks depend on
    # nondeterministic local-LLM codegen. Run them on their own with JUICE_MAKE_FLOWS=1
    # (see make_flows() above). DO NOT add flow_make* calls back into this list.
    flow_time
    flow_message

    echo ""
    echo "Results: ${PASS} passed, ${FAIL} failed"
    if [ "$FAIL" -gt 0 ]; then
        printf "Failures:%b\n" "$ERRS"
        exit 1
    fi
}

main
