package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assertActionRef verifies an action field is a well-formed "@owner/name" reference
// with exactly one leading "@" and exactly one "/".
func assertActionRef(t *testing.T, v any) {
	t.Helper()
	s, ok := v.(string)
	if !ok || s == "" {
		t.Errorf("action field is not a non-empty string: %v", v)
		return
	}
	if strings.HasPrefix(s, "@@") {
		t.Errorf("action field has double @: %q", s)
	}
	if !strings.HasPrefix(s, "@") {
		t.Errorf("action field must start with @, got %q", s)
	}
	if strings.Count(s, "/") != 1 {
		t.Errorf("action field must contain exactly one /, got %q", s)
	}
}

// newStepBackend creates a test HTTP server that acts as a backend for WASM-less step actions.
func newStepBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// createStepAction creates an enabled public HTTP action for step tests.
// Returns (actionID, "@owner/name" ref).
func createStepAction(t *testing.T, srv *httptest.Server, backendURL, ownerTok, handle, name string) (string, string) {
	t.Helper()
	cr := httpDo(t, srv, "POST", "/v1/actions", map[string]any{
		"name": name, "kind": "http", "price": 0, "source": backendURL,
		"description": "step test action", "input_schema": minSchema, "output_schema": minSchema,
	}, ownerTok)
	var act map[string]any
	decodeResponse(t, cr, &act)
	if cr.StatusCode != http.StatusCreated {
		t.Fatalf("create action %s: expected 201, got %d", name, cr.StatusCode)
	}
	id := act["id"].(string)
	httpDo(t, srv, "POST", "/v1/actions/"+id+"/enable", nil, ownerTok).Body.Close()
	httpDo(t, srv, "PUT", "/v1/actions/"+id, map[string]any{"public": true}, ownerTok).Body.Close()
	return id, handle + "/" + name
}

func TestServeCreateStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@cs-create-owner")
	makeUser(t, k, "@cs-create-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@cs-create-owner", "cs-create-svc")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	resp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"process_id":      pid,
		"next_action_id":  actionID,
		"required_caller": "@cs-create-caller",
		"partial_args":    map[string]any{"preset": "val"},
	}, ownerTok)
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("POST /v1/steps: expected 201, got %d", resp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, resp, &step)
	if step["id"] == nil || step["id"] == "" {
		t.Error("expected step id in response")
	}
	if step["status"] != "waiting" {
		t.Errorf("expected status=waiting, got %v", step["status"])
	}
	// Computed action field must be present and well-formed.
	if step["action"] == nil || step["action"] == "" {
		t.Error("expected computed action field in POST /v1/steps response")
	}
	assertActionRef(t, step["action"])
}

func TestServeListSteps(t *testing.T) {
	backend := newStepBackend(t)
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@sl-steps-owner")
	makeUser(t, k, "@sl-steps-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@sl-steps-owner", "sl-steps-svc")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	// Create two steps.
	for range 2 {
		r := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
			"process_id":      pid,
			"next_action_id":  actionID,
			"required_caller": "@sl-steps-caller",
		}, ownerTok)
		if r.StatusCode != http.StatusCreated {
			r.Body.Close()
			t.Fatalf("create step: expected 201, got %d", r.StatusCode)
		}
		r.Body.Close()
	}

	// List all steps.
	resp := httpDo(t, srv, "GET", "/v1/steps", nil, ownerTok)
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/steps: expected 200, got %d", resp.StatusCode)
	}
	var steps []map[string]any
	decodeResponse(t, resp, &steps)
	if len(steps) < 2 {
		t.Errorf("expected at least 2 steps, got %d", len(steps))
	}

	// Filter by process_id.
	resp2 := httpDo(t, srv, "GET", "/v1/steps?process_id="+pid, nil, ownerTok)
	if resp2.StatusCode != http.StatusOK {
		resp2.Body.Close()
		t.Fatalf("GET /v1/steps?process_id: expected 200, got %d", resp2.StatusCode)
	}
	var filtered []map[string]any
	decodeResponse(t, resp2, &filtered)
	if len(filtered) < 2 {
		t.Errorf("expected at least 2 steps by process_id filter, got %d", len(filtered))
	}

	// Filter by status=waiting.
	resp3 := httpDo(t, srv, "GET", "/v1/steps?status=waiting", nil, ownerTok)
	if resp3.StatusCode != http.StatusOK {
		resp3.Body.Close()
		t.Fatalf("GET /v1/steps?status=waiting: expected 200, got %d", resp3.StatusCode)
	}
	var waitingSteps []map[string]any
	decodeResponse(t, resp3, &waitingSteps)
	for _, s := range waitingSteps {
		if s["status"] != "waiting" {
			t.Errorf("list with status=waiting returned step with status=%v", s["status"])
		}
	}
}

func TestServeGetStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@gs-steps-owner")
	_, callerTok := makeUser(t, k, "@gs-steps-caller")
	_, unrelTok := makeUser(t, k, "@gs-steps-unrelated")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@gs-steps-owner", "gs-steps-svc")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"process_id":      pid,
		"next_action_id":  actionID,
		"required_caller": "@gs-steps-caller",
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	// Owner can read.
	r1 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, ownerTok)
	if r1.StatusCode != http.StatusOK {
		r1.Body.Close()
		t.Errorf("owner GET /v1/steps/%s: expected 200, got %d", sid, r1.StatusCode)
	} else {
		var got map[string]any
		decodeResponse(t, r1, &got)
		if got["id"] != sid {
			t.Error("step id mismatch in read response")
		}
		// Computed action field must be present and well-formed.
		if got["action"] == nil || got["action"] == "" {
			t.Error("expected computed action field in GET /v1/steps/{id} response")
		}
		assertActionRef(t, got["action"])
	}

	// Required caller can read.
	r2 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, callerTok)
	if r2.StatusCode != http.StatusOK {
		r2.Body.Close()
		t.Errorf("caller GET /v1/steps/%s: expected 200, got %d", sid, r2.StatusCode)
	} else {
		r2.Body.Close()
	}

	// Unrelated user is denied.
	r3 := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, unrelTok)
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusForbidden {
		t.Errorf("unrelated GET /v1/steps/%s: expected 403, got %d", sid, r3.StatusCode)
	}
}

func TestServeCompleteStepMissingArgs(t *testing.T) {
	backend := newStepBackend(t)
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@csmiss-owner")
	_, callerTok := makeUser(t, k, "@csmiss-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@csmiss-owner", "csmiss-svc")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"process_id":      pid,
		"next_action_id":  actionID,
		"required_caller": "@csmiss-caller",
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	// POST /complete with missing "args" field → 422.
	resp := httpDo(t, srv, "POST", "/v1/steps/"+sid+"/complete", map[string]any{
		"not_args": "value",
	}, callerTok)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("complete missing args: expected 422, got %d", resp.StatusCode)
	}
}

func TestServeCompleteStep(t *testing.T) {
	backend := newStepBackend(t)
	srv, k := newTestHTTPServer(t)
	defer srv.Close()

	_, ownerTok := makeUser(t, k, "@cs2-owner")
	_, callerTok := makeUser(t, k, "@cs2-caller")

	actionID, _ := createStepAction(t, srv, backend.URL, ownerTok, "@cs2-owner", "cs2-svc")

	pr := httpDo(t, srv, "POST", "/v1/processes", map[string]any{"funds": 0}, ownerTok)
	var proc map[string]any
	decodeResponse(t, pr, &proc)
	pid := proc["process_id"].(string)

	stepResp := httpDo(t, srv, "POST", "/v1/steps", map[string]any{
		"process_id":      pid,
		"next_action_id":  actionID,
		"required_caller": "@cs2-caller",
		"partial_args":    map[string]any{"from_partial": "A"},
	}, ownerTok)
	if stepResp.StatusCode != http.StatusCreated {
		stepResp.Body.Close()
		t.Fatalf("create step: expected 201, got %d", stepResp.StatusCode)
	}
	var step map[string]any
	decodeResponse(t, stepResp, &step)
	sid := step["id"].(string)

	// Required caller completes the step.
	complResp := httpDo(t, srv, "POST", "/v1/steps/"+sid+"/complete", map[string]any{
		"args": map[string]any{"from_caller": "B"},
	}, callerTok)
	if complResp.StatusCode != http.StatusOK {
		complResp.Body.Close()
		t.Fatalf("complete step: expected 200, got %d", complResp.StatusCode)
	}
	var reply map[string]any
	decodeResponse(t, complResp, &reply)
	if reply["tx_id"] == nil || reply["tx_id"] == "" {
		t.Error("expected tx_id in complete step reply")
	}
	if reply["step_id"] != sid {
		t.Errorf("expected step_id=%s in reply, got %v", sid, reply["step_id"])
	}

	// Step must now be done.
	getResp := httpDo(t, srv, "GET", "/v1/steps/"+sid, nil, ownerTok)
	var doneStep map[string]any
	decodeResponse(t, getResp, &doneStep)
	if doneStep["status"] != "done" {
		t.Errorf("expected step status=done after complete, got %v", doneStep["status"])
	}
}
