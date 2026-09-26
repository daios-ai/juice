// SPDX-License-Identifier: AGPL-3.0-only

package kernel

// This file contains tests that must remain in package kernel because they
// exercise unexported functions (validateHTTPSource, signRating,
// remoteManifestHash, documentMoved, parseOpenAPISpec, buildReceipt).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

// TestCanCallVisibilityMatrix exercises the caller-scoped, three-level visibility predicate (§4):
// public is callable by anyone; local by any local (non-peer) caller but not a peer; private only
// by the owner; and inactive/suspended-owner actions are never callable regardless of visibility.
func TestCanCallVisibilityMatrix(t *testing.T) {
	owner := &Account{ID: "owner"}
	other := &Account{ID: "other"}
	peer := &Account{ID: "peer", KernelPublicKey: "cGVlcg"}

	mk := func(vis ActionVisibility) *Action {
		return &Action{OwnerUserID: "owner", Active: true, Visibility: vis}
	}
	cases := []struct {
		name    string
		action  *Action
		caller  *Account
		canCall bool
	}{
		{"public/owner", mk(VisibilityPublic), owner, true},
		{"public/other", mk(VisibilityPublic), other, true},
		{"public/peer", mk(VisibilityPublic), peer, true},
		{"local/owner", mk(VisibilityLocal), owner, true},
		{"local/other", mk(VisibilityLocal), other, true},
		{"local/peer", mk(VisibilityLocal), peer, false},
		{"local/nil", mk(VisibilityLocal), nil, false},
		{"private/owner", mk(VisibilityPrivate), owner, true},
		{"private/other", mk(VisibilityPrivate), other, false},
		{"private/peer", mk(VisibilityPrivate), peer, false},
	}
	for _, c := range cases {
		if got := canCall(c.caller, c.action); got != c.canCall {
			t.Errorf("%s: canCall = %v, want %v", c.name, got, c.canCall)
		}
	}

	// Inactive and suspended-owner actions are never callable, even by the owner.
	inactive := &Action{OwnerUserID: "owner", Active: false, Visibility: VisibilityPublic}
	if canCall(owner, inactive) {
		t.Error("inactive action must not be callable")
	}
	suspended := &Action{OwnerUserID: "owner", Active: true, OwnerSuspended: true, Visibility: VisibilityPublic}
	if canCall(owner, suspended) {
		t.Error("suspended-owner action must not be callable")
	}
}

// recordingStore is a nil store that remembers what evidence ingress tried to write.
type recordingStore struct {
	Store
	rows []*EvidenceRow
}

func (r *recordingStore) UpsertEvidence(_ context.Context, e *EvidenceRow) error {
	r.rows = append(r.rows, e)
	return nil
}

// TestIngestEvidenceRejectsRatingOutsideContract: a gossiped rating is signed, hash-linked — and
// still a rating: a value outside {0,1} or a note over the bound is refused before it is stored.
// A signature proves who said it, not that it is a rating.
func TestIngestEvidenceRejectsRatingOutsideContract(t *testing.T) {
	rs := &recordingStore{}
	cfg := DefaultConfig()
	cfg.Network, cfg.TokenSecret, cfg.IssuerUserID = playNet, "s", "i"
	k := New(Dependencies{Config: cfg, Store: rs})
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	issuer := base64.RawURLEncoding.EncodeToString(pub)
	now := time.Now().UTC()
	bundle := func(value float64, note *string) EvidenceBundle {
		er := &EvidenceReceipt{ReceiptHash: "H1", SubjectKernelPublicKey: "subj", SubjectActionID: "act",
			Status: TxSuccess, StartedAt: now, CreatedAt: now}
		er.Signature, _ = playNet.sign(priv, sigDomainEvidenceReceipt, *er)
		rt := &RatingEvidence{Rating: value, Note: note, RatedReceiptHash: "H1", CreatedAt: now}
		rt.Signature, _ = playNet.sign(priv, sigDomainRating, *rt)
		return EvidenceBundle{EvidenceReceipt: er, Rating: rt}
	}
	long := strings.Repeat("n", maxRatingNoteBytes+1)
	for name, b := range map[string]EvidenceBundle{"value 7": bundle(7, nil), "oversized note": bundle(1, &long)} {
		if err := k.ingestEvidenceBundle(context.Background(), issuer, b, now); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
	if len(rs.rows) != 0 {
		t.Fatalf("a refused rating was still stored: %d row(s)", len(rs.rows))
	}
	ok := "fine"
	if err := k.ingestEvidenceBundle(context.Background(), issuer, bundle(1, &ok), now); err != nil {
		t.Fatalf("a rating inside the contract: %v", err)
	}
	if len(rs.rows) != 1 {
		t.Fatalf("the valid rating was not stored")
	}
}

// TestVerifyRemoteReceiptSignatureFailsClosedOnEmptyKey: an empty peer public key must make
// signature verification fail, never be silently skipped — a missing key cannot authenticate
// a receipt, so it must never let an unverified receipt pass as valid (§13).
func TestVerifyRemoteReceiptSignatureFailsClosedOnEmptyKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &Receipt{ID: "r1", ActionID: "a1", Status: TxSuccess, Gross: 5, Net: 5}
	sig, err := signReceipt(playNet, priv, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Signature = sig

	if err := playNet.verifyReceiptSignature(r, ""); err == nil {
		t.Fatal("empty peer key: got nil, want error (must fail closed)")
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if err := playNet.verifyReceiptSignature(r, pubB64); err != nil {
		t.Fatalf("valid signature with correct key: %v", err)
	}
}

// TestSettlementContextDetachesCancellation verifies that settlementContext strips
// execution-scoped cancellation/deadline (so a money commit never aborts when the
// call is cancelled or times out) while preserving context values.
func TestSettlementContextDetachesCancellation(t *testing.T) {
	type ctxKey struct{}
	parent := context.WithValue(context.Background(), ctxKey{}, "kept")
	parent, cancel := context.WithCancel(parent)

	sctx, scancel := settlementContext(parent)
	defer scancel()

	// Cancelling the parent must NOT cancel the settlement context.
	cancel()
	select {
	case <-sctx.Done():
		t.Fatal("settlementContext was cancelled when parent was cancelled")
	default:
	}

	// Values survive the detachment.
	if got, _ := sctx.Value(ctxKey{}).(string); got != "kept" {
		t.Errorf("settlementContext lost context value: got %q, want %q", got, "kept")
	}

	// It still carries its own backstop deadline.
	if _, ok := sctx.Deadline(); !ok {
		t.Error("settlementContext should have its own backstop deadline")
	}
}

// newMinimalKernel creates a Kernel with a nil store for tests that only
// exercise unexported methods that do not touch the store.
func newMinimalKernel() *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = "test-issuer-id"
	return New(Dependencies{Config: cfg})
}

func TestValidateHTTPSourceSSRF(t *testing.T) {
	k := newMinimalKernel()
	// Inject a resolver that never hits real DNS for known hosts.
	k.SetLookupHost(func(_ context.Context, _ string) ([]string, error) {
		return []string{"203.0.113.1"}, nil // TEST-NET, always public
	})
	rejected := []string{
		"http://::1/secret", // malformed bracketless IPv6 (must be [::1]); rejected as a probe, not by loopback policy
		"http://10.0.0.1/internal",
		"http://192.168.1.1/router",
		"http://172.16.0.1/internal",
		"http://169.254.169.254/latest/meta-data/",
		"http://0.0.0.0/admin",       // unspecified → localhost on Linux
		"http://[::]/admin",          // IPv6 unspecified
		"http://100.64.0.1/internal", // CGNAT shared space (RFC 6598)
		"ftp://example.com/file",
		"file:///etc/passwd",
		"://broken",
	}
	ctx := context.Background()
	for _, u := range rejected {
		if err := k.validateHTTPSource(ctx, u, false); err == nil {
			t.Errorf("validateHTTPSource(%q): expected error, got nil", u)
		}
	}

	accepted := []string{
		"https://example.com/api",
		"http://example.com/webhook",
		"https://api.stripe.com/v1/charges",
		// Loopback is permitted by default: a service on this same host (§7/§9).
		"http://127.0.0.1/secret",
		"http://[::1]/secret",
		"http://localhost/api", // resolves public here; localhost is no longer hard-rejected
		// Unresolvable hostnames are allowed through; the runtime dialer re-validates at call time.
		"https://this-does-not-exist.invalid/api",
		"https://api.example.com/v2",
	}
	for _, u := range accepted {
		if err := k.validateHTTPSource(ctx, u, false); err != nil {
			t.Errorf("validateHTTPSource(%q): unexpected error: %v", u, err)
		}
	}
}

func TestValidateHTTPSourceDNSResolvesToPrivate(t *testing.T) {
	k := newMinimalKernel()
	// Inject a fake resolver so the test does not need real DNS.
	k.SetLookupHost(func(_ context.Context, host string) ([]string, error) {
		m := map[string][]string{
			"internal.corp":        {"10.0.0.1"},
			"loopback.example":     {"127.0.0.1"},
			"linklocal.example":    {"169.254.1.1"},
			"public.example":       {"93.184.216.34"},
			"unresolvable.invalid": {},
		}
		if addrs, ok := m[host]; ok {
			return addrs, nil
		}
		return nil, fmt.Errorf("no such host")
	})

	ctx := context.Background()
	for _, u := range []string{
		"https://internal.corp/api",
		"https://linklocal.example/api",
	} {
		if err := k.validateHTTPSource(ctx, u, false); err == nil {
			t.Errorf("validateHTTPSource(%q): expected rejection for private-resolving hostname, got nil", u)
		}
	}
	// Public-resolving and loopback-resolving hostnames must be accepted (loopback is permitted).
	for _, u := range []string{"https://public.example/api", "https://loopback.example/api"} {
		if err := k.validateHTTPSource(ctx, u, false); err != nil {
			t.Errorf("validateHTTPSource(%q): unexpected error: %v", u, err)
		}
	}
	// DNS failure (empty result, no error) must be allowed through.
	if err := k.validateHTTPSource(ctx, "https://unresolvable.invalid/api", false); err != nil {
		t.Errorf("validateHTTPSource(unresolvable.invalid): DNS failure should be allowed: %v", err)
	}
	// allowLocal=true bypasses DNS resolution entirely.
	if err := k.validateHTTPSource(ctx, "https://internal.corp/api", true); err != nil {
		t.Errorf("validateHTTPSource(internal.corp, allowLocal=true): unexpected error: %v", err)
	}
}

func TestReceiptSigningRequiresConfiguredKey(t *testing.T) {
	k := newMinimalKernel()
	k.SetSigningKey(nil, "issuer-id")

	_, err := k.buildReceipt(&Transaction{
		ID:        "tx-id",
		TraceID:   "trace-id",
		ActionID:  "action-id",
		ArgsJSON:  json.RawMessage(`{}`),
		ReplyJSON: json.RawMessage(`{}`),
		Status:    TxSuccess,
		EndedAt:   time.Now().UTC(),
	}, 0, 0, 0, "", soldAs{})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestRatingSigningRequiresConfiguredKey(t *testing.T) {
	_, err := signRating(playNet, nil, &Rating{
		ID:          "rating-id",
		RatedTxID:   "tx-id",
		RaterUserID: "user-id",
		Rating:      1,
		CreatedAt:   time.Now().UTC(),
	})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestRemoteManifestHashIncludesKindAndArtifact(t *testing.T) {
	base := ActionManifest{
		ActionID:     "act-1",
		OwnerHandle:  "peer",
		Name:         "svc",
		Description:  "test",
		Kind:         KindHTTP,
		ArtifactHash: "abc123",
		Price:        10,
		InputSchema:  map[string]any{"type": "object"},
		OutputSchema: map[string]any{"type": "object"},
	}

	// Different kind must produce a different hash.
	wasmVariant := base
	wasmVariant.Kind = KindWasm
	if remoteManifestHash(base) == remoteManifestHash(wasmVariant) {
		t.Error("kind change should produce different hash")
	}

	// Different artifact_hash must produce a different hash.
	newArtifact := base
	newArtifact.ArtifactHash = "def456"
	if remoteManifestHash(base) == remoteManifestHash(newArtifact) {
		t.Error("artifact_hash change should produce different hash")
	}

	// Different action_id (execution identity) must produce a different hash.
	differentID := base
	differentID.ActionID = "act-2"
	if remoteManifestHash(base) == remoteManifestHash(differentID) {
		t.Error("action_id change should produce different hash")
	}

	// Stats changes must NOT affect the hash (stats are not contract fields per §12.2).
	withStats := base
	if remoteManifestHash(base) != remoteManifestHash(withStats) {
		t.Error("stats change must not affect the manifest hash")
	}

	// Identical manifests must produce the same hash.
	if remoteManifestHash(base) != remoteManifestHash(base) {
		t.Error("identical manifests should produce the same hash")
	}
}

func TestDocumentMovedDetectsBindingChange(t *testing.T) {
	schema := map[string]any{"type": "object"}
	src := HTTPSource{Type: "openapi", SpecURL: "http://s/spec.json", BaseURL: "http://api.example.com",
		Method: "POST", Path: "/do", OperationKey: "do", Params: []HTTPParam{{Name: "data", In: "body"}}}
	srcJSON, _ := json.Marshal(src)
	existing := &Action{Name: "app/do", Description: "do thing", Price: 0,
		InputSchema: schema, OutputSchema: schema, Source: string(srcJSON)}

	same := rawOp{key: "do", description: "do thing", inputSchema: schema, outputSchema: schema, source: src}
	if documentMoved(existing, same) {
		t.Error("identical document must compare equal")
	}

	// The same field bound to the query instead of the body is a different upstream request.
	moved := same
	moved.source.Params = []HTTPParam{{Name: "data", In: "query"}}
	if !documentMoved(existing, moved) {
		t.Error("a changed binding must be detected")
	}

	// A different upstream host is a different action, even at the same path.
	rehosted := same
	rehosted.source.BaseURL = "http://other.example.com"
	if !documentMoved(existing, rehosted) {
		t.Error("a changed base URL must be detected")
	}

	// Params are stored in name order, so a re-parse of one document compares equal to itself.
	reordered := same
	reordered.source.Params = []HTTPParam{{Name: "data", In: "body"}}
	if documentMoved(existing, reordered) {
		t.Error("an identical binding list must compare equal")
	}
}

func TestOpenAPIParamsAreNameOrdered(t *testing.T) {
	spec := []byte(`{"openapi":"3.0.0","servers":[{"url":"http://api.example.com"}],"paths":{"/do":{"post":{
		"operationId":"do","description":"do thing",
		"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{
			"zulu":{"type":"string"},"alpha":{"type":"string"},"mike":{"type":"string"}}}}}},
		"responses":{"200":{"content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`)
	for i := 0; i < 8; i++ {
		ops, _, _, err := parseOpenAPISpec(spec, "http://s/spec.json")
		if err != nil || len(ops) != 1 {
			t.Fatalf("parse: %v (%d ops)", err, len(ops))
		}
		var got []string
		for _, p := range ops[0].source.Params {
			got = append(got, p.Name)
		}
		if len(got) != 3 || got[0] != "alpha" || got[1] != "mike" || got[2] != "zulu" {
			t.Fatalf("params not name-ordered: %v", got)
		}
	}
}

func TestParseOpenAPISpecAcceptsYAML(t *testing.T) {
	spec := `
openapi: "3.0.0"
info:
  title: T
  version: "1"
servers:
  - url: http://api.example.com
paths:
  /hello:
    get:
      operationId: sayHello
      description: says hello
      parameters:
        - name: name
          in: query
          description: who to greet
          schema:
            type: string
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: object
`
	ops, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.yaml")
	if err != nil {
		t.Fatalf("parseOpenAPISpec (YAML): %v", err)
	}
	if len(rejected) != 0 {
		t.Errorf("unexpected rejections: %+v", rejected)
	}
	if len(ops) != 1 || ops[0].key != "sayHello" {
		t.Errorf("expected 1 op with key sayHello, got %+v", ops)
	}
}

func TestParseOpenAPISpecResolvesRefInResponseSchema(t *testing.T) {
	spec := `{
		"openapi":"3.0.0","info":{"title":"T","version":"1"},
		"servers":[{"url":"http://api.example.com"}],
		"components":{"schemas":{"Reply":{"type":"object","properties":{"id":{"type":"string","description":"the id"}}}}},
		"paths":{"/op":{"post":{
			"operationId":"doOp",
			"description":"does op",
			"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"q":{"type":"string","description":"query"}}}}}},
			"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"$ref":"#/components/schemas/Reply"}}}}}
		}}}
	}`
	ops, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec ($ref): %v", err)
	}
	if len(rejected) != 0 {
		t.Errorf("unexpected rejections: %+v", rejected)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	props, _ := ops[0].outputSchema["properties"].(map[string]any)
	if _, ok := props["id"]; !ok {
		t.Errorf("$ref not resolved in output schema: got %+v", ops[0].outputSchema)
	}
}

func TestParseOpenAPISpecResolvesRefInRequestBodySchema(t *testing.T) {
	spec := `{
		"openapi":"3.0.0","info":{"title":"T","version":"1"},
		"servers":[{"url":"http://api.example.com"}],
		"components":{"schemas":{"Body":{"type":"object","properties":{"name":{"type":"string","description":"the name"}},"required":["name"]}}},
		"paths":{"/op":{"post":{
			"operationId":"doOp",
			"description":"does op",
			"requestBody":{"content":{"application/json":{"schema":{"$ref":"#/components/schemas/Body"}}}},
			"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}
		}}}
	}`
	ops, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec ($ref body): %v", err)
	}
	if len(rejected) != 0 {
		t.Errorf("unexpected rejections: %+v", rejected)
	}
	if len(ops) != 1 {
		t.Fatalf("expected 1 op, got %d", len(ops))
	}
	props, _ := ops[0].inputSchema["properties"].(map[string]any)
	if _, ok := props["name"]; !ok {
		t.Errorf("$ref not resolved in input schema body: got %+v", ops[0].inputSchema)
	}
}

func TestParseOpenAPISpecAllowsSecurityRequirement(t *testing.T) {
	// Operations with security requirements are imported inactive (not rejected).
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"post":{"operationId":"sayHello","description":"says hello","security":[{"apiKey":[]}],"requestBody":{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string"}}}}}},"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	ops, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec: %v", err)
	}
	for _, r := range rejected {
		if r.Reason == "operation has security requirements" {
			t.Errorf("security operations should not be rejected; got rejection: %+v", r)
		}
	}
	if len(ops) == 0 {
		t.Error("expected operation to be parsed, got none")
	}
}

func TestParseOpenAPISpecRejectsMultipartOnlyBody(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/upload":{"post":{"operationId":"upload","description":"upload file","requestBody":{"content":{"multipart/form-data":{"schema":{"type":"object"}}}},"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	_, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "requestBody has no application/json content" {
		t.Errorf("expected multipart rejection, got %+v", rejected)
	}
}

func TestParseOpenAPISpecRejectsAmbiguous2xxSchemas(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/create":{"post":{"operationId":"create","description":"create item","responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"string"}}}},"201":{"description":"created","content":{"application/json":{"schema":{"type":"integer"}}}}}}}}}`
	_, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "ambiguous 2xx response schemas" {
		t.Errorf("expected ambiguous schema rejection, got %+v", rejected)
	}
}

func TestConnectionKeyDerivation(t *testing.T) {
	bearer := &Action{Kind: KindHTTP, Source: `{"base_url":"https://api.github.com","path":"/x"}`}
	pk, err := connectionKey(bearer, &AuthInput{Scheme: AuthSchemeDelegatedBearer})
	if err != nil || pk != "bearer:api.github.com" {
		t.Errorf("bearer key = %q, %v; want bearer:api.github.com", pk, err)
	}
	// oauth key binds the token issuer AND the resource-server registrable domain (eTLD+1 of the
	// source host) — the §8 confused-deputy defense.
	oauth := &Action{Kind: KindHTTP, Source: `{"base_url":"https://api.x.com"}`}
	auth := &AuthInput{Scheme: AuthSchemeOAuthDelegated, Config: map[string]any{"token_url": "https://x.com/token", "client_id": "cid"}}
	pk, err = connectionKey(oauth, auth)
	if err != nil || pk != "oauth:https://x.com/token|cid|x.com" {
		t.Errorf("oauth key = %q, %v; want oauth:https://x.com/token|cid|x.com", pk, err)
	}
	// Same token issuer + client_id but a DIFFERENT source domain must NOT collide — an attacker
	// cannot reuse a legit provider's token_url|client_id to ride a victim's connection.
	evil := &Action{Kind: KindHTTP, Source: `{"base_url":"https://evil.attacker.com"}`}
	if evilPK, _ := connectionKey(evil, auth); evilPK == pk {
		t.Errorf("different source domain must yield a different provider_key: both %q", evilPK)
	} else if evilPK != "oauth:https://x.com/token|cid|attacker.com" {
		t.Errorf("evil oauth key = %q, want …|attacker.com", evilPK)
	}
	// The key derives from facts, never the name: two actions with the same source+scheme but
	// different names share a key.
	other := &Action{Kind: KindHTTP, Name: "different/name", Source: bearer.Source}
	if pk2, _ := connectionKey(other, &AuthInput{Scheme: AuthSchemeDelegatedBearer}); pk2 != "bearer:api.github.com" {
		t.Errorf("name must not affect provider_key: got %q", pk2)
	}
	// Non-delegated scheme and non-http kind are rejected.
	if _, err := connectionKey(bearer, &AuthInput{Scheme: AuthSchemeBearer}); err == nil {
		t.Error("non-delegated scheme should not derive a provider_key")
	}
	if _, err := connectionKey(&Action{Kind: KindWasm}, &AuthInput{Scheme: AuthSchemeDelegatedBearer}); err == nil {
		t.Error("non-http action should not derive a provider_key")
	}
}

func TestSelectorSegmentMatch(t *testing.T) {
	// Segment matching: brief matches brief and brief/x, never briefing.
	if !selectorPathMatches("brief", "brief") || !selectorPathMatches("brief", "brief/eu") {
		t.Error("segment match should accept exact and sub-path")
	}
	if selectorPathMatches("brief", "briefing") {
		t.Error("segment match must reject a prefix that is not a full segment (briefing)")
	}
	if !selectorPathMatches("", "anything/at/all") {
		t.Error("empty path (whole-owner) should match every action")
	}
}

func TestUnionScopesCoverage(t *testing.T) {
	// Empty existing: nothing is covered unless the request is also empty.
	j, covered := unionScopes("", []string{"read", "write"})
	if covered {
		t.Error("request should not be covered by an empty connection")
	}
	if j != `["read","write"]` {
		t.Errorf("union = %q, want sorted array", j)
	}
	// Requesting a subset of the stored set is covered and widens nothing.
	j2, covered2 := unionScopes(`["read","write"]`, []string{"read"})
	if !covered2 || j2 != `["read","write"]` {
		t.Errorf("subset should be covered without widening: %q %v", j2, covered2)
	}
	// A new scope is not covered and widens the union.
	j3, covered3 := unionScopes(`["read"]`, []string{"admin"})
	if covered3 || j3 != `["admin","read"]` {
		t.Errorf("new scope should widen and not be covered: %q %v", j3, covered3)
	}
	// Empty request against empty existing is trivially covered.
	if _, c := unionScopes("", nil); !c {
		t.Error("empty request should be covered (bearer case)")
	}
}

func TestParseOpenAPISpecRejectsInvalidPrice(t *testing.T) {
	opTpl := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/op":{"get":{"operationId":"getOp","description":"an op","x-juice-price":%s,"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	for _, tc := range []struct{ price, reason string }{
		{"-5", "price must be a non-negative integer"},
		{"1.5", "price must be a non-negative integer"},
	} {
		spec := fmt.Sprintf(opTpl, tc.price)
		_, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
		if err != nil {
			t.Fatalf("price=%s: parseOpenAPISpec: %v", tc.price, err)
		}
		if len(rejected) != 1 || rejected[0].Reason != tc.reason {
			t.Errorf("price=%s: expected rejection %q, got %+v", tc.price, tc.reason, rejected)
		}
	}
}

// TestDrawIsFairAndAgreed exercises the settlement draw (P10): it is a pure function of the three
// values both kernels hold, so the two sides always agree; it pays nothing on a zero obligation and
// the obligation itself when no lottery applies; and over many nonces it pays the face value with
// probability d/L, which is what makes the expected payment the obligation.
func TestDrawIsFairAndAgreed(t *testing.T) {
	secret, nonce := []byte("secret"), []byte("nonce")
	if Draw("t1", secret, nonce, 40, 100) != Draw("t1", secret, nonce, 40, 100) {
		t.Fatal("the draw is not deterministic, so the two kernels could disagree")
	}
	if got := Draw("t1", secret, nonce, 0, 100); got != 0 {
		t.Errorf("a zero obligation drew %d, want 0", got)
	}
	if got := Draw("t1", secret, nonce, 40, 0); got != 40 {
		t.Errorf("with no lottery the obligation is paid exactly, got %d", got)
	}
	if got := Draw("t1", secret, nonce, 100, 100); got != 100 {
		t.Errorf("an obligation at the face value is paid exactly, got %d", got)
	}
	// Empirical E[payment] ≈ d over many nonces: the mechanism's whole point.
	const L, d, N = int64(100), int64(30), 20000
	pays := 0
	for i := 0; i < N; i++ {
		if Draw("t1", secret, []byte(fmt.Sprint(i)), d, L) == L {
			pays++
		}
	}
	if frac := float64(pays) / N; frac < 0.27 || frac > 0.33 {
		t.Errorf("empirical pay fraction %.3f, want ≈0.30 (d/L)", frac)
	}
}

// TestRevealDomainIsDisjoint: a reveal signature must not verify under any other domain (§12), so a
// captured reveal cannot be replayed as some other kind of authorization.
func TestRevealDomainIsDisjoint(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	p := RevealPayload{Counterparty: "c", Recipient: "r", Secret: "s", TicketID: "t1", Timestamp: "ts"}
	sig, err := playNet.sign(priv, sigDomainReveal, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := playNet.verify(pub, sigDomainReveal, p, sig); err != nil {
		t.Fatalf("a reveal should verify against itself: %v", err)
	}
	if err := playNet.verify(pub, sigDomainFedCall, p, sig); err == nil {
		t.Error("a reveal signature must not verify under the fed_call domain")
	}
}

// TestRemoteReceiptInvalidValue: the value channel is local to a kernel (§13), so no dispatched call
// carries value and a remote receipt claiming any is quarantined rather than settled — the reserve it
// would move does not exist here. The execution channel settles normally alongside.
func TestRemoteReceiptInvalidValue(t *testing.T) {
	replyHash, _ := jcsHashStr("null")
	const rbps, mp = int64(500), int64(100)
	k := &Kernel{econ: Economy{RemoteBPS: rbps}}
	if got := k.remoteReceiptInvalid(Receipt{Status: TxSuccess, Charge: mp, Premium: 5, Nonce: "n", ReplyHash: replyHash}, mp, rbps, []byte("null"), nil); got != "" {
		t.Errorf("valid value-free receipt rejected: %s", got)
	}
	if k.remoteReceiptInvalid(Receipt{Status: TxSuccess, Charge: mp, Premium: 5, Nonce: "n", Value: 100, ReplyHash: replyHash}, mp, rbps, []byte("null"), nil) == "" {
		t.Error("a success delivering value must be quarantined")
	}
	if k.remoteReceiptInvalid(Receipt{Status: TxFailure, Charge: 0, Value: 100}, mp, rbps, nil, nil) == "" {
		t.Error("a failure delivering value must be quarantined")
	}
	if k.remoteReceiptInvalid(Receipt{Status: TxFailure, Charge: 0}, mp, rbps, nil, nil) != "" {
		t.Error("a value-free failure must settle")
	}
	// An obligation nobody can draw for is not settleable: without the seller's nonce the buyer
	// would be choosing the outcome alone (P10).
	if k.remoteReceiptInvalid(Receipt{Status: TxSuccess, Charge: mp, Premium: 5, ReplyHash: replyHash}, mp, rbps, []byte("null"), nil) == "" {
		t.Error("a charged receipt with no nonce must be quarantined")
	}
}

// TestReceiptHashJoinDefinition pins the v0.13 rule that both sides of the remote-receipt evidence
// join hash the SAME canonical bytes (§13): receiptHash over a struct equals receiptHashFromJSON over
// that struct's JSON, so an origin's RemoteReceiptHash equals the serving kernel's ReceiptHash. It is
// deliberately NOT the raw-wire-bytes hash (sha256Hex of the marshaled JSON), which Go marshal order
// makes differ from the canonical hash.
func TestReceiptHashJoinDefinition(t *testing.T) {
	r := &Receipt{
		ID: "rid", IssuerUserID: "sys", ActionID: "act", Status: TxSuccess,
		ArgsHash: "ah", ReplyHash: "rh", Gross: 10, Net: 8, Fee: 2, Charge: 10,
		StartedAt: time.Unix(1000, 0).UTC(), CreatedAt: time.Unix(1001, 0).UTC(), Signature: "sig",
	}
	h1, err := ReceiptHash(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r)
	h2, err := receiptHashFromJSON(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("receiptHash and receiptHashFromJSON disagree: %s vs %s", h1, h2)
	}
	// Empty JSON yields an empty hash (no remote receipt).
	if h, _ := receiptHashFromJSON(""); h != "" {
		t.Errorf("empty remote receipt must hash to empty, got %q", h)
	}
}

// TestSignatureBindsDomainAndNetwork: one verification rule serves the wire and storage alike, so a
// signature made before this network's fingerprint existed — or on another network — is reported invalid
// rather than repaired (U36, G7, D23). A signature made under one domain never verifies under another.
func TestSignatureBindsDomainAndNetwork(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	payload := map[string]string{"k": "v"}
	net := playNet

	// A signature with no network prefix at all — what a kernel produced before worlds existed.
	canon, _ := CanonicalJSON(payload)
	legacySig := base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, canon))
	if err := net.verify(pub, sigDomainReceipt, payload, legacySig); err == nil {
		t.Error("a signature made before the network fingerprint existed must be reported invalid")
	}

	sig, err := net.sign(priv, sigDomainReceipt, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := net.verify(pub, sigDomainReceipt, payload, sig); err != nil {
		t.Errorf("a signature must verify on its own network: %v", err)
	}
	if err := net.verify(pub, sigDomainRating, payload, sig); err == nil {
		t.Error("a receipt-domain signature must not verify under the rating domain")
	}
	other := Network{Fingerprint: "0000000000000000000000000000000000000000000000000000000000000000"}
	if err := other.verify(pub, sigDomainReceipt, payload, sig); err == nil {
		t.Error("a signature from one network must not verify on another")
	}
}

// TestProjectRatingPrivacy: the gossip rating projection (§13) drops rater and transaction
// identity, is signed under sigDomainRating by the gossiping kernel, and a full-Rating signature
// does not verify over it — the projection is a distinct signed payload, not a field deletion.
func TestProjectRatingPrivacy(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)
	k := &Kernel{cfg: Config{SigningKey: priv, Network: playNet}}

	note := "frequently timed out"
	r := &Rating{
		ID: "rating-uuid", RatedTxID: "tx-uuid", RaterUserID: "alice-uuid",
		Rating: 1, Note: &note, RatedReceiptHash: "RECEIPT-HASH",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	// The projection is built where the rating is read — four public fields, no identity — and
	// signed here. The type is what guarantees the omission, so it is what this checks.
	proj := &RatingEvidence{Rating: r.Rating, Note: r.Note, RatedReceiptHash: r.RatedReceiptHash, CreatedAt: r.CreatedAt}
	if err := k.signRating(proj); err != nil {
		t.Fatalf("signRating: %v", err)
	}

	// No rater or transaction identity crosses the wire.
	js, _ := json.Marshal(proj)
	for _, leaked := range []string{"rater_user_id", "rated_tx_id", "rated_receipt_id", "alice-uuid", "tx-uuid", "rating-uuid"} {
		if strings.Contains(string(js), leaked) {
			t.Errorf("projection leaks %q: %s", leaked, js)
		}
	}
	// The public signal is present.
	if proj.Rating != 1 || proj.Note == nil || *proj.Note != note || proj.RatedReceiptHash != "RECEIPT-HASH" {
		t.Errorf("projection dropped a public field: %+v", proj)
	}

	// The projection signature verifies under its own domain.
	unsigned := *proj
	unsigned.Signature = ""
	if err := playNet.verify(pub, sigDomainRating, unsigned, proj.Signature); err != nil {
		t.Errorf("projection signature must verify: %v", err)
	}
	// Tampering the value breaks it.
	tampered := unsigned
	tampered.Rating = 0
	if err := playNet.verify(pub, sigDomainRating, tampered, proj.Signature); err == nil {
		t.Error("a tampered projection value must fail verification")
	}
	// A full-Rating signature (over the identity-bearing record) does not verify over the projection.
	rc := *r
	rc.Signature = ""
	fullSig, _ := playNet.sign(priv, sigDomainRating, rc)
	if err := playNet.verify(pub, sigDomainRating, unsigned, fullSig); err == nil {
		t.Error("a full-Rating signature must not verify over the projection")
	}
}

// TestMarkedUpPrice: the checked markup helper is exact against the naive product across the
// residue classes of 10000, and reports overflow instead of wrapping (§13 pricing). Prices are
// peer-supplied and every non-negative int64 price is legal (§3), so the only rejections are
// out-of-range inputs and an unrepresentable result.
func TestMarkedUpPrice(t *testing.T) {
	// Exactness: for values small enough that base*bps cannot overflow, the helper must equal
	// base + ceil(base*bps/10000) computed naively. Bases straddle every residue class boundary.
	for _, base := range []int64{0, 1, 9999, 10000, 10001, 19999, 123456, 999999999} {
		for _, bps := range []int64{0, 1, 500, 2000, 9999, 10000} {
			got, err := Economy{}.ServingPrice(base, bps)
			if err != nil {
				t.Fatalf("markedUpPrice(%d,%d): unexpected error %v", base, bps, err)
			}
			want := base + ceilDiv(base*bps, 10000)
			if got != want {
				t.Errorf("markedUpPrice(%d,%d) = %d, want %d", base, bps, got, want)
			}
		}
	}

	// A price that no naive implementation could handle: base*bps overflows int64, but the
	// quotient/remainder split keeps the result exact and representable.
	const big = int64(1) << 55
	got, err := Economy{}.ServingPrice(big, 10000)
	if err != nil {
		t.Fatalf("Economy{}.ServingPrice(2^55, 10000): unexpected error %v", err)
	}
	if got != 2*big {
		t.Errorf("Economy{}.ServingPrice(2^55, 10000) = %d, want %d", got, 2*big)
	}

	// Rejections: out-of-range inputs, and a result that cannot be represented.
	for _, c := range []struct{ base, bps int64 }{
		{-1, 500}, {100, -1}, {100, 10001}, {math.MaxInt64, 1}, {math.MaxInt64 - 1, 10000},
	} {
		if _, err := (Economy{}).ServingPrice(c.base, c.bps); err == nil {
			t.Errorf("a markup of %d at %d bps must be rejected", c.base, c.bps)
		}
	}
}

// The derived-price boundary. pricedStore is unexported on purpose: no caller outside the kernel
// can see it, which is what makes the derivation impossible to forget.
func TestPricedStoreDerivesLocalTotal(t *testing.T) {
	base := int64(100)
	rbps := int64(500)
	s := &pricedStore{econ: Economy{ImportBPS: 500}}

	// mp=100 at remote_bps=500 → sr=105; import_bps=500 → 105 + ceil(105*500/10000) = 111.
	a := &Action{Kind: KindRemoteProxy, Price: 999, BasePrice: &base, RemoteBPS: &rbps}
	if _, err := s.price(a); err != nil {
		t.Fatalf("price: %v", err)
	}
	if a.Price != 111 {
		t.Errorf("derived total = %d, want 111 (stored total is ignored)", a.Price)
	}

	// The same row under a different local policy: 105 + ceil(105*2000/10000) = 126. Nothing is
	// stored, so a policy change reprices with no re-resolve.
	b := &Action{Kind: KindRemoteProxy, BasePrice: &base, RemoteBPS: &rbps}
	if _, err := (&pricedStore{econ: Economy{ImportBPS: 2000}}).price(b); err != nil {
		t.Fatal(err)
	}
	if b.Price != 126 {
		t.Errorf("derived total at 2000 bps = %d, want 126", b.Price)
	}
}

func TestPricedStoreLeavesOtherRowsAlone(t *testing.T) {
	s := &pricedStore{econ: Economy{ImportBPS: 2000}}

	// A local action's price is authored, not derived.
	local := &Action{Kind: KindHTTP, Price: 42}
	if _, err := s.price(local); err != nil || local.Price != 42 {
		t.Errorf("local action price = %d (err %v), want 42 untouched", local.Price, err)
	}
	// A proxy predating the snapshot keeps its stored total: reversing it cannot recover the
	// seller's price, so it waits to heal instead of being guessed at.
	legacy := &Action{Kind: KindRemoteProxy, Price: 111}
	if _, err := s.price(legacy); err != nil || legacy.Price != 111 {
		t.Errorf("legacy proxy price = %d (err %v), want 111 untouched", legacy.Price, err)
	}
	// A nil action is not a panic.
	if got, err := s.price(nil); got != nil || err != nil {
		t.Errorf("price(nil) = %v, %v; want nil, nil", got, err)
	}
}

func TestPricedStorePropagatesOverflow(t *testing.T) {
	// A signed manifest may name any non-negative int64 price. One whose markup does not fit must
	// surface as an error at the read, not silently serve the stored total: these rows fund calls.
	base := int64(math.MaxInt64)
	rbps := int64(500)
	a := &Action{Kind: KindRemoteProxy, Price: 7, BasePrice: &base, RemoteBPS: &rbps}
	if _, err := (&pricedStore{econ: Economy{ImportBPS: 500}}).price(a); err == nil {
		t.Fatal("an unrepresentable total must be an error, not a fallback to the stored price")
	}
	if a.Price != 7 {
		t.Errorf("a failed derivation must not half-write the row: price = %d, want 7", a.Price)
	}

	// The list form fails the whole read rather than returning a mix of derived and stale rows.
	if _, err := (&pricedStore{econ: Economy{ImportBPS: 500}}).priceMany([]*Action{a}, nil); err == nil {
		t.Error("priceMany must propagate the overflow")
	}
}

// TestRemoteManifestHashFixture pins the manifest-hash wire format. This digest is the
// cross-kernel contract identity (§8 If-Match, cached in Action.ArtifactHash): a change
// invalidates every peer's cached proxy. Never regenerate these values to make a test pass.
func TestRemoteManifestHashFixture(t *testing.T) {
	for _, tc := range manifestFixtures() {
		if got := remoteManifestHash(tc.m); got != tc.want {
			t.Errorf("%s: manifest hash changed\n got  %s\n want %s", tc.name, got, tc.want)
		}
	}
}

func manifestFixtures() []struct {
	name string
	m    ActionManifest
	want string
} {
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"outer": map[string]any{"type": "object", "properties": map[string]any{"inner": map[string]any{"type": "number"}}},
		},
	}
	base := ActionManifest{
		ActionID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", OwnerID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		OwnerHandle: "alice", Name: "greet", Description: "greets a caller", Price: 100, Kind: KindWasm,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		ArtifactHash: "deadbeef",
	}
	withBPS := base
	withBPS.RemoteBPS = 500
	withNested := base
	withNested.InputSchema, withNested.OutputSchema = nested, nested
	renamed := base
	renamed.OwnerHandle = "alice-renamed" // display metadata: must NOT change the hash
	return []struct {
		name string
		m    ActionManifest
		want string
	}{
		{"zero-bps", base, "b909e43f663733f19c202805bda8be0b3ce5fb2f4285de1d5135eb0198c0012b"},
		{"remote-bps-500", withBPS, "ebed5d1fc13f043ece714a5e341c69d37255eddca9f2ca8d79f35761ef6f4252"},
		{"nested-schemas", withNested, "ec8e5e5f130550f733f4e45e56ce552dc87b35a1517b01537f4bf1edc4e0793e"},
		// A display-only handle rename must hash identically to the base fixture.
		{"owner-handle-renamed", renamed, "b909e43f663733f19c202805bda8be0b3ce5fb2f4285de1d5135eb0198c0012b"},
	}
}

// A dispatch that can owe nothing carries no ticket terms: no secret, no commitment, no face value,
// no stake (P4). A priced one carries all of them.
func TestAFreeDispatchCarriesNoTicketTerms(t *testing.T) {
	k := &Kernel{econ: Economy{Lottery: 100}}
	for _, price := range []int64{0, 10} {
		t := t
		a := &Action{OwnerUserID: "seller", Price: price}
		tr := &Trace{}
		if err := k.prepareDispatch(context.Background(), tr, a, nil, "", price, 0, nil); err != nil {
			t.Fatal(err)
		}
		d := dispatched(tr.DispatchJSON)
		if price == 0 && (d.Secret != "" || d.Lottery != 0 || tr.Ticket != 0) {
			t.Errorf("a free dispatch carried terms: secret=%q lottery=%d stake=%d", d.Secret, d.Lottery, tr.Ticket)
		}
		if price > 0 && (d.Secret == "" || d.Lottery != 100 || tr.Ticket != 100) {
			t.Errorf("a priced dispatch lacked terms: secret=%q lottery=%d stake=%d", d.Secret, d.Lottery, tr.Ticket)
		}
	}
}
