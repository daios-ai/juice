# PKCE auth, refresh rotation, receipts, lookup, chat, and OpenAPI flows. Built on flows/lib.sh.
# Dedupe: CLI == HTTP client (CLI login/refresh/run/action-import already hit the endpoints),
# so per-flow curl mirrors are dropped. Curl is kept only for raw PKCE wire assertions.

flow_pkce_auth() {
    echo "=== FLOW pkce_auth ==="
    local dir db hs base v ch code; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys)
    make_admin "$db" "$hs" || { fail "pkce_auth.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$(home "$dir" alice)" alice
    base=$(url "$db")

    v=$(pkce_verifier); ch=$(pkce_challenge "$v"); code=$(pkce_code "$base" alice userpass "$ch")
    # authorization_code + correct verifier → access_token.
    local tok; tok=$(strfield "$(curl -sf -X POST "$base/v1/auth/token" -H 'Content-Type: application/json' \
        -d "{\"code\":\"$code\",\"code_verifier\":\"$v\"}" 2>/dev/null)" access_token)
    assert_nonempty "pkce_auth.token_obtained" "$tok"

    # Reusing the (now-consumed) code → rejected (non-2xx).
    assert_ne "pkce_auth.code_reuse_rejected" 200 \
        "$(http_code POST "$base/v1/auth/token" "{\"code\":\"$code\",\"code_verifier\":\"$v\"}")"

    # A fresh code with the wrong verifier → rejected.
    local code2; code2=$(pkce_code "$base" alice userpass "$ch")
    assert_ne "pkce_auth.wrong_verifier_rejected" 200 \
        "$(http_code POST "$base/v1/auth/token" "{\"code\":\"$code2\",\"code_verifier\":\"wrong\"}")"
}

flow_refresh_rotation() {
    echo "=== FLOW refresh_rotation ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "refresh_rotation.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice   # CLI login uses PKCE → stores a refresh token

    # Refresh is automatic on a 401 (no standalone command): dropping the access token makes the
    # next authenticated call transparently refresh and rotate the refresh token.
    local rt1; rt1=$(profile_get "$ha" refresh_token)
    profile_set "$ha" token ""
    assert_json "refresh_rotation.refresh_succeeds" "$(jj "$db" "$ha" user me)" handle alice
    local rt2; rt2=$(profile_get "$ha" refresh_token)
    assert_ne "refresh_rotation.rt_rotated" "$rt1" "$rt2"

    # Old (rotated-away) refresh token is rejected: a stale access token 401s, and its auto-refresh
    # with the old refresh token is refused.
    local ho; ho=$(home "$dir" old)
    profile_set "$ho" endpoint "$(url "$db")"
    profile_set "$ho" token 'stale.access.token'
    profile_set "$ho" refresh_token "$rt1"
    assert_fails "refresh_rotation.old_rt_rejected" "invalid\|expired\|unauthenticated" -- j "$db" "$ho" user me

    # Logout revokes the current refresh token.
    j "$db" "$ha" auth logout >/dev/null 2>&1
    local hr; hr=$(home "$dir" rt2)
    profile_set "$hr" endpoint "$(url "$db")"
    profile_set "$hr" token 'stale.access.token'
    profile_set "$hr" refresh_token "$rt2"
    assert_fails "refresh_rotation.revoked_rt_rejected" "invalid\|expired\|unauthenticated" -- j "$db" "$hr" user me
}

flow_successful_receipt() {
    echo "=== FLOW successful_receipt ==="
    local dir db hs ha hb bport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 200 '{"ok":true}'
    make_admin "$db" "$hs" || { fail "successful_receipt.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    deposit "$db" "$hs" bob 100

    local aid; aid=$(publish "$db" "$ha" receipt-action --kind http --source "http://127.0.0.1:${bport}/act" --price 10 --description "receipt")

    local out; out=$(jj "$db" "$hb" run alice/receipt-action '{}')
    assert_nonempty "successful_receipt.call_succeeded" "$(strfield "$out" tx_id)"
    assert_nonempty "successful_receipt.receipt_id_returned" "$(strfield "$out" receipt_id)"
}

flow_failed_receipt() {
    echo "=== FLOW failed_receipt ==="
    local dir db hs ha hb bport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    bport=$(backend_port); start_backend "$bport" 500 '{"error":"backend error"}'
    make_admin "$db" "$hs" || { fail "failed_receipt.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    deposit "$db" "$hs" bob 100

    local aid; aid=$(publish "$db" "$ha" fail-action --kind http --source "http://127.0.0.1:${bport}/fail" --price 10 --description "fail")

    j "$db" "$hb" run alice/fail-action '{}' >/dev/null 2>&1 || true
    local txs; txs=$(jj "$db" "$hb" tx list)
    local tx_id; tx_id=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])[0]['id'])" "$txs" 2>/dev/null)
    assert_eq   "failed_receipt.failure_tx_recorded" failure "$(python3 -c "import sys,json;print(json.loads(sys.argv[1])[0]['status'])" "$txs" 2>/dev/null)"
    assert_json "failed_receipt.failed_tx_accessible" "$(jj "$db" "$hb" tx show "$tx_id")" status failure
}

flow_lookup() {
    echo "=== FLOW lookup ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "lookup.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    # sys/lookup requires "query"; missing it → schema violation.
    assert_fails "lookup.missing_query_rejected" "query\|required\|schema" -- j "$db" "$ha" run sys/lookup '{}'

    # Hybrid lookup degrades to the lexical (BM25) leg with no Ollama, so a distinctively-named
    # action is discoverable by keyword — the offline happy path, untestable before.
    local aid
    aid=$(strfield "$(jj "$db" "$ha" action create zqxwvprobe --kind http --source "https://api.example/x" --price 0 --description "zqxwvprobe lexical lookup probe")" id)
    assert_nonempty "lookup.action_created" "$aid"
    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    assert_contains "lookup.lexical_hit" "alice/zqxwvprobe" "$(jj "$db" "$ha" run sys/lookup '{"query":"zqxwvprobe"}')"
}

flow_chat() {
    echo "=== FLOW chat ==="
    local dir db hs ha; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    make_admin "$db" "$hs" || { fail "chat.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    # No chatter configured → ErrInvalidState (or a reply if one is); either is acceptable.
    local out; out=$(j "$db" "$ha" run sys/llm/chat '{"messages":[{"role":"user","content":"hello"}]}' 2>&1)
    case "$out" in *invalid?state*|*invalid_state*|*content*|*assistant*|*message*) ok "chat.no_chatter_or_reply";; *) fail "chat.no_chatter_or_reply" "got: $out";; esac
    # Missing "messages" → schema violation.
    assert_fails "chat.missing_messages_rejected" "messages\|required\|schema" -- j "$db" "$ha" run sys/llm/chat '{}'
}

# --- OpenAPI ---
# _greet_spec port file [desc]  — one-operation OpenAPI spec owned by alice.
_greet_spec() {
    python3 - "$1" "$2" "${3:-Say hello}" <<'PY'
import json,sys
port,f,desc=sys.argv[1],sys.argv[2],sys.argv[3]
json.dump({"openapi":"3.0.0","info":{"title":"T","version":"1"},
  "servers":[{"url":f"http://127.0.0.1:{port}"}],
  "paths":{"/greet":{"post":{"operationId":"greet","description":desc,"x-juice-price":5,
    "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string","description":"who"}}}}}},
    "responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"message":{"type":"string","description":"reply"}}}}}}}}}}},open(f,"w"))
PY
}

# _greet_spec_nopricing port file [desc] — as _greet_spec, but the document declares no price, so
# the price belongs to the owner.
_greet_spec_nopricing() {
    python3 - "$1" "$2" "${3:-Say hello}" <<'PY'
import json,sys
port,f,desc=sys.argv[1],sys.argv[2],sys.argv[3]
json.dump({"openapi":"3.0.0","info":{"title":"T","version":"1"},
  "servers":[{"url":f"http://127.0.0.1:{port}"}],
  "paths":{"/greet":{"post":{"operationId":"greet","description":desc,
    "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string","description":"who"}}}}}},
    "responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"message":{"type":"string","description":"reply"}}}}}}}}}}},open(f,"w"))
PY
}

flow_openapi_import_execute() {
    echo "=== FLOW openapi_import_execute ==="
    local dir db hs ha hb aport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    aport=$(backend_port); _greet_spec "$aport" "$dir/spec.json"; start_api_server "$aport" "$dir/spec.json"
    make_admin "$db" "$hs" || { fail "openapi_import.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    deposit "$db" "$hs" bob 50

    # Server fetches the spec (allow_local_sources=true). The application is named once.
    local imp; imp=$(jj "$db" "$ha" action import mail "http://127.0.0.1:${aport}/")
    assert_eq "openapi_import.created_1" 1 "$(python3 -c "import sys,json;print(len(json.loads(sys.argv[1]).get('created',[])))" "$imp" 2>/dev/null)"
    local aid name
    aid=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])['created'][0]['id'])" "$imp" 2>/dev/null)
    name=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])['created'][0]['name'])" "$imp" 2>/dev/null)
    assert_eq "openapi_import.action_name" mail/greet "$name"
    # The import output names what it installed rather than counting it.
    assert_contains "openapi_import.names_rows" "mail/greet" "$(j "$db" "$ha" action import mail)"

    j "$db" "$ha" action enable "$aid" >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --visibility public >/dev/null 2>&1
    assert_nonempty "openapi_import.call_succeeds" "$(strfield "$(jj "$db" "$hb" run "alice/$name" '{}')" tx_id)"
}

flow_openapi_application() {
    echo "=== FLOW openapi_application ==="
    local dir db hs ha hb aport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice); hb=$(home "$dir" bob)
    aport=$(backend_port)
    python3 - "$aport" "$dir/spec.json" <<'PY'
import json,sys
port,f=sys.argv[1],sys.argv[2]
op=lambda oid,desc:{"post":{"operationId":oid,"description":desc,
  "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string","description":"who"}}}}}},
  "responses":{"200":{"content":{"application/json":{"schema":{"type":"object","properties":{"message":{"type":"string","description":"reply"}}}}}}}}}
json.dump({"openapi":"3.0.0","info":{"title":"T","version":"1"},
  "servers":[{"url":f"http://127.0.0.1:{port}"}],
  "paths":{"/":op("index","what this application is"),"/greet":op("greet","says hello")}},open(f,"w"))
PY
    start_api_server "$aport" "$dir/spec.json"
    make_admin "$db" "$hs" || { fail "openapi_application.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice
    make_user "$db" "$hs" "$hb" bob
    deposit "$db" "$hs" bob 50

    # One spec, one application: every operation lands under the prefix, and the operation named
    # index becomes the group's root.
    local imp; imp=$(jj "$db" "$ha" action import mail "http://127.0.0.1:${aport}/")
    assert_eq "openapi_application.created_2" 2 "$(python3 -c "import sys,json;print(len(json.loads(sys.argv[1]).get('created',[])))" "$imp" 2>/dev/null)"
    local idx_id greet_id
    idx_id=$(python3 -c "import sys,json;print(next(a['id'] for a in json.loads(sys.argv[1])['created'] if a['name']=='mail/index'))" "$imp" 2>/dev/null)
    greet_id=$(python3 -c "import sys,json;print(next(a['id'] for a in json.loads(sys.argv[1])['created'] if a['name']=='mail/greet'))" "$imp" 2>/dev/null)
    assert_nonempty "openapi_application.index_imported" "$idx_id"

    # One command for the whole application: the path names the action at it and everything below.
    j "$db" "$ha" action enable alice/mail >/dev/null 2>&1
    j "$db" "$ha" action update alice/mail --visibility public >/dev/null 2>&1

    # The group answers to its own name: reading and running alice/mail both reach the index.
    assert_json "openapi_application.show_root" "$(jj "$db" "$hb" action show alice/mail)" id "$idx_id"
    assert_nonempty "openapi_application.run_root" "$(strfield "$(jj "$db" "$hb" run alice/mail '{}')" tx_id)"
    assert_nonempty "openapi_application.run_member" "$(strfield "$(jj "$db" "$hb" run alice/mail/greet '{}')" tx_id)"
    # Members keep their own identity; the index does not shadow them.
    assert_json "openapi_application.member_distinct" "$(jj "$db" "$hb" action show alice/mail/greet)" id "$greet_id"
    # The same document under a second name is an independent application; a different document
    # under an occupied name is refused.
    assert_eq "openapi_application.second_name" 2 "$(python3 -c "import sys,json;print(len(json.loads(sys.argv[1]).get('created',[])))" "$(jj "$db" "$ha" action import inbox "http://127.0.0.1:${aport}/")" 2>/dev/null)"
    assert_fails "openapi_application.rebind_refused" "already holds" -- j "$db" "$ha" action import mail "http://127.0.0.1:${aport}/other.json"
}

flow_openapi_changed_reimport() {
    echo "=== FLOW openapi_changed_reimport ==="
    local dir db hs ha aport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    aport=$(backend_port); _greet_spec "$aport" "$dir/spec.json" "hello v1"; start_api_server "$aport" "$dir/spec.json"
    make_admin "$db" "$hs" || { fail "openapi_reimport.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    local imp1; imp1=$(jj "$db" "$ha" action import mail "http://127.0.0.1:${aport}/")
    local aid; aid=$(python3 -c "import sys,json;print(json.loads(sys.argv[1])['created'][0]['id'])" "$imp1" 2>/dev/null)

    # Change the description → the document moved → re-import updates + deactivates, id preserved.
    # The re-import needs only the application's name; the document URL was recorded at install.
    _greet_spec "$aport" "$dir/spec.json" "hello v2"
    local imp2; imp2=$(jj "$db" "$ha" action import mail)
    assert_eq "openapi_reimport.updated_1" 1 "$(python3 -c "import sys,json;print(len(json.loads(sys.argv[1]).get('updated',[])))" "$imp2" 2>/dev/null)"
    assert_eq "openapi_reimport.id_preserved" "$aid" "$(python3 -c "import sys,json;print(json.loads(sys.argv[1])['updated'][0]['id'])" "$imp2" 2>/dev/null)"
    assert_json "openapi_reimport.deactivated" "$(jj "$db" "$ha" action show "$aid")" active False

    # A price the owner set stays the owner's across a re-import when the document declares none.
    _greet_spec_nopricing "$aport" "$dir/spec.json" "hello v3"
    j "$db" "$ha" action import mail >/dev/null 2>&1
    j "$db" "$ha" action update "$aid" --price 11 >/dev/null 2>&1
    _greet_spec_nopricing "$aport" "$dir/spec.json" "hello v4"
    j "$db" "$ha" action import mail >/dev/null 2>&1
    assert_json "openapi_reimport.owner_price_kept" "$(jj "$db" "$ha" action show "$aid")" price 11
}

flow_openapi_disable_tree() {
    echo "=== FLOW openapi_disable_tree ==="
    local dir db hs ha aport; dir=$(new_dir); db="$(kdb "$dir")"; hs=$(home "$dir" sys); ha=$(home "$dir" alice)
    aport=$(backend_port)
    python3 - "$aport" "$dir/spec.json" <<'PY'
import json,sys
port,f=sys.argv[1],sys.argv[2]
op=lambda oid,desc:{"post":{"operationId":oid,"description":desc,
  "requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string","description":"who"}}}}}},
  "responses":{"200":{"content":{"application/json":{"schema":{"type":"object"}}}}}}}
json.dump({"openapi":"3.0.0","info":{"title":"T","version":"1"},
  "servers":[{"url":f"http://127.0.0.1:{port}"}],
  "paths":{"/greet":op("greet","hi"),"/farewell":op("farewell","bye")}},open(f,"w"))
PY
    start_api_server "$aport" "$dir/spec.json"
    make_admin "$db" "$hs" || { fail "openapi_unimport.boot" "server did not start"; return; }
    make_user "$db" "$hs" "$ha" alice

    local imp; imp=$(jj "$db" "$ha" action import mail "http://127.0.0.1:${aport}/")
    local greet_id; greet_id=$(python3 -c "import sys,json;print(next(a['id'] for a in json.loads(sys.argv[1])['created'] if 'greet' in a['name']))" "$imp" 2>/dev/null)
    j "$db" "$ha" action enable alice/mail >/dev/null 2>&1
    # A manual action outside the application's path must NOT be touched.
    local manual_id; manual_id=$(enabled "$db" "$ha" manual --kind http --source "http://127.0.0.1:${aport}/manual" --price 0 --description "manual")

    # Withdrawing an application is the ordinary disable verb over its path: no separate verb.
    assert_contains "openapi_disable.names_rows" "mail/greet" "$(j "$db" "$ha" action disable alice/mail)"
    assert_json "openapi_disable.openapi_deactivated" "$(jj "$db" "$ha" action show "$greet_id")" active False
    assert_json "openapi_disable.manual_unaffected"   "$(jj "$db" "$ha" action show "$manual_id")" active True

    # Deleting the application keeps its history and leaves everything outside its path alone.
    j "$db" "$ha" action delete alice/mail >/dev/null 2>&1
    assert_fails "openapi_disable.deleted" "not found" -- j "$db" "$ha" action show "$greet_id"
    assert_json "openapi_disable.manual_survives_delete" "$(jj "$db" "$ha" action show "$manual_id")" active True
}
