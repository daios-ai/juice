package kernel

// This file contains tests that must remain in package kernel because they
// exercise unexported functions (validateHTTPSource, signRating,
// remoteManifestHash, openAPIOperationHash, parseOpenAPISpec, buildReceipt).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// newMinimalKernel creates a Kernel with a nil store for tests that only
// exercise unexported methods that do not touch the store.
func newMinimalKernel() *Kernel {
	cfg := DefaultConfig()
	cfg.TokenSecret = "test-secret"
	cfg.IssuerUserID = "test-issuer-id"
	return New(nil, nil, nil, nil, cfg, nil)
}

func TestValidateHTTPSourceSSRF(t *testing.T) {
	rejected := []string{
		"http://localhost/api",
		"http://127.0.0.1/secret",
		"http://::1/secret",
		"http://10.0.0.1/internal",
		"http://192.168.1.1/router",
		"http://172.16.0.1/internal",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://example.com/file",
		"file:///etc/passwd",
		"://broken",
	}
	ctx := context.Background()
	for _, u := range rejected {
		if err := validateHTTPSource(ctx, u, false); err == nil {
			t.Errorf("validateHTTPSource(%q): expected error, got nil", u)
		}
	}

	accepted := []string{
		"https://example.com/api",
		"http://example.com/webhook",
		"https://api.stripe.com/v1/charges",
		// Hostnames that are not literal private IPs are accepted without DNS resolution (§7).
		"http://internal.corp/api",
		"https://api.example.com/v2",
	}
	for _, u := range accepted {
		if err := validateHTTPSource(ctx, u, false); err != nil {
			t.Errorf("validateHTTPSource(%q): unexpected error: %v", u, err)
		}
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
	})
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
		OwnerHandle:  "@peer",
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
	paramsBody := []OpenAPIParam{{Name: "data", In: "body"}}
	paramsQuery := []OpenAPIParam{{Name: "data", In: "query"}}

	hashBody := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, paramsBody)
	hashQuery := openAPIOperationHash("http://api.example.com", "do thing", "POST", "/do",
		schema, schema, 0, paramsQuery)

	if hashBody == hashQuery {
		t.Error("params with different 'in' values should produce different hashes")
	}

	// Order of params must not affect the hash.
	p1 := []OpenAPIParam{{Name: "a", In: "query"}, {Name: "b", In: "body"}}
	p2 := []OpenAPIParam{{Name: "b", In: "body"}, {Name: "a", In: "query"}}
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

func TestParseOpenAPISpecRejectsSecurityRequirement(t *testing.T) {
	spec := `{"openapi":"3.0.0","info":{"title":"T","version":"1"},"servers":[{"url":"http://api.example.com"}],"paths":{"/hello":{"get":{"operationId":"sayHello","description":"says hello","security":[{"apiKey":[]}],"responses":{"200":{"description":"ok","content":{"application/json":{"schema":{"type":"object"}}}}}}}}}`
	_, rejected, _, err := parseOpenAPISpec([]byte(spec), "https://spec.example.com/api.json")
	if err != nil {
		t.Fatalf("parseOpenAPISpec: %v", err)
	}
	if len(rejected) != 1 || rejected[0].Reason != "operation has security requirements" {
		t.Errorf("expected security rejection, got %+v", rejected)
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
