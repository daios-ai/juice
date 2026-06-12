package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	yaml "go.yaml.in/yaml/v2"
)

// yamlToJSON converts a YAML byte slice to canonical JSON bytes.
func yamlToJSON(src []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(src, &v); err != nil {
		return nil, err
	}
	return json.Marshal(normalizeYAML(v))
}

// normalizeYAML converts map[interface{}]interface{} values produced by yaml/v2
// into map[string]any so encoding/json can marshal them.
func normalizeYAML(v any) any {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		out := make(map[string]any, len(val))
		for k, v := range val {
			out[fmt.Sprintf("%v", k)] = normalizeYAML(v)
		}
		return out
	case []interface{}:
		for i, item := range val {
			val[i] = normalizeYAML(item)
		}
		return val
	default:
		return v
	}
}

// rawOp is one parsed OpenAPI operation before it is bound to an owner.
type rawOp struct {
	key          string
	description  string
	method       string
	path         string
	baseURL      string
	params       []OpenAPIParam
	inputSchema  map[string]any
	outputSchema map[string]any
	price        int64
	hash         string
	sourceJSON   string
}

// resolveJSONPointer follows a JSON Pointer path (e.g. "components/schemas/Foo") inside doc.
func resolveJSONPointer(doc map[string]any, ptr string) any {
	var curr any = doc
	for _, part := range strings.Split(ptr, "/") {
		if part == "" {
			continue
		}
		m, ok := curr.(map[string]any)
		if !ok {
			return nil
		}
		curr = m[part]
	}
	return curr
}

// resolveRefsValue recursively inlines local #/... $ref values found anywhere in node.
// External $ref values (not starting with "#/") are left as-is.
// depth limits recursion to prevent infinite loops from circular schemas.
func resolveRefsValue(doc map[string]any, node any, depth int) any {
	if depth > 10 {
		return node
	}
	switch v := node.(type) {
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
			target := resolveJSONPointer(doc, strings.TrimPrefix(ref, "#/"))
			if target == nil {
				return v
			}
			return resolveRefsValue(doc, target, depth+1)
		}
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = resolveRefsValue(doc, val, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = resolveRefsValue(doc, item, depth+1)
		}
		return out
	default:
		return v
	}
}

// resolveRefsMap resolves all local $ref values in schema using doc as the root document.
func resolveRefsMap(doc, schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	resolved := resolveRefsValue(doc, schema, 0)
	if m, ok := resolved.(map[string]any); ok {
		return m
	}
	return schema
}

// looksLikeJSON reports whether b (ignoring leading whitespace) starts with '{' or '['.
func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// parseOpenAPISpec parses specBytes (already-fetched JSON or YAML) and returns one rawOp per
// supported operation (GET/POST/PUT/PATCH/DELETE). specURL is stored in provenance only; no HTTP
// is performed. The third return value is the base URL extracted from the spec's servers array.
// Local $ref values (#/components/schemas/…) in parameter and response schemas are resolved inline.
func parseOpenAPISpec(specBytes []byte, specURL string) ([]rawOp, []ImportRejection, string, error) {
	jsonBytes := specBytes
	if !looksLikeJSON(specBytes) {
		var yamlErr error
		jsonBytes, yamlErr = yamlToJSON(specBytes)
		if yamlErr != nil {
			return nil, nil, "", ErrInvalidInput.Wrapf("spec is not valid JSON or YAML: %v", yamlErr)
		}
	}
	var spec map[string]any
	if err := json.Unmarshal(jsonBytes, &spec); err != nil {
		return nil, nil, "", ErrInvalidInput.Wrap("spec is not valid JSON")
	}

	// Extract base URL from first server entry.
	baseURL := ""
	if servers, ok := spec["servers"].([]any); ok && len(servers) > 0 {
		if s, ok := servers[0].(map[string]any); ok {
			baseURL, _ = s["url"].(string)
		}
	}
	if baseURL == "" {
		u, _ := url.Parse(specURL)
		baseURL = u.Scheme + "://" + u.Host
	}
	baseURL = strings.TrimRight(baseURL, "/")

	paths, _ := spec["paths"].(map[string]any)
	var ops []rawOp
	var rejected []ImportRejection

	for path, pathItemRaw := range paths {
		pathItem, ok := pathItemRaw.(map[string]any)
		if !ok {
			continue
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			opRaw, ok := pathItem[method]
			if !ok {
				continue
			}
			op, ok := opRaw.(map[string]any)
			if !ok {
				continue
			}

			key := openAPIOperationKey(op, method, path)

			// Require an explicit name field; slug fallback is not a valid match key.
			_, hasJuiceName := op["x-juice-name"].(string)
			_, hasOpID := op["operationId"].(string)
			if !hasJuiceName && !hasOpID {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "missing operationId or x-juice-name"})
				continue
			}

			desc := openAPIDescription(op)
			if desc == "" {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "missing description and summary"})
				continue
			}

			outputSchema, ok := openAPIOutputSchema(op, spec)
			if !ok {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "no 2xx JSON response schema"})
				continue
			}

			// Operations with security requirements are imported inactive.
			// Configure upstream auth via POST/PUT /v1/actions and activate once ready.

			_, hasBody := op["requestBody"].(map[string]any)
			if hasBody {
				rb := op["requestBody"].(map[string]any)
				if content, ok := rb["content"].(map[string]any); ok && len(content) > 0 {
					if _, hasJSON := content["application/json"]; !hasJSON {
						rejected = append(rejected, ImportRejection{Key: key, Reason: "requestBody has no application/json content"})
						continue
					}
				}
			}

			if ambig, reason := openAPIAmbiguous2xxSchema(op, spec); ambig {
				rejected = append(rejected, ImportRejection{Key: key, Reason: reason})
				continue
			}

			var price int64
			if v, ok := op["x-juice-price"]; ok {
				if f, ok := v.(float64); ok {
					if int64(f) < 0 || f != float64(int64(f)) {
						rejected = append(rejected, ImportRejection{Key: key, Reason: "price must be a non-negative integer"})
						continue
					}
					price = int64(f)
				}
			}

			inputSchema, params := openAPICompileOperation(op, pathItem, spec)

			// Require at least one input parameter or a requestBody schema.
			if len(params) == 0 && !hasBody {
				rejected = append(rejected, ImportRejection{Key: key, Reason: "missing parameters and requestBody schema"})
				continue
			}

			hash := openAPIOperationHash(baseURL, desc, method, path, inputSchema, outputSchema, price, params)

			src := OpenAPISource{
				Type:          "openapi",
				SpecURL:       specURL,
				BaseURL:       baseURL,
				Method:        strings.ToUpper(method),
				Path:          path,
				OperationKey:  key,
				OperationHash: hash,
				Params:        params,
			}
			srcBytes, _ := json.Marshal(src)

			ops = append(ops, rawOp{
				key:          key,
				description:  desc,
				method:       strings.ToUpper(method),
				path:         path,
				baseURL:      baseURL,
				params:       params,
				inputSchema:  inputSchema,
				outputSchema: outputSchema,
				price:        price,
				hash:         hash,
				sourceJSON:   string(srcBytes),
			})
		}
	}
	return ops, rejected, baseURL, nil
}

func openAPIOperationKey(op map[string]any, method, path string) string {
	if v, ok := op["x-juice-name"].(string); ok && v != "" {
		return v
	}
	if v, ok := op["operationId"].(string); ok && v != "" {
		return v
	}
	return openAPISlug(method + "-" + path)
}

func openAPIDescription(op map[string]any) string {
	if v, ok := op["description"].(string); ok && v != "" {
		return v
	}
	if v, ok := op["summary"].(string); ok && v != "" {
		return v
	}
	return ""
}

func openAPIOutputSchema(op, doc map[string]any) (map[string]any, bool) {
	responses, ok := op["responses"].(map[string]any)
	if !ok {
		return nil, false
	}
	for _, code := range []string{"200", "201", "202", "203", "204"} {
		if schema := openAPIJSONSchema(responses[code], doc); schema != nil {
			return schema, true
		}
	}
	for code, resp := range responses {
		if len(code) == 3 && code[0] == '2' {
			if schema := openAPIJSONSchema(resp, doc); schema != nil {
				return schema, true
			}
		}
	}
	return nil, false
}

func openAPIJSONSchema(respRaw any, doc map[string]any) map[string]any {
	resp, ok := respRaw.(map[string]any)
	if !ok {
		return nil
	}
	content, _ := resp["content"].(map[string]any)
	jsonContent, _ := content["application/json"].(map[string]any)
	schema, _ := jsonContent["schema"].(map[string]any)
	if len(schema) == 0 {
		return nil
	}
	return resolveRefsMap(doc, schema)
}

func openAPIAmbiguous2xxSchema(op, doc map[string]any) (bool, string) {
	responses, _ := op["responses"].(map[string]any)
	var seen []string
	for code, resp := range responses {
		if len(code) != 3 || code[0] != '2' {
			continue
		}
		s := openAPIJSONSchema(resp, doc)
		if s == nil {
			continue
		}
		b, _ := json.Marshal(s)
		seen = append(seen, string(b))
	}
	if len(seen) < 2 {
		return false, ""
	}
	first := seen[0]
	for _, s := range seen[1:] {
		if s != first {
			return true, "ambiguous 2xx response schemas"
		}
	}
	return false, ""
}

// openAPICompileOperation builds both the validation input schema and the HTTP parameter
// binding list for one operation in a single pass, resolving local $ref values throughout.
// This is the single source of truth for what fields an operation accepts and where they go.
func openAPICompileOperation(op, pathItem, doc map[string]any) (inputSchema map[string]any, params []OpenAPIParam) {
	properties := map[string]any{}
	var required []string
	seen := map[string]struct{}{}

	for _, source := range []map[string]any{pathItem, op} {
		ps, _ := source["parameters"].([]any)
		for _, pRaw := range ps {
			p, ok := pRaw.(map[string]any)
			if !ok {
				continue
			}
			in, _ := p["in"].(string)
			if in != "path" && in != "query" {
				continue
			}
			name, _ := p["name"].(string)
			if name == "" {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			seen[name] = struct{}{}
			schema, _ := p["schema"].(map[string]any)
			if schema == nil {
				schema = map[string]any{"type": "string"}
			} else {
				schema = resolveRefsMap(doc, schema)
			}
			if desc, ok := p["description"].(string); ok && desc != "" {
				schema["description"] = desc
			}
			properties[name] = schema
			params = append(params, OpenAPIParam{Name: name, In: in})
			if req, _ := p["required"].(bool); req || in == "path" {
				required = append(required, name)
			}
		}
	}

	if rb, ok := op["requestBody"].(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		jc, _ := content["application/json"].(map[string]any)
		if rawSchema, ok := jc["schema"].(map[string]any); ok {
			bodySchema := resolveRefsMap(doc, rawSchema)
			if props, ok := bodySchema["properties"].(map[string]any); ok {
				for name, v := range props {
					if _, dup := seen[name]; !dup {
						seen[name] = struct{}{}
						properties[name] = v
						params = append(params, OpenAPIParam{Name: name, In: "body"})
					}
				}
			}
			if reqs, ok := bodySchema["required"].([]any); ok {
				for _, r := range reqs {
					if s, ok := r.(string); ok {
						required = append(required, s)
					}
				}
			}
		}
	}

	result := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		result["required"] = required
	}
	return result, params
}

func openAPIOperationHash(baseURL, description, method, path string, inputSchema, outputSchema map[string]any, price int64, params []OpenAPIParam) string {
	sorted := make([]OpenAPIParam, len(params))
	copy(sorted, params)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	paramsSlice := make([]any, len(sorted))
	for i, p := range sorted {
		paramsSlice[i] = map[string]any{"in": p.In, "name": p.Name}
	}
	payload := map[string]any{
		"base_url":      baseURL,
		"description":   description,
		"input_schema":  inputSchema,
		"method":        method,
		"output_schema": outputSchema,
		"params":        paramsSlice,
		"path":          path,
		"price":         price,
	}
	b, _ := CanonicalJSON(payload)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func openAPISlug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prev := '-'
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prev = r
		} else if prev != '-' {
			b.WriteRune('-')
			prev = '-'
		}
	}
	return strings.Trim(b.String(), "-")
}

// ImportOpenAPI parses specBytes (caller-fetched OpenAPI JSON), reconciles operations with
// existing OpenAPI-imported actions for the owner, and returns the diff. It is idempotent.
func (k *Kernel) ImportOpenAPI(ctx context.Context, subjectID, ownerID, specURL string, specBytes []byte) (*ImportResult, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("openapi.import.start", "spec_url", specURL)
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		logger.Warn("openapi.import.failed", "spec_url", specURL, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	owner, err := k.store.ReadUser(ctx, ownerID)
	if err != nil {
		return nil, err
	}

	rawOps, rejected, baseURL, err := parseOpenAPISpec(specBytes, specURL)
	if err != nil {
		return nil, err
	}

	// Proof 1: well-known challenge — GET {baseURL}/.well-known/juice-owner.txt must return the owner handle.
	// Proof 2: x-juice-owner field in the spec document (embedded challenge, less strong).
	ownershipVerified := false
	if baseURL != "" {
		if uf, ok := k.http.(URLFetcher); ok {
			wkURL := strings.TrimRight(baseURL, "/") + "/.well-known/juice-owner.txt"
			if body, fetchErr := uf.FetchURL(ctx, wkURL); fetchErr == nil {
				ownershipVerified = strings.TrimSpace(string(body)) == owner.Handle
			}
		}
	}
	if !ownershipVerified {
		var specMap map[string]any
		_ = json.Unmarshal(specBytes, &specMap)
		ownershipVerified = specMap["x-juice-owner"] == owner.Handle
	}

	existing, err := k.store.ListActionsByOwnerOpenAPISpec(ctx, ownerID, specURL)
	if err != nil {
		return nil, err
	}

	existingByKey := make(map[string]*Action, len(existing))
	for _, a := range existing {
		var src OpenAPISource
		if err := json.Unmarshal([]byte(a.Source), &src); err == nil {
			existingByKey[src.OperationKey] = a
		}
	}

	hashOf := func(a *Action) string {
		var src OpenAPISource
		if err := json.Unmarshal([]byte(a.Source), &src); err == nil {
			return src.OperationHash
		}
		return ""
	}

	var incoming []incomingOp
	for _, raw := range rawOps {
		raw := raw
		name := raw.key
		// Name collision: only check for truly new ops (not already imported).
		if _, exists := existingByKey[raw.key]; !exists {
			if _, err := k.store.ReadActionByOwnerName(ctx, ownerID, name); err == nil {
				rejected = append(rejected, ImportRejection{Key: raw.key, Reason: "name collision with existing action"})
				continue
			}
		}
		// Validate schemas before storing to prevent invalid data from being written.
		if raw.inputSchema != nil {
			if err := ValidateSchema(raw.inputSchema); err != nil {
				rejected = append(rejected, ImportRejection{Key: raw.key, Reason: "invalid input schema: " + err.Error()})
				continue
			}
		}
		if raw.outputSchema != nil {
			if err := ValidateSchema(raw.outputSchema); err != nil {
				rejected = append(rejected, ImportRejection{Key: raw.key, Reason: "invalid output schema: " + err.Error()})
				continue
			}
		}
		// Validate base URL against SSRF rules (same as CreateAction for KindHTTP).
		if raw.baseURL != "" {
			if err := validateHTTPSource(ctx, raw.baseURL, k.cfg.AllowLocalSources); err != nil {
				rejected = append(rejected, ImportRejection{Key: raw.key, Reason: "unsafe source URL: " + err.Error()})
				continue
			}
		}
		sourceJSON := raw.sourceJSON
		if ownershipVerified {
			var osrc OpenAPISource
			_ = json.Unmarshal([]byte(raw.sourceJSON), &osrc)
			osrc.OwnershipVerified = true
			if b, marshalErr := json.Marshal(osrc); marshalErr == nil {
				sourceJSON = string(b)
			}
		}
		incoming = append(incoming, incomingOp{
			key:  raw.key,
			hash: raw.hash,
			apply: func(a *Action) {
				a.Description  = raw.description
				a.Price        = raw.price
				a.InputSchema  = raw.inputSchema
				a.OutputSchema = raw.outputSchema
				a.Source       = sourceJSON
			},
			new: func() *Action {
				now := time.Now().UTC()
				return &Action{
					ID:           uuid.New().String(),
					OwnerUserID:  ownerID,
					Name:         name,
					Kind:         KindHTTP,
					Active:       false,
					Description:  raw.description,
					Price:        raw.price,
					InputSchema:  raw.inputSchema,
					OutputSchema: raw.outputSchema,
					Source:       sourceJSON,
					CreatedAt:    now,
					UpdatedAt:    now,
				}
			},
		})
	}

	result, err := k.reconcileImport(ctx, existingByKey, hashOf, incoming, true)
	if err != nil {
		return nil, err
	}

	// Staleness fix: re-evaluate ownership on Unchanged actions too.
	// Proof state may have changed since the last import (e.g., well-known file removed).
	for _, a := range result.Unchanged {
		var src OpenAPISource
		if jsonErr := json.Unmarshal([]byte(a.Source), &src); jsonErr == nil && src.OwnershipVerified != ownershipVerified {
			src.OwnershipVerified = ownershipVerified
			if b, marshalErr := json.Marshal(src); marshalErr == nil {
				a.Source = string(b)
				a.UpdatedAt = time.Now().UTC()
				_ = k.store.UpdateAction(ctx, a)
			}
		}
	}

	result.Rejected = append(result.Rejected, rejected...)
	logger.Info("openapi.import.done", "spec_url", specURL, "created", len(result.Created), "updated", len(result.Updated), "unchanged", len(result.Unchanged), "deactivated", len(result.Deactivated), "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return result, nil
}

// UnimportOpenAPI deactivates all OpenAPI-imported actions with matching owner + spec_url.
// If name is non-empty, only actions whose name or operation_key matches are deactivated.
// The subject must be the owner or the platform superuser.
func (k *Kernel) UnimportOpenAPI(ctx context.Context, subjectID, ownerID, specURL, name string) ([]*Action, error) {
	// Always require an authenticated, non-suspended subject.
	if _, err := k.requireActiveUser(ctx, subjectID); err != nil {
		return nil, err
	}
	actions, err := k.store.ListActionsByOwnerOpenAPISpec(ctx, ownerID, specURL)
	if err != nil {
		return nil, err
	}
	if name != "" {
		var filtered []*Action
		for _, a := range actions {
			var src OpenAPISource
			json.Unmarshal([]byte(a.Source), &src)
			if a.Name == name || src.OperationKey == name {
				filtered = append(filtered, a)
			}
		}
		actions = filtered
	}
	// If no actions match the spec, require subject == owner (or @sys) to prevent
	// arbitrary users from probing spec URLs for existence.
	if len(actions) == 0 {
		return nil, k.requireSelf(ctx, subjectID, ownerID)
	}
	// For each matched action, require owner or superuser.
	for _, a := range actions {
		if err := k.requireAdmin(ctx, subjectID, a); err != nil {
			return nil, err
		}
	}
	if err := k.deactivateImported(ctx, actions, false); err != nil {
		return nil, err
	}
	return actions, nil
}
