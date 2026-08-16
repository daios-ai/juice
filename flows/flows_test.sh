#!/usr/bin/env bash
#
# flows_test.sh — end-to-end integration suite for the juice CLI/server.
#
# Each flow is an independent user story driving ONLY the built binary ($JUICE) against a
# real SQLite database and a real `juice serve` (see flows/lib.sh for the harness contract:
# one process registry, one trap, OS-assigned ports via `server.ready`, zero leaks).
#
# Usage:
#   go build -o /tmp/juice ./cmd/juice/
#   JUICE=/tmp/juice bash flows/flows_test.sh                  # default suite
#   FLOW=flow_time JUICE=/tmp/juice bash flows/flows_test.sh   # one flow (debugging)
#
# Opt-in suites (excluded from the default run; each needs a heavy external toolchain):
#   JUICE_TINYGO_FLOWS=1 ... — @sys/tinygo/compile (real TinyGo toolchain on PATH)
#
# Via the Go suite:  go test -tags integration ./cmd/juice/ -run TestFlowsIntegration
#
# Requires: bash >=4, python3, curl.

set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

source "$here/lib.sh"
source "$here/flows_foundation.sh"
source "$here/flows_calls.sh"
source "$here/flows_wasm.sh"
source "$here/flows_remote.sh"
source "$here/flows_federation.sh"
source "$here/flows_network.sh"
source "$here/flows_admin.sh"

# Opt-in: real-network federation check (public internet; run from a NAT'd machine).
if [ "${JUICE_NETWORK_FLOWS:-0}" = "1" ]; then
    echo "=== real-network federation check ONLY (JUICE_NETWORK_FLOWS=1) ==="
    run_flows flow_network_reachability
    exit $?
fi

# Opt-in: @sys/tinygo/compile only (real TinyGo toolchain on PATH).
if [ "${JUICE_TINYGO_FLOWS:-0}" = "1" ]; then
    echo "=== @sys/tinygo/compile flow ONLY (JUICE_TINYGO_FLOWS=1) ==="
    run_flows flow_tinygo_compile
    exit $?
fi

# Default suite. Excludes flow_tinygo_compile (opt-in above).
run_flows \
    flow_bootstrap flow_signup_errors flow_local_auth flow_recovery flow_suspension flow_deposits flow_transfers \
    flow_action_lifecycle flow_action_owner_visibility \
    flow_process_lifecycle flow_acl_public flow_successful_paid_call flow_http_verbs \
    flow_failed_call_refund flow_terms_changed_refused flow_input_schema_failure flow_output_schema_failure \
    flow_wasm_execution flow_contractor_subcall flow_contractor_failure \
    flow_step_success flow_step_failure flow_step_restart flow_locked_funds_recovery \
    flow_rating \
    flow_pkce_auth flow_refresh_rotation flow_successful_receipt flow_failed_receipt \
    flow_lookup flow_chat flow_openapi_import_execute flow_openapi_changed_reimport \
    flow_openapi_unimport \
    flow_federation_import_execute flow_federation_changed_reimport flow_fed_rename \
    flow_fed_verify_receipt flow_fed_all_receipt_checks flow_fed_suspend_blocks \
    flow_fed_denial_underfunded flow_fed_disabled_action_rejection flow_fed_import_duty flow_fed_failed_action_refund \
    flow_fed_gossip_discovery flow_fed_discovery flow_fed_offline flow_fed_peer_sync flow_fed_inspect_read_only \
    flow_fed_step_complete flow_settlement flow_transfer \
    flow_transaction_access flow_admin_supervision flow_native_orphan_purge flow_time flow_message flow_grant flow_grant_bearer
exit $?
