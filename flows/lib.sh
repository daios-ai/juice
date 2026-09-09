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
declare -A SERVER_OLD
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
# write_config db [key=value ...]  — config.json next to db. The CLI dials the real bound address
# via --server (see the wrappers below). log_format is json so `server.ready` is machine-readable.
# bootstrap_peers seeds the federation transport (§13); empty = no discovery.
# Keys: fee_bps import_bps script_timeout_ms kernel_handle bootstrap_peers.
write_config() {
    local db="$1"; shift
    local fee_bps=0 script_timeout_ms=10000 kernel_handle="test-kernel" bootstrap_peers="" remote_retry_interval_seconds=60 discovery_interval_seconds=300
    local lottery=0 credit_limit=100000 import_bps=500 world="play" rail_rpc="" fed_listen_addrs=""
    local a
    for a in "$@"; do case "$a" in
        fee_bps=*)                       fee_bps=${a#*=} ;;
        script_timeout_ms=*)             script_timeout_ms=${a#*=} ;;
        kernel_handle=*)                 kernel_handle=${a#*=} ;;
        bootstrap_peers=*)               bootstrap_peers=${a#*=} ;;
        remote_retry_interval_seconds=*) remote_retry_interval_seconds=${a#*=} ;;
        discovery_interval_seconds=*)    discovery_interval_seconds=${a#*=} ;;
        lottery=*)                       lottery=${a#*=} ;;
        credit_limit=*)                  credit_limit=${a#*=} ;;
        import_bps=*)                    import_bps=${a#*=} ;;
        world=*)                         world=${a#*=} ;;
        rail_rpc=*)                      rail_rpc=${a#*=} ;;
        fed_listen_addrs=*)              fed_listen_addrs=${a#*=} ;;
    esac; done
    local bp_json="[]" fl_json="[]"
    [ -n "$bootstrap_peers" ] && bp_json="[\"$bootstrap_peers\"]"
    [ -n "$fed_listen_addrs" ] && fl_json="[\"$fed_listen_addrs\"]"
    cat > "$(dirname "$db")/config.json" <<EOF
{
  "script_timeout_ms": $script_timeout_ms,
  "script_memory_bytes": 67108864,
  "fee_bps": $fee_bps,
  "import_bps": $import_bps,
  "lottery": $lottery,
  "credit_limit": $credit_limit,
  "token_ttl": "15m",
  "log_level": "info",
  "log_format": "json",
  "allow_local_sources": true,
  "world": "$world",
  "rail_rpc": "$rail_rpc",
  "kernel_handle": "$kernel_handle",
  "bootstrap_peers": $bp_json,
  "fed_listen_addrs": $fl_json,
  "remote_retry_interval_seconds": $remote_retry_interval_seconds,
  "discovery_interval_seconds": $discovery_interval_seconds
}
EOF
}

# server_log db — where start_server captures that kernel's output. It sits in the installation
# root rather than the kernel's home, so a home that moves does not take its own log with it.
server_log() { echo "$(khome "$1")/$(basename "$(dirname "$1")")-server.log"; }

# kernel_fed_addr db  — print a running kernel's loopback libp2p multiaddr, scraped from the
# fed_addrs on its `server.ready` log line. Every kernel now serves as a DHT+relay node, so one
# kernel can be the bootstrap for the others — there is no separate seed process.
kernel_fed_addr() {
    local db="$1"
    local log; log=$(server_log "$db")
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
    # The instance is the directory the database sits in, so serving a second kernel under one
    # installation root needs nothing but a second path (D20).
    local inst; inst=$(basename "$(dirname "$db")")
    # keep_config=1 leaves whatever configuration is already in the home alone — for the boot that
    # has to find an untouched pre-instance home and move it.
    local keep=0 a cfg=()
    for a in "$@"; do case "$a" in keep_config=1) keep=1 ;; *) cfg+=("$a") ;; esac; done
    if [ "$keep" = 0 ]; then
        mkdir -p "$(dirname "$db")"
        write_config "$db" ${cfg[@]+"${cfg[@]}"}
    fi
    local log; log=$(server_log "$db")
    # Truncate here, in the parent, before the server is launched: the redirection below truncates
    # only once the background child runs, and on a restart the wait loop could otherwise grep this
    # server's predecessor's `server.ready` line and lock onto its now-dead port.
    : >"$log"
    JUICE_BOOTSTRAP_PASSWORD=sys-pass HOME="$home" JUICE_HOME="$(khome "$db")" \
        "$JUICE" serve --addr 127.0.0.1:0 --instance "$inst" >>"$log" 2>&1 &
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
    [ -n "${SERVER_OLD[$db]:-}" ] && repoint_contexts "${SERVER_OLD[$db]}" "http://$addr"
    return 0
}

# stop_server db — stop the server for db (used by recovery flows that stop, inject DB
# state, then start_server again on the same db).
stop_server() {
    local pid="${SERVER_PID[$1]:-}"
    [ -n "$pid" ] && { kill -9 "$pid" 2>/dev/null; wait "$pid" 2>/dev/null; }
    SERVER_OLD["$1"]="${SERVER_URL[$1]:-}"
    unset "SERVER_URL[$1]" "SERVER_PID[$1]" 2>/dev/null
}

# repoint_contexts old new — a restarted server answers on a new address, and a client's login is
# sent only to the address recorded for its kernel. Every client that knew the old address is told
# the new one, which is what an operator does with `juice use --endpoint` after a restart. The
# recorded key is untouched: a restart changes where a kernel answers, never who it is.
repoint_contexts() {
    local f
    while IFS= read -r f; do
        python3 -c '
import json, sys
path, old, new = sys.argv[1:4]
d = json.load(open(path))
for k in d.get("kernels", {}).values():
    if k.get("endpoint", "").rstrip("/") == old.rstrip("/"):
        k["endpoint"] = new
json.dump(d, open(path, "w"))
' "$f" "$1" "$2"
    done < <(find "$_RUNROOT" -path "*/.juice/client/config.json" 2>/dev/null)
}

# ---------------------------------------------------------------------------
# CLI wrappers — always the built binary, targeting the db's server via --server.
# admin/peer commands are TCP clients too: they hit superuser-gated routes on the same public API
# as user commands.
# ---------------------------------------------------------------------------
_srv() { local db="$1"; [ -n "${SERVER_URL[$db]:-}" ] && printf -- '--server\n%s\n' "${SERVER_URL[$db]}"; }
j()  { local db="$1" home="$2"; shift 2; local a=(); mapfile -t a < <(_srv "$db"); HOME="$home" "$JUICE" "${a[@]}" "$@" 2>&1; }
jj() { local db="$1" home="$2"; shift 2; local a=(); mapfile -t a < <(_srv "$db"); HOME="$home" "$JUICE" "${a[@]}" --json "$@" 2>/dev/null; }

# kdb root — the database of the kernel served under an installation root. One kernel is one named
# directory, kernels/<instance>/, holding the ledger, the config, the rail key and the single-server
# lock (D23); the flows serve the default instance.
kdb() { echo "$1/kernels/${2:-default}/juice.db"; }

# khome db — the installation root a database belongs to, the inverse of kdb.
khome() { dirname "$(dirname "$(dirname "$1")")"; }

# await_login db home — log in and wait until the server actually answers as that user. A restart
# under load can bind its port a moment before it is serving, and a flow that reads too early sees
# an empty answer rather than the truth it is asserting about.
await_login() {
    local db="$1" home="$2" i
    for i in $(seq 20); do
        j "$db" "$home" auth login sys --password sys-pass >/dev/null 2>&1
        [ -n "$(strfield "$(jj "$db" "$home" user me)" handle)" ] && return 0
        sleep 0.5
    done
    return 1
}

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
# rowfield json list field — a field of the first row of a named list inside a JSON object.
rowfield() { python3 -c "
import sys,json
rows=json.loads(sys.argv[1]).get(sys.argv[2]) or []
print('' if not rows else rows[0].get(sys.argv[3],''))" "$1" "$2" "$3" 2>/dev/null; }

# owed_count db home peer — how many obligations one buyer still owes this kernel, from the
# operator's own worklist. An obligation is kept only by the side that is owed, so this is read on
# the seller and counted by the buyer it names.
owed_count() {
    local db="$1" home="$2" peer="$3"
    python3 -c "
import sys,json
rows=json.loads(sys.argv[1] or '{}').get('owed') or []
peer=sys.argv[2]
print(sum(1 for r in rows if r.get('peer') in (peer, peer[:8]) or peer.startswith(r.get('peer',''))))" \
        "$(jj "$db" "$home" admin deposit)" "$peer" 2>/dev/null || echo 0
}

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

# A client keeps what it knows in $HOME/.juice/client/: config.json holds the kernels (address, key,
# network) and the contexts naming one kernel and one login on it, and credentials/<context>.json
# holds that session's tokens (D20, ecosystem-standard.md). These read and write one field of the
# current context, dispatching by field name to whichever of the two files owns it — which is how a
# flow plants a stale token or checks that logout dropped one.
juice_client_dir() { echo "$1/.juice/client"; }

_ctx_py() {
    python3 -c '
import json, os, sys
d, field = sys.argv[1], sys.argv[2]
write = len(sys.argv) > 3
cfgp = os.path.join(d, "config.json")
try:
    cfg = json.load(open(cfgp))
except Exception:
    cfg = {"current": "default", "kernels": {}, "contexts": {}}
name = cfg.get("current") or "default"
if field in ("token", "refresh_token"):
    path = os.path.join(d, "credentials", name + ".json")
    try:
        cred = json.load(open(path))
    except Exception:
        cred = {}
    if not write:
        print(cred.get(field, "")); raise SystemExit
    cred[field] = sys.argv[3]
    os.makedirs(os.path.dirname(path), mode=0o700, exist_ok=True)
    json.dump(cred, open(path, "w")); os.chmod(path, 0o600)
    raise SystemExit
kname = cfg.get("contexts", {}).get(name, {}).get("kernel") or name
k = cfg.get("kernels", {}).get(kname, {})
if not write:
    print(k.get(field, "")); raise SystemExit
cfg.setdefault("kernels", {}).setdefault(kname, {})[field] = sys.argv[3]
cfg.setdefault("contexts", {}).setdefault(name, {})["kernel"] = kname
cfg["current"] = name
os.makedirs(d, mode=0o700, exist_ok=True)
json.dump(cfg, open(cfgp, "w"))
' "$@"
}

profile_get() { _ctx_py "$(juice_client_dir "$1")" "$2"; }
profile_set() { _ctx_py "$(juice_client_dir "$1")" "$2" "$3"; }

# ---------------------------------------------------------------------------
# Fixtures — the repeated preambles, once.
# ---------------------------------------------------------------------------
# make_admin db home         — boot a server, record its kernel, and log sys in (home is sys's home).
# The client records the kernel before it sends a password, because that is what an operator does
# and what the kernel requires: no secret leaves for an address whose key is not recorded (D20).
make_admin() {
    start_server "$1" "$2" "${@:3}" || return 1
    j "$1" "$2" auth login sys --password sys-pass >/dev/null 2>&1
}
# make_user db admin_home user_home handle [password]  — create handle (as sys) and log it
# in under user_home. Default password is "userpass" so curl-based checks can reference it.
make_user() {
    local db="$1" ah="$2" uh="$3" h="$4" pw="${5:-userpass}"
    j "$db" "$ah" user create "$h" --password "$pw" >/dev/null 2>&1
    j "$db" "$uh" auth login "$h" --password "$pw" >/dev/null 2>&1
}
# deposit db sys_home handle amount
# newref — a distinct name for one payment. Every crossing names the payment it records, so two
# fundings of the same account are two payments rather than one repeated (U3). It reads the clock
# rather than a counter because it is called from a subshell, where a counter would never advance.
newref() { echo "flow-$(date +%s%N)-$RANDOM"; }

# deposit db home target amount [ref] — records money that arrived from outside. Every crossing
# names the payment it stands for, so a reference is minted when the caller does not give one (U3).
deposit() { j "$1" "$2" admin deposit "$3" "$4" --ref "${5:-flow-$RANDOM$RANDOM}" >/dev/null 2>&1; :; }
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
# start_slow_backend port seconds — a POST backend that takes its time. A flow that must interrupt a
# call needs the call to still be in flight when it pulls the plug; without this the kernel has
# already committed and the crash lands nowhere interesting.
start_slow_backend() {
    local port="$1" secs="${2:-5}"
    python3 - "$port" "$secs" <<'PYEOF' &
import sys, time, http.server, socketserver
port, secs = int(sys.argv[1]), float(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', 0)))
        time.sleep(secs)
        b = b'{"ok":true}'
        self.send_response(200); self.send_header('Content-Type','application/json')
        self.send_header('Content-Length', str(len(b))); self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
class S(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
S(('127.0.0.1', port), H).serve_forever()
PYEOF
    track_pid $!
    sleep 0.3
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
# Local chain. Only the rail's opt-in release gate uses these, and they need Foundry
# (anvil, cast) on PATH; anvil goes into the same process registry as everything else.
# ---------------------------------------------------------------------------
ANVIL_RPC=""
# anvil's first account is funded at genesis and signs every setup transaction.
ANVIL_KEY=0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80

# start_anvil — boot a chain on a free port and set ANVIL_RPC. One slot per epoch, so the
# finalized head trails the tip by two blocks and a flow can say what has settled.
start_anvil() {
    local port; port=$(backend_port)
    anvil --port "$port" --chain-id 31337 --slots-in-an-epoch 1 --silent >/dev/null 2>&1 &
    track_pid $!
    ANVIL_RPC="http://127.0.0.1:$port"
    local deadline=$(( $(date +%s) + 20 ))
    while ! cast block-number --rpc-url "$ANVIL_RPC" >/dev/null 2>&1; do
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.1
    done
    return 0
}

# rail_contracts — juice-rail's compiled mocks. The published module carries their sources but
# not the artifacts, so a sibling checkout that has run `forge build` is what supplies them.
rail_contracts() {
    local dir="${JUICE_RAIL_CONTRACTS:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/juice-rail/contracts/out}"
    [ -d "$dir" ] || return 1
    echo "$dir"
}

# anvil_deploy name [value] [abi-encoded constructor args] — deploy one mock; echo its address.
anvil_deploy() {
    local name="$1" value="${2:-0}" args="${3:-}" out code
    out=$(rail_contracts) || return 1
    code=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['bytecode']['object'])" \
        "$out/$name.sol/$name.json") || return 1
    strfield "$(cast send --rpc-url "$ANVIL_RPC" --private-key "$ANVIL_KEY" --value "$value" \
        --create "${code}${args#0x}" --json 2>/dev/null)" contractAddress
}

# anvil_send key to [cast send arguments...] — one transaction, mined at once.
anvil_send() {
    local key="$1" to="$2"; shift 2
    cast send --rpc-url "$ANVIL_RPC" --private-key "$key" "$to" "$@" >/dev/null 2>&1
}

# anvil_mine n — advance the chain, which is how a flow reaches finality on demand.
anvil_mine() {
    local i
    for ((i=0; i<$1; i++)); do cast rpc --rpc-url "$ANVIL_RPC" evm_mine >/dev/null 2>&1; done
}

# anvil_uint to signature args... — read one number straight from the chain, never through the
# kernel, so an assertion about money has an independent witness.
anvil_uint() {
    local to="$1" sig="$2"; shift 2
    cast call --rpc-url "$ANVIL_RPC" "$to" "$sig" "$@" 2>/dev/null | awk '{print $1}'
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
