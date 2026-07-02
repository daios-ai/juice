#!/usr/bin/env bash
#
# flows/lib.sh — integration-test harness core.
#
# TWO INVARIANTS, both enforced structurally:
#   1. Tests drive ONLY the built binary ($JUICE). Bash physically cannot reach the kernel
#      in-process, so a flow can never bypass the binary. This is the whole point.
#   2. Every spawned process is killed. One registry + one trap; leaks are impossible.
#
# Servers bind an OS-assigned port (--addr 127.0.0.1:0) and announce it via a JSON
# `server.ready` log line the harness reads. No port allocator, no /dev/null, no polling
# a fixed port. Requires: bash>=4, python3, curl.

set -uo pipefail

JUICE="${JUICE:-$(command -v juice 2>/dev/null || true)}"
if [ ! -x "$JUICE" ]; then
    echo "JUICE binary not found. Set JUICE=/path/to/binary." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Counters
# ---------------------------------------------------------------------------
PASS=0; FAIL=0; ERRS=""
ok()   { echo "  PASS: $1"; PASS=$((PASS+1)); }
fail() { echo "  FAIL: $1 — $2"; FAIL=$((FAIL+1)); ERRS="${ERRS}\n  [$1] $2"; }

# ---------------------------------------------------------------------------
# Process/dir registry + cleanup. Every server and backend registers its PID here;
# `reap` (per-flow) and the EXIT/INT/TERM trap (whole run) kill them all, unconditionally.
# ---------------------------------------------------------------------------
declare -a _PIDS=()
declare -a _DIRS=()
declare -A SERVER_URL=()   # db path -> http://host:port of its running server
declare -A SERVER_PID=()   # db path -> serve pid

track_pid() { _PIDS+=("$1"); }
track_dir() { _DIRS+=("$1"); }

# reap: kill everything spawned so far (SIGKILL for fast, deterministic teardown — test DBs
# are disposable) and forget it. Called after every flow and by the exit trap.
reap() {
    local p
    for p in "${_PIDS[@]:-}"; do [ -n "$p" ] && kill -9 "$p" 2>/dev/null; done
    for p in "${_PIDS[@]:-}"; do [ -n "$p" ] && wait "$p" 2>/dev/null; done
    _PIDS=()
    SERVER_URL=()
    SERVER_PID=()
}
trap reap EXIT INT TERM

# new_dir — a temp dir tracked for cleanup; echoes its path.
new_dir() { local d; d=$(mktemp -d); track_dir "$d"; echo "$d"; }

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
# write_config db [key=value ...]  — juice.json next to db. server_url is intentionally
# empty: the server advertises its real bound address (works under --addr :0). log_format is
# json so `server.ready` is machine-readable; log_level info so it is emitted at all.
# Keys: fee_bps script_timeout_ms peer_handle make_max_steps.
write_config() {
    local db="$1"; shift
    local fee_bps=0 script_timeout_ms=10000 peer_handle="" make_max_steps=5
    local a
    for a in "$@"; do case "$a" in
        fee_bps=*)           fee_bps=${a#*=} ;;
        script_timeout_ms=*) script_timeout_ms=${a#*=} ;;
        peer_handle=*)       peer_handle=${a#*=} ;;
        make_max_steps=*)    make_max_steps=${a#*=} ;;
    esac; done
    cat > "$(dirname "$db")/juice.json" <<EOF
{
  "script_timeout_ms": $script_timeout_ms,
  "script_memory_bytes": 67108864,
  "fee_bps": $fee_bps,
  "token_ttl": "15m",
  "log_level": "info",
  "log_format": "json",
  "make_max_steps": $make_max_steps,
  "allow_local_sources": true,
  "server_url": "",
  "peer_handle": "$peer_handle"
}
EOF
}

# ---------------------------------------------------------------------------
# Server lifecycle
# ---------------------------------------------------------------------------
# start_server db home [cfg key=val ...]
# Boots `juice serve` on an OS-assigned port, captures its log, waits for the JSON
# `server.ready` line, records SERVER_URL[db]. On failure, dumps the captured log and
# returns 1 (never a silent timeout).
start_server() {
    local db="$1" home="$2"; shift 2
    write_config "$db" "$@"
    local log; log="$(dirname "$db")/server.log"
    JUICE_BOOTSTRAP_PASSWORD=syspass HOME="$home" \
        "$JUICE" --db "$db" serve --addr 127.0.0.1:0 >"$log" 2>&1 &
    local pid=$!; track_pid "$pid"
    local addr deadline=$(( $(date +%s) + 20 ))
    while :; do
        addr=$(sed -n 's/.*"msg":"server.ready".*"addr":"\([^"]*\)".*/\1/p' "$log" 2>/dev/null | head -1)
        [ -n "$addr" ] && break
        if ! kill -0 "$pid" 2>/dev/null; then
            echo "  server exited during boot (db=$db):" >&2; sed 's/^/    | /' "$log" >&2; return 1
        fi
        if [ "$(date +%s)" -ge "$deadline" ]; then
            echo "  server not ready within 20s (db=$db):" >&2; sed 's/^/    | /' "$log" >&2; return 1
        fi
        sleep 0.05
    done
    SERVER_URL["$db"]="http://$addr"
    SERVER_PID["$db"]="$pid"
    return 0
}

# stop_server db — stop the server for db (used by recovery flows that stop, inject DB
# state, then start_server again on the same db).
stop_server() {
    local pid="${SERVER_PID[$1]:-}"
    [ -n "$pid" ] && { kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; }
    unset "SERVER_URL[$1]" "SERVER_PID[$1]" 2>/dev/null
}

# ---------------------------------------------------------------------------
# CLI wrappers — always the built binary, targeting the db's server.
# admin/peer commands ignore JUICE_SERVER and run locally against --db (correct).
# ---------------------------------------------------------------------------
j()  { local db="$1" home="$2"; shift 2; HOME="$home" JUICE_SERVER="${SERVER_URL[$db]:-}" "$JUICE" --db "$db" "$@" 2>&1; }
jj() { local db="$1" home="$2"; shift 2; HOME="$home" JUICE_SERVER="${SERVER_URL[$db]:-}" "$JUICE" --db "$db" --json "$@" 2>/dev/null; }

# url db — the base URL of db's server (for curl-based HTTP-only assertions).
url() { echo "${SERVER_URL[$1]:-}"; }

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------
assert_eq()       { [ "$2" = "$3" ]  && ok "$1" || fail "$1" "want [$2] got [$3]"; }
assert_ne()       { [ "$2" != "$3" ] && ok "$1" || fail "$1" "should not equal [$2]"; }
assert_nonempty() { [ -n "$2" ]      && ok "$1" || fail "$1" "expected non-empty"; }
assert_contains()     { case "$3" in *"$2"*) ok "$1";; *) fail "$1" "expected to contain [$2], got: $3";; esac; }
assert_not_contains() { case "$3" in *"$2"*) fail "$1" "should NOT contain [$2], got: $3";; *) ok "$1";; esac; }
# assert_json label json field want
assert_json()     { assert_eq "$1" "$4" "$(strfield "$2" "$3")"; }
# assert_jnum label json field want  (numeric)
assert_jnum()     { assert_eq "$1" "$4" "$(numfield "$2" "$3")"; }
# assert_fails label pattern -- cmd...  (expects nonzero exit AND output matching pattern)
assert_fails() {
    local label="$1" pat="$2"; shift 2; [ "$1" = "--" ] && shift
    local out rc; out=$("$@" 2>&1); rc=$?
    if [ "$rc" -ne 0 ] && printf '%s' "$out" | grep -qi -- "$pat"; then
        ok "$label"
    else
        fail "$label" "expected failure matching [$pat] (rc=$rc): $out"
    fi
}

# ---------------------------------------------------------------------------
# JSON field extractors (python-backed; tolerate multi-line/indented JSON)
# ---------------------------------------------------------------------------
strfield() { python3 -c "import sys,json; print(json.loads(sys.argv[1]).get(sys.argv[2],''))" "$1" "$2" 2>/dev/null; }
numfield() { python3 -c "import sys,json; print(int(json.loads(sys.argv[1]).get(sys.argv[2],0)))" "$1" "$2" 2>/dev/null; }
# resultf json field — a field inside a run/step reply's nested "result" object.
resultf()  { python3 -c "import sys,json; print(json.loads(sys.argv[1]).get('result',{}).get(sys.argv[2],''))" "$1" "$2" 2>/dev/null; }
# list_len json — number of elements in a top-level JSON array.
list_len() { python3 -c "import sys,json; print(len(json.loads(sys.argv[1])))" "$1" 2>/dev/null; }
# has_action json name — 'yes'/'no' whether a name appears in an action-list response.
has_action() { python3 -c "import sys,json; print('yes' if any(a.get('name')==sys.argv[2] for a in json.loads(sys.argv[1])) else 'no')" "$1" "$2" 2>/dev/null; }

# PKCE helpers (the authorization-code flow is genuinely HTTP-wire; the CLI abstracts it).
pkce_verifier()  { python3 -c "import secrets;print(secrets.token_urlsafe(32))"; }
pkce_challenge() { python3 -c "import sys,hashlib,base64;print(base64.urlsafe_b64encode(hashlib.sha256(sys.argv[1].encode()).digest()).rstrip(b'=').decode())" "$1"; }
# pkce_code base handle password challenge — POST /v1/auth/authorize, return the auth code.
pkce_code() {
    curl -sf -X POST "$1/v1/auth/authorize" -H 'Content-Type: application/json' \
        -d "{\"handle\":\"$2\",\"password\":\"$3\",\"code_challenge\":\"$4\"}" 2>/dev/null \
    | python3 -c "import sys,json,urllib.parse as u; d=json.load(sys.stdin); print(u.parse_qs(u.urlparse(d['redirect']).query)['code'][0])" 2>/dev/null
}
# http_code method url [json] — HTTP status of a request (no auth).
http_code() {
    if [ -n "${3:-}" ]; then curl -s -o /dev/null -w '%{http_code}' -X "$1" "$2" -H 'Content-Type: application/json' -d "$3" 2>/dev/null
    else curl -s -o /dev/null -w '%{http_code}' -X "$1" "$2" 2>/dev/null; fi
}

# juice_token_dir home db — mirrors tokenDir() in cmd/juice/main.go:
# $HOME/.juice/tokens/{sha256(abs(db))[:12]}
juice_token_dir() {
    python3 -c "import hashlib,os,sys; print(os.path.join(sys.argv[1],'.juice','tokens',hashlib.sha256(os.path.abspath(sys.argv[2]).encode()).hexdigest()[:12]))" "$1" "$2"
}

# ---------------------------------------------------------------------------
# Fixtures — the repeated preambles, once.
# ---------------------------------------------------------------------------
# make_admin db home         — boot a server and log @sys in (home is @sys's home).
make_admin() { start_server "$1" "$2" "${@:3}" && j "$1" "$2" auth login @sys --password syspass >/dev/null 2>&1; }
# make_user db admin_home user_home handle [password]  — create @handle (as @sys) and log it
# in under user_home. Default password is "pw" so curl-based checks can reference it.
make_user() {
    local db="$1" ah="$2" uh="$3" h="$4" pw="${5:-pw}"
    j "$db" "$ah" user create "$h" "${h#@}@test.com" --password "$pw" >/dev/null 2>&1
    j "$db" "$uh" auth login "$h" --password "$pw" >/dev/null 2>&1
}
# deposit db sys_home handle amount
deposit() { j "$1" "$2" admin deposit "$3" "$4" >/dev/null 2>&1; :; }

# home dir handle — make + echo a per-user HOME dir under dir (token isolation).
home() { local d="$1/$2"; mkdir -p "$d/.juice"; echo "$d"; }

# ---------------------------------------------------------------------------
# HTTP test backends (python). Each tracks its PID in the registry.
# ---------------------------------------------------------------------------
# start_backend port [status] [body]  — fixed-response POST backend.
start_backend() {
    local port="$1" code="${2:-200}" body="${3:-}"
    [ -z "$body" ] && body='{"ok":true}'
    python3 - "$port" "$code" "$body" <<'PYEOF' &
import sys, http.server
port, code, body = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3].encode()
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.send_response(code); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    track_pid $!
    _await_http "$port" POST
}
# start_echo_backend port  — reflects request method + echoed `v` (query or body) for every verb.
start_echo_backend() {
    local port="$1"
    python3 - "$port" <<'PYEOF' &
import sys, json, http.server
from urllib.parse import urlparse, parse_qs
port = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def respond(self):
        v = parse_qs(urlparse(self.path).query).get('v', [''])[0]
        n = int(self.headers.get('Content-Length', 0))
        if n:
            try:
                b = json.loads(self.rfile.read(n) or b'{}')
                if not v: v = b.get('v', '')
            except Exception: pass
        body = json.dumps({"method": self.command, "v": v}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(body)
    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = respond
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    track_pid $!
    _await_http "$port" GET
}
# start_api_server port spec_file  — serves spec_file on GET, {"message":"ok"} on POST.
start_api_server() {
    local port="$1" spec="$2"
    python3 - "$port" "$spec" <<'PYEOF' &
import sys, http.server
port, spec = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(spec, 'rb') as f: body = f.read()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(body)
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
        self.wfile.write(b'{"message":"ok"}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
PYEOF
    track_pid $!
    _await_http "$port" GET
}
# _await_http port method — wait until a backend answers (or 5s).
_await_http() {
    local port="$1" method="$2" deadline=$(( $(date +%s) + 5 ))
    while :; do
        if [ "$method" = POST ]; then
            curl -sf -X POST "http://127.0.0.1:$port/" -d '{}' -H 'Content-Type: application/json' >/dev/null 2>&1 && return 0
        else
            curl -sf "http://127.0.0.1:$port/" >/dev/null 2>&1 && return 0
        fi
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.05
    done
}
# backend_port — a currently-free TCP port for a python backend (backends need a fixed port
# so actions can reference them by URL). Small race window, but ports aren't reused in a run.
backend_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()'; }

# ---------------------------------------------------------------------------
# WASM byte generators (from script/wasm_test.go fixtures). Unchanged.
# ---------------------------------------------------------------------------
make_echo_wasm() {
    python3 -c "
import sys
sys.stdout.buffer.write(bytes([
    0x00,0x61,0x73,0x6d,0x01,0x00,0x00,0x00,0x01,0x0d,0x02,0x60,0x01,0x7f,0x01,0x7f,
    0x60,0x02,0x7f,0x7f,0x02,0x7f,0x7f,0x03,0x03,0x02,0x00,0x01,0x05,0x03,0x01,0x00,0x01,
    0x06,0x06,0x01,0x7f,0x01,0x41,0x00,0x0b,0x07,0x18,0x03,0x06,0x6d,0x65,0x6d,0x6f,0x72,
    0x79,0x02,0x00,0x05,0x61,0x6c,0x6c,0x6f,0x63,0x00,0x00,0x03,0x72,0x75,0x6e,0x00,0x01,
    0x0a,0x1a,0x02,0x11,0x01,0x01,0x7f,0x23,0x00,0x21,0x01,0x23,0x00,0x20,0x00,0x6a,0x24,
    0x00,0x20,0x01,0x0b,0x06,0x00,0x20,0x00,0x20,0x01,0x0b,
]))" > "$1"
}
make_infinite_loop_wasm() {
    python3 -c "
import sys
sys.stdout.buffer.write(bytes([
    0x00,0x61,0x73,0x6d,0x01,0x00,0x00,0x00,0x01,0x0d,0x02,0x60,0x01,0x7f,0x01,0x7f,
    0x60,0x02,0x7f,0x7f,0x02,0x7f,0x7f,0x03,0x03,0x02,0x00,0x01,0x05,0x03,0x01,0x00,0x01,
    0x07,0x18,0x03,0x06,0x6d,0x65,0x6d,0x6f,0x72,0x79,0x02,0x00,0x05,0x61,0x6c,0x6c,0x6f,
    0x63,0x00,0x00,0x03,0x72,0x75,0x6e,0x00,0x01,0x0a,0x12,0x02,0x04,0x00,0x41,0x00,0x0b,
    0x0b,0x00,0x03,0x40,0x0c,0x00,0x0b,0x20,0x00,0x20,0x01,0x0b,
]))" > "$1"
}
make_contractor_wasm() {
    python3 - "$1" "$2" <<'PYEOF'
import sys
outfile, action_name = sys.argv[1], sys.argv[2]
name_b = action_name.encode(); name_len = len(name_b)
def leb128u(n):
    r=[]
    while True:
        b=n&0x7f; n>>=7; r.append(b|(0x80 if n else 0))
        if not n: break
    return bytes(r)
def vec(d): return leb128u(len(d))+bytes(d)
def sec(i,p): return bytes([i])+vec(p)
types   = leb128u(3)+b'\x60\x04\x7f\x7f\x7f\x7f\x01\x7e'+b'\x60\x01\x7f\x01\x7f'+b'\x60\x02\x7f\x7f\x01\x7e'
imports = leb128u(1)+vec(b'juice')+vec(b'call')+b'\x00\x00'
funcs   = leb128u(2)+b'\x01\x02'
mems    = leb128u(1)+b'\x00\x01'
globs   = leb128u(1)+b'\x7f\x01\x41'+leb128u(512)+b'\x0b'
exports = leb128u(3)+vec(b'memory')+b'\x02\x00'+vec(b'alloc')+b'\x00\x01'+vec(b'run')+b'\x00\x02'
alloc_body = bytes([0x01,0x01,0x7f,0x23,0x00,0x21,0x01,0x23,0x00,0x20,0x00,0x6a,0x24,0x00,0x20,0x01,0x0b])
run_body   = b'\x00\x41\x00'+b'\x41'+leb128u(name_len)+b'\x41\x20\x41\x02\x10\x00\x0b'
codes      = leb128u(2)+vec(alloc_body)+vec(run_body)
def dseg(off,d): return b'\x00\x41'+leb128u(off)+b'\x0b'+vec(d)
data = leb128u(2)+dseg(0,name_b)+dseg(32,b'{}')
wasm = (b'\x00asm\x01\x00\x00\x00'+sec(1,types)+sec(2,imports)+sec(3,funcs)
        +sec(5,mems)+sec(6,globs)+sec(7,exports)+sec(10,codes)+sec(11,data))
open(outfile,'wb').write(wasm)
PYEOF
}

# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------
# run_flows flow_a flow_b ...  — run each flow, reaping its processes afterwards. FLOW=name
# runs a single flow (extensibility/debugging). Prints results and sets exit status.
run_flows() {
    local f
    for f in "$@"; do
        [ -n "${FLOW:-}" ] && [ "$f" != "$FLOW" ] && continue
        "$f"
        reap
    done
    echo ""
    echo "Results: ${PASS} passed, ${FAIL} failed"
    if [ "$FAIL" -gt 0 ]; then printf "Failures:%b\n" "$ERRS"; return 1; fi
    return 0
}
