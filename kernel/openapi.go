// SPDX-License-Identifier: AGPL-3.0-only

package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	yaml "go.yaml.in/yaml/v2"
	"net/url"
	"sort"
	"strings"
	"time"
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
	key           string
	location      string // "METHOD /path" in the document
	title         string
	description   string
	inputSchema   map[string]any // canonical (D4)
	outputSchema  map[string]any // canonical (D4)
	notes         []string       // folds that make the stored contract differ from the document
	price         int64
	priceDeclared bool // the document set x-juice-price; an absent price leaves it to the owner (§8)
	source        HTTPSource
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

// derefObject follows a local $ref on a document object that is not a schema — a parameter, a
// request body, a response — which OpenAPI lets authors share under components. Schema references
// are left to the normalizer, which inlines them with cycle detection (D4).
func derefObject(doc map[string]any, v any) map[string]any {
	m, _ := v.(map[string]any)
	for i := 0; i < 8 && m != nil; i++ { // a chain of references ends; a loop of them is no object
		ref, ok := m["$ref"].(string)
		if !ok {
			return m
		}
		ptr, local := strings.CutPrefix(ref, "#/")
		if !local {
			return nil
		}
		m, _ = resolveJSONPointer(doc, ptr).(map[string]any)
	}
	return nil
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
// supported operation (GET/POST/PUT/PATCH/DELETE), each with canonical schemas, and one rejection
// per operation that cannot become an action — never one failure for the whole document. specURL is
// stored in provenance only; no HTTP is performed. The third return value is the base URL
// extracted from the spec's servers array.
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
			op, ok := pathItem[method].(map[string]any)
			if !ok {
				continue
			}
			key := openAPIOperationKey(op, method, path)
			location := strings.ToUpper(method) + " " + path
			reject := func(reason string) {
				rejected = append(rejected, ImportRejection{Key: key, Location: location, Reason: reason})
			}

			// Require an explicit name field; slug fallback is not a valid match key.
			_, hasJuiceName := op["x-juice-name"].(string)
			_, hasOpID := op["operationId"].(string)
			if !hasJuiceName && !hasOpID {
				reject("missing operationId or x-juice-name")
				continue
			}

			title, err := openAPITitle(op)
			if err != nil {
				reject(err.Error())
				continue
			}

			desc := openAPIDescription(op)
			if desc == "" {
				reject("missing description and summary")
				continue
			}

			rawOutput, ok := openAPIOutputSchema(op, spec)
			if !ok {
				reject("no 2xx JSON response schema")
				continue
			}

			var body map[string]any
			if rb := derefObject(spec, op["requestBody"]); rb != nil {
				if content, ok := rb["content"].(map[string]any); ok && len(content) > 0 {
					jc, hasJSON := content["application/json"].(map[string]any)
					if !hasJSON {
						reject("requestBody has no application/json content")
						continue
					}
					body, _ = jc["schema"].(map[string]any)
				}
			}

			if ambig, reason := openAPIAmbiguous2xxSchema(op, spec); ambig {
				reject(reason)
				continue
			}

			var price int64
			priceDeclared := false
			if v, ok := op["x-juice-price"]; ok {
				f, isNum := v.(float64)
				if !isNum || f < 0 || f != float64(int64(f)) {
					reject("price must be a non-negative integer")
					continue
				}
				price, priceDeclared = int64(f), true
			}

			inputSchema, params, inNotes, err := openAPICompileOperation(op, pathItem, body, spec)
			if err != nil {
				reject(err.Error())
				continue
			}
			// Require at least one input parameter or a requestBody schema.
			if len(params) == 0 && body == nil {
				reject("missing parameters and requestBody schema")
				continue
			}
			outputSchema, outNotes, err := normalizeIn("output", rawOutput, spec)
			if err != nil {
				reject(err.Error())
				continue
			}

			ops = append(ops, rawOp{
				key:           key,
				location:      location,
				title:         title,
				description:   desc,
				inputSchema:   inputSchema,
				outputSchema:  outputSchema,
				notes:         append(inNotes, outNotes...),
				price:         price,
				priceDeclared: priceDeclared,
				source: HTTPSource{
					Type:          "openapi",
					SpecURL:       specURL,
					BaseURL:       baseURL,
					Method:        strings.ToUpper(method),
					Path:          path,
					OperationKey:  key,
					PriceDeclared: priceDeclared,
					Params:        params,
				},
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

// openAPITitle is the operation's title: x-juice-title, else the summary OpenAPI already has for
// exactly this purpose. Nothing is derived — an operation key is as technical as an address — so
// an operation with neither is refused, naming what to add.
func openAPITitle(op map[string]any) (string, error) {
	title, _ := op["x-juice-title"].(string)
	if strings.TrimSpace(title) == "" {
		title, _ = op["summary"].(string)
	}
	if strings.TrimSpace(title) == "" {
		return "", ErrInvalidInput.Wrap("no summary: a title is required (summary or x-juice-title)")
	}
	return ValidateTitle(title)
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

// openAPIJSONSchema is a response's JSON schema as written, its references unresolved.
func openAPIJSONSchema(respRaw any, doc map[string]any) map[string]any {
	resp := derefObject(doc, respRaw)
	content, _ := resp["content"].(map[string]any)
	jsonContent, _ := content["application/json"].(map[string]any)
	schema, _ := jsonContent["schema"].(map[string]any)
	if len(schema) == 0 {
		return nil
	}
	return schema
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

// openAPICompileOperation builds both the canonical input schema and the HTTP parameter binding
// list for one operation in a single pass: path and query parameters and the JSON body's
// properties become one object (D4). It is the single source of truth for what fields an operation
// accepts and where they go. The notes are the folds normalization made.
func openAPICompileOperation(op, pathItem, body, doc map[string]any) (map[string]any, []HTTPParam, []string, error) {
	properties := map[string]any{}
	var required []any
	var params []HTTPParam
	var notes []string
	seen := map[string]struct{}{}

	for _, source := range []map[string]any{pathItem, op} {
		ps, _ := source["parameters"].([]any)
		for _, pRaw := range ps {
			p := derefObject(doc, pRaw)
			if p == nil {
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
			schema := map[string]any{"type": "string"}
			if s, ok := p["schema"].(map[string]any); ok {
				schema = make(map[string]any, len(s)+1)
				for k, v := range s {
					schema[k] = v
				}
			}
			if desc, ok := p["description"].(string); ok && desc != "" {
				schema["description"] = desc
			}
			properties[name] = schema
			params = append(params, HTTPParam{Name: name, In: in})
			if req, _ := p["required"].(bool); req || in == "path" {
				required = append(required, name)
			}
		}
	}

	if body != nil {
		// The body is normalized on its own first, so a shared body schema ($ref) is read as the
		// one object it is before its fields join the parameters.
		canon, bodyNotes, err := normalizeIn("input", body, doc)
		if err != nil {
			return nil, nil, nil, err
		}
		notes = append(notes, bodyNotes...)
		props, _ := canon["properties"].(map[string]any)
		for name, v := range props {
			if _, dup := seen[name]; !dup {
				seen[name] = struct{}{}
				properties[name] = v
				params = append(params, HTTPParam{Name: name, In: "body"})
			}
		}
		reqs, _ := canon["required"].([]any)
		required = append(required, reqs...)
	}

	result := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		result["required"] = required
	}
	inputSchema, paramNotes, err := normalizeIn("input", result, doc)
	if err != nil {
		return nil, nil, nil, err
	}
	// Body fields come out of a map, so their order varies between parses of one document. Bindings
	// are looked up by name and never by position, so ordering them by name costs nothing and makes
	// a stored source compare equal to itself on the next import (§8).
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })
	return inputSchema, params, append(notes, paramNotes...), nil
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

// validateImportName checks the application path an import lands under: non-empty, no kernel
// qualifier, and no empty segment — one rule covering a leading, trailing, or doubled slash.
// Nothing further, since action names carry no character class of their own and inventing one here
// would be a second naming authority.
func validateImportName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", ErrInvalidInput.Wrap("import name is required: it is the application's path under your account")
	}
	if strings.Contains(name, "@") {
		return "", ErrInvalidInput.Wrap("import name must not contain @: it qualifies a kernel")
	}
	if looksLikeID(name) {
		return "", ErrInvalidInput.Wrap("import name must not be id-shaped")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" {
			return "", ErrInvalidInput.Wrap("import name must not have an empty path segment")
		}
	}
	return name, nil
}

// installRootOf derives the application path a row was imported under, by taking its name apart
// from its own operation key. Nothing stores it: a stored copy could drift from the names
// themselves, and creation is the only writer of a name — there is no rename path. The derivation
// is exact, so an application installed beneath another (mail and mail/calendar) keeps its own
// rows: re-importing the parent classifies only rows whose root is the parent (§8).
func installRootOf(a *Action) (string, bool) {
	var src HTTPSource
	if err := json.Unmarshal([]byte(a.Source), &src); err != nil || src.Type != "openapi" || src.OperationKey == "" {
		return "", false
	}
	switch {
	case a.Name == src.OperationKey:
		return "", true
	case strings.HasSuffix(a.Name, "/"+src.OperationKey):
		return strings.TrimSuffix(a.Name, "/"+src.OperationKey), true
	}
	return "", false
}

// installedRows returns the owner's OpenAPI rows whose derived application root is exactly name.
func (k *Kernel) installedRows(ctx context.Context, ownerID, name string) ([]*Action, error) {
	all, err := k.store.ListActionsByOwner(ctx, ownerID, maxOwnerActions, 0)
	if err != nil {
		return nil, err
	}
	var out []*Action
	for _, a := range all {
		if root, ok := installRootOf(a); ok && root == name {
			out = append(out, a)
		}
	}
	return out, nil
}

// storedSpecURL reports the document an installed application was imported from, so a re-import
// needs only the application's name. Rows that disagree are a conflict rather than a coin toss.
func (k *Kernel) storedSpecURL(rows []*Action) (string, error) {
	found := ""
	for _, a := range rows {
		var src HTTPSource
		if err := json.Unmarshal([]byte(a.Source), &src); err != nil {
			return "", ErrInvalidState.Wrap("installed action has an unreadable source")
		}
		if found != "" && src.SpecURL != found {
			return "", ErrInvalidState.Wrapf("actions under %q come from more than one document", a.Name)
		}
		found = src.SpecURL
	}
	if found == "" {
		return "", ErrNotFound.Wrap("no OpenAPI application is installed under that name")
	}
	return found, nil
}

// StoredOpenAPISpecURL returns the document URL an installed application was imported from.
func (k *Kernel) StoredOpenAPISpecURL(ctx context.Context, subjectID, ownerID, name string) (string, error) {
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		return "", err
	}
	name, err := validateImportName(name)
	if err != nil {
		return "", err
	}
	rows, err := k.installedRows(ctx, ownerID, name)
	if err != nil {
		return "", err
	}
	return k.storedSpecURL(rows)
}

// documentMoved reports whether the document now offers something different from what the stored
// row holds. The comparison is against the row's current values rather than a hash recorded at the
// last import, so it answers one question in both directions: the document changed, or the row was
// edited away from the document. Either way the document owns these fields and restores them. The
// price is the owner's unless the document declares one (§8).
func documentMoved(existing *Action, raw rawOp) bool {
	var src HTTPSource
	if err := json.Unmarshal([]byte(existing.Source), &src); err != nil {
		return true
	}
	if existing.Title != raw.title || existing.Description != raw.description ||
		src.BaseURL != raw.source.BaseURL || src.Method != raw.source.Method || src.Path != raw.source.Path ||
		src.SpecURL != raw.source.SpecURL || src.PriceDeclared != raw.priceDeclared {
		return true
	}
	if raw.priceDeclared && existing.Price != raw.price {
		return true
	}
	if !jsonEqual(src.Params, raw.source.Params) {
		return true
	}
	return !jsonEqual(existing.InputSchema, raw.inputSchema) || !jsonEqual(existing.OutputSchema, raw.outputSchema)
}

// jsonEqual compares two JSON-shaped values by their canonical encoding.
func jsonEqual(a, b any) bool {
	ab, aerr := CanonicalJSON(a)
	bb, berr := CanonicalJSON(b)
	return aerr == nil && berr == nil && string(ab) == string(bb)
}

// ImportOpenAPI installs or re-installs one OpenAPI document as the application at name, under the
// owner's account: one ordinary action per representable operation, created and updated through the
// same requests `action create` and `action update` use, so an imported action is held to exactly
// the rules a hand-written one is (§8). The application's path is its identity — one name holds one
// document, and re-importing needs only the name — while the same document may be installed under
// several names as independent applications. Every operation is parsed, checked, and prepared
// before the first row is written, and the writes then run in name order, so an interrupted import
// leaves whole actions and re-running it continues where it stopped.
func (k *Kernel) ImportOpenAPI(ctx context.Context, subjectID, ownerID, name, specURL string, specBytes []byte, auth *AuthInput) (*ImportResult, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("openapi.import.start", "name", name, "spec_url", specURL)
	if err := k.requireSelf(ctx, subjectID, ownerID); err != nil {
		logger.Warn("openapi.import.failed", "name", name, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	name, err := validateImportName(name)
	if err != nil {
		return nil, err
	}
	// Credentials are the caller's one input for the whole application, so a bad scheme or a
	// missing key fails the import outright rather than as a defect of each operation in turn.
	if auth != nil {
		if err := k.validateAuthInput(ctx, auth); err != nil {
			return nil, err
		}
		if k.secretBox == nil {
			return nil, ErrInvalidState.Wrap("credential encryption is not configured; cannot store upstream auth")
		}
	}

	rawOps, rejected, _, err := parseOpenAPISpec(specBytes, specURL)
	if err != nil {
		return nil, err
	}

	existing, err := k.installedRows(ctx, ownerID, name)
	if err != nil {
		return nil, err
	}
	// One name holds one document: re-importing a different document under an occupied name would
	// silently mix two upstreams under one application, so it is refused and the rows stay put. The
	// same document under a second name is an independent application and needs no permission.
	if len(existing) > 0 {
		bound, berr := k.storedSpecURL(existing)
		if berr != nil {
			return nil, berr
		}
		if bound != specURL {
			return nil, ErrInvalidInput.Wrapf("%q already holds the application imported from %s", name, bound)
		}
	}
	existingByKey := make(map[string]*Action, len(existing))
	for _, a := range existing {
		var src HTTPSource
		if err := json.Unmarshal([]byte(a.Source), &src); err == nil {
			existingByKey[src.OperationKey] = a
		}
	}

	// Operations come out of a map-ordered parse, so order them before anything selects among them:
	// which of two operations claiming one key is kept must not vary between runs.
	sort.Slice(rawOps, func(i, j int) bool {
		if rawOps[i].key != rawOps[j].key {
			return rawOps[i].key < rawOps[j].key
		}
		if rawOps[i].source.Path != rawOps[j].source.Path {
			return rawOps[i].source.Path < rawOps[j].source.Path
		}
		return rawOps[i].source.Method < rawOps[j].source.Method
	})

	// Preflight the whole document: a duplicate key or name, an unusable schema, or an unsafe
	// upstream is reported before anything is written, never halfway through.
	seenKey := map[string]struct{}{}
	seenName := map[string]struct{}{}
	byKey := map[string]rawOp{}
	var incoming []incomingOp
	reject := func(raw rawOp, reason string) {
		rejected = append(rejected, ImportRejection{Key: raw.key, Location: raw.location, Reason: reason})
	}
	for _, raw := range rawOps {
		raw := raw
		actionName := name + "/" + raw.key
		if _, dup := seenKey[raw.key]; dup {
			reject(raw, "duplicate operation key in document")
			continue
		}
		if _, dup := seenName[actionName]; dup {
			reject(raw, "duplicate action name in document")
			continue
		}
		// A name already held by an action outside this application is not ours to take.
		if _, held := existingByKey[raw.key]; !held {
			if other, rerr := k.store.ReadActionByOwnerName(ctx, ownerID, actionName); rerr == nil && other != nil {
				reject(raw, "name collision with existing action")
				continue
			}
		}
		seenKey[raw.key] = struct{}{}
		seenName[actionName] = struct{}{}
		byKey[raw.key] = raw
		incoming = append(incoming, incomingOp{key: raw.key, name: actionName})
	}

	plan := classifyImport(existingByKey, incoming, func(ex *Action, op incomingOp) bool {
		return documentMoved(ex, byKey[op.key])
	})

	// Prepare every write before committing any of them: an unsafe upstream URL or a credential the
	// kernel cannot seal fails here, with the installation untouched.
	type pendingUpdate struct {
		a                        *Action
		resetStats, revokeGrants bool
	}
	var creates []*Action
	var updates []pendingUpdate
	var updatedRows, unchangedRows []*Action

	for _, op := range plan.New {
		raw := byKey[op.key]
		src := raw.source
		a, perr := k.prepareCreateAction(ctx, CreateActionRequest{
			OwnerUserID:  ownerID,
			Name:         op.name,
			Title:        raw.title,
			Kind:         KindHTTP,
			Price:        raw.price,
			Description:  raw.description,
			InputSchema:  raw.inputSchema,
			OutputSchema: raw.outputSchema,
			HTTP:         &src,
			Auth:         auth,
		})
		if perr != nil {
			reject(raw, perr.Error())
			continue
		}
		creates = append(creates, a)
	}

	for _, ch := range plan.Changed {
		raw := byKey[ch.op.key]
		src := raw.source
		req := UpdateActionRequest{
			Title:        &raw.title,
			Description:  &raw.description,
			InputSchema:  raw.inputSchema,
			OutputSchema: raw.outputSchema,
			HTTP:         &src,
			Auth:         auth,
		}
		// The price is the document's only where the document declares one; otherwise it is the
		// owner's number and survives every re-import.
		if raw.priceDeclared {
			price := raw.price
			req.Price = &price
		}
		resetStats, revokeGrants, perr := k.prepareUpdateAction(ctx, ch.existing, req)
		if perr != nil {
			reject(raw, perr.Error())
			continue
		}
		updates = append(updates, pendingUpdate{a: ch.existing, resetStats: resetStats, revokeGrants: revokeGrants})
		updatedRows = append(updatedRows, ch.existing)
	}

	for _, ch := range plan.Unchanged {
		// The document says nothing new, but the caller may still be attaching credentials to the
		// whole application; that is a write, and the row is reported as updated because it is.
		if auth != nil {
			resetStats, revokeGrants, perr := k.prepareUpdateAction(ctx, ch.existing, UpdateActionRequest{Auth: auth})
			if perr != nil {
				reject(byKey[ch.op.key], perr.Error())
				continue
			}
			updates = append(updates, pendingUpdate{a: ch.existing, resetStats: resetStats, revokeGrants: revokeGrants})
			updatedRows = append(updatedRows, ch.existing)
			continue
		}
		unchangedRows = append(unchangedRows, ch.existing)
	}

	// Rejections come out of a map-ordered parse; order them so one document always reports the
	// same way.
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].Key < rejected[j].Key })
	// Every operation that lands carries its notes, on every import: the stored contract differs
	// from the document in those places for as long as the document says what it says.
	refused := map[string]bool{}
	for _, r := range rejected {
		refused[r.Key] = true
	}
	var notices []ImportNotice
	for _, op := range incoming {
		if refused[op.key] {
			continue
		}
		raw := byKey[op.key]
		for _, note := range raw.notes {
			notices = append(notices, ImportNotice{Key: raw.key, Location: raw.location, Note: note})
		}
	}
	result := &ImportResult{Unchanged: unchangedRows, Rejected: rejected, Notices: notices}
	// Commit. Deactivations first, so a document that renamed an operation frees nothing it still
	// needs; then updates and creates in name order.
	for _, a := range plan.Stale {
		a.Active = false
		if err := k.commitUpdateAction(ctx, a, true, true); err != nil {
			return result, err
		}
		result.Deactivated = append(result.Deactivated, a)
	}
	for _, u := range updates {
		if err := k.commitUpdateAction(ctx, u.a, u.resetStats, u.revokeGrants); err != nil {
			return result, err
		}
		k.indexForLookup(ctx, u.a)
	}
	result.Updated = updatedRows
	for _, a := range creates {
		if err := k.store.CreateAction(ctx, a); err != nil {
			return result, err
		}
		result.Created = append(result.Created, a)
	}

	logger.Info("openapi.import.done", "name", name, "spec_url", specURL, "created", len(result.Created),
		"updated", len(result.Updated), "unchanged", len(result.Unchanged), "deactivated", len(result.Deactivated),
		"rejected", len(result.Rejected), "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return result, nil
}
