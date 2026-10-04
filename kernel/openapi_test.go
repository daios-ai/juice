// SPDX-License-Identifier: AGPL-3.0-only

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

// appOpenAPISpec has two operations, one of them named index, so an installed application has a
// root that is an ordinary imported action.
const appOpenAPISpec = `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/":{"get":{"operationId":"index","summary":"index","description":"what this application is","parameters":[{"name":"q","in":"query","description":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}},"/hello":{"get":{"operationId":"greet","summary":"greet","description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

// pricedSpec renders the one-operation spec with an x-juice-price extension, or without one when
// price is empty, so a test can say whether the document declares a price at all.
func pricedSpec(price string) string {
	ext := ""
	if price != "" {
		ext = `"x-juice-price":` + price + `,`
	}
	return `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","summary":"sayHello",` + ext +
		`"description":"says hello","parameters":[{"name":"name","in":"query","description":"who to greet","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
}

func sourceOf(t *testing.T, a *kernel.Action) kernel.HTTPSource {
	t.Helper()
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
		t.Fatalf("action source is not valid HTTPSource JSON: %v", err)
	}
	return src
}

func TestImportOpenAPI(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "oapi-import-owner", 0)
	specURL := "https://spec.example.com/api.json"

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(minOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d (updated=%d unchanged=%d rejected=%+v)",
			len(result.Created), len(result.Updated), len(result.Unchanged), result.Rejected)
	}
	a := result.Created[0]
	if a.Name != "mail/sayHello" {
		t.Errorf("name: got %q, want %q", a.Name, "mail/sayHello")
	}
	if a.Active {
		t.Error("imported action must be inactive")
	}
	if a.Visibility != kernel.VisibilityPrivate {
		t.Errorf("imported action must start private, got %q", a.Visibility)
	}
	if src := sourceOf(t, a); src.OperationKey != "sayHello" || src.SpecURL != specURL {
		t.Errorf("provenance: got key=%q spec=%q", src.OperationKey, src.SpecURL)
	}

	// Re-import needs the name alone: the document URL was recorded at installation.
	stored, err := k.StoredOpenAPISpecURL(ctx, owner.ID, owner.ID, "mail")
	if err != nil {
		t.Fatalf("StoredOpenAPISpecURL: %v", err)
	}
	if stored != specURL {
		t.Errorf("stored spec url: got %q, want %q", stored, specURL)
	}
	result2, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", stored, []byte(minOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if len(result2.Unchanged) != 1 || len(result2.Created) != 0 {
		t.Errorf("reimport: want 1 unchanged, got created=%d updated=%d unchanged=%d",
			len(result2.Created), len(result2.Updated), len(result2.Unchanged))
	}
}

// TestImportOpenAPINameIsIdentity: the application's path identifies it. One name holds one
// document, the same document may be installed under several names, and an application installed
// beneath another keeps its own rows.
func TestImportOpenAPINameIsIdentity(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)
	specURL := "http://api.example.com/openapi.json"

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(appOpenAPISpec), nil); err != nil {
		t.Fatalf("import: %v", err)
	}

	// The same document under a second name is an independent application.
	second, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "inbox", specURL, []byte(appOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(second.Created) != 2 {
		t.Fatalf("second install: created=%d, want 2", len(second.Created))
	}
	for _, a := range second.Created {
		if !strings.HasPrefix(a.Name, "inbox/") {
			t.Errorf("second install put %q outside its own name", a.Name)
		}
	}

	// An application installed beneath another is not absorbed by the parent's re-import.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail/calendar", "http://api.example.com/cal.json", []byte(minOpenAPISpec), nil); err != nil {
		t.Fatalf("nested install: %v", err)
	}
	again, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(appOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("parent re-import: %v", err)
	}
	if len(again.Deactivated) != 0 {
		t.Errorf("parent re-import withdrew %d nested row(s); it must see only its own", len(again.Deactivated))
	}
	if nested, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/calendar/sayHello"); nested == nil {
		t.Error("nested application row must survive the parent's re-import")
	}

	// A different document under an occupied name is refused, and the rows stay put.
	err = errOf(k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", "http://api.example.com/other.json", []byte(minOpenAPISpec), nil))
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("rebinding a name to another document: want ErrInvalidInput, got %v", err)
	}
	if a, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/greet"); a == nil {
		t.Error("a refused re-binding must leave the installed rows in place")
	}
}

// errOf drops an import result and keeps its error, for the refusal cases.
func errOf(_ *kernel.ImportResult, err error) error { return err }

// TestImportOpenAPIGroupRoot: the operation keyed index is the application's root, so the
// application answers to its own name.
func TestImportOpenAPIGroupRoot(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", "http://api.example.com/openapi.json", []byte(appOpenAPISpec), nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	root, err := k.ResolveAction(ctx, "acme@k/mail")
	if err != nil {
		t.Fatalf("resolve acme/mail: %v", err)
	}
	if root.Name != "mail/index" {
		t.Errorf("acme/mail resolved to %q, want mail/index", root.Name)
	}
}

// TestImportOpenAPINameValidation: the name is part of every action name it creates, so it must be
// addressable — no kernel qualifier, no empty segment, not id-shaped, and never absent.
func TestImportOpenAPINameValidation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)

	for _, bad := range []string{"", "ma@il", "/mail", "mail/", "mail//x", uuid.New().String()} {
		err := errOf(k.ImportOpenAPI(ctx, owner.ID, owner.ID, bad, "http://api.example.com/s.json", []byte(minOpenAPISpec), nil))
		if !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("name %q: want ErrInvalidInput, got %v", bad, err)
		}
	}
	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "  mail  ", "http://api.example.com/ok.json", []byte(minOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("padded name: %v", err)
	}
	if len(result.Created) != 1 || result.Created[0].Name != "mail/sayHello" {
		t.Errorf("padded name must trim: got %+v", result.Created)
	}
}

// TestImportOpenAPIPriceOwnership: the document owns the price only where it declares one.
func TestImportOpenAPIPriceOwnership(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)
	specURL := "http://api.example.com/openapi.json"

	// No x-juice-price: the owner's price survives a re-import that changes the document.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(pricedSpec("")), nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	a, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/sayHello")
	price := int64(7)
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Price: &price}); err != nil {
		t.Fatalf("set price: %v", err)
	}
	changed := strings.Replace(pricedSpec(""), "says hello", "greets you", 1)
	res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(changed), nil)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("changed description must update the row: %+v", res)
	}
	after, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/sayHello")
	if after.Price != 7 {
		t.Errorf("an undeclared price belongs to the owner: got %d, want 7", after.Price)
	}

	// A declared price is the document's, including an explicit zero, which must be distinguishable
	// from no declaration at all.
	declared, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(pricedSpec("0")), nil)
	if err != nil {
		t.Fatalf("declared-zero import: %v", err)
	}
	if len(declared.Updated) != 1 {
		t.Fatalf("declaring a price is a change: %+v", declared)
	}
	afterZero, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/sayHello")
	if afterZero.Price != 0 {
		t.Errorf("declared price 0 must apply: got %d", afterZero.Price)
	}
	if !sourceOf(t, afterZero).PriceDeclared {
		t.Error("provenance must record that the document declared a price")
	}

	// A declared price that is not a non-negative integer is refused, not silently zeroed.
	for _, bad := range []string{`"5"`, `-1`, `1.5`} {
		out, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "bad"+strings.Trim(bad, `"-.`), "http://api.example.com/"+strings.Trim(bad, `"-.`)+".json", []byte(pricedSpec(bad)), nil)
		if err != nil {
			t.Fatalf("bad price %s: %v", bad, err)
		}
		if len(out.Created) != 0 || len(out.Rejected) != 1 || !strings.Contains(out.Rejected[0].Reason, "price") {
			t.Errorf("bad price %s: want one price rejection, got %+v", bad, out)
		}
	}
}

// TestImportOpenAPIRestoresDocumentFields: the document owns its fields, so a hand edit to one of
// them is put back on the next import — the comparison is against the row, not a recorded hash.
func TestImportOpenAPIRestoresDocumentFields(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)
	specURL := "http://api.example.com/openapi.json"

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(minOpenAPISpec), nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	a, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/sayHello")
	edited := "hand-written description"
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Description: &edited}); err != nil {
		t.Fatalf("hand edit: %v", err)
	}
	res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(minOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if len(res.Updated) != 1 {
		t.Fatalf("an edited document field must re-import as changed: %+v", res)
	}
	back, _ := k.ReadActionByOwnerName(ctx, owner.ID, "mail/sayHello")
	if back.Description != "says hello" {
		t.Errorf("document field not restored: got %q", back.Description)
	}
}

// TestImportOpenAPIRejectsDuplicateKeys: two operations claiming one name are caught in the
// preflight, so the second cannot fail after the first has been written.
func TestImportOpenAPIRejectsDuplicateKeys(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)

	dup := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{
		"/a":{"get":{"operationId":"same","summary":"same","description":"first","parameters":[{"name":"q","in":"query","description":"q","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}},
		"/b":{"get":{"operationId":"same","summary":"same","description":"second","parameters":[{"name":"q","in":"query","description":"q","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "dup", "http://api.example.com/dup.json", []byte(dup), nil)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(res.Created) != 1 {
		t.Errorf("one of the two duplicates must be installed, got %d", len(res.Created))
	}
	if len(res.Rejected) != 1 || !strings.Contains(res.Rejected[0].Reason, "duplicate") {
		t.Errorf("want one duplicate rejection, got %+v", res.Rejected)
	}
}

// TestImportOpenAPIAuthParity: credentials attached at import are validated, sealed, and hidden
// exactly as `action create` does it — an imported action is an ordinary action.
func TestImportOpenAPIAuthParity(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	k.SetSecretBox(b64Box{})
	ctx := context.Background()
	owner := setupUser(t, st, "acme", 0)

	good := []*kernel.AuthInput{
		{Scheme: "header", Config: map[string]any{"name": "X-Api-Key"}, Secrets: map[string]any{"value": "s3cret"}},
		{Scheme: "delegated_bearer"},
	}
	bad := []*kernel.AuthInput{
		{Scheme: "no-such-scheme"},
		{Scheme: "delegated_bearer", Secrets: map[string]any{"token": "owner-side"}},
	}

	for i, auth := range good {
		name := "ok" + string(rune('a'+i))
		res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, name, "http://api.example.com/"+name+".json", []byte(minOpenAPISpec), auth)
		if err != nil {
			t.Fatalf("import with %s: %v", auth.Scheme, err)
		}
		if len(res.Created) != 1 {
			t.Fatalf("import with %s: %+v", auth.Scheme, res)
		}
		got, _ := k.ReadAction(ctx, res.Created[0].ID)
		if got.AuthJSON == "" {
			t.Errorf("%s: credentials were not stored", auth.Scheme)
		}
		if strings.Contains(got.AuthJSON, "s3cret") {
			t.Errorf("%s: secret stored in the clear", auth.Scheme)
		}
		// A manual create must accept the same configuration.
		if _, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Title: "Test action", Name: "manual-" + name, Kind: kernel.KindHTTP,
			Source: "https://api.example.com/x", Method: "POST", Auth: auth,
		}); err != nil {
			t.Errorf("create rejects what import accepted (%s): %v", auth.Scheme, err)
		}
	}

	for i, auth := range bad {
		name := "bad" + string(rune('a'+i))
		err := errOf(k.ImportOpenAPI(ctx, owner.ID, owner.ID, name, "http://api.example.com/"+name+".json", []byte(minOpenAPISpec), auth))
		if err == nil {
			t.Errorf("import accepted an invalid auth config (%s)", auth.Scheme)
		}
		_, cerr := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
			OwnerUserID: owner.ID, Title: "Test action", Name: "manual-" + name, Kind: kernel.KindHTTP,
			Source: "https://api.example.com/x", Method: "POST", Auth: auth,
		})
		if cerr == nil {
			t.Errorf("create accepted what import refused (%s)", auth.Scheme)
		}
	}

	// A document that moves revokes the application's standing consent, whether or not its rows
	// were live: a grant survives a disable, so it must not survive the contract moving under it.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "moving", "http://api.example.com/moving.json", []byte(minOpenAPISpec), good[1]); err != nil {
		t.Fatalf("import: %v", err)
	}
	row, _ := k.ReadActionByOwnerName(ctx, owner.ID, "moving/sayHello")
	grantor := setupUser(t, st, "acme-caller", 0)
	g := &kernel.Grant{ID: uuid.New().String(), GrantorUserID: grantor.ID, ActionID: row.ID, CreatedAt: time.Now().UTC()}
	if err := st.CreateOrReplaceGrant(ctx, g); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(minOpenAPISpec, "says hello", "greets you", 1)
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "moving", "http://api.example.com/moving.json", []byte(changed), nil); err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if _, gerr := st.ReadGrant(ctx, grantor.ID, row.ID); gerr == nil {
		t.Error("consent survived a re-import that moved the document")
	}

	// Attaching credentials to an installed application is a write, reported as such.
	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "later", "http://api.example.com/later.json", []byte(minOpenAPISpec), nil); err != nil {
		t.Fatalf("import: %v", err)
	}
	res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "later", "http://api.example.com/later.json", []byte(minOpenAPISpec), good[0])
	if err != nil {
		t.Fatalf("attach credentials: %v", err)
	}
	if len(res.Updated) != 1 || len(res.Unchanged) != 0 {
		t.Errorf("an auth-only change is a write: updated=%d unchanged=%d", len(res.Updated), len(res.Unchanged))
	}
}

// TestImportOpenAPILeavesManualHTTPUntouched: a hand-written kind=http action is invisible to
// import reconciliation — it belongs to no application.
func TestImportOpenAPILeavesManualHTTPUntouched(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "oapi-isolation-owner", 0)
	specURL := "https://spec.example.com/api.json"

	manual, err := k.CreateAction(ctx, owner.ID, kernel.CreateActionRequest{
		OwnerUserID: owner.ID, Title: "Test action", Name: "mail/manual-svc", Kind: kernel.KindHTTP,
		Source: "https://api.example.com/manual", Method: "POST",
	})
	if err != nil {
		t.Fatalf("CreateAction: %v", err)
	}

	if _, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(minOpenAPISpec), nil); err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	// Even inside the application's path, a hand-written row is not the document's to withdraw.
	res, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", specURL, []byte(appOpenAPISpec), nil)
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	for _, a := range res.Deactivated {
		if a.ID == manual.ID {
			t.Fatal("re-import must not withdraw a hand-written action")
		}
	}
	got, err := k.ReadAction(ctx, manual.ID)
	if err != nil {
		t.Fatalf("ReadAction: %v", err)
	}
	if s := sourceOf(t, got); s.Type != "http" {
		t.Errorf("manual source changed: type=%q", s.Type)
	}
}

func TestOpenAPIActivation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "oapi-activate-owner", 0)

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", "https://spec.example.com/api.json", []byte(minOpenAPISpec), nil)
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

	owner := setupUser(t, st, "oapi-private-owner", 0)

	// Craft an HTTPSource with a private execution base URL.
	src := kernel.HTTPSource{
		Type:         "openapi",
		SpecURL:      "https://spec.example.com/api.json",
		BaseURL:      "http://10.0.0.1",
		Method:       "GET",
		Path:         "/secret",
		OperationKey: "getSecret",
	}
	srcBytes, _ := json.Marshal(src)
	a := &kernel.Action{
		ID:           uuid.New().String(),
		OwnerUserID:  owner.ID,
		Name:         "oapi-private-owner/getSecret",
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

func TestImportOpenAPISubjectMismatchRejected(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	userA := setupUser(t, st, "user-a-imp", 0)
	userB := setupUser(t, st, "user-b-imp", 0)

	err := errOf(k.ImportOpenAPI(ctx, userA.ID, userB.ID, "mail", "http://spec.example.com", []byte(minOpenAPISpec), nil))
	if !errors.Is(err, kernel.ErrUnauthorized) {
		t.Errorf("expected ErrUnauthorized when subject != owner, got %v", err)
	}
}

func TestOpenAPIRejectMissingName(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()

	owner := setupUser(t, st, "oapi-no-name", 0)

	// Operation has neither operationId nor x-juice-name.
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"description":"says hello","parameters":[{"name":"q","in":"query","description":"query","schema":{"type":"string"}}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", "https://spec.example.com/api.json", []byte(spec), nil)
	if err != nil {
		t.Fatalf("ImportOpenAPI returned error: %v", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("expected 0 created, got %d", len(result.Created))
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

	owner := setupUser(t, st, "oapi-no-input", 0)

	// Operation has operationId and description but no parameters and no requestBody.
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/ping":{"get":{"operationId":"ping","summary":"ping","description":"ping the server","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "mail", "https://spec.example.com/api.json", []byte(spec), nil)
	if err != nil {
		t.Fatalf("ImportOpenAPI returned error: %v", err)
	}
	if len(result.Created) != 0 {
		t.Errorf("expected 0 created, got %d", len(result.Created))
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

	owner := setupUser(t, st, "oapi-ref-body", 0)

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
					"operationId": "createItem","summary":"createItem",
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

	result, err := k.ImportOpenAPI(ctx, owner.ID, owner.ID, "items", "https://spec.example.com/api.json", []byte(spec), nil)
	if err != nil {
		t.Fatalf("ImportOpenAPI error: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected 1 created action, got %d (rejected: %+v)", len(result.Created), result.Rejected)
	}

	src := sourceOf(t, result.Created[0])
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

// ---- the contract an import produces (D4, D21) ----

// openAPIDoc renders a document from its paths and shared components, so a test states only the
// operations it is about.
func openAPIDoc(t *testing.T, paths, components map[string]any) []byte {
	t.Helper()
	doc := map[string]any{
		"openapi": "3.0.0", "info": map[string]any{"title": "T", "version": "1"},
		"servers": []any{map[string]any{"url": "http://api.example.com"}},
		"paths":   paths,
	}
	if components != nil {
		doc["components"] = components
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// getOp is a GET operation with one described query parameter and the given response schema;
// extra fields override or add to it.
func getOp(id string, response map[string]any, extra map[string]any) map[string]any {
	op := map[string]any{
		"operationId": id, "summary": "Run " + id, "description": "does " + id,
		"parameters": []any{map[string]any{"name": "q", "in": "query", "description": "query", "schema": map[string]any{"type": "string"}}},
		"responses": map[string]any{"200": map[string]any{"description": "ok", "content": map[string]any{
			"application/json": map[string]any{"schema": response}}}},
	}
	for k, v := range extra {
		op[k] = v
	}
	return op
}

func importDoc(t *testing.T, k *kernel.Kernel, owner *kernel.Account, name string, doc []byte) *kernel.ImportResult {
	t.Helper()
	result, err := k.ImportOpenAPI(context.Background(), owner.ID, owner.ID, name, "https://spec.example.com/"+name+".json", doc, nil)
	if err != nil {
		t.Fatalf("ImportOpenAPI: %v", err)
	}
	return result
}

func rejection(result *kernel.ImportResult, key string) *kernel.ImportRejection {
	for i := range result.Rejected {
		if result.Rejected[i].Key == key {
			return &result.Rejected[i]
		}
	}
	return nil
}

func createdNamed(result *kernel.ImportResult, name string) *kernel.Action {
	for _, a := range result.Created {
		if a.Name == name {
			return a
		}
	}
	return nil
}

// The title is x-juice-title, else the summary; an operation with neither is refused naming what to
// add, since an operation key is no more readable than an address.
func TestImportOpenAPITitle(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	owner := setupUser(t, st, "oapi-title", 0)
	obj := map[string]any{"type": "object"}
	doc := openAPIDoc(t, map[string]any{
		"/a": map[string]any{"get": getOp("both", obj, map[string]any{"x-juice-title": "Juice title", "summary": "Summary title"})},
		"/b": map[string]any{"get": getOp("summaryOnly", obj, nil)},
		"/c": map[string]any{"get": getOp("neither", obj, map[string]any{"summary": nil})},
		"/d": map[string]any{"get": getOp("tooLong", obj, map[string]any{"summary": strings.Repeat("s", kernel.MaxTitleLength+1)})},
	}, nil)
	result := importDoc(t, k, owner, "titles", doc)

	if a := createdNamed(result, "titles/both"); a == nil || a.Title != "Juice title" {
		t.Errorf("x-juice-title must win over summary: %+v", a)
	}
	if a := createdNamed(result, "titles/summaryOnly"); a == nil || a.Title != "Run summaryOnly" {
		t.Errorf("summary must become the title: %+v", a)
	}
	if r := rejection(result, "neither"); r == nil || r.Location != "GET /c" || !strings.Contains(r.Reason, "no summary") {
		t.Errorf("an operation without a summary must be refused at its location: %+v", r)
	}
	if r := rejection(result, "tooLong"); r == nil || r.Location != "GET /d" || !strings.Contains(r.Reason, "longer than") {
		t.Errorf("an over-long summary must be refused: %+v", r)
	}
}

// One import reports every operation that cannot land, each at its location and with the schema
// path and rule it broke, while the rest of the document lands.
func TestImportOpenAPIReportsEveryOperation(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	owner := setupUser(t, st, "oapi-report", 0)
	obj := map[string]any{"type": "object"}
	union := []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}
	doc := openAPIDoc(t, map[string]any{
		"/ok":    map[string]any{"get": getOp("ok", obj, nil)},
		"/union": map[string]any{"get": getOp("union", map[string]any{"type": "object", "properties": map[string]any{"v": map[string]any{"oneOf": union}}}, nil)},
		"/list":  map[string]any{"get": getOp("list", map[string]any{"type": "array", "items": obj}, nil)},
		"/tree":  map[string]any{"get": getOp("tree", map[string]any{"$ref": "#/components/schemas/Node"}, nil)},
		"/param": map[string]any{"post": map[string]any{
			"operationId": "param", "summary": "Param", "description": "bad parameter",
			"parameters": []any{map[string]any{"name": "id", "in": "path", "description": "id", "schema": map[string]any{"anyOf": union}}},
			"responses":  map[string]any{"200": map[string]any{"description": "ok", "content": map[string]any{"application/json": map[string]any{"schema": obj}}}},
		}},
	}, map[string]any{"schemas": map[string]any{"Node": map[string]any{
		"type": "object", "properties": map[string]any{"child": map[string]any{"$ref": "#/components/schemas/Node"}},
	}}})
	result := importDoc(t, k, owner, "report", doc)

	if len(result.Created) != 1 || result.Created[0].Name != "report/ok" {
		t.Errorf("the valid operation must land beside the refused ones: created %d", len(result.Created))
	}
	want := map[string]struct{ location, reason string }{
		"union": {"GET /union", "output.properties.v: oneOf is not supported"},
		"list":  {"GET /list", "output: the top level must be an object, not array"},
		"tree":  {"GET /tree", "is recursive"},
		"param": {"POST /param", "input.properties.id: anyOf is not supported"},
	}
	for key, w := range want {
		r := rejection(result, key)
		if r == nil {
			t.Errorf("%s: not reported", key)
			continue
		}
		if r.Location != w.location || !strings.Contains(r.Reason, w.reason) {
			t.Errorf("%s: got %q at %q, want %q at %q", key, r.Reason, r.Location, w.reason, w.location)
		}
	}
}

// OpenAPI 3.0's nullable and 3.1's type list mean one thing and are stored as one thing; the fold is
// reported, the canonical spelling is not, and anyOf-with-null is refused like every combinator.
func TestImportOpenAPINullableSpellingsAgree(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	owner := setupUser(t, st, "oapi-null", 0)
	resp := func(field map[string]any) map[string]any {
		field["description"] = "count"
		return map[string]any{"type": "object", "properties": map[string]any{"n": field}}
	}
	doc := openAPIDoc(t, map[string]any{
		"/v30":   map[string]any{"get": getOp("v30", resp(map[string]any{"type": "integer", "nullable": true}), nil)},
		"/v31":   map[string]any{"get": getOp("v31", resp(map[string]any{"type": []any{"integer", "null"}}), nil)},
		"/anyOf": map[string]any{"get": getOp("anyOf", resp(map[string]any{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "null"}}}), nil)},
	}, nil)
	result := importDoc(t, k, owner, "nulls", doc)
	if r := rejection(result, "anyOf"); r == nil || r.Location != "GET /anyOf" || !strings.Contains(r.Reason, "output.properties.n: anyOf is not supported") {
		t.Errorf("anyOf with null must be refused at its location: %+v", r)
	}
	if len(result.Created) != 2 {
		t.Fatalf("created %d, rejected %+v", len(result.Created), result.Rejected)
	}
	want := map[string]any{"type": []any{"integer", "null"}, "description": "count"}
	for _, a := range result.Created {
		props, _ := a.OutputSchema["properties"].(map[string]any)
		got, _ := json.Marshal(props["n"])
		w, _ := json.Marshal(want)
		if string(got) != string(w) {
			t.Errorf("%s stored %s, want %s", a.Name, got, w)
		}
	}
	noted := map[string]string{}
	for _, n := range result.Notices {
		noted[n.Key] = n.Location + " " + n.Note
	}
	if !strings.Contains(noted["v30"], "GET /v30 output.properties.n: nullable folded") {
		t.Errorf("the 3.0 fold must be reported at its location: %q", noted["v30"])
	}
	if _, reported := noted["v31"]; reported {
		t.Errorf("the canonical spelling changes nothing and is not reported: %q", noted["v31"])
	}
}

// A body, a response and a schema shared under components are followed, the schema's annotations
// survive, its fields bind to the body; a body composed with allOf is refused at its location.
func TestImportOpenAPISharedBody(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	owner := setupUser(t, st, "oapi-shared", 0)
	doc := openAPIDoc(t, map[string]any{
		"/pets": map[string]any{"post": map[string]any{
			"operationId": "addPet", "summary": "Add a pet", "description": "adds a pet",
			"requestBody": map[string]any{"$ref": "#/components/requestBodies/NewPet"},
			"responses":   map[string]any{"201": map[string]any{"$ref": "#/components/responses/Pet"}},
		}},
		"/composed": map[string]any{"post": map[string]any{
			"operationId": "composed", "summary": "Composed", "description": "allOf body",
			"requestBody": map[string]any{"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/Pet"}}},
			}}},
			"responses": map[string]any{"201": map[string]any{"$ref": "#/components/responses/Pet"}},
		}},
	}, map[string]any{
		"requestBodies": map[string]any{"NewPet": map[string]any{"content": map[string]any{"application/json": map[string]any{
			"schema": map[string]any{"$ref": "#/components/schemas/Pet"},
		}}}},
		"responses": map[string]any{"Pet": map[string]any{"description": "the pet", "content": map[string]any{"application/json": map[string]any{
			"schema": map[string]any{"$ref": "#/components/schemas/Pet"},
		}}}},
		"schemas": map[string]any{"Pet": map[string]any{
			"type": "object", "required": []any{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "default": "Rex", "description": "name"},
				"born": map[string]any{"type": "string", "format": "date", "title": "Birthday", "description": "birth date", "example": "2020-01-01"},
			},
		}},
	})
	result := importDoc(t, k, owner, "pets", doc)
	a := createdNamed(result, "pets/addPet")
	if a == nil {
		t.Fatalf("not imported: %+v", result.Rejected)
	}
	props, _ := a.InputSchema["properties"].(map[string]any)
	born, _ := props["born"].(map[string]any)
	if born["format"] != "date" || born["title"] != "Birthday" {
		t.Errorf("annotations must survive import: %v", born)
	}
	if ex, _ := born["examples"].([]any); len(ex) != 1 || ex[0] != "2020-01-01" {
		t.Errorf("example must become examples: %v", born)
	}
	if name, _ := props["name"].(map[string]any); name["default"] != "Rex" {
		t.Errorf("the referenced field must be inlined with its default: %v", props["name"])
	}
	if req, _ := a.InputSchema["required"].([]any); len(req) != 1 || req[0] != "name" {
		t.Errorf("required must carry over from the shared body: %v", a.InputSchema["required"])
	}
	bound := map[string]string{}
	for _, p := range sourceOf(t, a).Params {
		bound[p.Name] = p.In
	}
	if bound["name"] != "body" || bound["born"] != "body" {
		t.Errorf("shared body fields must bind to the body: %v", bound)
	}
	if _, ok := a.OutputSchema["properties"].(map[string]any)["name"]; !ok {
		t.Errorf("a shared response must resolve to its schema: %v", a.OutputSchema)
	}
	if r := rejection(result, "composed"); r == nil || r.Location != "POST /composed" || !strings.Contains(r.Reason, "input: allOf is not supported") {
		t.Errorf("an allOf body must be refused at its location: %+v", r)
	}
}

// A shared parameter is followed like an inline one.
func TestImportOpenAPISharedParameter(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	owner := setupUser(t, st, "oapi-param", 0)
	op := getOp("list", map[string]any{"type": "object"}, map[string]any{
		"parameters": []any{map[string]any{"$ref": "#/components/parameters/Limit"}},
	})
	doc := openAPIDoc(t, map[string]any{"/items": map[string]any{"get": op}}, map[string]any{"parameters": map[string]any{
		"Limit": map[string]any{"name": "limit", "in": "query", "description": "page size", "schema": map[string]any{"type": "integer", "maximum": 100}},
	}})
	result := importDoc(t, k, owner, "items", doc)
	a := createdNamed(result, "items/list")
	if a == nil {
		t.Fatalf("not imported: %+v", result.Rejected)
	}
	limit, _ := a.InputSchema["properties"].(map[string]any)["limit"].(map[string]any)
	if limit["type"] != "integer" || limit["maximum"] != float64(100) {
		t.Errorf("shared parameter not followed: %v", a.InputSchema)
	}
}

// The title is document-owned (D21): a changed summary updates the row on re-import, and a title
// edited by hand is restored from the document, as every document-owned field is.
func TestImportOpenAPITitleIsDocumentOwned(t *testing.T) {
	st := newTestStore(t)
	k := newTestKernel(st)
	ctx := context.Background()
	owner := setupUser(t, st, "oapi-retitle", 0)
	doc := func(summary string) []byte {
		return openAPIDoc(t, map[string]any{"/a": map[string]any{"get": getOp("op", map[string]any{"type": "object"}, map[string]any{"summary": summary})}}, nil)
	}
	importDoc(t, k, owner, "app", doc("First title"))
	if result := importDoc(t, k, owner, "app", doc("Second title")); len(result.Updated) != 1 {
		t.Fatalf("a changed summary must update the row: updated %d unchanged %d", len(result.Updated), len(result.Unchanged))
	}
	a := mustResolve(t, k, ctx, owner.ID, "app/op")
	if a.Title != "Second title" {
		t.Errorf("title %q, want the document's new summary", a.Title)
	}
	edited := "Edited by hand"
	if _, err := k.UpdateAction(ctx, owner.ID, kernel.UpdateActionRequest{ID: a.ID, Title: &edited}); err != nil {
		t.Fatal(err)
	}
	if result := importDoc(t, k, owner, "app", doc("Second title")); len(result.Updated) != 1 {
		t.Fatalf("a hand-edited title must count as a moved document field: updated %d", len(result.Updated))
	}
	if a := mustResolve(t, k, ctx, owner.ID, "app/op"); a.Title != "Second title" {
		t.Errorf("title %q, want the document's restored", a.Title)
	}
}
