package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/google/uuid"
)

func TestImportOpenAPI(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-import-owner", 0)
	specURL := "https://spec.example.com/api.json"

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d (updated=%d unchanged=%d rejected=%d)",
			len(result.Created), len(result.Updated), len(result.Unchanged), len(result.Rejected))
	}
	a := result.Created[0]
	if a.Name != "sayHello" {
		t.Errorf("name: got %q, want %q", a.Name, "sayHello")
	}
	if a.Active {
		t.Error("imported action must be inactive")
	}
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
		t.Fatalf("action source is not valid HTTPSource JSON: %v", err)
	}
	if src.OperationKey != "sayHello" {
		t.Errorf("operation_key: got %q, want %q", src.OperationKey, "sayHello")
	}

	// Re-import with identical spec → Unchanged.
	result2, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if len(result2.Unchanged) != 1 || len(result2.Created) != 0 {
		t.Errorf("reimport: want 1 unchanged, got created=%d updated=%d unchanged=%d",
			len(result2.Created), len(result2.Updated), len(result2.Unchanged))
	}
}

func TestUnimportOpenAPI(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-unimport-owner", 0)
	specURL := "https://spec.example.com/api.json"

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec)); err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}

	actions, err := k.UnimportOpenAPI(ctx, owner.ID, owner.ID, specURL, "")
	if err != nil {
		t.Fatalf("UnimportOpenAPI: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 deactivated action, got %d", len(actions))
	}

	// UnimportOpenAPI with name filter deactivates only the matching action.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec)); err != nil {
		t.Fatalf("reimport: %v", err)
	}
	actions2, err := k.UnimportOpenAPI(ctx, owner.ID, owner.ID, specURL, "sayHello")
	if err != nil {
		t.Fatalf("UnimportOpenAPI by name: %v", err)
	}
	if len(actions2) != 1 {
		t.Fatalf("expected 1 action for name filter, got %d", len(actions2))
	}
}

// TestUnimportOpenAPILeavesManualHTTPUntouched verifies a manually-created
// kind=http action (type:"http") owned by the same user is invisible to OpenAPI
// reconciliation: reimport does not see it and unimport does not deactivate it.
func TestUnimportOpenAPILeavesManualHTTPUntouched(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-isolation-owner", 0)
	specURL := "https://spec.example.com/api.json"

	// A manual http action with the same owner.
	manual, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Name: "manual-svc", Kind: kernel.KindHTTP,
		Source: "https://api.example.com/manual", Method: "POST",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec)); err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	deactivated, err := k.UnimportOpenAPI(ctx, owner.ID, owner.ID, specURL, "")
	if err != nil {
		t.Fatalf("UnimportOpenAPI: %v", err)
	}
	for _, a := range deactivated {
		if a.ID == manual.ID {
			t.Fatal("unimport must not touch the manual http action")
		}
	}
	// The manual action's source must still be its structured http form.
	got, err := k.ReadAction(ctx, manual.ID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	var s kernel.HTTPSource
	if err := json.Unmarshal([]byte(got.Source), &s); err != nil || s.Type != "http" {
		t.Errorf("manual source changed: type=%q err=%v", s.Type, err)
	}
}

func TestOpenAPIActivation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-activate-owner", 0)
	specURL := "https://spec.example.com/api.json"

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(minOpenAPISpec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	a := result.Created[0]

	// Activation must succeed: base_url is api.example.com (public), schemas are valid.
	if err := k.SetActive(ctx, owner.ID, a.ID, true); err != nil {
		t.Fatalf("SetActive: %v", err)
	}
	updated, err := k.ReadAction(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Active {
		t.Error("action should be active after SetActive(true)")
	}
}

func TestOpenAPIActivationRejectsPrivateBaseURL(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st) // AllowLocalSources = false
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-private-owner", 0)

	// Craft an HTTPSource with a private execution base URL.
	src := kernel.HTTPSource{
		Type:          "openapi",
		SpecURL:       "https://spec.example.com/api.json",
		BaseURL:       "http://10.0.0.1",
		Method:        "GET",
		Path:          "/secret",
		OperationKey:  "getSecret",
		OperationHash: "hash",
	}
	srcBytes, _ := json.Marshal(src)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "@oapi-private-owner/getSecret",
		Kind:         kernel.KindHTTP,
		Source:       string(srcBytes),
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	if err := k.SetActive(ctx, owner.ID, a.ID, true); err == nil {
		t.Error("expected error activating action with private base URL, got nil")
	}
}

func TestImportOpenAPISetsOwnershipVerified(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-owner-verified", 0)
	specURL := "https://spec.example.com/api.json"
	specWithOwner := `{"openapi":"3.0.0","x-juice-owner":"@oapi-owner-verified","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(specWithOwner))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(result.Created[0].Source), &src); err != nil {
		t.Fatalf("source JSON invalid: %v", err)
	}
	if !src.OwnershipVerified {
		t.Error("expected OwnershipVerified=true when x-juice-owner matches handle")
	}
}

func TestMakePublicOpenAPIRequiresOwnershipVerified(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-grant-owner", 0)
	src := kernel.HTTPSource{Type: "openapi", SpecURL: "https://spec.example.com/api.json", BaseURL: "http://api.example.com", Method: "GET", Path: "/hello", OperationKey: "sayHello", OwnershipVerified: false}
	srcBytes, _ := json.Marshal(src)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "@oapi-grant-owner/sayHello",
		Kind: kernel.KindHTTP, Active: false, Description: "test", Source: string(srcBytes),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	pub := true
	_, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pub})
	if err == nil {
		t.Fatal("expected error making OpenAPI action public without ownership verification, got nil")
	}
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestMakePublicOpenAPIWithOwnershipVerified(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-grant-verified", 0)
	src := kernel.HTTPSource{Type: "openapi", SpecURL: "https://spec.example.com/api.json", BaseURL: "http://api.example.com", Method: "GET", Path: "/hello", OperationKey: "sayHello", OwnershipVerified: true}
	srcBytes, _ := json.Marshal(src)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "@oapi-grant-verified/sayHello",
		Kind: kernel.KindHTTP, Active: false, Description: "test", Source: string(srcBytes),
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	pub := true
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Public: &pub}); err != nil {
		t.Errorf("UpdateAction with OwnershipVerified=true: unexpected error: %v", err)
	}
}

func TestSetActivePublicOpenAPIRequiresOwnershipVerified(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-setactive-owner", 0)
	src := kernel.HTTPSource{Type: "openapi", SpecURL: "https://spec.example.com/api.json", BaseURL: "http://api.example.com", Method: "GET", Path: "/hello", OperationKey: "sayHello", OwnershipVerified: false}
	srcBytes, _ := json.Marshal(src)
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: owner.ID, Name: "@oapi-setactive-owner/sayHello",
		Kind: kernel.KindHTTP, Active: false, Public: true, Description: "test", Source: string(srcBytes),
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{}},
		OutputSchema: map[string]any{"type": "object"},
		CreatedAt:    time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(ctx, a); err != nil {
		t.Fatal(err)
	}

	err := k.SetActive(ctx, owner.ID, a.ID, true)
	if err == nil {
		t.Fatal("expected error from SetActive on public unverified OpenAPI action, got nil")
	}
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized, got %v", err)
	}
}

func TestImportOpenAPISubjectMismatchRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	userA := setupUser(t, st, "@user-a-imp", 0)
	userB := setupUser(t, st, "@user-b-imp", 0)

	_, err := k.ImportOpenAPI(ctx, userA.ID, userB.ID, "http://spec.example.com", []byte(minOpenAPISpec))
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized when subject != owner, got %v", err)
	}
}

// ---- OpenAPI well-known ownership proof tests ----

func TestImportOpenAPIWellKnownSetsOwnershipVerified(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "@wk-owner", 0)
	fetcher := &fakeURLFetcher{wellKnown: map[string]string{
		"http://api.example.com/.well-known/juice-owner.txt": "@wk-owner",
	}}
	k := newTestKernelWithHTTP(st, fetcher)

	specURL := "https://spec.example.com/api.json"
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"msg":{"type":"string","description":"the message"}}}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(result.Created[0].Source), &src); err != nil {
		t.Fatalf("source JSON invalid: %v", err)
	}
	if !src.OwnershipVerified {
		t.Error("expected OwnershipVerified=true from well-known challenge")
	}
}

func TestImportOpenAPIOwnershipStalenessFixed(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	owner := setupUser(t, st, "@stale-owner", 0)
	// First import: well-known returns owner handle → OwnershipVerified=true.
	fetcher := &fakeURLFetcher{wellKnown: map[string]string{
		"http://api.example.com/.well-known/juice-owner.txt": "@stale-owner",
	}}
	k := newTestKernelWithHTTP(st, fetcher)

	specURL := "https://spec.example.com/api.json"
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"msg":{"type":"string","description":"the message"}}}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil || len(result.Created) != 1 {
		t.Fatalf("first import failed: %v, created=%d", err, len(result.Created))
	}

	// Second import: well-known now returns wrong handle → proof revoked.
	fetcher.wellKnown["http://api.example.com/.well-known/juice-owner.txt"] = "@other-owner"
	k2 := newTestKernelWithHTTP(st, fetcher)

	result2, err := k2.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("second import failed: %v", err)
	}
	// Action is Unchanged (hash same) but OwnershipVerified must be updated to false.
	if len(result2.Unchanged) != 1 {
		t.Fatalf("expected 1 unchanged action, got %d", len(result2.Unchanged))
	}
	a, err := st.ReadAction(ctx, result2.Unchanged[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
		t.Fatalf("source JSON: %v", err)
	}
	if src.OwnershipVerified {
		t.Error("expected OwnershipVerified=false after proof was revoked")
	}
}

// ---- UnimportOpenAPI owner-only test ----

func TestUnimportOpenAPIOwnerOnly(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@openapi-owner", 0)

	specURL := "https://spec.example.com/admin-test.json"
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"adminHello","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object","properties":{"msg":{"type":"string","description":"the message"}}}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d", len(result.Created))
	}

	// Owner can unimport their own actions.
	deactivated, err := k.UnimportOpenAPI(ctx, owner.ID, owner.ID, specURL, "")
	if err != nil {
		t.Fatalf("UnimportOpenAPI as owner: %v", err)
	}
	if len(deactivated) != 1 {
		t.Errorf("expected 1 deactivated action, got %d", len(deactivated))
	}

	// Unrelated user should be rejected.
	other := setupUser(t, st, "@openapi-other", 0)
	// Re-import to have an action to unimport.
	result2, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	_ = result2
	if _, err := k.UnimportOpenAPI(ctx, other.ID, owner.ID, specURL, ""); !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized for non-owner, got %v", err)
	}
}

func TestOpenAPIRejectMissingName(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-no-name", 0)
	specURL := "https://spec.example.com/api.json"

	// Operation has neither operationId nor x-juice-name.
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"description":"says hello","parameters":[{"name":"q","in":"query","description":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("ImportOpenAPI returned error: %v", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("expected 0 created, got %d", len(result.Created))
	}
	if len(result.Rejected) == 0 {
		t.Error("expected at least 1 rejection for missing operationId/x-juice-name")
	}
	found := false
	for _, r := range result.Rejected {
		if strings.Contains(r.Reason, "operationId") || strings.Contains(r.Reason, "x-juice-name") {
			found = true
		}
	}
	if !found {
		t.Errorf("rejection reason did not mention operationId or x-juice-name: %+v", result.Rejected)
	}
}

func TestOpenAPIRejectMissingInputContract(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-no-input", 0)
	specURL := "https://spec.example.com/api.json"

	// Operation has operationId and description but no parameters and no requestBody.
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/ping":{"get":{"operationId":"ping","description":"ping the server","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("ImportOpenAPI returned error: %v", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("expected 0 created, got %d", len(result.Created))
	}
	if len(result.Rejected) == 0 {
		t.Error("expected at least 1 rejection for missing input contract")
	}
	found := false
	for _, r := range result.Rejected {
		if strings.Contains(r.Reason, "parameters") || strings.Contains(r.Reason, "requestBody") {
			found = true
		}
	}
	if !found {
		t.Errorf("rejection reason did not mention parameters/requestBody: %+v", result.Rejected)
	}
}

func TestOpenAPIBodyRefParamsIncluded(t *testing.T) {
	// Verify that a requestBody whose schema is a $ref produces Params entries for the
	// body fields, so the executor routes them correctly at call time.
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "@oapi-ref-body", 0)
	specURL := "https://spec.example.com/api.json"

	spec := `{
		"openapi": "3.0.0",
		"info": {"title": "T", "version": "1"},
		"servers": [{"url": "http://api.example.com"}],
		"components": {
			"schemas": {
				"CreateReq": {
					"type": "object",
					"properties": {
						"title": {"type": "string", "description": "item title"},
						"count": {"type": "integer", "description": "quantity"}
					},
					"required": ["title"]
				}
			}
		},
		"paths": {
			"/items/{id}": {
				"post": {
					"operationId": "createItem",
					"description": "Create an item",
					"parameters": [{"name": "id", "in": "path", "required": true, "description": "item id", "schema": {"type": "string"}}],
					"requestBody": {
						"required": true,
						"content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateReq"}}}
					},
					"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "object", "properties": {"ok": {"type": "boolean", "description": "success"}}}}}}}
				}
			}
		}
	}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, specURL, []byte(spec))
	if err != nil {
		t.Fatalf("ImportOpenAPI error: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d (rejected: %+v)", len(result.Created), result.Rejected)
	}

	a := result.Created[0]
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
		t.Fatalf("unmarshal source: %v", err)
	}

	paramsByName := make(map[string]string)
	for _, p := range src.Params {
		paramsByName[p.Name] = p.In
	}
	if paramsByName["id"] != "path" {
		t.Errorf("expected id param in=path, got %q", paramsByName["id"])
	}
	if paramsByName["title"] != "body" {
		t.Errorf("expected title param in=body, got %q (params: %+v)", paramsByName["title"], src.Params)
	}
	if paramsByName["count"] != "body" {
		t.Errorf("expected count param in=body, got %q", paramsByName["count"])
	}
}
