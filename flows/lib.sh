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
# All per-flow temp dirs live under one run root, so cleanup is a single rm — no per-dir
# tracking (new_dir runs in a $() subshell and can't mutate a parent-shell array).
# ---------------------------------------------------------------------------
declare -a _PIDS=()
_RUNROOT="$(mktemp -d)"
declare -A SERVER_URL=()   # db path -> http://host:port of its running server
declare -A SERVER_PID=()   # db path -> serve pid

track_pid() { _PIDS+=("$1"); }

# reap: kill everything spawned so far (SIGKILL for fast, deterministic teardown — test DBs
# are disposable) and clear this flow's temp dirs. SIGKILL means a server's own cleanup never
# runs, so wiping the run root is what clears any leftover artifacts.
reap() {
    local p
    for p in "${_PIDS[@]:-}"; do [ -n "$p" ] && kill -9 "$p" 2>/dev/null; done
    for p in "${_PIDS[@]:-}"; do [ -n "$p" ] && wait "$p" 2>/dev/null; done
    rm -rf "${_RUNROOT:?}"/* 2>/dev/null
    _PIDS=()
    SERVER_URL=()
    SERVER_PID=()
}
# On exit also remove the run root itself; per-flow reap only clears its contents.
cleanup_all() { reap; rm -rf "$_RUNROOT" 2>/dev/null; }
trap cleanup_all EXIT INT TERM

# new_dir — a temp dir under the run root; echoes its path. Cleaned by reap/cleanup_all.
new_dir() { mktemp -d -p "$_RUNROOT"; }

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
# write_config db [key=value ...]  — config.json next to db. server_url is intentionally
# empty (CLI dials the real bound address). log_format is json so `server.ready` is
# machine-readable. bootstrap_peers seeds the federation transport (§13); empty = no discovery.
# Keys: fee_bps import_bps script_timeout_ms kernel_handle bootstrap_peers.
write_config() {
    local db="$1"; shift
    local fee_bps=0 script_timeout_ms=10000 kernel_handle="test-kernel" bootstrap_peers="" remote_retry_interval_seconds=60 discovery_interval_seconds=300
    local exposure_max=0 settlement_trigger=0 settlement_quantum=0 import_bps=500
    local a
    for a in "$@"; do case "$a" in
        fee_bps=*)                       fee_bps=${a#*=} ;;
        script_timeout_ms=*)             script_timeout_ms=${a#*=} ;;
        kernel_handle=*)                 kernel_handle=${a#*=} ;;
        bootstrap_peers=*)               bootstrap_peers=${a#*=} ;;
        remote_retry_interval_seconds=*) remote_retry_interval_seconds=${a#*=} ;;
        discovery_interval_seconds=*)    discovery_interval_seconds=${a#*=} ;;
        exposure_max=*)                  exposure_max=${a#*=} ;;
        settlement_trigger=*)            settlement_trigger=${a#*=} ;;
        settlement_quantum=*)            settlement_quantum=${a#*=} ;;
        import_bps=*)                    import_bps=${a#*=} ;;
    esac; done
    local bp_json="[]"
    [ -n "$bootstrap_peers" ] && bp_json="[\"$bootstrap_peers\"]"
    cat > "$(dirname "$db")/config.json" <<EOF
{
  "script_timeout_ms": $script_timeout_ms,
  "script_memory_bytes": 67108864,
  "fee_bps": $fee_bps,
  "import_bps": $import_bps,
  "exposure_max": $exposure_max,
  "settlement_trigger": $settlement_trigger,
  "settlement_quantum": $settlement_quantum,
  "token_ttl": "15m",
  "log_level": "info",
  "log_format": "json",
  "allow_local_sources": true,
  "server_url": "",
  "kernel_handle": "$kernel_handle",
  "bootstrap_peers": $bp_json,
  "remote_retry_interval_seconds": $remote_retry_interval_seconds,
  "discovery_interval_seconds": $discovery_interval_seconds
}
EOF
}

# kernel_fed_addr db  — print a running kernel's loopback libp2p multiaddr, scraped from the
# fed_addrs on its `server.ready` log line. Every kernel now serves as a DHT+relay node, so one
# kernel can be the bootstrap for the others — there is no separate seed process.
kernel_fed_addr() {
    local db="$1"
    local log; log="$(dirname "$db")/server.log"
    sed 's/\x1b\[[0-9;]*m//g' "$log" 2>/dev/null \
        | grep -o '/ip4/127\.0\.0\.1/tcp/[0-9]*/p2p/[A-Za-z0-9]*' | head -1 | tr -d '\r'
}

# kernel_key db home  — print a kernel's own federation public key (via admin identity).
kernel_key() {
    strfield "$(jj "$1" "$2" admin identity)" public_key
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
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$home" \
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
# admin/peer commands are TCP clients too now: they use JUICE_SERVER (set below) and hit
# superuser-gated routes on the same public API as user commands.
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
# pathf json dotted.path — a nested field, e.g. pathf "$out" result.step_id or checks.signature.
pathf() { python3 -c "
import sys,json
v=json.loads(sys.argv[1])
for k in sys.argv[2].split('.'):
    v = v.get(k) if isinstance(v,dict) else None
print('' if v is None else v)" "$1" "$2" 2>/dev/null; }
# find_id json field value — the id of the first list element whose field equals value.
find_id() { python3 -c "
import sys,json
print(next((e.get('id','') for e in json.loads(sys.argv[1]) if str(e.get(sys.argv[2],''))==sys.argv[3]), ''))" "$1" "$2" "$3" 2>/dev/null; }
# resultf json field — a field inside a run/step reply's nested "result" object.
resultf()  { pathf "$1" "result.$2"; }
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
# http_code method url [json] [token] — HTTP status of a request.
http_code() {
    local hdr=(); [ -n "${4:-}" ] && hdr=(-H "Authorization: Bearer $4")
    if [ -n "${3:-}" ]; then curl -s -o /dev/null -w '%{http_code}' -X "$1" "$2" -H 'Content-Type: application/json' "${hdr[@]}" -d "$3" 2>/dev/null
    else curl -s -o /dev/null -w '%{http_code}' -X "$1" "$2" "${hdr[@]}" 2>/dev/null; fi
}
# http_body method url [json] [token] — response body of a request (for error-shape assertions).
http_body() {
    local hdr=(); [ -n "${4:-}" ] && hdr=(-H "Authorization: Bearer $4")
    if [ -n "${3:-}" ]; then curl -s -X "$1" "$2" -H 'Content-Type: application/json' "${hdr[@]}" -d "$3" 2>/dev/null
    else curl -s -X "$1" "$2" "${hdr[@]}" 2>/dev/null; fi
}
# assert_status label want method url [json] [token] — assert an exact HTTP status. Without this
# an error contract is unassertable, which is why the suite never had one.
assert_status() { local l="$1" w="$2"; shift 2; assert_eq "$l" "$w" "$(http_code "$@")"; }
# token base handle password — an access token via authorize→exchange (§12, the only token-issuing
# form; there is no password grant), for raw-HTTP checks.
token() {
    local v ch code; v=$(pkce_verifier); ch=$(pkce_challenge "$v"); code=$(pkce_code "$1" "$2" "$3" "$ch")
    [ -n "$code" ] || return 0
    strfield "$(curl -sf -X POST "$1/v1/auth/token" -H 'Content-Type: application/json' \
        -d "{\"code\":\"$code\",\"code_verifier\":\"$v\"}" 2>/dev/null)" access_token
}

# juice_token_dir home db — mirrors tokenDir() in cmd/juice/main.go:
# $HOME/.juice/kernel/tokens/{sha256(abs(db))[:12]}
juice_token_dir() {
    python3 -c "import hashlib,os,sys; print(os.path.join(sys.argv[1],'.juice','kernel','tokens',hashlib.sha256(os.path.abspath(sys.argv[2]).encode()).hexdigest()[:12]))" "$1" "$2"
}

# ---------------------------------------------------------------------------
# Fixtures — the repeated preambles, once.
# ---------------------------------------------------------------------------
# make_admin db home         — boot a server and log sys in (home is sys's home).
make_admin() { start_server "$1" "$2" "${@:3}" && j "$1" "$2" auth login sys --password sys-pass >/dev/null 2>&1; }
# make_user db admin_home user_home handle [password]  — create handle (as sys) and log it
# in under user_home. Default password is "userpass" so curl-based checks can reference it.
make_user() {
    local db="$1" ah="$2" uh="$3" h="$4" pw="${5:-userpass}"
    j "$db" "$ah" user create "$h" --password "$pw" >/dev/null 2>&1
    j "$db" "$uh" auth login "$h" --password "$pw" >/dev/null 2>&1
}
# deposit db sys_home handle amount
deposit() { j "$1" "$2" admin deposit "$3" "$4" >/dev/null 2>&1; :; }
# _mkaction db home visibility name [action-create flags...] — create + enable (+ publish); echo id.
_mkaction() {
    local db="$1" h="$2" vis="$3" name="$4"; shift 4
    local id; id=$(strfield "$(jj "$db" "$h" action create "$name" "$@")" id)
    [ -n "$id" ] || return 1
    j "$db" "$h" action enable "$id" >/dev/null 2>&1
    [ "$vis" = public ] && j "$db" "$h" action update "$id" --visibility public >/dev/null 2>&1
    echo "$id"
}
# publish db home name [flags...] — create, enable, make public. enabled — same without publishing.
publish() { _mkaction "$1" "$2" public  "${@:3}"; }
enabled() { _mkaction "$1" "$2" private "${@:3}"; }

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
# start_header_echo_backend port header  — POST backend that reflects one request header as
# {"seen": <value>}, so a flow can prove an auth credential actually reached the upstream.
start_header_echo_backend() {
    local port="$1" header="$2"
    python3 - "$port" "$header" <<'PYEOF' &
import sys, json, http.server
port, header = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        body = json.dumps({"seen": self.headers.get(header, "")}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
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
