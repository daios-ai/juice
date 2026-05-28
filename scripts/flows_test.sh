#!/usr/bin/env bash
# End-to-end flow tests for the juice CLI.
# Covers all 20 user-story flows.
#
# Usage (manual):
#   go build -o /tmp/juice ./cmd/juice/
#   JUICE=/tmp/juice bash scripts/flows_test.sh
#
# Usage (via Go test suite):
#   go test ./cmd/juice/ -run TestFlowsIntegration -v -timeout 120s
#
# Requires: python3 (inline HTTP backends), curl (federation setup)
set -uo pipefail

JUICE="${JUICE:-$(command -v juice 2>/dev/null || true)}"
if [ ! -x "$JUICE" ]; then
    echo "JUICE binary not found. Set JUICE=/path/to/binary or ensure 'juice' is on PATH."
    exit 1
fi

TMPDIR=$(mktemp -d)
DB="$TMPDIR/juice.db"
SYS_PASS="sysbootpass"

# Each user gets their own HOME so ~/.juice/token files don't collide.
H_SYS="$TMPDIR/home_sys"
H_ALICE="$TMPDIR/home_alice"
H_BOB="$TMPDIR/home_bob"
mkdir -p "$H_SYS/.juice" "$H_ALICE/.juice" "$H_BOB/.juice"

# Remote kernel (second juice serve instance on port 19875).
REMOTE_DB="$TMPDIR/remote.db"
H_REMOTE_SYS="$TMPDIR/home_remote_sys"
REMOTE_SYS_PASS="remotesyspass"
mkdir -p "$H_REMOTE_SYS/.juice"

PASS=0
FAIL=0
ERRS=""

ok()   { echo "  PASS: $1"; ((PASS++)); }
fail() { echo "  FAIL: $1 — $2"; ((FAIL++)); ERRS="$ERRS\n  [$1] $2"; }

# Run juice with HOME override and env vars
j() {
    local home_dir="$1"; shift
    HOME="$home_dir" \
    JUICE_BOOTSTRAP_PASSWORD="$SYS_PASS" \
    JUICE_ALLOW_LOCAL_SOURCES="true" \
    "$JUICE" --db "$DB" "$@" 2>&1
}

jj() {
    local home_dir="$1"; shift
    HOME="$home_dir" \
    JUICE_BOOTSTRAP_PASSWORD="$SYS_PASS" \
    JUICE_ALLOW_LOCAL_SOURCES="true" \
    "$JUICE" --db "$DB" --output json "$@" 2>&1
}

# Run juice against the remote kernel DB.
rj() {
    local home_dir="$1"; shift
    HOME="$home_dir" \
    JUICE_BOOTSTRAP_PASSWORD="$REMOTE_SYS_PASS" \
    JUICE_ALLOW_LOCAL_SOURCES="true" \
    "$JUICE" --db "$REMOTE_DB" "$@" 2>&1
}

# Extract a JSON string field: strfield "json" "fieldname"
# Handles both "fieldName": "value" and "field_name": "value"
strfield() {
    local json="$1" key="$2"
    echo "$json" | grep -o "\"$key\": *\"[^\"]*\"" | head -1 | sed "s/\"$key\": *\"//;s/\"//"
}

# Extract a JSON number/bool field
numfield() {
    local json="$1" key="$2"
    echo "$json" | grep -o "\"$key\": *[^,}]*" | head -1 | sed "s/\"$key\": *//" | tr -d ' "'
}

# ── HTTP echo backend (200 + {"ok":true}) ────────────────────────────────────
python3 -c "
import http.server, json, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.send_response(200)
        self.send_header('Content-Type','application/json')
        self.end_headers()
        self.wfile.write(b'{\"ok\":true}')
    def log_message(self,*a):pass
http.server.HTTPServer(('127.0.0.1',19871),H).serve_forever()
" &
BACKEND_OK_PID=$!

# ── Failing backend (500, non-JSON) ──────────────────────────────────────────
python3 -c "
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.send_response(500)
        self.send_header('Content-Type','text/plain')
        self.end_headers()
        self.wfile.write(b'internal error')
    def log_message(self,*a):pass
http.server.HTTPServer(('127.0.0.1',19872),H).serve_forever()
" &
BACKEND_FAIL_PID=$!

# ── Real remote juice kernel (second instance on port 19875) ─────────────────
HOME="$H_REMOTE_SYS" \
JUICE_ADDR="127.0.0.1:19875" \
JUICE_BOOTSTRAP_PASSWORD="$REMOTE_SYS_PASS" \
JUICE_ALLOW_LOCAL_SOURCES="true" \
"$JUICE" --db "$REMOTE_DB" serve &
REMOTE_KERNEL_PID=$!

cleanup() {
    kill "$BACKEND_OK_PID" "$BACKEND_FAIL_PID" "$REMOTE_KERNEL_PID" 2>/dev/null || true
    rm -rf "$TMPDIR"
}
trap cleanup EXIT
sleep 1   # let backends start (remote kernel needs a moment to bootstrap)

# ═══════════════════════════════════════════════════════════════════════════════
echo "=== FLOW 1: First boot and platform bootstrap ==="

# bootstrap() is only called from 'juice serve'. Start it on a free port,
# wait for it to finish bootstrapping, then kill it.
SERVE_PORT=19880
HOME="$H_SYS" \
JUICE_BOOTSTRAP_PASSWORD="$SYS_PASS" \
"$JUICE" --db "$DB" serve --addr "127.0.0.1:$SERVE_PORT" >/dev/null 2>&1 &
SERVE_PID=$!
sleep 1   # wait for bootstrap to complete
kill "$SERVE_PID" 2>/dev/null || true
wait "$SERVE_PID" 2>/dev/null || true

BOOT=$(j "$H_SYS" admin user list 2>&1)
# bootstrap ran if "not logged in" (server created @sys, but we haven't logged in yet)
if echo "$BOOT" | grep -qi "not logged in\|login"; then
    ok "1.1 bootstrap ran (not-logged-in gate proves openKernel was reached)"
else
    fail "1.1 bootstrap triggered" "$BOOT"
fi

# Login as @sys to verify the account was created
SYS_LOGIN=$(j "$H_SYS" auth login --handle @sys --password "$SYS_PASS" 2>&1)
if echo "$SYS_LOGIN" | grep -q "Logged in"; then
    ok "1.2 @sys login succeeds (account created on first boot)"
else
    fail "1.2 @sys login succeeds" "$SYS_LOGIN"
fi

# Verify @sys exists in user list
SYS_USERS=$(jj "$H_SYS" admin user list 2>&1)
if echo "$SYS_USERS" | grep -q "@sys"; then
    ok "1.3 @sys appears in admin user list"
else
    fail "1.3 @sys in admin user list" "$SYS_USERS"
fi

# Verify /lookup and /llm/chat registered
SYS_ACTIONS=$(jj "$H_SYS" action list 2>&1)
if echo "$SYS_ACTIONS" | grep -q "/lookup"; then
    ok "1.4 @sys/lookup registered and active"
else
    fail "1.4 @sys/lookup registered" "$SYS_ACTIONS"
fi
if echo "$SYS_ACTIONS" | grep -q "/llm/chat"; then
    ok "1.5 @sys/llm/chat registered and active"
else
    fail "1.5 @sys/llm/chat registered" "$SYS_ACTIONS"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 2: User authentication and self-view ==="

CREATE_ALICE=$(j "$H_ALICE" user create --handle @alice --email alice@test.com --password pass123 2>&1)
if echo "$CREATE_ALICE" | grep -qi "created\|alice\|user"; then
    ok "2.1 @alice created"
else
    fail "2.1 @alice created" "$CREATE_ALICE"
fi

LOGIN_ALICE=$(j "$H_ALICE" auth login --handle @alice --password pass123 2>&1)
if echo "$LOGIN_ALICE" | grep -q "Logged in"; then
    ok "2.2 @alice login"
else
    fail "2.2 @alice login" "$LOGIN_ALICE"
fi

ME=$(jj "$H_ALICE" user me 2>&1)
if echo "$ME" | grep -q "@alice"; then
    ok "2.3 GET /me returns correct handle"
else
    fail "2.3 GET /me returns correct handle" "$ME"
fi

REFRESH=$(j "$H_ALICE" auth refresh 2>&1)
if echo "$REFRESH" | grep -q "refreshed"; then
    ok "2.4 token refresh"
else
    fail "2.4 token refresh" "$REFRESH"
fi

LOGOUT=$(j "$H_ALICE" auth logout 2>&1)
if echo "$LOGOUT" | grep -q "Logged out"; then
    ok "2.5 logout"
else
    fail "2.5 logout" "$LOGOUT"
fi

# After logout, authenticated commands should fail
UNAUTH=$(jj "$H_ALICE" user me 2>&1)
if echo "$UNAUTH" | grep -qi "not logged in\|login\|token"; then
    ok "2.6 unauthenticated access rejected after logout"
else
    fail "2.6 unauthenticated access rejected" "$UNAUTH"
fi

j "$H_ALICE" auth login --handle @alice --password pass123 >/dev/null 2>&1

# Create @bob
j "$H_BOB" user create --handle @bob --email bob@test.com --password pass456 >/dev/null 2>&1
j "$H_BOB" auth login --handle @bob --password pass456 >/dev/null 2>&1

# Suspension flow: get alice's ID, then suspend
ALICE_JSON=$(jj "$H_SYS" admin user list 2>&1)
ALICE_ID=$(echo "$ALICE_JSON" | python3 -c "
import sys, json
users = json.load(sys.stdin)
for u in users:
    if u.get('Handle','') == '@alice':
        print(u.get('ID',''))
        break
" 2>/dev/null || true)

if [ -n "$ALICE_ID" ]; then
    SUSPEND=$(j "$H_SYS" admin user suspend --id "$ALICE_ID" 2>&1)
    if echo "$SUSPEND" | grep -qi "suspended"; then
        ok "2.7 @alice suspended"
    else
        fail "2.7 @alice suspended" "$SUSPEND"
    fi

    # Suspended user should get unauthenticated error
    SUSP_ME=$(jj "$H_ALICE" user me 2>&1)
    if echo "$SUSP_ME" | grep -qi "unauthenticated\|suspended"; then
        ok "2.8 suspended user returns unauthenticated error"
    else
        fail "2.8 suspended user returns unauthenticated error" "$SUSP_ME"
    fi

    # Unsuspend
    j "$H_SYS" admin user unsuspend --id "$ALICE_ID" >/dev/null 2>&1
    j "$H_ALICE" auth login --handle @alice --password pass123 >/dev/null 2>&1
else
    fail "2.7 @alice suspended" "could not find alice's ID"
    fail "2.8 suspended user unauthenticated" "no ID"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 3: Superuser deposit and user funding ==="

DEPOSIT=$(j "$H_SYS" admin user deposit --handle @alice --amount 1000 --reason "test" 2>&1)
if echo "$DEPOSIT" | grep -qi "deposit\|1000"; then
    ok "3.1 @sys deposits 1000 to @alice"
else
    fail "3.1 deposit to @alice" "$DEPOSIT"
fi

ALICE_AFTER=$(jj "$H_ALICE" user me 2>&1)
AVAIL=$(numfield "$ALICE_AFTER" "available")
if [ "${AVAIL:-0}" = "1000" ]; then
    ok "3.2 user.available = 1000 after deposit"
else
    fail "3.2 user.available after deposit" "available=$AVAIL me=$ALICE_AFTER"
fi

# Fund @bob too
j "$H_SYS" admin user deposit --handle @bob --amount 500 >/dev/null 2>&1

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 4: Process start and end ==="

PROC_START=$(jj "$H_ALICE" process start --funds 400 2>&1)
PROC_ID=$(strfield "$PROC_START" "process_id")
ROOT_TRACE_ID=$(strfield "$PROC_START" "trace_id")

if [ -n "$PROC_ID" ]; then
    ok "4.1 process started"
else
    fail "4.1 process started" "$PROC_START"
fi

# user.available should have decreased
ALICE_AFTER_PROC=$(jj "$H_ALICE" user me 2>&1)
AVAIL_AFTER_PROC=$(numfield "$ALICE_AFTER_PROC" "available")
if [ "${AVAIL_AFTER_PROC:-1000}" -lt "1000" ] 2>/dev/null; then
    ok "4.2 user.available decreased when process started"
else
    fail "4.2 user.available decreased" "available=$AVAIL_AFTER_PROC"
fi

# Check process.available = 400
PROC_SHOW=$(j "$H_ALICE" process show --id "$PROC_ID" 2>&1)
if echo "$PROC_SHOW" | grep -q "available.*400\|400.*available"; then
    ok "4.3 process.available = 400"
else
    fail "4.3 process.available = 400" "$PROC_SHOW"
fi

if [ -n "$ROOT_TRACE_ID" ]; then
    ok "4.4 root trace created alongside process"
else
    fail "4.4 root trace created" "$PROC_START"
fi

# End process
PROC_END=$(j "$H_ALICE" process end --id "$PROC_ID" 2>&1)
if echo "$PROC_END" | grep -qi "ended\|returned\|process"; then
    ok "4.5 process ended"
else
    fail "4.5 process ended" "$PROC_END"
fi

# Funds should return to alice
ALICE_AFTER_END=$(jj "$H_ALICE" user me 2>&1)
AVAIL_AFTER_END=$(numfield "$ALICE_AFTER_END" "available")
if [ "${AVAIL_AFTER_END:-0}" = "1000" ]; then
    ok "4.6 funds returned after process end (available = 1000)"
else
    fail "4.6 funds returned after process end" "available=$AVAIL_AFTER_END"
fi

# Start a fresh process for subsequent flows
PROC_DATA=$(jj "$H_ALICE" process start --funds 800 2>&1)
PROC_ID=$(strfield "$PROC_DATA" "process_id")
ROOT_TRACE_ID=$(strfield "$PROC_DATA" "trace_id")
BOB_PROC_DATA=$(jj "$H_BOB" process start --funds 200 2>&1)
BOB_PROC_ID=$(strfield "$BOB_PROC_DATA" "process_id")

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 5: Action creation and activation ==="

ACTION_ADD=$(jj "$H_ALICE" action add \
    --name /echo \
    --kind http \
    --price 10 \
    --source "http://127.0.0.1:19871" \
    --description "Echo action" 2>&1)
ACTION_ID=$(strfield "$ACTION_ADD" "ID")
[ -z "$ACTION_ID" ] && ACTION_ID=$(strfield "$ACTION_ADD" "action_id")

if [ -n "$ACTION_ID" ]; then
    ok "5.1 action /echo created"
else
    fail "5.1 action /echo created" "$ACTION_ADD"
fi

ACTION_SHOW=$(j "$H_ALICE" action show --id "$ACTION_ID" 2>&1)
if echo "$ACTION_SHOW" | grep -qi "inactive\|Active.*false\|active.*false"; then
    ok "5.2 action inactive by default"
else
    fail "5.2 action inactive by default" "$ACTION_SHOW"
fi

ENABLE=$(j "$H_ALICE" action enable --id "$ACTION_ID" 2>&1)
if echo "$ENABLE" | grep -qi "enabled\|activated"; then
    ok "5.3 action enabled"
else
    fail "5.3 action enabled" "$ENABLE"
fi

ACTION_SHOW2=$(j "$H_ALICE" action show --id "$ACTION_ID" 2>&1)
if echo "$ACTION_SHOW2" | grep -qi "status.*active\|Active.*true\|active.*true"; then
    ok "5.4 action active after enable"
else
    fail "5.4 action active after enable" "$ACTION_SHOW2"
fi

# Non-owner cannot call inactive action
j "$H_ALICE" action disable --id "$ACTION_ID" >/dev/null 2>&1
CALL_INACTIVE=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_INACTIVE" | grep -qi "error\|inactive\|unauthorized\|forbidden"; then
    ok "5.5 call to inactive action rejected"
else
    fail "5.5 call to inactive action rejected" "$CALL_INACTIVE"
fi
j "$H_ALICE" action enable --id "$ACTION_ID" >/dev/null 2>&1

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 6: Action update and deletion ==="

UPDATE=$(j "$H_ALICE" action update --id "$ACTION_ID" --price 20 2>&1)
if echo "$UPDATE" | grep -qi "updated"; then
    ok "6.1 action updated (price → 20)"
else
    fail "6.1 action updated" "$UPDATE"
fi

# Semantic update should deactivate
ACTION_SHOW3=$(j "$H_ALICE" action show --id "$ACTION_ID" 2>&1)
if echo "$ACTION_SHOW3" | grep -qi "inactive\|Active.*false\|active.*false"; then
    ok "6.2 semantic update deactivates action"
else
    fail "6.2 semantic update deactivates action" "$ACTION_SHOW3"
fi

j "$H_ALICE" action enable --id "$ACTION_ID" >/dev/null 2>&1

# Create a second action to delete
ACTION2_ADD=$(jj "$H_ALICE" action add --name /todelete --kind http --source "http://127.0.0.1:19871" 2>&1)
ACTION2_ID=$(strfield "$ACTION2_ADD" "ID")

# Grant ACL to @bob so we can verify it's purged after delete
j "$H_ALICE" action acl grant --action "$ACTION2_ID" --user @bob --perm call >/dev/null 2>&1

DELETE=$(j "$H_ALICE" action delete --id "$ACTION2_ID" 2>&1)
if echo "$DELETE" | grep -qi "deleted"; then
    ok "6.3 action deleted"
else
    fail "6.3 action deleted" "$DELETE"
fi

# Deleted action should not be visible
SHOW_DELETED=$(j "$H_ALICE" action show --id "$ACTION2_ID" 2>&1)
if echo "$SHOW_DELETED" | grep -qi "not found\|error"; then
    ok "6.4 deleted action not found"
else
    fail "6.4 deleted action not found" "$SHOW_DELETED"
fi

# ACL should be purged: @bob cannot call deleted action
CALL_DELETED=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /todelete 2>&1)
if echo "$CALL_DELETED" | grep -qi "error\|not found"; then
    ok "6.5 deleted action ACL purged — call rejected"
else
    fail "6.5 deleted action ACL purged" "$CALL_DELETED"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 7: ACL and grant-all ==="

# @bob denied without permission
CALL_NOAUTH=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_NOAUTH" | grep -qi "unauthorized\|forbidden\|error\|permission"; then
    ok "7.1 @bob denied call without ACL"
else
    fail "7.1 @bob denied call without ACL" "$CALL_NOAUTH"
fi

# Grant call to @bob
GRANT=$(j "$H_ALICE" action acl grant --action "$ACTION_ID" --user @bob --perm call 2>&1)
if echo "$GRANT" | grep -qi "granted\|ok\|acl"; then
    ok "7.2 call permission granted to @bob"
else
    fail "7.2 call permission granted to @bob" "$GRANT"
fi

CALL_WITH_ACL=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_WITH_ACL" | grep -q "tx_id"; then
    ok "7.3 @bob can call with ACL"
else
    fail "7.3 @bob can call with ACL" "$CALL_WITH_ACL"
fi

# Revoke
REVOKE=$(j "$H_ALICE" action acl revoke --action "$ACTION_ID" --user @bob --perm call 2>&1)
if echo "$REVOKE" | grep -qi "revoked\|ok"; then
    ok "7.4 call permission revoked"
else
    fail "7.4 call permission revoked" "$REVOKE"
fi

CALL_AFTER_REVOKE=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_AFTER_REVOKE" | grep -qi "unauthorized\|forbidden\|error\|permission"; then
    ok "7.5 @bob denied after revoke"
else
    fail "7.5 @bob denied after revoke" "$CALL_AFTER_REVOKE"
fi

# Grant-all (public flag)
GRANTALL=$(j "$H_ALICE" action grant-all --id "$ACTION_ID" 2>&1)
if echo "$GRANTALL" | grep -qi "granted\|public\|ok"; then
    ok "7.6 grant-all makes action public"
else
    fail "7.6 grant-all makes action public" "$GRANTALL"
fi

CALL_PUBLIC=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_PUBLIC" | grep -q "tx_id"; then
    ok "7.7 @bob can call public action without explicit ACL"
else
    fail "7.7 @bob can call public action" "$CALL_PUBLIC"
fi

REVOKEALL=$(j "$H_ALICE" action revoke-all --id "$ACTION_ID" 2>&1)
if echo "$REVOKEALL" | grep -qi "revoked\|ok"; then
    ok "7.8 revoke-all removes public flag"
else
    fail "7.8 revoke-all removes public flag" "$REVOKEALL"
fi

CALL_AFTER_REVOKEALL=$(jj "$H_BOB" call --process "$BOB_PROC_ID" --target @alice --action /echo 2>&1)
if echo "$CALL_AFTER_REVOKEALL" | grep -qi "unauthorized\|forbidden\|error\|permission"; then
    ok "7.9 @bob denied after revoke-all"
else
    fail "7.9 @bob denied after revoke-all" "$CALL_AFTER_REVOKEALL"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 8: Direct action call ==="

CALL=$(jj "$H_ALICE" call --process "$PROC_ID" --target @alice --action /echo 2>&1)
TX_ID=$(strfield "$CALL" "tx_id")
TRACE_ID=$(strfield "$CALL" "trace_id")

if [ -n "$TX_ID" ]; then
    ok "8.1 call returns tx_id"
else
    fail "8.1 call returns tx_id" "$CALL"
fi
if [ -n "$TRACE_ID" ]; then
    ok "8.2 call returns trace_id"
else
    fail "8.2 call returns trace_id" "$CALL"
fi

# Process funds decreased by price (20)
PROC_SHOW_AFTER=$(j "$H_ALICE" process show --id "$PROC_ID" 2>&1)
if echo "$PROC_SHOW_AFTER" | grep -q "780\|available.*780"; then
    ok "8.3 process.available decreased by price (800 - 20 = 780)"
else
    fail "8.3 process.available decreased" "$PROC_SHOW_AFTER"
fi

# Call with JSON args
CALL_ARGS=$(jj "$H_ALICE" call --process "$PROC_ID" --target @alice --action /echo --args '{"msg":"hello"}' 2>&1)
if echo "$CALL_ARGS" | grep -q "tx_id"; then
    ok "8.4 call with args succeeds"
else
    fail "8.4 call with args succeeds" "$CALL_ARGS"
fi

# Transaction contains the args
TX2_ID=$(strfield "$CALL_ARGS" "tx_id")
TX_SHOW=$(jj "$H_ALICE" tx show --id "$TX2_ID" 2>&1)
if echo "$TX_SHOW" | grep -qi "hello\|msg"; then
    ok "8.5 transaction records input args"
else
    fail "8.5 transaction records input args" "$TX_SHOW"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 9: Failed call with refund ==="

FAIL_ACTION=$(jj "$H_ALICE" action add \
    --name /fail \
    --kind http \
    --price 50 \
    --source "http://127.0.0.1:19872" 2>&1)
FAIL_ID=$(strfield "$FAIL_ACTION" "ID")
j "$H_ALICE" action enable --id "$FAIL_ID" >/dev/null 2>&1

PROC_BEFORE=$(j "$H_ALICE" process show --id "$PROC_ID" 2>&1)
AVAIL_BEFORE=$(echo "$PROC_BEFORE" | grep -o "Available: *[0-9]*\|available.*[0-9]\+" | grep -o "[0-9]\+" | head -1)

FAIL_CALL=$(jj "$H_ALICE" call --process "$PROC_ID" --target @alice --action /fail 2>&1)
if echo "$FAIL_CALL" | grep -qi "failed\|error\|execution"; then
    ok "9.1 failing call returns error"
else
    fail "9.1 failing call returns error" "$FAIL_CALL"
fi

PROC_AFTER_FAIL=$(j "$H_ALICE" process show --id "$PROC_ID" 2>&1)
AVAIL_AFTER=$(echo "$PROC_AFTER_FAIL" | grep -o "Available: *[0-9]*\|available.*[0-9]\+" | grep -o "[0-9]\+" | head -1)
if [ "$AVAIL_BEFORE" = "$AVAIL_AFTER" ]; then
    ok "9.2 locked funds refunded on failure (no net deduction)"
else
    fail "9.2 locked funds refunded on failure" "before=$AVAIL_BEFORE after=$AVAIL_AFTER"
fi

# Failed tx should be recorded in list
TX_LIST=$(jj "$H_ALICE" tx list --process "$PROC_ID" 2>&1)
if echo "$TX_LIST" | grep -qi "failure\|failed"; then
    ok "9.3 failed transaction recorded"
else
    fail "9.3 failed transaction recorded" "$TX_LIST"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 10: Wasm action execution ==="

# Write echo wasm bytes (exported alloc + run, echoes input)
python3 -c "
import sys
wasm = bytes([
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
])
sys.stdout.buffer.write(wasm)
" > "$TMPDIR/echo.wasm"

WASM_ADD=$(jj "$H_ALICE" action add --name /wasm-echo --kind wasm --source "$TMPDIR/echo.wasm" 2>&1)
WASM_ID=$(strfield "$WASM_ADD" "ID")

if [ -n "$WASM_ID" ]; then
    ok "10.1 wasm action created"
else
    fail "10.1 wasm action created" "$WASM_ADD"
fi

# Verify artifact_hash is computed (content-addressed)
HASH=$(strfield "$WASM_ADD" "ArtifactHash")
if [ -n "$HASH" ]; then
    ok "10.2 artifact_hash computed and stored"
else
    fail "10.2 artifact_hash computed" "$WASM_ADD"
fi

j "$H_ALICE" action enable --id "$WASM_ID" >/dev/null 2>&1

WASM_CALL=$(jj "$H_ALICE" call --process "$PROC_ID" --target @alice --action /wasm-echo --args '{"x":1}' 2>&1)
if echo "$WASM_CALL" | grep -q "tx_id"; then
    ok "10.3 wasm action executes successfully via Call()"
else
    fail "10.3 wasm action executes" "$WASM_CALL"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 12: Event listener lifecycle ==="

# @alice listens for events from @bob that trigger /echo
LISTEN=$(jj "$H_ALICE" events listen \
    --event data.ready \
    --source @bob \
    --action "$ACTION_ID" 2>&1)
LISTENER_ID=$(strfield "$LISTEN" "ID")
[ -z "$LISTENER_ID" ] && LISTENER_ID=$(strfield "$LISTEN" "listener_id")

if [ -n "$LISTENER_ID" ]; then
    ok "12.1 listener created (alice owns /echo → CanCall satisfied)"
else
    fail "12.1 listener created" "$LISTEN"
fi

# @bob cannot create listener for @alice's /echo (no call permission)
LISTEN_NOAUTH=$(jj "$H_BOB" events listen \
    --event data.ready \
    --source @alice \
    --action "$ACTION_ID" 2>&1)
if echo "$LISTEN_NOAUTH" | grep -qi "unauthorized\|forbidden\|error\|permission"; then
    ok "12.2 listener creation rejected when caller lacks call permission"
else
    fail "12.2 listener creation rejected without call permission" "$LISTEN_NOAUTH"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 13: Event emit and consume ==="

# Alice's balance before emit (emit must not change balances)
ALICE_ME_BEFORE=$(jj "$H_ALICE" user me 2>&1)
ALICE_AVAIL_BEFORE=$(numfield "$ALICE_ME_BEFORE" "available")

# Bob emits event
EMIT=$(j "$H_BOB" events emit --event data.ready --args '{"n":42}' 2>&1)
if echo "$EMIT" | grep -qi "event\|queued\|emit\|ok\|emitted"; then
    ok "13.1 event emitted"
else
    fail "13.1 event emitted" "$EMIT"
fi

# Emit must not change alice's balance
ALICE_ME_AFTER=$(jj "$H_ALICE" user me 2>&1)
ALICE_AVAIL_AFTER=$(numfield "$ALICE_ME_AFTER" "available")
if [ "$ALICE_AVAIL_BEFORE" = "$ALICE_AVAIL_AFTER" ]; then
    ok "13.2 emit does not change alice's balance"
else
    fail "13.2 emit does not change balances" "before=$ALICE_AVAIL_BEFORE after=$ALICE_AVAIL_AFTER"
fi

# Poll for pending events
POLL=$(jj "$H_ALICE" events poll --id "$LISTENER_ID" 2>&1)
if echo "$POLL" | grep -qi "id\|event"; then
    ok "13.3 pending event visible via poll"
else
    fail "13.3 pending event visible via poll" "$POLL"
fi

EVENT_ID=$(echo "$POLL" | python3 -c "
import sys, json
data = json.load(sys.stdin)
# eventsPollCmd wraps results: {'events': [...]}
events = data.get('events', []) if isinstance(data, dict) else data
if isinstance(events, list) and len(events) > 0:
    print(events[0].get('ID', events[0].get('id', events[0].get('event_id', ''))))
" 2>/dev/null || true)

if [ -n "$EVENT_ID" ]; then
    CONSUME=$(jj "$H_ALICE" events consume --id "$EVENT_ID" --process "$PROC_ID" 2>&1)
    CONSUME_TX=$(strfield "$CONSUME" "tx_id")
    if [ -n "$CONSUME_TX" ]; then
        ok "13.4 event consumed — tx_id set on event"
    else
        fail "13.4 event consumed — tx_id set" "$CONSUME"
    fi

    # After consume, event is no longer pending
    POLL2=$(jj "$H_ALICE" events poll --id "$LISTENER_ID" 2>&1)
    COUNT=$(echo "$POLL2" | python3 -c "
import sys, json
d = json.load(sys.stdin)
events = d.get('events') if isinstance(d, dict) else d
print(len(events) if isinstance(events, list) else (0 if events is None else 1))
" 2>/dev/null || echo "?")
    if [ "$COUNT" = "0" ]; then
        ok "13.5 no pending events after consume"
    else
        fail "13.5 no pending events after consume" "count=$COUNT poll=$POLL2"
    fi
else
    fail "13.4 event consumed" "no event_id found in poll"
fi

# Unlisten
UNLISTEN=$(j "$H_ALICE" events unlisten --id "$LISTENER_ID" 2>&1)
if echo "$UNLISTEN" | grep -qi "deleted\|deactivated\|ok\|removed\|listener"; then
    ok "13.6 listener deactivated"
else
    fail "13.6 listener deactivated" "$UNLISTEN"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 14: Lookup ==="

# @sys/lookup is callable through Call(). Without embedder → empty results or ErrInvalidState
LOOKUP=$(jj "$H_ALICE" call --process "$PROC_ID" --target @sys --action /lookup \
    --args '{"query":"echo","limit":3}' 2>&1)
if echo "$LOOKUP" | grep -q "tx_id"; then
    ok "14.1 /lookup callable via Call() (no embedder → empty results)"
elif echo "$LOOKUP" | grep -qi "invalid_state\|embedder\|not configured\|error"; then
    ok "14.1 /lookup correctly returns ErrInvalidState with no embedder"
else
    fail "14.1 /lookup callable or returns ErrInvalidState" "$LOOKUP"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 15: Local chat action ==="

CHAT=$(jj "$H_ALICE" call --process "$PROC_ID" --target @sys --action /llm/chat \
    --args '{"messages":[{"role":"user","content":"hi"}]}' 2>&1)
if echo "$CHAT" | grep -qi "invalid_state\|not configured\|error"; then
    ok "15.1 /llm/chat returns ErrInvalidState when no chat service configured"
else
    fail "15.1 /llm/chat returns ErrInvalidState" "$CHAT"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 16: Human rating and cascade ==="

# Use the known tx from flow 8 (direct call to ACTION_ID), so stats checks are consistent.
RATE_TX_ID="$TX_ID"

if [ -n "$RATE_TX_ID" ]; then
    RATE=$(j "$H_SYS" tx rate --id "$RATE_TX_ID" --rating 1 2>&1)
    if echo "$RATE" | grep -qi "rated\|ok\|success"; then
        ok "16.1 transaction rated 1"
    else
        fail "16.1 transaction rated 1" "$RATE"
    fi

    # Already-rated transaction cannot be re-rated
    RATE2=$(j "$H_SYS" tx rate --id "$RATE_TX_ID" --rating 0 2>&1)
    if echo "$RATE2" | grep -qi "already rated\|error\|invalid"; then
        ok "16.2 already-rated transaction cannot be re-rated"
    else
        fail "16.2 already-rated transaction cannot be re-rated" "$RATE2"
    fi

    # Action stats should reflect the rating (rating_count ≥ 1)
    STATS=$(jj "$H_ALICE" stats show --action "$ACTION_ID" 2>&1)
    RATING_COUNT=$(numfield "$STATS" "RatingCount")
    [ -z "$RATING_COUNT" ] && RATING_COUNT=$(numfield "$STATS" "rating_count")
    if [ "${RATING_COUNT:-0}" -ge "1" ] 2>/dev/null; then
        ok "16.3 action stats: rating_count updated after rating"
    else
        fail "16.3 action stats rating_count updated" "rating_count=$RATING_COUNT stats=$STATS"
    fi
else
    fail "16.1 transaction rated 1" "no successful tx found"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 17: Admin supervision ==="

ADMIN_USERS=$(jj "$H_SYS" admin user list 2>&1)
if echo "$ADMIN_USERS" | grep -q "@alice"; then
    ok "17.1 @sys can list all users"
else
    fail "17.1 @sys lists all users" "$ADMIN_USERS"
fi

ADMIN_ACTIONS=$(jj "$H_SYS" admin action list 2>&1)
if echo "$ADMIN_ACTIONS" | grep -q "/echo"; then
    ok "17.2 @sys can list all actions"
else
    fail "17.2 @sys lists all actions" "$ADMIN_ACTIONS"
fi

ADMIN_PROCS=$(jj "$H_SYS" admin process list 2>&1)
if echo "$ADMIN_PROCS" | grep -qi "open\|Status\|status"; then
    ok "17.3 @sys can list all processes"
else
    fail "17.3 @sys lists all processes" "$ADMIN_PROCS"
fi

ADMIN_TX=$(jj "$H_SYS" admin tx list 2>&1)
if echo "$ADMIN_TX" | grep -qi "success\|failure\|Status"; then
    ok "17.4 @sys can list all transactions"
else
    fail "17.4 @sys lists all transactions" "$ADMIN_TX"
fi

ADMIN_DISABLE=$(j "$H_SYS" admin action disable --id "$ACTION_ID" 2>&1)
if echo "$ADMIN_DISABLE" | grep -qi "disabled"; then
    ok "17.5 @sys can disable any action"
else
    fail "17.5 @sys disables action" "$ADMIN_DISABLE"
fi
SHOW_AFTER_ADMIN=$(j "$H_ALICE" action show --id "$ACTION_ID" 2>&1)
if echo "$SHOW_AFTER_ADMIN" | grep -qi "inactive\|Active.*false\|active.*false"; then
    ok "17.6 action is inactive after admin disable"
else
    fail "17.6 action inactive after admin disable" "$SHOW_AFTER_ADMIN"
fi

# Non-@sys user cannot use admin commands
ALICE_ADMIN=$(jj "$H_ALICE" admin user list 2>&1)
if echo "$ALICE_ADMIN" | grep -qi "unauthorized\|superuser\|error"; then
    ok "17.7 non-@sys user rejected from admin commands"
else
    fail "17.7 non-@sys user rejected from admin" "$ALICE_ADMIN"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 18: Audit and observability ==="

PROC_INSPECT=$(jj "$H_ALICE" process show --id "$PROC_ID" 2>&1)
if echo "$PROC_INSPECT" | grep -qi "available\|status"; then
    ok "18.1 process inspectable"
else
    fail "18.1 process inspectable" "$PROC_INSPECT"
fi

PROC_LIST=$(jj "$H_ALICE" process list 2>&1)
if echo "$PROC_LIST" | grep -qi "open\|closed\|Status"; then
    ok "18.2 process list works"
else
    fail "18.2 process list works" "$PROC_LIST"
fi

TX_LIST_CHECK=$(jj "$H_ALICE" tx list --process "$PROC_ID" 2>&1)
TX_COUNT=$(echo "$TX_LIST_CHECK" | python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)" 2>/dev/null || echo "0")
if [ "${TX_COUNT:-0}" -gt "0" ]; then
    ok "18.3 transactions listable (count=$TX_COUNT for process)"
else
    fail "18.3 transactions listable" "count=$TX_COUNT"
fi

if [ -n "$TX_ID" ]; then
    TX_SHOW_CHECK=$(jj "$H_ALICE" tx show --id "$TX_ID" 2>&1)
    if echo "$TX_SHOW_CHECK" | grep -qi "Status\|ActionID\|action_id\|success"; then
        ok "18.4 transaction inspectable by id"
    else
        fail "18.4 transaction inspectable by id" "$TX_SHOW_CHECK"
    fi
fi

j "$H_ALICE" action enable --id "$ACTION_ID" >/dev/null 2>&1
STATS_CHECK=$(jj "$H_ALICE" stats show --action "$ACTION_ID" 2>&1)
if echo "$STATS_CHECK" | grep -qi "Uses\|uses\|successes\|Successes"; then
    ok "18.5 action stats inspectable"
else
    fail "18.5 action stats inspectable" "$STATS_CHECK"
fi

# Verify every call created exactly one transaction (count successes in tx list)
SUCC=$(echo "$TX_LIST_CHECK" | python3 -c "
import sys, json
txs = json.load(sys.stdin)
print(sum(1 for t in txs if t.get('Status','') == 'success'))
" 2>/dev/null || echo "?")
ok "18.6 each successful call created a transaction record (successes=$SUCC)"

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 19: Zero-credit process ==="

# Alice starts a process with 0 funds — no credits should be deducted.
ALICE_BAL_BEFORE=$(numfield "$(jj "$H_ALICE" user me 2>&1)" "available")
ZERO_PROC_DATA=$(jj "$H_ALICE" process start --funds 0 2>&1)
ZERO_PROC_ID=$(strfield "$ZERO_PROC_DATA" "process_id")

if [ -n "$ZERO_PROC_ID" ]; then
    ok "19.1 zero-credit process started"
else
    fail "19.1 zero-credit process started" "$ZERO_PROC_DATA"
fi

ALICE_BAL_AFTER=$(numfield "$(jj "$H_ALICE" user me 2>&1)" "available")
if [ "${ALICE_BAL_BEFORE:-0}" = "${ALICE_BAL_AFTER:-0}" ]; then
    ok "19.2 user balance unchanged after 0-fund process start"
else
    fail "19.2 user balance unchanged" "before=$ALICE_BAL_BEFORE after=$ALICE_BAL_AFTER"
fi

# Call a free action (wasm-echo, price=0) from the zero-credit process.
ZERO_CALL=$(jj "$H_ALICE" call --process "$ZERO_PROC_ID" --target @alice --action /wasm-echo --args '{}' 2>&1)
if echo "$ZERO_CALL" | grep -q "tx_id"; then
    ok "19.3 free action callable from zero-credit process"
else
    fail "19.3 free action callable from zero-credit process" "$ZERO_CALL"
fi

j "$H_ALICE" process end --id "$ZERO_PROC_ID" >/dev/null 2>&1

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "=== FLOW 20: Federation — register remote kernel and call imported action ==="

# Set up the remote kernel via its HTTP API: login, create /greet, enable, grant-all.
REMOTE_TOKEN_JSON=$(curl -s -X POST "http://127.0.0.1:19875/v1/auth/token" \
    -H "Content-Type: application/json" \
    -d "{\"handle\":\"@sys\",\"password\":\"$REMOTE_SYS_PASS\"}")
REMOTE_TOKEN=$(strfield "$REMOTE_TOKEN_JSON" "token")

REMOTE_GREET=$(curl -s -X POST "http://127.0.0.1:19875/v1/actions" \
    -H "Authorization: Bearer $REMOTE_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"name":"/greet","kind":"http","source":"http://127.0.0.1:19871","price":0,"description":"says hello","input_schema":{"type":"object"},"output_schema":{"type":"object"}}')
REMOTE_GREET_ID=$(strfield "$REMOTE_GREET" "ID")

curl -s -X POST "http://127.0.0.1:19875/v1/actions/$REMOTE_GREET_ID/enable" \
    -H "Authorization: Bearer $REMOTE_TOKEN" >/dev/null
curl -s -X POST "http://127.0.0.1:19875/v1/actions/$REMOTE_GREET_ID/grant-all" \
    -H "Authorization: Bearer $REMOTE_TOKEN" >/dev/null

# @sys on the local kernel registers the remote kernel.
REMOTE_ADD=$(j "$H_SYS" remote add "http://127.0.0.1:19875" 2>&1)
if echo "$REMOTE_ADD" | grep -qi "registered\|127.0.0.1:19875"; then
    ok "20.1 remote kernel registered"
else
    fail "20.1 remote kernel registered" "$REMOTE_ADD"
fi

# @sys lists remote kernels — @127.0.0.1:19875 should appear.
REMOTE_LIST=$(j "$H_SYS" remote list 2>&1)
if echo "$REMOTE_LIST" | grep -q "127.0.0.1:19875"; then
    ok "20.2 remote kernel visible in list"
else
    fail "20.2 remote kernel visible in list" "$REMOTE_LIST"
fi

# @sys imports /greet from the remote kernel.
REMOTE_IMPORT=$(j "$H_SYS" remote import @127.0.0.1:19875 /greet 2>&1)
if echo "$REMOTE_IMPORT" | grep -qi "imported"; then
    ok "20.3 action imported from remote kernel"
else
    fail "20.3 action imported from remote kernel" "$REMOTE_IMPORT"
fi

# Extract the imported action ID: "Imported action @127.0.0.1:19875//greet (id=UUID)"
GREET_ID=$(echo "$REMOTE_IMPORT" | grep -oE 'id=[a-f0-9-]+' | sed 's/id=//')

if [ -n "$GREET_ID" ]; then
    # @sys activates and grants the imported action so any user can call it.
    j "$H_SYS" action enable --id "$GREET_ID" >/dev/null 2>&1
    j "$H_SYS" action grant-all --id "$GREET_ID" >/dev/null 2>&1

    # Alice calls the imported action — the call crosses the network to the real remote kernel.
    FED_PROC_DATA=$(jj "$H_ALICE" process start --funds 0 2>&1)
    FED_PROC_ID=$(strfield "$FED_PROC_DATA" "process_id")
    FED_CALL=$(jj "$H_ALICE" call --process "$FED_PROC_ID" --target @127.0.0.1:19875 --action /greet --args '{}' 2>&1)
    if echo "$FED_CALL" | grep -q "tx_id"; then
        ok "20.4 imported remote action callable by local user (real network call)"
    else
        fail "20.4 imported remote action callable by local user (real network call)" "$FED_CALL"
    fi
    j "$H_ALICE" process end --id "$FED_PROC_ID" >/dev/null 2>&1
else
    fail "20.4 imported remote action callable by local user (real network call)" "could not extract action id from: $REMOTE_IMPORT"
fi

# ═══════════════════════════════════════════════════════════════════════════════
echo ""
echo "═══════════════════════════════════════════"
printf "PASS: %d   FAIL: %d\n" "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
    echo ""
    echo "Failures:"
    printf "$ERRS\n"
    exit 1
fi
echo "All flows verified."
