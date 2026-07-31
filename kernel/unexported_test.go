package kernel

// This file contains tests that must remain in package kernel because they
// exercise unexported functions (validateHTTPSource, signRating,
// remoteManifestHash, openAPIOperationHash, parseOpenAPISpec, buildReceipt).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestCanCallVisibilityMatrix exercises the caller-scoped, three-level visibility predicate (§4):
// public is callable by anyone; local by any local (non-peer) caller but not a peer; private only
// by the owner; and inactive/suspended-owner actions are never callable regardless of visibility.
func TestCanCallVisibilityMatrix(t *testing.T) {
	owner := &User{ID: "owner"}
	other := &User{ID: "other"}
	peer := &User{ID: "peer", PublicKey: "cGVlcg"}

	mk := func(vis ActionVisibility) *Action {
		return &Action{OwnerUserID: "owner", Active: true, Visibility: vis}
	}
	cases := []struct {
		name    string
		action  *Action
		caller  *User
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

// TestVerifyRemoteReceiptSignatureFailsClosedOnEmptyKey: an empty peer public key must make
// signature verification fail, never be silently skipped — a missing key cannot authenticate
// a receipt, so it must never let an unverified receipt pass as valid (§13).
func TestVerifyRemoteReceiptSignatureFailsClosedOnEmptyKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	r := &Receipt{ID: "r1", ActionID: "a1", Status: TxSuccess, Gross: 5, Net: 5}
	sig, err := signReceipt(priv, r)
	if err != nil {
		t.Fatal(err)
	}
	r.Signature = sig

	if err := verifyRemoteReceiptSignature(r, ""); err == nil {
		t.Fatal("empty peer key: got nil, want error (must fail closed)")
	}
	pubB64 := base64.RawURLEncoding.EncodeToString(pub)
	if err := verifyRemoteReceiptSignature(r, pubB64); err != nil {
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
	return New(nil, nil, nil, nil, cfg, nil)
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
	}, 0)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("expected ErrInvalidState without signing key, got %v", err)
	}
}

func TestRatingSigningRequiresConfiguredKey(t *testing.T) {
	_, err := signRating(nil, &Rating{
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
	withStats.Stats = &Stats{Uses: 99, Successes: 99}
	if remoteManifestHash(base) != remoteManifestHash(withStats) {
		t.Error("stats change must not affect the manifest hash")
	}

	// Identical manifests must produce the same hash.
	if remoteManifestHash(base) != remoteManifestHash(base) {
		t.Error("identical manifests should produce the same hash")
	}
}

func TestOpenAPIOperationHashIncludesParams(t *testing.T) {
	schema := map[string]any{"type": "object"}
	paramsBody := []HTTPParam{{Name: "data", In: "body"}}
	paramsQuery := []HTTPParam{{Name: "data", In: "query"}}

	hashBody := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, paramsBody)
	hashQuery := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, paramsQuery)

	if hashBody == hashQuery {
		t.Error("params with different 'in' values should produce different hashes")
	}

	// Order of params must not affect the hash.
	p1 := []HTTPParam{{Name: "a", In: "query"}, {Name: "b", In: "body"}}
	p2 := []HTTPParam{{Name: "b", In: "body"}, {Name: "a", In: "query"}}
	h1 := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, p1)
	h2 := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, p2)
	if h1 != h2 {
		t.Error("param order should not affect hash")
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

func TestSelectorParsingAndSegmentMatch(t *testing.T) {
	cases := []struct{ sel, owner, path string }{
		{"tom", "tom", ""},
		{"tom/brief", "tom", "brief"},
		{"tom/brief/eu", "tom", "brief/eu"},
		{"tom/*", "tom", ""},
		{"tom/brief/*", "tom", "brief"},
		{"tom/brief", "tom", "brief"}, // missing @ tolerated
	}
	for _, c := range cases {
		o, p, err := ParseGrantSelector(c.sel)
		if err != nil || o != c.owner || p != c.path {
			t.Errorf("ParseGrantSelector(%q) = (%q,%q,%v), want (%q,%q,nil)", c.sel, o, p, err, c.owner, c.path)
		}
	}
	if _, _, err := ParseGrantSelector("@"); err == nil {
		t.Error("bare @ should be rejected")
	}
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
