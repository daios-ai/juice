package kernel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daios-ai/juice/log"
	"github.com/google/uuid"
	"golang.org/x/net/publicsuffix"
)

// Config holds kernel-level configuration.
type Config struct {
	FeeBPS            int64         // basis points, e.g. 2000 = 20%
	RemoteBPS         int64         // basis points provider premium on inbound remote calls, default 500
	FeeRecipientID    string        // user ID that receives fees
	TokenSecret       string        // HMAC secret for JWT signing
	TokenTTL          time.Duration // token validity window
	ScriptTimeout     time.Duration
	ScriptMemory      int64              // bytes
	AllowLocalSources bool               // permit private/LAN/reserved URLs as action sources (loopback is allowed by default)
	SigningKey        ed25519.PrivateKey // Ed25519 private key for receipt/manifest signatures; nil until bootstrap
	IssuerUserID      string             // @sys user ID, set during bootstrap
	AuthIssuer        string             // config.json auth_issuer — iss claim in JWTs; empty = no claim
	AuthAudience      string             // config.json auth_audience — aud claim in JWTs; empty = no validation
	// RemotePendingMaxAge bounds how long a remote-proxy call may stay pending before it settles
	// as a failure with full refund, so a silent peer can't pin a process open. 0 = default 24h.
	RemotePendingMaxAge time.Duration
	// PeerRetention bounds how long a peer may stay idle at zero balance before it is purged
	// (§13 Retention). 0 = disabled (never purge). Set from peer_retention_days.
	PeerRetention time.Duration
}

// DefaultConfig returns safe local defaults.
func DefaultConfig() Config {
	return Config{
		FeeBPS:        2000,
		RemoteBPS:     500,
		TokenTTL:      15 * time.Minute,
		ScriptTimeout: 10 * time.Second,
		ScriptMemory:  64 * 1024 * 1024, // 64 MiB
	}
}

// ceilDiv returns ceil(a/b) using integer arithmetic.
func ceilDiv(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	return (a + b - 1) / b
}

// AllowsLocalSources reports whether the kernel is configured to permit private/LAN/reserved source
// URLs (loopback is permitted regardless).
func (k *Kernel) AllowsLocalSources() bool { return k.cfg.AllowLocalSources }

// NativeFunc is the signature for a registered native action handler.
// targetID is the action's owner; callerID is the call caller; ownerUserID is the process owner.
type NativeFunc func(ctx context.Context, args map[string]any, targetID, callerID, ownerUserID, processID, parentTraceID string) (map[string]any, error)

// Kernel is the central service object.
// It holds all dependencies and exposes operations to both the CLI and HTTP server.
type Kernel struct {
	store          Store
	scripts        ScriptExecutor
	http           HTTPExecutor
	llm            Embedder
	cfg            Config
	log            *log.Logger
	nativeHandlers map[string]NativeFunc
	secretBox      SecretBox
	lookupHost     func(context.Context, string) ([]string, error)
	userHandles    sync.Map // user ID → handle, cached for readable logging
	// traceStripes serialize a trace's fund-spends against that trace's settlement (§9): with
	// out-of-kernel capability composition, callbacks mutate a live trace concurrently with the
	// settlement that reads its taxable available, so the two must be mutually exclusive.
	traceStripes [64]sync.Mutex
}

// traceLock returns the striped mutex guarding a trace's spend/settlement exclusivity (§9).
// Keyed by trace id: the same trace always maps to the same stripe; distinct traces rarely
// collide and, if they do, merely serialize harmlessly. Fixed size, no per-trace cleanup.
func (k *Kernel) traceLock(traceID string) *sync.Mutex {
	var h uint32 = 2166136261
	for i := 0; i < len(traceID); i++ { // FNV-1a
		h = (h ^ uint32(traceID[i])) * 16777619
	}
	return &k.traceStripes[h%uint32(len(k.traceStripes))]
}

// callerHandle returns a user's handle for logging, caching id→handle lookups.
// Returns "" on error so callers can fall back to the raw id; handles are effectively
// stable, so a never-invalidated cache only risks a cosmetic stale handle in logs.
func (k *Kernel) callerHandle(ctx context.Context, id string) string {
	if id == "" {
		return ""
	}
	if v, ok := k.userHandles.Load(id); ok {
		return v.(string)
	}
	u, err := k.store.ReadUser(ctx, id)
	if err != nil || u == nil {
		return ""
	}
	k.userHandles.Store(id, u.Handle)
	return u.Handle
}

// ActionRef resolves an action id to its "@owner/name" reference, falling back to the bare name
// or the id when the action or its owner handle can't be resolved.
func (k *Kernel) ActionRef(ctx context.Context, actionID string) string {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil || a == nil {
		return actionID
	}
	return k.actionRefOf(ctx, a)
}

// ProcessOwnerID returns a process's owner user id, unauthorized — a display resolver like
// ActionRef; "" if the process is unknown. The caller resolves the handle.
func (k *Kernel) ProcessOwnerID(ctx context.Context, processID string) string {
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil || p == nil {
		return ""
	}
	return p.OwnerUserID
}

// actionRefOf builds "@owner/name" for an already-read action (no redundant read); ActionRef and
// callers that already hold the action share it.
func (k *Kernel) actionRefOf(ctx context.Context, a *Action) string {
	if h := k.callerHandle(ctx, a.OwnerUserID); h != "" {
		return h + "/" + a.Name
	}
	return a.Name
}

// SetSecretBox installs the credential encryption adapter. Must be called before any
// CreateAction/UpdateAction calls that include an Auth payload.
func (k *Kernel) SetSecretBox(box SecretBox) { k.secretBox = box }

// New constructs a Kernel. scripts, http, and llm may be nil if those features are unused.
func New(store Store, scripts ScriptExecutor, http HTTPExecutor, llm Embedder, cfg Config, logger *log.Logger) *Kernel {
	if logger == nil {
		logger = log.Default()
	}
	return &Kernel{
		store:          store,
		scripts:        scripts,
		http:           http,
		llm:            llm,
		cfg:            cfg,
		log:            logger,
		nativeHandlers: make(map[string]NativeFunc),
		lookupHost:     net.DefaultResolver.LookupHost,
	}
}

// SetLookupHost overrides the DNS resolver used by validateHTTPSource. For tests only.
func (k *Kernel) SetLookupHost(fn func(context.Context, string) ([]string, error)) {
	k.lookupHost = fn
}

// RegisterNativeHandler registers a native action handler by action name.
// Call from bootstrap to wire each native action without touching call.go.
func (k *Kernel) RegisterNativeHandler(name string, fn NativeFunc) {
	k.nativeHandlers[name] = fn
}

// PruneOrphanedNativeActions soft-deletes every kind=native action whose handler is no longer
// registered in this build — self-healing after a native is dropped from the platform stdlib, so a
// removed native does not linger as a listed-but-uncallable row. Run at startup after all native
// handlers are registered. Soft delete disables discovery and preserves history (§7); it removes the
// row from every listing, lookup, and callability. Returns the pruned action names.
func (k *Kernel) PruneOrphanedNativeActions(ctx context.Context) ([]string, error) {
	natives, err := k.store.ListNativeActions(ctx)
	if err != nil {
		return nil, err
	}
	var pruned []string
	for _, a := range natives {
		if _, ok := k.nativeHandlers[a.Name]; ok {
			continue
		}
		if err := k.store.DeleteAction(ctx, a.ID); err != nil {
			return pruned, err
		}
		k.log.With(ctx).Info("native.pruned", "action_id", a.ID, "name", a.Name)
		pruned = append(pruned, a.Name)
	}
	return pruned, nil
}

// sealAuthJSON encrypts auth with the configured SecretBox into a.AuthJSON. Fails closed with no
// box: credentials are encrypted at rest (§8), never stored as plaintext.
func (k *Kernel) sealAuthJSON(a *Action, auth *AuthInput) error {
	if k.secretBox == nil {
		return ErrInvalidState.Wrap("credential encryption is not configured; cannot store upstream auth")
	}
	b, err := json.Marshal(auth)
	if err != nil {
		return ErrInvalidInput.Wrapf("marshal auth: %v", err)
	}
	ciphertext, err := k.secretBox.Seal(a.ID, string(b))
	if err != nil {
		return ErrInternal.Wrapf("seal auth: %v", err)
	}
	a.AuthJSON = ciphertext
	return nil
}

// openAuthInput decrypts and parses an action's stored auth payload. Returns (nil, nil) when the
// action carries no auth or no credential box is configured — callers treat that as "no auth".
func (k *Kernel) openAuthInput(a *Action) (*AuthInput, error) {
	if a.AuthJSON == "" || k.secretBox == nil {
		return nil, nil
	}
	plaintext, err := k.secretBox.Open(a.ID, a.AuthJSON)
	if err != nil {
		return nil, ErrInvalidState.Wrap("upstream auth credentials could not be decrypted")
	}
	var auth AuthInput
	if err := json.Unmarshal([]byte(plaintext), &auth); err != nil {
		return nil, ErrInvalidState.Wrap("upstream auth credentials could not be parsed")
	}
	return &auth, nil
}

// authConfigString reads a string field from an AuthInput's Config (or Secrets) map.
func authField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// authSchemeSpec lists, per upstream auth scheme (§8), the required config and secret keys and the
// config keys that must be safe URLs. A scheme absent from the table is rejected as unknown.
var authSchemeSpec = map[string]struct{ config, secrets, urls []string }{
	AuthSchemeHeader:           {config: []string{"name"}, secrets: []string{"value"}},
	AuthSchemeQuery:            {config: []string{"name"}, secrets: []string{"value"}},
	AuthSchemeBearer:           {secrets: []string{"token"}},
	AuthSchemeBasic:            {secrets: []string{"username", "password"}},
	AuthSchemeOAuthClientCreds: {config: []string{"token_url", "client_id"}, secrets: []string{"client_secret"}, urls: []string{"token_url"}},
	AuthSchemeOAuthJWTBearer:   {config: []string{"token_url", "client_id"}, secrets: []string{"private_key"}, urls: []string{"token_url"}},
	AuthSchemeOAuthDelegated:   {config: []string{"auth_url", "token_url", "client_id"}, urls: []string{"auth_url", "token_url", "device_auth_url"}},
	AuthSchemeDelegatedBearer:  {}, // optional config {header, template}; no owner-held secret — the per-caller token is a Grant
}

// isDelegatedScheme reports whether a scheme binds its per-caller credential to a Grant row (§8):
// the token is not in auth_json but supplied per caller and applied under the binding rule.
func isDelegatedScheme(scheme string) bool {
	return scheme == AuthSchemeOAuthDelegated || scheme == AuthSchemeDelegatedBearer
}

// validateAuthInput rejects an upstream auth payload with an unknown scheme or missing required
// config/secret keys at action create/update — so a malformed OAuth config never reaches dispatch
// and every URL it names is SSRF-checked up front (§8). Fails closed: unknown scheme is an error.
func (k *Kernel) validateAuthInput(ctx context.Context, auth *AuthInput) error {
	if auth == nil {
		return nil
	}
	spec, ok := authSchemeSpec[auth.Scheme]
	if !ok {
		return ErrInvalidInput.Wrapf("unknown upstream auth scheme %q", auth.Scheme)
	}
	for _, m := range []struct {
		fields []string
		vals   map[string]any
	}{{spec.config, auth.Config}, {spec.secrets, auth.Secrets}} {
		for _, key := range m.fields {
			if authField(m.vals, key) == "" {
				return ErrInvalidInput.Wrapf("auth scheme %q requires %q", auth.Scheme, key)
			}
		}
	}
	for _, key := range spec.urls {
		raw := authField(auth.Config, key)
		if raw == "" {
			continue
		}
		if err := k.validateHTTPSource(ctx, raw, k.cfg.AllowLocalSources); err != nil {
			return ErrInvalidInput.Wrapf("auth %q: %v", key, err)
		}
	}
	// delegated_bearer holds no owner-side secret (the token is per-caller, in a Grant); reject one
	// so it is not silently used for every caller. Its optional value template must name the token.
	if auth.Scheme == AuthSchemeDelegatedBearer {
		if len(auth.Secrets) != 0 {
			return ErrInvalidInput.Wrap(`auth scheme "delegated_bearer" takes no secrets; the per-caller token is supplied via grant consent`)
		}
		if tmpl := authField(auth.Config, "template"); tmpl != "" && !strings.Contains(tmpl, "{token}") {
			return ErrInvalidInput.Wrap(`auth scheme "delegated_bearer" template must contain the {token} placeholder`)
		}
	}
	return nil
}

// isDelegatedAuth reports whether an action uses a delegated (per-caller Grant) scheme, so
// federation can exclude it from manifests: a remote peer's single proxy user can never complete
// a browser consent nor hold a per-caller token, so importing one could only fail (§8/§13).
func (k *Kernel) isDelegatedAuth(a *Action) bool {
	if a.Kind != KindHTTP || a.AuthJSON == "" {
		return false
	}
	auth, err := k.openAuthInput(a)
	return err == nil && auth != nil && isDelegatedScheme(auth.Scheme)
}

// ActionAuthInfo reports an action's non-secret auth summary for read paths (§8): the scheme name
// and whether calling it requires a per-caller grant. It never returns config or secrets — only the
// scheme string, which is not a secret (R9 governs secrets, not the scheme). Returns ("", false)
// when the action carries no upstream auth or the credential box is unconfigured or undecryptable.
func (k *Kernel) ActionAuthInfo(a *Action) (scheme string, requiresGrant bool) {
	auth, err := k.openAuthInput(a)
	if err != nil || auth == nil {
		return "", false
	}
	return auth.Scheme, isDelegatedScheme(auth.Scheme)
}

// readDelegatedAction reads an action and its decrypted auth, requiring a delegated (per-caller
// Grant) scheme (§8). Shared by the grant operations; the OAuth-only consent-config lookup narrows
// further to oauth_delegated itself.
func (k *Kernel) readDelegatedAction(ctx context.Context, actionID string) (*Action, *AuthInput, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, nil, err
	}
	auth, err := k.openAuthInput(a)
	if err != nil {
		return nil, nil, err
	}
	if auth == nil || !isDelegatedScheme(auth.Scheme) {
		return nil, nil, ErrInvalidInput.Wrap("action does not use a delegated auth scheme")
	}
	return a, auth, nil
}

// AttachBearerGrant stores a caller-supplied static token for one delegated_bearer action (§8): the
// direct, non-OAuth consent path. It requires the action to use delegated_bearer (an oauth_delegated
// action must use the browser flow), then routes through CreateGrant, which homes the token on the
// action's Connection and mints the per-action consent. The raw token never appears in any read path.
func (k *Kernel) AttachBearerGrant(ctx context.Context, callerID, actionID, token string) (*Grant, error) {
	_, auth, err := k.readDelegatedAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if auth.Scheme != AuthSchemeDelegatedBearer {
		return nil, ErrInvalidInput.Wrap("action does not use delegated_bearer; connect via the consent flow instead")
	}
	return k.CreateGrant(ctx, callerID, actionID, token)
}

// CreateGrant records a user's delegated consent for one action (§8): the token — an OAuth refresh
// token (oauth_delegated) or a static bearer/API-key token (delegated_bearer) — is homed on the
// action's Connection (shared per upstream account) and the grant points at it. Single-action twin
// of CreateGrants.
func (k *Kernel) CreateGrant(ctx context.Context, callerID, actionID, token string) (*Grant, error) {
	if token == "" {
		return nil, ErrInvalidInput.Wrap("token is required")
	}
	a, auth, err := k.readDelegatedAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	pk, err := connectionKey(a, auth)
	if err != nil {
		return nil, err
	}
	grants, err := k.CreateGrants(ctx, callerID, pk, []string{actionID}, token, actionScopesJSON(auth))
	if err != nil {
		return nil, err
	}
	return grants[0], nil
}

// ListGrantViews returns the caller's grants as token-free views for GET /v1/me, resolving each
// action to @owner/name and surfacing the scopes its auth config requests.
func (k *Kernel) ListGrantViews(ctx context.Context, callerID string) ([]*GrantView, error) {
	grants, err := k.store.ListGrantsByUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	// Resolve each grant's backing Connection to its stable provider_key from the stored FK (§8),
	// never by recomputing connectionKey — so the key always matches its connection's read view.
	conns, err := k.store.ListConnectionsByUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	connKey := make(map[string]string, len(conns))
	for _, c := range conns {
		connKey[c.ID] = c.ProviderKey
	}
	out := make([]*GrantView, 0, len(grants))
	for _, g := range grants {
		v := &GrantView{Action: g.ActionID, ProviderKey: connKey[g.ConnectionID], CreatedAt: g.CreatedAt}
		if a, err := k.store.ReadAction(ctx, g.ActionID); err == nil && a != nil {
			v.Action = k.actionRefOf(ctx, a)
			if auth, err := k.openAuthInput(a); err == nil && auth != nil {
				v.Scopes = auth.Config["scopes"]
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// DelegatedAuthConfig returns the decrypted provider config for a delegated-OAuth action the
// caller may use, to drive the consent flow (§8). Internal consent helper, never a read path:
// the returned AuthInput may carry the client_secret and is used only server-side.
func (k *Kernel) DelegatedAuthConfig(ctx context.Context, callerID, actionID string) (*AuthInput, error) {
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	a, auth, err := k.readDelegatedAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	if auth.Scheme != AuthSchemeOAuthDelegated {
		return nil, ErrInvalidInput.Wrap("action does not use delegated OAuth; supply its token via POST /v1/grants")
	}
	if !canCall(caller, a) {
		return nil, ErrUnauthorized.Wrap("cannot grant for an action you may not call")
	}
	return auth, nil
}

// RevokeGrant deletes the caller's grant for an action (self-service; §8). The Connection it
// pointed at survives — a shared upstream account outlives any one action's consent.
func (k *Kernel) RevokeGrant(ctx context.Context, callerID, actionID string) error {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return err
	}
	if err := k.store.DeleteGrant(ctx, callerID, actionID); err != nil {
		return err
	}
	k.log.With(ctx).Info("grant.revoked", "action_id", actionID, "grantor", callerID, "status", "success")
	return nil
}

// ---- Connections, selectors, and consent plans (§8) ----

// connectionKey derives a Connection's provider_key from an action's verified facts, never from
// names (§8): the pinned base-URL host for delegated_bearer, token_url|client_id for oauth_delegated.
func connectionKey(a *Action, auth *AuthInput) (string, error) {
	if a.Kind != KindHTTP {
		return "", ErrInvalidInput.Wrap("grants apply only to http actions")
	}
	switch auth.Scheme {
	case AuthSchemeDelegatedBearer:
		host := hostOf(httpSourceBaseURL(a.Source))
		if host == "" {
			return "", ErrInvalidState.Wrap("action has no resolvable upstream host")
		}
		return "bearer:" + host, nil
	case AuthSchemeOAuthDelegated:
		tokenURL := authField(auth.Config, "token_url")
		clientID := authField(auth.Config, "client_id")
		if tokenURL == "" || clientID == "" {
			return "", ErrInvalidState.Wrap("oauth action missing token_url or client_id")
		}
		// Bind the resource server (source registrable domain) into the key, not just the token
		// issuer — otherwise a malicious action with a legitimate provider's token_url|client_id
		// (client_id is public) but an attacker-controlled source host could ride a victim's
		// existing connection and have the minted access token delivered to the attacker (§8
		// confused-deputy defense). A different destination domain is a distinct connection.
		return "oauth:" + tokenURL + "|" + clientID + "|" + sourceDomain(a), nil
	}
	return "", ErrInvalidInput.Wrap("action does not use a delegated auth scheme")
}

// sourceDomain returns the registrable domain (eTLD+1) of an action's source host — the resource
// server an oauth token is delivered to (§8). Falls back to the bare host for IPs / unlisted TLDs.
func sourceDomain(a *Action) string {
	host := hostOf(httpSourceBaseURL(a.Source))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if d, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return d
	}
	return host
}

// hostOf returns the host[:port] of a URL, falling back to a scheme-less host string.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	raw = strings.TrimPrefix(raw, "//")
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return raw
}

// ProviderLabel renders a provider_key for display (§14): the bare host for bearer, the token
// endpoint's host for oauth. Used by the kernel plan/views and the service/CLI layer.
func ProviderLabel(providerKey string) string {
	if rest, ok := strings.CutPrefix(providerKey, "bearer:"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(providerKey, "oauth:"); ok {
		if i := strings.LastIndex(rest, "|"); i >= 0 {
			return hostOf(rest[:i])
		}
		return hostOf(rest)
	}
	return providerKey
}

// splitScopes splits an OAuth scope string on whitespace and commas.
func splitScopes(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ','
	})
}

// parseScopeJSON reads a stored scopes_json (a JSON array), tolerating a legacy space-separated form.
func parseScopeJSON(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var arr []string
	if json.Unmarshal([]byte(s), &arr) == nil {
		return arr
	}
	return splitScopes(s)
}

// actionScopesJSON returns an action's requested scopes as a JSON array string.
func actionScopesJSON(auth *AuthInput) string {
	sc := splitScopes(authField(auth.Config, "scopes"))
	if len(sc) == 0 {
		return ""
	}
	b, _ := json.Marshal(sc)
	return string(b)
}

// unionScopes merges requested scopes into an existing stored set, returning the widened JSON and
// whether the requested scopes were already covered (requested ⊆ existing).
func unionScopes(existingJSON string, requested []string) (string, bool) {
	set := map[string]bool{}
	for _, s := range parseScopeJSON(existingJSON) {
		set[s] = true
	}
	covered := true
	for _, s := range requested {
		if !set[s] {
			covered = false
			set[s] = true
		}
	}
	if len(set) == 0 {
		return "", covered
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	b, _ := json.Marshal(out)
	return string(b), covered
}

// ParseGrantSelector splits a consent selector into bare owner handle and path (§8). A selector is
// owner or owner/path (a legacy leading "@" is tolerated); a trailing "/*" aliases the whole-owner form.
func ParseGrantSelector(sel string) (ownerHandle, path string, err error) {
	sel = strings.TrimPrefix(strings.TrimSpace(sel), "@") // bare; legacy "@" tolerated
	sel = strings.TrimSuffix(sel, "/*")
	if sel == "" {
		return "", "", ErrInvalidInput.Wrap("selector must be owner or owner/path")
	}
	if idx := strings.Index(sel, "/"); idx >= 0 {
		owner := sel[:idx]
		if owner == "" {
			return "", "", ErrInvalidInput.Wrap("selector must be owner or owner/path")
		}
		return owner, sel[idx+1:], nil
	}
	return sel, "", nil
}

// selectorPathMatches implements path-segment matching (§8): the empty path matches all of an
// owner's actions; otherwise an action name matches iff it equals the path or lies beneath it
// (path + "/…") — so "brief" matches "brief" and "brief/x" but never "briefing".
func selectorPathMatches(path, name string) bool {
	return path == "" || name == path || strings.HasPrefix(name, path+"/")
}

// delegatedMatch is one delegated action selected by a selector, with its provider grouping.
type delegatedMatch struct {
	action      *Action
	auth        *AuthInput
	providerKey string
	scopes      []string
}

// maxOwnerActions bounds a selector's owner-action enumeration (ListActionsByOwner needs a limit).
const maxOwnerActions = 100000

// expandSelector resolves a selector to the caller-callable delegated actions it names, grouped by
// provider (§8). It applies the CanCall gate (§4) exactly as a single grant does, and reports how
// many name-matched actions were skipped for needing no login (non-delegated) or being uncallable.
func (k *Kernel) expandSelector(ctx context.Context, callerID, sel string) (matches []delegatedMatch, skippedLoginless, skippedUncallable int, ownerID string, err error) {
	ownerHandle, path, err := ParseGrantSelector(sel)
	if err != nil {
		return nil, 0, 0, "", err
	}
	owner, err := k.store.ReadUserByHandle(ctx, NormalizeHandle(ownerHandle))
	if err != nil {
		return nil, 0, 0, "", ErrNotFound.Wrap("selector owner not found")
	}
	ownerID = owner.ID
	caller, _ := k.store.ReadUser(ctx, callerID)
	actions, err := k.store.ListActionsByOwner(ctx, owner.ID, maxOwnerActions, 0)
	if err != nil {
		return nil, 0, 0, "", err
	}
	for _, a := range actions {
		if a.Kind != KindHTTP || !a.Active || !selectorPathMatches(path, a.Name) {
			continue
		}
		auth, aerr := k.openAuthInput(a)
		if aerr != nil || auth == nil || !isDelegatedScheme(auth.Scheme) {
			skippedLoginless++
			continue
		}
		if !canCall(caller, a) {
			skippedUncallable++
			continue
		}
		pk, perr := connectionKey(a, auth)
		if perr != nil {
			skippedUncallable++
			continue
		}
		matches = append(matches, delegatedMatch{action: a, auth: auth, providerKey: pk, scopes: splitScopes(authField(auth.Config, "scopes"))})
	}
	return matches, skippedLoginless, skippedUncallable, ownerID, nil
}

// ConsentGroup is one provider account within a consent plan (§8). Destinations lists the upstream
// hosts the group's actions send the credential to — surfaced at consent so the recipient is visible
// (a delegated token goes to the action's own source host, which need not be the token issuer, §8).
type ConsentGroup struct {
	ProviderKey  string          `json:"provider_key"`
	Provider     string          `json:"provider"`
	Scheme       string          `json:"scheme"`
	Scopes       []string        `json:"scopes"`
	Destinations []string        `json:"destinations"`
	Connected    bool            `json:"connected"`
	Covered      bool            `json:"covered"`
	Actions      []ConsentAction `json:"actions"`
}

// ConsentAction is one action within a consent group, with its current grant status.
type ConsentAction struct {
	ActionID string `json:"action_id"`
	Action   string `json:"action"`
	Granted  bool   `json:"granted"`
}

// ConsentPlan is the grouped, per-provider result of expanding a selector — what user connect walks.
type ConsentPlan struct {
	Groups            []ConsentGroup `json:"groups"`
	SkippedLoginless  int            `json:"skipped_loginless"`
	SkippedUncallable int            `json:"skipped_uncallable"`
}

// ConsentPlan expands a selector and groups the caller's connectable delegated actions by provider
// account, marking each group connected/covered and each action granted (§8).
func (k *Kernel) ConsentPlan(ctx context.Context, callerID, sel string) (*ConsentPlan, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	matches, loginless, uncallable, _, err := k.expandSelector(ctx, callerID, sel)
	if err != nil {
		return nil, err
	}
	var order []string
	byPK := map[string][]delegatedMatch{}
	for _, m := range matches {
		if _, ok := byPK[m.providerKey]; !ok {
			order = append(order, m.providerKey)
		}
		byPK[m.providerKey] = append(byPK[m.providerKey], m)
	}
	plan := &ConsentPlan{SkippedLoginless: loginless, SkippedUncallable: uncallable}
	for _, pk := range order {
		ms := byPK[pk]
		scopeSet := map[string]bool{}
		for _, m := range ms {
			for _, s := range m.scopes {
				scopeSet[s] = true
			}
		}
		scopes := make([]string, 0, len(scopeSet))
		for s := range scopeSet {
			scopes = append(scopes, s)
		}
		sort.Strings(scopes)

		destSet := map[string]bool{}
		for _, m := range ms {
			destSet[hostOf(httpSourceBaseURL(m.action.Source))] = true
		}
		dests := make([]string, 0, len(destSet))
		for d := range destSet {
			dests = append(dests, d)
		}
		sort.Strings(dests)

		grp := ConsentGroup{ProviderKey: pk, Provider: ProviderLabel(pk), Scheme: ms[0].auth.Scheme, Scopes: scopes, Destinations: dests}
		if conn, cerr := k.store.ReadConnectionByUserProvider(ctx, callerID, pk); cerr == nil {
			grp.Connected = true
			if ms[0].auth.Scheme == AuthSchemeDelegatedBearer {
				grp.Covered = true
			} else {
				_, grp.Covered = unionScopes(conn.ScopesJSON, scopes)
			}
		}
		for _, m := range ms {
			granted := false
			if _, gerr := k.store.ReadGrant(ctx, callerID, m.action.ID); gerr == nil {
				granted = true
			}
			grp.Actions = append(grp.Actions, ConsentAction{ActionID: m.action.ID, Action: k.actionRefOf(ctx, m.action), Granted: granted})
		}
		plan.Groups = append(plan.Groups, grp)
	}
	return plan, nil
}

// CreateGrants homes a secret on a user's Connection for one provider account and mints one grant
// per action against it (§8). All actions are validated (delegated scheme, CanCall, matching
// provider_key) before any write. A non-empty secret is sealed under AAD callerID|connectionID and
// widens the connection's scopes; an empty secret is the instant-grant path, valid only when a
// covering connection already exists. Per-action binding at dispatch is unchanged.
func (k *Kernel) CreateGrants(ctx context.Context, callerID, providerKey string, actionIDs []string, secret, scopesJSON string) ([]*Grant, error) {
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if k.secretBox == nil {
		return nil, ErrInvalidState.Wrap("credential encryption is not configured")
	}
	if len(actionIDs) == 0 {
		return nil, ErrInvalidInput.Wrap("no actions to grant")
	}
	// Validate every action before any write.
	for _, aid := range actionIDs {
		a, auth, err := k.readDelegatedAction(ctx, aid)
		if err != nil {
			return nil, err
		}
		if !canCall(caller, a) {
			return nil, ErrUnauthorized.Wrap("cannot grant for an action you may not call")
		}
		pk, err := connectionKey(a, auth)
		if err != nil {
			return nil, err
		}
		if pk != providerKey {
			return nil, ErrInvalidInput.Wrap("action does not belong to this provider group")
		}
	}
	// Resolve or mint the connection.
	connID, createdAt, existingScopes, sealed := "", time.Now().UTC(), "", ""
	if existing, err := k.store.ReadConnectionByUserProvider(ctx, callerID, providerKey); err == nil {
		connID, createdAt, existingScopes, sealed = existing.ID, existing.CreatedAt, existing.ScopesJSON, existing.SealedSecret
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	unionJSON, covered := unionScopes(existingScopes, parseScopeJSON(scopesJSON))
	if secret != "" {
		if connID == "" {
			connID = uuid.New().String()
		}
		s, serr := k.secretBox.Seal(callerID+"|"+connID, secret)
		if serr != nil {
			return nil, ErrInternal.Wrapf("seal connection secret: %v", serr)
		}
		sealed = s
	} else {
		if connID == "" {
			return nil, ErrInvalidState.Wrap("no connection to grant against; supply a token")
		}
		if !covered {
			return nil, ErrInvalidState.Wrap("connection does not cover the requested scopes")
		}
	}
	if err := k.store.CreateOrUpdateConnection(ctx, &Connection{
		ID: connID, UserID: callerID, ProviderKey: providerKey,
		SealedSecret: sealed, ScopesJSON: unionJSON, CreatedAt: createdAt, UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	out := make([]*Grant, 0, len(actionIDs))
	for _, aid := range actionIDs {
		g := &Grant{ID: uuid.New().String(), GrantorUserID: callerID, ActionID: aid, ConnectionID: connID, CreatedAt: time.Now().UTC()}
		if err := k.store.CreateOrReplaceGrant(ctx, g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	k.log.With(ctx).Info("grant.created", "provider", providerKey, "grantor", callerID, "count", len(out), "status", "success")
	return out, nil
}

// AttachBearerGrants stores one static token across a selector's delegated_bearer group (§8): the
// batch twin of AttachBearerGrant. providerKey is optional when the selector resolves to exactly
// one bearer group.
func (k *Kernel) AttachBearerGrants(ctx context.Context, callerID, sel, providerKey, token string) ([]*Grant, error) {
	matches, _, _, _, err := k.expandSelector(ctx, callerID, sel)
	if err != nil {
		return nil, err
	}
	groups := map[string][]string{}
	for _, m := range matches {
		if m.auth.Scheme == AuthSchemeDelegatedBearer {
			groups[m.providerKey] = append(groups[m.providerKey], m.action.ID)
		}
	}
	if len(groups) == 0 {
		return nil, ErrNotFound.Wrap("no connectable delegated_bearer actions for selector")
	}
	pk := providerKey
	if pk == "" {
		if len(groups) > 1 {
			return nil, ErrInvalidInput.Wrap("selector spans multiple bearer providers; specify one")
		}
		for g := range groups {
			pk = g
		}
	}
	ids, ok := groups[pk]
	if !ok {
		return nil, ErrNotFound.Wrap("no bearer actions for that provider in the selector")
	}
	return k.CreateGrants(ctx, callerID, pk, ids, token, "")
}

// RevokeGrantsBySelector deletes the caller's grants whose action matches the selector (§8): grants
// only — connections survive. Returns the revoked action refs.
func (k *Kernel) RevokeGrantsBySelector(ctx context.Context, callerID, sel string) ([]string, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	ownerHandle, path, err := ParseGrantSelector(sel)
	if err != nil {
		return nil, err
	}
	owner, err := k.store.ReadUserByHandle(ctx, NormalizeHandle(ownerHandle))
	if err != nil {
		return nil, ErrNotFound.Wrap("selector owner not found")
	}
	grants, err := k.store.ListGrantsByUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	var revoked []string
	for _, g := range grants {
		a, aerr := k.store.ReadAction(ctx, g.ActionID)
		if aerr != nil || a == nil || a.OwnerUserID != owner.ID || !selectorPathMatches(path, a.Name) {
			continue
		}
		if derr := k.store.DeleteGrant(ctx, callerID, g.ActionID); derr == nil {
			revoked = append(revoked, k.actionRefOf(ctx, a))
		}
	}
	if len(revoked) == 0 {
		return nil, ErrNotFound.Wrap("no matching grants to revoke")
	}
	return revoked, nil
}

// RevokeConnection deletes the caller's Connection for a provider and cascades its grants (§8):
// disconnect --account. Returns the affected action refs.
func (k *Kernel) RevokeConnection(ctx context.Context, callerID, providerKey string) ([]string, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	conn, err := k.store.ReadConnectionByUserProvider(ctx, callerID, providerKey)
	if err != nil {
		return nil, err
	}
	grants, _ := k.store.ListGrantsByUser(ctx, callerID)
	var refs []string
	for _, g := range grants {
		if g.ConnectionID == conn.ID {
			refs = append(refs, k.ActionRef(ctx, g.ActionID))
		}
	}
	if err := k.store.DeleteConnectionCascade(ctx, conn.ID); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("connection.revoked", "provider", providerKey, "grantor", callerID, "count", len(refs), "status", "success")
	return refs, nil
}

// ListConnectionViews returns the caller's connections as token-free views for GET /v1/me (§8),
// each with the number of actions consented against it and whether it is currently unused.
func (k *Kernel) ListConnectionViews(ctx context.Context, callerID string) ([]*ConnectionView, error) {
	conns, err := k.store.ListConnectionsByUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	grants, err := k.store.ListGrantsByUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, g := range grants {
		if g.ConnectionID != "" {
			counts[g.ConnectionID]++
		}
	}
	out := make([]*ConnectionView, 0, len(conns))
	for _, c := range conns {
		n := counts[c.ID]
		out = append(out, &ConnectionView{Provider: ProviderLabel(c.ProviderKey), Actions: n, Unused: n == 0, ProviderKey: c.ProviderKey, CreatedAt: c.CreatedAt})
	}
	return out, nil
}

// BackfillGrantConnections is the one-time migration to the Connection model (§8): it re-homes each
// legacy per-grant token onto its derived Connection and reseals it under the new AAD. Idempotent —
// linking clears the legacy token, so a second pass finds nothing. Processing oldest-first means the
// latest grant wins the connection's secret on a provider collision. A grant whose action is gone,
// non-delegated, or whose token cannot be recovered is deleted. With no credential box configured it
// no-ops, leaving legacy rows for a later boot rather than destroying recoverable tokens.
func (k *Kernel) BackfillGrantConnections(ctx context.Context) error {
	if k.secretBox == nil {
		return nil
	}
	legacy, err := k.store.ListLegacyTokenGrants(ctx)
	if err != nil {
		return err
	}
	for _, g := range legacy {
		// A legacy grant we cannot re-home (action gone, no longer delegated, token unrecoverable,
		// or no derivable provider) is dropped — consent binds to a contract that no longer holds.
		drop := func(reason string) {
			_ = k.store.DeleteGrant(ctx, g.GrantorUserID, g.ActionID)
			k.log.With(ctx).Warn("grant.backfill.dropped", "grant_id", g.ID, "reason", reason)
		}
		a, aerr := k.store.ReadAction(ctx, g.ActionID)
		if aerr != nil || a == nil {
			drop("action_missing")
			continue
		}
		auth, autherr := k.openAuthInput(a)
		if autherr != nil || auth == nil || !isDelegatedScheme(auth.Scheme) {
			drop("not_delegated")
			continue
		}
		plain, oerr := k.secretBox.Open(g.GrantorUserID+"|"+g.ActionID, g.RefreshToken)
		if oerr != nil {
			drop("token_unrecoverable")
			continue
		}
		pk, perr := connectionKey(a, auth)
		if perr != nil {
			drop("no_provider_key")
			continue
		}
		connID, createdAt, existingScopes := uuid.New().String(), g.CreatedAt, ""
		if existing, cerr := k.store.ReadConnectionByUserProvider(ctx, g.GrantorUserID, pk); cerr == nil {
			connID, createdAt, existingScopes = existing.ID, existing.CreatedAt, existing.ScopesJSON
		}
		sealed, serr := k.secretBox.Seal(g.GrantorUserID+"|"+connID, plain)
		if serr != nil {
			return ErrInternal.Wrapf("reseal backfilled token: %v", serr)
		}
		unionJSON, _ := unionScopes(existingScopes, splitScopes(authField(auth.Config, "scopes")))
		if err := k.store.CreateOrUpdateConnection(ctx, &Connection{
			ID: connID, UserID: g.GrantorUserID, ProviderKey: pk,
			SealedSecret: sealed, ScopesJSON: unionJSON, CreatedAt: createdAt, UpdatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := k.store.LinkGrantConnection(ctx, g.ID, connID); err != nil {
			return err
		}
	}
	if len(legacy) > 0 {
		k.log.With(ctx).Info("grant.backfill.done", "processed", len(legacy))
	}
	return nil
}

// SetSigningKey stores the Ed25519 signing key and issuer user ID after bootstrap completes.
func (k *Kernel) SetSigningKey(priv ed25519.PrivateKey, issuerUserID string) {
	k.cfg.SigningKey = priv
	k.cfg.IssuerUserID = issuerUserID
	k.cfg.FeeRecipientID = issuerUserID
}

// SetTokenSecret updates the JWT HMAC secret after bootstrap completes.
func (k *Kernel) SetTokenSecret(secret string) {
	k.cfg.TokenSecret = secret
}

// ---- User operations ----

// CreateUserRequest holds validated input for user creation. RecoveryPublicKey is the account's
// own base64url Ed25519 recovery key, derived client-side from a seed phrase (§12); optional so a
// programmatic/webhook registrant may omit it, but the CLI always supplies it.
type CreateUserRequest struct {
	Handle            string
	Password          string
	RecoveryPublicKey string
}

// NormalizeHandle canonicalizes a user handle to start with "@". It trims surrounding
// whitespace and prepends "@" when missing, so "bob" and "@bob" denote the same user.
// Idempotent; leaves "" untouched (validateHandle rejects it).
func NormalizeHandle(h string) string {
	return strings.TrimPrefix(strings.TrimSpace(h), "@")
}

// validateHandle rejects empty handles, a bare "@", and handles containing /, enforcing
// the invariant that @owner/name references are unambiguous (handles ≡ hostnames, no /).
// Callers normalize with NormalizeHandle first, so a valid handle is "@" followed by ≥1 char.
func validateHandle(handle string) error {
	if handle == "" {
		return ErrInvalidInput.Wrap("handle is required")
	}
	if strings.ContainsAny(handle, "@/") {
		return ErrInvalidInput.Wrap("handle must not contain @ or /")
	}
	if len(handle) > 64 {
		return ErrInvalidInput.Wrap("handle must be at most 64 characters")
	}
	// Handles must not collide with the other two account productions (§14), so one resolver
	// disambiguates by shape without a sigil.
	if looksLikeKey(handle) || looksLikeID(handle) {
		return ErrInvalidInput.Wrap("handle must not look like a public key or id")
	}
	return nil
}

// CreateUser creates a new user account and returns the user.
func (k *Kernel) CreateUser(ctx context.Context, req CreateUserRequest) (*User, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	req.Handle = NormalizeHandle(req.Handle)
	logger.Info("user.create.start", "handle", req.Handle)
	if err := validateHandle(req.Handle); err != nil {
		return nil, err
	}
	if err := validatePassword(req.Password); err != nil {
		return nil, err
	}
	if req.RecoveryPublicKey != "" {
		if _, err := decodeRemotePublicKey(req.RecoveryPublicKey); err != nil {
			return nil, ErrInvalidInput.Wrap("invalid recovery public key")
		}
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	u := &User{
		ID:                uuid.New().String(),
		Handle:            req.Handle,
		PasswordHash:      hash,
		RecoveryPublicKey: req.RecoveryPublicKey,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	if err := k.store.CreateUser(ctx, u); err != nil {
		logger.Warn("user.create.failed", "handle", req.Handle, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("user.created", "user_id", u.ID, "handle", u.Handle, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return u, nil
}

// UpdateUserRequest holds validated input for user self-service update. Description is a pointer so
// a nil value means "don't change" while a non-nil "" clears it.
type UpdateUserRequest struct {
	Description     *string // nil = don't change
	CurrentPassword string  // required when NewPassword is set
	NewPassword     string  // empty = don't change
}

// UpdateUser lets an authenticated password account update its own description and/or password.
func (k *Kernel) UpdateUser(ctx context.Context, callerID string, req UpdateUserRequest) (*User, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user.update.start", "user_id", callerID)

	u, err := k.store.ReadUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if u.PasswordHash == "" {
		return nil, ErrInvalidState.Wrap("account has no password to update")
	}
	if req.Description == nil && req.NewPassword == "" {
		return nil, ErrInvalidInput.Wrap("at least one of description or password must be provided")
	}
	if req.NewPassword != "" {
		if err := validatePassword(req.NewPassword); err != nil {
			return nil, err
		}
		if !CheckPassword(req.CurrentPassword, u.PasswordHash) {
			return nil, ErrUnauthenticated.Wrap("invalid current password")
		}
		hash, err := HashPassword(req.NewPassword)
		if err != nil {
			return nil, err
		}
		u.PasswordHash = hash
	}
	if req.Description != nil {
		u.Description = *req.Description
	}
	u.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateUser(ctx, u); err != nil {
		logger.Warn("user.update.failed", "user_id", callerID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("user.updated", "user_id", u.ID, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return u, nil
}

// ReadUser returns the user with the given ID.
func (k *Kernel) ReadUser(ctx context.Context, id string) (*User, error) {
	return k.store.ReadUser(ctx, id)
}

// ReadUserByHandle returns the user with the given handle.
func (k *Kernel) ReadUserByHandle(ctx context.Context, handle string) (*User, error) {
	return k.store.ReadUserByHandle(ctx, handle)
}

// ReadUserByPublicKey returns the user with the given base64url Ed25519 public key.
func (k *Kernel) ReadUserByPublicKey(ctx context.Context, publicKey string) (*User, error) {
	return k.store.ReadUserByPublicKey(ctx, publicKey)
}

// Login authenticates handle+password and returns a signed JWT.
func (k *Kernel) Login(ctx context.Context, handle, password string) (string, error) {
	u, err := k.authenticateLocal(ctx, handle, password)
	if err != nil {
		return "", err
	}
	tok, err := IssueToken(u.ID, k.cfg.TokenSecret, k.cfg.AuthIssuer, k.cfg.AuthAudience, k.cfg.TokenTTL)
	if err != nil {
		return "", err
	}
	k.log.With(ctx).Info("user.login", "user_id", u.ID)
	return tok, nil
}

func rejectSuspended(u *User) error {
	if u.SuspendedAt != nil {
		return ErrUnauthenticated.Wrap("account suspended")
	}
	return nil
}

// ListUsers returns all users ordered by creation time.
func (k *Kernel) ListUsers(ctx context.Context, limit, offset int) ([]*User, error) {
	return k.store.ListUsers(ctx, limit, offset)
}

// SuspendUser marks the user as suspended, preventing login.
// Only the superuser may call this.
func (k *Kernel) setSuspended(ctx context.Context, operatorID, targetID string, suspend bool) error {
	verb, pastVerb := "unsuspend", "unsuspended"
	storeOp := k.store.UnsuspendUser
	if suspend {
		verb, pastVerb = "suspend", "suspended"
		storeOp = k.store.SuspendUser
	}
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("user."+verb+".start", "target_id", targetID)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("user."+verb+".failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	if err := storeOp(ctx, targetID); err != nil {
		logger.Warn("user."+verb+".failed", "target_id", targetID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return err
	}
	logger.Info("user."+pastVerb, "target_id", targetID, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (k *Kernel) SuspendUser(ctx context.Context, operatorID, targetID string) error {
	return k.setSuspended(ctx, operatorID, targetID, true)
}

// UnsuspendUser removes the suspension from a user.
// Only the superuser may call this.
func (k *Kernel) UnsuspendUser(ctx context.Context, operatorID, targetID string) error {
	return k.setSuspended(ctx, operatorID, targetID, false)
}

// RenameUser changes a user's handle, the sole path that mutates it (self-service UpdateUser
// never touches it). Superuser-only. The superuser's own account is refused, since its handle is
// bound to config.superuser_handle; a rename onto a handle another account holds is refused too.
// Renaming a defunct account vacates its old handle for reuse (§12).
func (k *Kernel) RenameUser(ctx context.Context, operatorID, targetID, newHandle string) (*User, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	newHandle = NormalizeHandle(newHandle)
	if err := validateHandle(newHandle); err != nil {
		return nil, err
	}
	target, err := k.store.ReadUser(ctx, targetID)
	if err != nil {
		return nil, err
	}
	if k.isUserSuperuser(ctx, target) {
		return nil, ErrInvalidInput.Wrap("the superuser handle cannot be renamed")
	}
	if existing, _ := k.store.ReadUserByHandle(ctx, newHandle); existing != nil && existing.ID != target.ID {
		return nil, ErrInvalidInput.Wrapf("handle %s is already taken", newHandle)
	}
	if err := k.store.RenameUser(ctx, targetID, newHandle); err != nil {
		return nil, err
	}
	target.Handle = newHandle
	k.log.With(ctx).Info("user.renamed", "user_id", targetID, "handle", newHandle)
	return target, nil
}

// requireSuperuser returns ErrUnauthorized if operatorID is not the configured superuser.
func (k *Kernel) requireSuperuser(ctx context.Context, operatorID string) error {
	u, err := k.requireActiveUser(ctx, operatorID)
	if err != nil {
		return err
	}
	if !k.isUserSuperuser(ctx, u) {
		return ErrUnauthorized.Wrap("only the superuser may perform this operation")
	}
	return nil
}

// Deposit adds credits directly to a user's available balance and records an audit entry.
// Only the superuser may call this; the check is enforced here, not only at the CLI boundary.
// ValidateFeeRecipient returns an error if the fee configuration is inconsistent.
// fee_bps > 0 requires a non-empty fee_recipient_id that resolves to a known user.
// Call after bootstrap to reject misconfiguration before serving requests.
func (k *Kernel) ValidateFeeRecipient(ctx context.Context) error {
	if k.cfg.FeeBPS == 0 {
		return nil
	}
	if k.cfg.FeeRecipientID == "" {
		return ErrInvalidState.Wrap("fee_bps > 0 requires fee_recipient_id to be set")
	}
	if _, err := k.store.ReadUser(ctx, k.cfg.FeeRecipientID); err != nil {
		return ErrInvalidState.Wrapf("fee recipient %q not found in database", k.cfg.FeeRecipientID)
	}
	return nil
}

func (k *Kernel) Deposit(ctx context.Context, operatorID, targetUserID string, amount int64, reason, externalKey string) (*LedgerEntry, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("deposit.start", "target_user_id", targetUserID, "amount", amount)
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	if amount <= 0 {
		return nil, ErrInvalidInput.Wrap("amount must be positive")
	}
	if _, err := k.store.ReadUser(ctx, targetUserID); err != nil {
		return nil, err
	}
	e := &LedgerEntry{
		ID:             uuid.New().String(),
		OperatorUserID: operatorID,
		ToUserID:       targetUserID,
		Amount:         amount,
		Reason:         reason,
		ExternalKey:    externalKey,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateLedgerEntry(ctx, e); err != nil {
		logger.Warn("deposit.failed", "target_user_id", targetUserID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("deposit.created", "deposit_id", e.ID, "target_user_id", targetUserID, "amount", amount, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return e, nil
}

// Withdraw deducts credits from a user's available balance. Superuser only.
func (k *Kernel) Withdraw(ctx context.Context, operatorID, targetUserID string, amount int64, reason, externalKey string) (*LedgerEntry, error) {
	if err := k.requireSuperuser(ctx, operatorID); err != nil {
		return nil, err
	}
	if amount <= 0 {
		return nil, ErrInvalidInput.Wrap("amount must be positive")
	}
	if _, err := k.store.ReadUser(ctx, targetUserID); err != nil {
		return nil, err
	}
	e := &LedgerEntry{
		ID:             uuid.New().String(),
		OperatorUserID: operatorID,
		FromUserID:     targetUserID,
		Amount:         amount,
		Reason:         reason,
		ExternalKey:    externalKey,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateLedgerEntry(ctx, e); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("withdrawal.created", "withdrawal_id", e.ID, "target_user_id", targetUserID, "amount", amount)
	return e, nil
}

// Transfer moves credits from the caller's own available balance to another local
// user, recording one ledger entry (from caller, to recipient). It is user self-service
// — the self-authorized sibling of Deposit/Withdraw — not superuser supervision, and it
// never routes through Call(), so it has no composition surface. The recipient must be a
// local account (a peer/proxy user is rejected, as crediting it would corrupt the
// bilateral federation account, §13). Sufficient-funds is enforced atomically at the
// store debit, so a concurrent spend cannot overdraw.
func (k *Kernel) Transfer(ctx context.Context, callerID, recipientID string, amount int64, reason, externalKey string) (*LedgerEntry, error) {
	start := time.Now()
	logger := k.log.With(ctx)
	logger.Info("transfer.start", "recipient_user_id", recipientID, "amount", amount)
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if amount <= 0 {
		return nil, ErrInvalidInput.Wrap("amount must be positive")
	}
	if callerID == recipientID {
		return nil, ErrInvalidInput.Wrap("cannot transfer to yourself")
	}
	recipient, err := k.store.ReadUser(ctx, recipientID)
	if err != nil {
		return nil, err
	}
	if recipient.PublicKey != "" {
		return nil, ErrInvalidInput.Wrap("cannot transfer to a peer user")
	}
	if recipient.SuspendedAt != nil {
		return nil, ErrInvalidInput.Wrap("recipient is suspended")
	}
	e := &LedgerEntry{
		ID:             uuid.New().String(),
		OperatorUserID: caller.ID,
		FromUserID:     caller.ID,
		ToUserID:       recipient.ID,
		Amount:         amount,
		Reason:         reason,
		ExternalKey:    externalKey,
		CreatedAt:      time.Now().UTC(),
	}
	if err := k.store.CreateLedgerEntry(ctx, e); err != nil {
		logger.Warn("transfer.failed", "recipient_user_id", recipientID, "error", err, "duration_ms", time.Since(start).Milliseconds())
		return nil, err
	}
	logger.Info("transfer.created", "transfer_id", e.ID, "recipient_user_id", recipientID, "amount", amount, "status", "success", "duration_ms", time.Since(start).Milliseconds())
	return e, nil
}

// ListLedger returns the authenticated caller's own ledger entries (deposits,
// withdrawals, and transfers where they are the source or destination), most recent
// first, bounded by limit/offset.
func (k *Kernel) ListLedger(ctx context.Context, callerID string, limit, offset int) ([]*LedgerEntry, error) {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	return k.store.ListLedgerByUser(ctx, callerID, limit, offset)
}

// VerifyToken validates a bearer token and returns the subject user ID.
func (k *Kernel) VerifyToken(token string) (string, error) {
	return VerifyToken(token, k.cfg.TokenSecret, k.cfg.AuthIssuer, k.cfg.AuthAudience)
}

// ---- Action operations ----

// CreateActionRequest holds validated input for action creation.
type CreateActionRequest struct {
	OwnerUserID  string
	Name         string
	Kind         ActionKind
	Price        int64
	Description  string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Source       string      // for http: the upstream URL; assembled into canonical HTTPSource JSON
	Method       string      // http only: verb (default POST); GET/POST/PUT/PATCH/DELETE
	Params       []HTTPParam // http only: explicit field bindings; empty = implicit routing
	WasmArtifact string      // base64-encoded pre-compiled WASM; if set, stored as-is and used for the hash
	Auth         *AuthInput  // upstream credentials; sealed into auth_json at rest; write-only
}

// cgnatRange is RFC 6598 shared address space (100.64.0.0/10) — routable-looking but not covered by
// net.IP.IsPrivate, so a fetch could otherwise reach a carrier-internal host.
var _, cgnatRange, _ = net.ParseCIDR("100.64.0.0/10")

// UnsafeIP reports whether ip is RFC 1918 private, link-local (incl. cloud metadata 169.254.169.254),
// unspecified (0.0.0.0 / ::), or CGNAT shared space — the addresses an outbound fetch, redirect, or
// peer URL must not target unless allow_local_sources is set (SSRF discipline, §7/§9). Loopback is
// deliberately NOT unsafe: a service on this same host (a local LLM, the §9 co-located callback) is
// permitted by default; only the LAN and metadata classes stay gated. This is the single source of
// truth for that predicate; do not re-inline the checks elsewhere.
func UnsafeIP(ip net.IP) bool {
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsUnspecified() || cgnatRange.Contains(ip)
}

// UnsafeHost reports whether host (a hostname or literal IP) is empty or a literal private/
// link-local/reserved IP (see UnsafeIP; loopback is permitted). Non-IP hostnames return false —
// callers that resolve DNS must check the resolved addresses with UnsafeIP separately.
func UnsafeHost(host string) bool {
	if host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return UnsafeIP(ip)
	}
	return false
}

// ErrUnsafeSourceURL is the single SSRF rejection for a source/redirect/peer URL that targets a
// loopback, private, reserved, or link-local address (§7/§9). detail names the specific condition;
// the message always points at the allow_local_sources escape hatch (§14) so a local/dev caller
// learns the one setting that permits it. This is the single source of truth for that message; do
// not phrase the rejection elsewhere. (The scheme/host-shape errors are not escape-hatch cases.)
func ErrUnsafeSourceURL(detail string) error {
	return ErrInvalidInput.Wrapf("%s; set allow_local_sources to permit local/dev URLs", detail)
}

// validateHTTPSource rejects URLs that could be used for SSRF attacks.
// Allowed: http and https schemes with public hostnames, literal public IPs, or loopback.
// Rejected: other schemes, RFC 1918 private, link-local, and reserved addresses (unless allowLocal).
// For hostname (non-literal-IP) sources, DNS is resolved to catch SSRF via private hostnames — so a
// hostname (incl. localhost) that resolves to loopback is allowed, but one resolving to a private IP
// is not. DNS failures are allowed through; the runtime dialer re-validates at call time.
func (k *Kernel) validateHTTPSource(ctx context.Context, source string, allowLocal bool) error {
	u, err := url.Parse(source)
	if err != nil {
		return ErrInvalidInput.Wrapf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrInvalidInput.Wrap("URL scheme must be http or https")
	}
	// Bare (unbracketed) IPv6 in a URL is malformed and may be an SSRF probe.
	// Go 1.25's url.splitHostPort misparses "::1" as host=":" port="1", so we
	// must catch this before calling Hostname().
	if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
		return ErrUnsafeSourceURL("URL must not target private or reserved addresses")
	}
	host := u.Hostname()
	if host == "" {
		return ErrInvalidInput.Wrap("URL must have a host")
	}
	if !allowLocal {
		if ip := net.ParseIP(host); ip != nil {
			if UnsafeIP(ip) {
				return ErrUnsafeSourceURL("URL must not target private or reserved addresses")
			}
		} else {
			// Resolve the hostname and reject if any address is private/loopback/link-local.
			if addrs, err := k.lookupHost(ctx, host); err == nil {
				for _, a := range addrs {
					if ip := net.ParseIP(a); ip != nil && UnsafeIP(ip) {
						return ErrUnsafeSourceURL("URL must not target private or reserved addresses")
					}
				}
			}
		}
	}
	return nil
}

// httpMethods is the set of verbs a kind=http action may use (§8).
var httpMethods = map[string]bool{"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// splitHTTPURL splits a raw URL into base (scheme://host[:port]) and the path
// remainder, preserving "{}" path placeholders and any literal query verbatim
// (string split, not url re-encoding) so the result matches the base_url+path
// shape that OpenAPI import produces.
func splitHTTPURL(raw string) (base, path string, err error) {
	u, perr := url.Parse(raw)
	if perr != nil || u.Scheme == "" || u.Host == "" {
		return "", "", ErrInvalidInput.Wrap("source must be an absolute http(s) URL")
	}
	base = u.Scheme + "://" + u.Host
	rest := raw
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		path = rest[j:]
	}
	return base, path, nil
}

// validateHTTPParams checks explicit parameter bindings.
func validateHTTPParams(params []HTTPParam) error {
	for _, p := range params {
		if p.Name == "" {
			return ErrInvalidInput.Wrap("http param name is required")
		}
		if p.In != "path" && p.In != "query" && p.In != "body" {
			return ErrInvalidInput.Wrapf("http param %q: 'in' must be path, query, or body", p.Name)
		}
	}
	return nil
}

// httpSourceFromURL builds the canonical HTTPSource JSON for a manual kind=http
// action from a raw upstream URL plus optional method/params, validating the URL
// against SSRF rules and the verb against the allowed set.
func (k *Kernel) httpSourceFromURL(ctx context.Context, rawURL, method string, params []HTTPParam) (string, error) {
	base, path, err := splitHTTPURL(rawURL)
	if err != nil {
		return "", err
	}
	if err := k.validateHTTPSource(ctx, base, k.cfg.AllowLocalSources); err != nil {
		return "", err
	}
	if method == "" {
		method = "POST"
	}
	method = strings.ToUpper(method)
	if !httpMethods[method] {
		return "", ErrInvalidInput.Wrapf("unsupported HTTP method %q", method)
	}
	if err := validateHTTPParams(params); err != nil {
		return "", err
	}
	src := HTTPSource{Type: "http", BaseURL: base, Path: path, Method: method, Params: params}
	b, err := json.Marshal(src)
	if err != nil {
		return "", ErrInternal.Wrapf("marshal http source: %v", err)
	}
	return string(b), nil
}

// mergeHTTPSource applies a partial update (any of url/method/params) onto an
// action's existing HTTPSource JSON, preserving every other field — including
// OpenAPI provenance — and re-validating the base URL and verb. nil arguments
// leave the corresponding field unchanged.
func (k *Kernel) mergeHTTPSource(ctx context.Context, existing string, rawURL, method *string, params *[]HTTPParam) (string, error) {
	var s HTTPSource
	_ = json.Unmarshal([]byte(existing), &s) // tolerate empty/legacy source
	if s.Type == "" {
		s.Type = "http"
	}
	if rawURL != nil {
		base, path, err := splitHTTPURL(*rawURL)
		if err != nil {
			return "", err
		}
		s.BaseURL, s.Path = base, path
	}
	if method != nil {
		m := strings.ToUpper(*method)
		if !httpMethods[m] {
			return "", ErrInvalidInput.Wrapf("unsupported HTTP method %q", m)
		}
		s.Method = m
	}
	if params != nil {
		if err := validateHTTPParams(*params); err != nil {
			return "", err
		}
		s.Params = *params
	}
	if s.Method == "" {
		s.Method = "POST"
	}
	if err := k.validateHTTPSource(ctx, s.BaseURL, k.cfg.AllowLocalSources); err != nil {
		return "", err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", ErrInternal.Wrapf("marshal http source: %v", err)
	}
	return string(b), nil
}

// httpSourceBaseURL extracts the base URL from a stored kind=http source for
// validation. Falls back to the raw string for legacy/unstructured sources.
func httpSourceBaseURL(source string) string {
	var s HTTPSource
	if json.Unmarshal([]byte(source), &s) == nil && s.BaseURL != "" {
		return s.BaseURL
	}
	return source
}

// deriveWasmArtifact populates a.WasmArtifact and a.ArtifactHash for a wasm action from either a
// pre-compiled base64 artifact (stored verbatim, decoded for the hash) or TinyGo source text
// (compiled). Shared by CreateAction and UpdateAction so both register a precompiled artifact
// identically (§7). wasmArtifact is authoritative: a non-empty value pins the artifact, an empty
// value clears any stale one so updated source text takes effect at activation. No-op without a
// configured script executor, matching the pre-existing create behavior.
func (k *Kernel) deriveWasmArtifact(ctx context.Context, a *Action, source, wasmArtifact string) error {
	if k.scripts == nil {
		return nil
	}
	a.WasmArtifact = wasmArtifact
	wasmBytes := []byte(source)
	if wasmArtifact != "" {
		decoded, err := base64.StdEncoding.DecodeString(wasmArtifact)
		if err != nil {
			return ErrInvalidInput.Wrapf("wasm artifact invalid: %v", err)
		}
		wasmBytes = decoded
	}
	if len(wasmBytes) > 0 {
		_, hash, err := k.scripts.Compile(ctx, wasmBytes)
		if err != nil {
			return ErrInvalidInput.Wrapf("wasm compilation failed: %v", err)
		}
		a.ArtifactHash = hash
	}
	return nil
}

func (k *Kernel) CreateAction(ctx context.Context, callerID string, req CreateActionRequest) (*Action, error) {
	if err := k.requireSelf(ctx, callerID, req.OwnerUserID); err != nil {
		return nil, err
	}
	if req.Name == "" {
		return nil, ErrInvalidInput.Wrap("name is required")
	}
	if req.Kind != KindHTTP && req.Kind != KindWasm && req.Kind != KindNative {
		return nil, ErrInvalidInput.Wrapf("unknown kind %q", req.Kind)
	}
	if req.Kind == KindNative {
		return nil, ErrUnauthorized.Wrap("native actions may only be registered by the kernel")
	}
	if req.Price < 0 {
		return nil, ErrInvalidInput.Wrap("price must be non-negative")
	}
	if req.Kind == KindHTTP && req.Source != "" {
		srcJSON, err := k.httpSourceFromURL(ctx, req.Source, req.Method, req.Params)
		if err != nil {
			return nil, err
		}
		req.Source = srcJSON
	}
	if req.InputSchema != nil {
		if err := ValidateSchema(req.InputSchema); err != nil {
			return nil, err
		}
	}
	if req.OutputSchema != nil {
		if err := ValidateSchema(req.OutputSchema); err != nil {
			return nil, err
		}
	}

	now := time.Now().UTC()
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  req.OwnerUserID,
		Name:         req.Name,
		Kind:         req.Kind,
		Active:       false,
		Visibility:   VisibilityPrivate,
		Price:        req.Price,
		Description:  req.Description,
		InputSchema:  req.InputSchema,
		OutputSchema: req.OutputSchema,
		Source:       req.Source,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if req.Kind == KindWasm {
		if err := k.deriveWasmArtifact(ctx, a, req.Source, req.WasmArtifact); err != nil {
			return nil, err
		}
	}

	if req.Auth != nil {
		if err := k.validateAuthInput(ctx, req.Auth); err != nil {
			return nil, err
		}
		if err := k.sealAuthJSON(a, req.Auth); err != nil {
			return nil, err
		}
	}
	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.created", "action_id", a.ID, "name", a.Name, "status", "success")
	return a, nil
}

// RegisterNativeAction creates a native action for bootstrap use.
// Unlike CreateAction, it does not reject KindNative. Call only from bootstrap.
func (k *Kernel) RegisterNativeAction(ctx context.Context, req CreateActionRequest) (*Action, error) {
	now := time.Now().UTC()
	a := &Action{
		ID:           uuid.New().String(),
		OwnerUserID:  req.OwnerUserID,
		Name:         req.Name,
		Kind:         KindNative,
		Active:       false,
		Visibility:   VisibilityPrivate, // promoted to public by ActivateNativeAction
		Price:        req.Price,
		Description:  req.Description,
		InputSchema:  req.InputSchema,
		OutputSchema: req.OutputSchema,
		Source:       "native",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := k.store.CreateAction(ctx, a); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("action.registered_native", "action_id", a.ID, "name", a.Name)
	return a, nil
}

// validateAndInitActivation validates schema descriptions and ensures a stats row exists.
// Called by both SetActive and ActivateNativeAction to eliminate duplicated checks.
// Errors from validateSchemaDescriptions are returned as-is (ErrSchemaViolation).
func (k *Kernel) validateAndInitActivation(ctx context.Context, a *Action) error {
	if err := validateSchemaDescriptions(a.InputSchema, "input"); err != nil {
		return err
	}
	if err := validateSchemaDescriptions(a.OutputSchema, "output"); err != nil {
		return err
	}
	stats, _ := k.store.ReadStats(ctx, a.ID)
	if stats == nil {
		if err := k.store.UpsertStats(ctx, DefaultStats(a.ID)); err != nil {
			return err
		}
	}
	return nil
}

// ActivateNativeAction reconciles spec fields and activates a native action for bootstrap use.
// It overwrites price, description, inputSchema, and outputSchema so drift is corrected on every boot.
func (k *Kernel) ActivateNativeAction(ctx context.Context, actionID, description string, inputSchema, outputSchema map[string]any, price int64) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind != KindNative {
		return ErrInvalidInput.Wrap("action is not native")
	}
	a.Price = price
	a.Description = description
	a.InputSchema = inputSchema
	a.OutputSchema = outputSchema
	a.Visibility = VisibilityPublic
	if err := k.validateAndInitActivation(ctx, a); err != nil {
		return err
	}
	a.Active = true
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
		return err
	}
	k.indexForLookup(ctx, a)
	k.log.With(ctx).Info("action.native_enabled", "action_id", actionID)
	return nil
}

// ReadAction returns the action with the given ID (no authorization check).
// Used internally; external callers should use ReadActionForSubject.
func (k *Kernel) ReadAction(ctx context.Context, id string) (*Action, error) {
	return k.store.ReadAction(ctx, id)
}

// ReadActionForSubject returns an action only if the subject has read access.
// Public actions are readable by anyone; local actions by any authenticated user; private actions
// only by their owner or the superuser. This endpoint is session-authenticated, so a non-empty
// callerID is a local user (peers hold no session).
func (k *Kernel) ReadActionForSubject(ctx context.Context, callerID, actionID string) (*Action, error) {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return nil, err
	}
	switch a.Visibility {
	case VisibilityPublic:
		return a, nil
	case VisibilityLocal:
		if callerID != "" {
			return a, nil
		}
	default: // private
		if a.OwnerUserID == callerID {
			return a, nil
		}
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && k.isUserSuperuser(ctx, u) {
		return a, nil
	}
	return nil, ErrUnauthorized.Wrap("read permission denied")
}

// ReadActionByOwnerName returns an action by (ownerID, name).
func (k *Kernel) ReadActionByOwnerName(ctx context.Context, ownerID, name string) (*Action, error) {
	return k.store.ReadActionByOwnerName(ctx, ownerID, name)
}

// ReadCallableAction resolves an @owner/name reference and returns the action only if
// canCall(caller, action) is satisfied. Used by native actions (§9 composition) to discover
// composable actions without bypassing the kernel's access-control layer; the subject is the
// immediate caller, matching subcall dispatch (§4).
func (k *Kernel) ReadCallableAction(ctx context.Context, ownerHandle, actionName, callerID string) (*Action, error) {
	owner, err := k.store.ReadUserByHandle(ctx, ownerHandle)
	if err != nil {
		return nil, ErrNotFound.Wrap("action owner not found")
	}
	a, err := k.store.ReadActionByOwnerName(ctx, owner.ID, actionName)
	if err != nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	caller, _ := k.store.ReadUser(ctx, callerID)
	if !canCall(caller, a) {
		return nil, ErrUnauthorized.Wrap("action not callable by this caller")
	}
	return a, nil
}

// ListVisibleActions returns active actions visible network-wide (public) and, when includeLocal is
// set, also kernel-local ones. Manifests and gossip pass false (public only); an authenticated local
// listing passes true (§14).
func (k *Kernel) ListVisibleActions(ctx context.Context, includeLocal bool, limit, offset int) ([]*Action, error) {
	return k.store.ListVisibleActions(ctx, includeLocal, limit, offset)
}

// ListOwnedActions returns all non-deleted actions owned by ownerID, including inactive
// and private ones. Intended for authenticated owner list views.
func (k *Kernel) ListOwnedActions(ctx context.Context, ownerID string, limit, offset int) ([]*Action, error) {
	return k.store.ListActionsByOwner(ctx, ownerID, limit, offset)
}

// ListAllActions returns all actions regardless of active state.
func (k *Kernel) ListAllActions(ctx context.Context, limit, offset int) ([]*Action, error) {
	return k.store.ListAllActions(ctx, limit, offset)
}

// ListProcesses returns the caller's processes, or all of them when the caller is the
// superuser (supervision is scope on the normal endpoint, mirroring ListTransactions).
func (k *Kernel) ListProcesses(ctx context.Context, callerID string, limit, offset int) ([]*Process, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if k.isUserSuperuser(ctx, u) {
		return k.store.ListAllProcesses(ctx, limit, offset)
	}
	return k.store.ListProcesses(ctx, callerID, limit, offset)
}

// ListAllTransactions returns all transactions ordered by started_at.
func (k *Kernel) ListAllTransactions(ctx context.Context, limit, offset int) ([]*Transaction, error) {
	return k.store.ListAllTransactions(ctx, limit, offset)
}

// ListAllTransactionViews returns all transactions as TransactionViews (with embedded
// rating), so admin listings expose the same canonical shape as ListTransactions (§14).
func (k *Kernel) ListAllTransactionViews(ctx context.Context, limit, offset int) ([]*TransactionView, error) {
	txs, err := k.store.ListAllTransactions(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	views := make([]*TransactionView, len(txs))
	for i, tx := range txs {
		views[i] = k.toTransactionView(ctx, tx)
	}
	return views, nil
}

// GetConfig returns a persistent config value by key.
func (k *Kernel) GetConfig(ctx context.Context, key string) (string, error) {
	return k.store.GetConfig(ctx, key)
}

// SetConfig stores a persistent config value.
func (k *Kernel) SetConfig(ctx context.Context, key, value string) error {
	return k.store.SetConfig(ctx, key, value)
}

// FirstBoot atomically creates the @sys superuser account, generates an Ed25519 signing
// keypair, and stores all three config entries in a single SQLite transaction.
// Safe to call on a database that was already initialized — user INSERT is skipped.
// recoveryPublicKey is @sys's own recovery key (§12), enrolled from the operator's seed phrase;
// optional (empty leaves @sys unrecoverable, as before).
func (k *Kernel) FirstBoot(ctx context.Context, password, recoveryPublicKey string) error {
	if err := validatePassword(password); err != nil {
		return err
	}
	if recoveryPublicKey != "" {
		if _, err := decodeRemotePublicKey(recoveryPublicKey); err != nil {
			return ErrInvalidInput.Wrap("invalid recovery public key")
		}
	}
	hash, err := HashPassword(password)
	if err != nil {
		return ErrInvalidInput.Wrapf("could not hash password: %v", err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return ErrInternal.Wrapf("generate signing key: %v", err)
	}
	now := time.Now().UTC()
	u := &User{
		ID:                uuid.New().String(),
		Handle:            SuperuserHandle,
		PasswordHash:      hash,
		RecoveryPublicKey: recoveryPublicKey,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	jwtRaw := make([]byte, 32)
	if _, err := rand.Read(jwtRaw); err != nil {
		return ErrInternal.Wrapf("generate jwt secret: %v", err)
	}
	configs := map[string]string{
		"superuser_handle":    SuperuserHandle,
		"signing_public_key":  base64.RawURLEncoding.EncodeToString(pub),
		"signing_private_key": base64.RawURLEncoding.EncodeToString(priv),
		"jwt_secret":          hex.EncodeToString(jwtRaw),
	}
	if err := k.store.InitFirstBoot(ctx, u, configs); err != nil {
		return err
	}
	su, err := k.store.ReadUserByHandle(ctx, SuperuserHandle)
	if err != nil {
		return err
	}
	// Read persisted values rather than using in-memory generated ones.
	// InitFirstBoot uses INSERT OR IGNORE, so on a re-run the stored values may differ
	// from those generated above. Using stored values ensures the kernel always
	// matches what is in the database.
	storedPrivB64, err := k.store.GetConfig(ctx, "signing_private_key")
	if err != nil {
		return ErrInternal.Wrapf("read stored signing key: %v", err)
	}
	storedPriv, err := base64.RawURLEncoding.DecodeString(storedPrivB64)
	if err != nil {
		return ErrInternal.Wrapf("decode stored signing key: %v", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(storedPriv), su.ID)
	// Only apply the stored secret when no secret was provided at construction
	// (e.g. no JUICE_SECRET_KEY env var). If one was already configured, it takes
	// precedence and the stored value serves as the fallback for future startups.
	if k.cfg.TokenSecret == "" {
		storedJWT, err := k.store.GetConfig(ctx, "jwt_secret")
		if err != nil {
			return ErrInternal.Wrapf("read stored jwt secret: %v", err)
		}
		k.SetTokenSecret(storedJWT)
	}
	return nil
}

// UpdateActionRequest holds validated input for action updates.
type UpdateActionRequest struct {
	ID           string
	Price        *int64
	Description  *string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Source       *string           // http: new upstream URL (merged into existing HTTPSource); wasm: new TinyGo source
	WasmArtifact string            // wasm: new pre-compiled base64 artifact (symmetric with CreateActionRequest)
	Method       *string           // http: new verb (merged into existing HTTPSource)
	Params       *[]HTTPParam      // http: new explicit bindings (merged into existing HTTPSource)
	Visibility   *ActionVisibility // private | local | public (§4)
	Auth         *AuthInput        // upstream credentials; sealed into auth_json at rest; write-only
}

// UpdateAction modifies an action and deactivates it (schema/source changes require re-activation).
func (k *Kernel) UpdateAction(ctx context.Context, callerID string, req UpdateActionRequest) (*Action, error) {
	a, err := k.store.ReadAction(ctx, req.ID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound.Wrap("action not found")
	}
	if a.Kind == KindNative {
		return nil, ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
		return nil, err
	}
	wasActive := a.Active

	if req.Price != nil {
		if *req.Price < 0 {
			return nil, ErrInvalidInput.Wrap("price must be non-negative")
		}
		a.Price = *req.Price
		a.Active = false
	}
	if req.Description != nil {
		a.Description = *req.Description
	}
	if req.InputSchema != nil {
		if err := ValidateSchema(req.InputSchema); err != nil {
			return nil, err
		}
		a.InputSchema = req.InputSchema
		a.Active = false
	}
	if req.OutputSchema != nil {
		if err := ValidateSchema(req.OutputSchema); err != nil {
			return nil, err
		}
		a.OutputSchema = req.OutputSchema
		a.Active = false
	}
	if a.Kind == KindHTTP && (req.Source != nil || req.Method != nil || req.Params != nil) {
		srcJSON, err := k.mergeHTTPSource(ctx, a.Source, req.Source, req.Method, req.Params)
		if err != nil {
			return nil, err
		}
		a.Source = srcJSON
		a.Active = false
	}
	if (req.Source != nil || req.WasmArtifact != "") && a.Kind != KindHTTP {
		if req.Source != nil {
			a.Source = *req.Source
		}
		a.Active = false
		if a.Kind == KindWasm {
			if err := k.deriveWasmArtifact(ctx, a, a.Source, req.WasmArtifact); err != nil {
				return nil, err
			}
		}
	}
	if req.Visibility != nil {
		if !ValidActionVisibility(*req.Visibility) {
			return nil, ErrInvalidInput.Wrap("visibility must be private, local, or public")
		}
		a.Visibility = *req.Visibility
		if err := requireOpenAPIOwnershipIfVisible(a); err != nil {
			return nil, err
		}
	}
	if req.Auth != nil {
		if err := k.validateAuthInput(ctx, req.Auth); err != nil {
			return nil, err
		}
		if err := k.sealAuthJSON(a, req.Auth); err != nil {
			return nil, err
		}
	}
	a.UpdatedAt = time.Now().UTC()

	if err := k.store.UpdateAction(ctx, a); err != nil {
		return nil, err
	}
	// Revoke standing delegated grants when the update deactivates the action (a contract change,
	// §7) or replaces its auth: consent must not silently carry over to changed code or credentials
	// (§8). A plain enable/disable via SetActive leaves the contract intact and keeps grants.
	if (wasActive && !a.Active) || req.Auth != nil {
		if err := k.store.DeleteGrantsForAction(ctx, a.ID); err != nil {
			k.log.With(ctx).Warn("action.grant_revoke_failed", "action_id", a.ID, "error", err)
		}
	}
	if req.Description != nil {
		k.indexForLookup(ctx, a)
	}
	k.log.With(ctx).Info("action.updated", "action_id", a.ID, "status", "success")
	return a, nil
}

// SetActive activates or deactivates an action.
func (k *Kernel) SetActive(ctx context.Context, callerID, actionID string, active bool) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
		return err
	}
	if active {
		if strings.TrimSpace(a.Description) == "" {
			return ErrInvalidState.Wrap("description is required before activation")
		}
		// A pre-compiled wasm artifact (e.g. from @sys/tinygo/compile registered via
		// `action create --artifact`) is itself the executable, so it satisfies the
		// source requirement even when the TinyGo source is not stored.
		if a.Source == "" && a.Kind != KindNative && !(a.Kind == KindWasm && a.WasmArtifact != "") {
			return ErrInvalidState.Wrap("cannot activate action with no source")
		}
		if err := ValidateSchema(a.InputSchema); err != nil {
			return ErrInvalidState.Wrapf("invalid input schema: %v", err)
		}
		if err := ValidateSchema(a.OutputSchema); err != nil {
			return ErrInvalidState.Wrapf("invalid output schema: %v", err)
		}
		if err := k.validateAndInitActivation(ctx, a); err != nil {
			return err
		}
		if a.Kind == KindHTTP {
			if err := k.validateHTTPSource(ctx, httpSourceBaseURL(a.Source), k.cfg.AllowLocalSources); err != nil {
				return err
			}
			// Fail closed: don't activate an action with stored credentials when no box is
			// configured — they'd be unreadable (or legacy plaintext) at dispatch (§8).
			if a.AuthJSON != "" && k.secretBox == nil {
				return ErrInvalidState.Wrap("cannot activate action with upstream auth: credential encryption is not configured")
			}
		}
		if a.Kind == KindWasm {
			if k.scripts == nil {
				return ErrInvalidState.Wrap("cannot activate wasm action: script executor not configured")
			}
			wasmBytes := []byte(a.Source)
			if a.WasmArtifact != "" {
				// Artifact pre-stored (e.g. via action create --artifact); compile it for the hash, not the TinyGo source.
				decoded, decErr := base64.StdEncoding.DecodeString(a.WasmArtifact)
				if decErr != nil {
					return ErrInvalidState.Wrapf("wasm artifact decode failed: %v", decErr)
				}
				wasmBytes = decoded
			}
			_, hash, err := k.scripts.Compile(ctx, wasmBytes)
			if err != nil {
				return ErrInvalidState.Wrapf("wasm compile failed: %v", err)
			}
			a.ArtifactHash = hash
		}
		if err := requireOpenAPIOwnershipIfVisible(a); err != nil {
			return err
		}
	}
	a.Active = active
	a.UpdatedAt = time.Now().UTC()
	if err := k.store.UpdateAction(ctx, a); err != nil {
		return err
	}
	if active {
		k.indexForLookup(ctx, a)
	}
	event := "action.disabled"
	if active {
		event = "action.enabled"
	}
	k.log.With(ctx).Info(event, "action_id", actionID, "status", "success")
	return nil
}

// DeleteAction removes an action (marks deleted; keeps transaction history).
func (k *Kernel) DeleteAction(ctx context.Context, callerID, actionID string) error {
	a, err := k.store.ReadAction(ctx, actionID)
	if err != nil {
		return err
	}
	if a.Kind == KindNative {
		return ErrUnauthorized.Wrap("native actions are managed by bootstrap")
	}
	if err := k.requireAdmin(ctx, callerID, a); err != nil {
		return err
	}
	if err := k.store.DeleteAction(ctx, actionID); err != nil {
		return err
	}
	// A deleted action can never be called again; drop any delegated grants pointing at it (§8).
	if err := k.store.DeleteGrantsForAction(ctx, actionID); err != nil {
		k.log.With(ctx).Warn("action.grant_revoke_failed", "action_id", actionID, "error", err)
	}
	k.log.With(ctx).Info("action.deleted", "action_id", actionID, "status", "success")
	return nil
}

// ---- Process / Run operations ----

// beginRun consolidates all preconditions for a new process, atomically creates the process
// and root trace via BeginRun, then executes the root call. Shared by Run and RunFederated.
func (k *Kernel) beginRun(ctx context.Context, caller *User, targetUserID, actionName string, args map[string]any, idempotencyRecordID string) (*CallReply, error) {
	action, err := k.store.ReadActionByOwnerName(ctx, targetUserID, actionName)
	if err != nil || action == nil {
		return nil, ErrNotFound.Wrapf("action %s/%s not found", targetUserID, actionName)
	}
	// Pre-funding validity gate: Call re-runs checkCallPreconditions authoritatively, but a
	// rejection must not leave a funded process behind (a rejected call creates no transaction,
	// §6), so the same check runs here before BeginRun parks funds.
	// Root/federated runs have C = P (the caller owns the process), so one identity feeds both the
	// caller-scoped visibility check and the process-owner-scoped grant check.
	if err := k.checkCallPreconditions(ctx, caller, caller.ID, action, args, true); err != nil {
		return nil, err
	}
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}
	// Bilateral credit (§13): a peer caller may draw its Available negative down to -CreditMax; an
	// ordinary caller has CreditMax=0, so this is the classic Available>=Price check. The atomic
	// guard in store.BeginRun enforces the same bound against concurrent draws.
	if caller.Available-action.Price < -caller.CreditMax {
		return nil, ErrInsufficientFunds.Wrapf("user has %d credits, action costs %d", caller.Available, action.Price)
	}
	now := time.Now().UTC()
	p := &Process{
		ID:          uuid.New().String(),
		OwnerUserID: caller.ID,
		Status:      ProcessOpen,
		CreatedAt:   now,
	}
	t := &Trace{
		ID:            uuid.New().String(),
		ProcessID:     p.ID,
		ActionOwnerID: action.OwnerUserID,
		ActionID:      action.ID,
		CallerUserID:  caller.ID,
		CreatedAt:     now,
	}
	// Persist the inbound cross-kernel record on the trace, for every action kind: whichever
	// settlement resolves this call — commit, retry, max-age, forced closure, crash recovery —
	// then completes it, so a peer is never left waiting on a record nothing will finish (§13).
	if idempotencyRecordID != "" {
		t.IdempotencyRecordID = &idempotencyRecordID
	}
	if action.Kind == KindRemoteProxy {
		key := uuid.New().String()
		t.IdempotencyKey = &key
		t.DispatchJSON = marshalDispatch(args, "", k.remoteManifestPrice(action.Price))
	}
	if err := k.store.BeginRun(ctx, p, t, caller.ID, action.Price); err != nil {
		return nil, err
	}
	k.log.With(ctx).Info("process.created", "process_id", p.ID, "owner", caller.ID, "price", action.Price)
	// Pass the validated Action snapshot and the funded root trace into Call: binds execution to
	// the row just funded (no TOCTOU window). Call re-validates the snapshot.
	return k.Call(ctx, CallRequest{
		CallerID:            caller.ID,
		Action:              action,
		Args:                args,
		ExistingTraceID:     t.ID,
		IdempotencyRecordID: idempotencyRecordID,
	})
}

// Run atomically creates a process funded with action.Price, then executes the root call.
func (k *Kernel) Run(ctx context.Context, callerID, actionRef string, args map[string]any) (*CallReply, error) {
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	action, err := k.ResolveAction(ctx, actionRef)
	if err != nil {
		return nil, err
	}
	return k.beginRun(ctx, caller, action.OwnerUserID, action.Name, args, "")
}

// RunFederated is like Run but accepts an idempotencyRecordID for federation calls.
// Used by the federation handler to atomically settle the idempotency record.
func (k *Kernel) RunFederated(ctx context.Context, callerID, targetUserID, actionName string, args map[string]any, idempotencyRecordID string) (*CallReply, error) {
	caller, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	return k.beginRun(ctx, caller, targetUserID, actionName, args, idempotencyRecordID)
}

// EndProcess closes a process and returns all remaining funds to the owner.
// Any unsettled in-flight traces (e.g. remote-proxy calls awaiting a receipt) are failed
// and refunded before the process is closed.
func (k *Kernel) EndProcess(ctx context.Context, callerID, processID string) error {
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return err
	}
	p, err := k.store.ReadProcess(ctx, processID)
	if err != nil {
		return err
	}
	if p.OwnerUserID != callerID {
		return ErrUnauthorized.Wrap("only the process owner may end it")
	}
	if p.Status != ProcessOpen {
		return ErrInvalidState.Wrap("process is already closed")
	}
	// Closure cancels waiting steps, settles unsettled traces, and returns funds — a
	// money transition + audit record that must commit regardless of caller cancellation
	// (§5). Detach from execution-scoped cancellation from here on.
	sctx, cancel := settlementContext(ctx)
	defer cancel()
	// Settle any unsettled traces (e.g. stalled remote-proxy calls holding locked funds).
	// When the last trace settles and no steps remain, closeProcessTx auto-closes the process;
	// in that case store.EndProcess is unnecessary — check before calling to avoid an error.
	logger := k.log.With(ctx)
	// Map each running step-completion trace to its step so recoverTrace fails the completion
	// call with CallerStep semantics (transaction + receipt), mirroring startup Recover.
	// A failure to enumerate aborts the close: closing a process whose traces were not all
	// settled would return funds without a complete audit record (§5).
	stepByTrace := map[string]string{}
	runs, err := k.store.ListOrphanRunningStepsForProcess(sctx, processID)
	if err != nil {
		return err
	}
	for _, r := range runs {
		stepByTrace[r.CompletionTraceID] = r.StepID
	}
	// Settle every unsettled trace deepest-first (now including running step-completion traces).
	// Children settle before parents, so a completion trace's subcalls gain a tx before it settles.
	// Any settlement failure aborts before close so a half-settled process is never closed and
	// credited — the all-or-nothing audit guarantee holds even under store errors.
	unsettled, err := k.store.ListUnsettledTracesForProcess(sctx, processID)
	if err != nil {
		return err
	}
	for _, trace := range unsettled {
		if err := k.recoverTrace(sctx, logger, trace, "process force-closed", stepByTrace[trace.ID]); err != nil {
			logger.Error("process.end.settle_failed", "trace_id", trace.ID, "error", err)
			return err
		}
	}
	// Re-read: recoverTrace may have auto-closed the process (became quiescent after settlement).
	if p2, readErr := k.store.ReadProcess(sctx, processID); readErr == nil && p2.Status == ProcessClosed {
		k.log.With(ctx).Info("process.ended", "process_id", processID, "status", "success")
		return nil
	}
	if err := k.store.EndProcess(sctx, processID); err != nil {
		return err
	}
	k.log.With(ctx).Info("process.ended", "process_id", processID, "status", "success")
	return nil
}

// ReadTrace returns a trace by ID. Used by the service layer for authority checks.
func (k *Kernel) ReadTrace(ctx context.Context, id string) (*Trace, error) {
	return k.store.ReadTrace(ctx, id)
}

// AwaitingReceiptSince maps each of the given processes that has a remote-proxy call still awaiting
// its receipt (§13) to the earliest such call's start time. It reports the awaiting-receipt state
// the process listings must surface, plus how long funds have been parked. Scoped to the passed
// processes so a listing scans only what it displays (process_id is indexed), not every trace.
// Read-only; no probing.
func (k *Kernel) AwaitingReceiptSince(ctx context.Context, processIDs []string) (map[string]time.Time, error) {
	since := make(map[string]time.Time, len(processIDs))
	for _, pid := range processIDs {
		traces, err := k.store.ListUnsettledTracesForProcess(ctx, pid)
		if err != nil {
			return nil, err
		}
		for _, tr := range traces {
			// A remote-proxy call awaiting its receipt is an unsettled trace with an idempotency
			// key (the same filter as the global ListPendingRemoteTraces); a local call still
			// executing has none and is not "awaiting a receipt".
			if tr.IdempotencyKey == nil {
				continue
			}
			if cur, ok := since[pid]; !ok || tr.CreatedAt.Before(cur) {
				since[pid] = tr.CreatedAt
			}
		}
	}
	return since, nil
}

// AuthorizeTraceUse returns nil if callerID may use traceID for step creation.
// Allowed if: caller == process.owner OR caller == Trace(trace).action_owner_id.
func (k *Kernel) AuthorizeTraceUse(ctx context.Context, callerID, traceID string) error {
	trace, err := k.store.ReadTrace(ctx, traceID)
	if err != nil {
		return ErrNotFound.Wrap("trace not found")
	}
	if trace.ActionOwnerID == callerID {
		return nil
	}
	process, err := k.store.ReadProcess(ctx, trace.ProcessID)
	if err != nil {
		return ErrNotFound.Wrap("process not found")
	}
	if process.OwnerUserID == callerID {
		return nil
	}
	return ErrUnauthorized.Wrap("caller is not authorized to use this trace")
}

// ReadProcess returns a process by ID, requiring the caller to be its owner.
func (k *Kernel) ReadProcess(ctx context.Context, callerID, id string) (*Process, error) {
	p, err := k.store.ReadProcess(ctx, id)
	if err != nil {
		return nil, err
	}
	if p.OwnerUserID != callerID && !k.IsSuperuser(ctx, callerID) {
		return nil, ErrUnauthorized.Wrap("not authorized to view this process")
	}
	return p, nil
}

// ---- Transaction operations ----

// ReadTransaction returns a transaction by ID with embedded rating, checking subject authority.
func (k *Kernel) ReadTransaction(ctx context.Context, callerID, txID string) (*TransactionView, error) {
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	if !k.canReadTransaction(ctx, callerID, tx) {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	return k.toTransactionView(ctx, tx), nil
}

// canReadTransaction reports whether callerID is a party to tx — process owner, call caller,
// or action owner — or a superuser. All checks use immutable transaction fields.
func (k *Kernel) canReadTransaction(ctx context.Context, callerID string, tx *Transaction) bool {
	if tx.OwnerUserID == callerID || tx.CallerUserID == callerID || tx.TargetUserID == callerID {
		return true
	}
	if u, err := k.store.ReadUser(ctx, callerID); err == nil && u != nil && k.isUserSuperuser(ctx, u) {
		return true
	}
	return false
}

// ListTransactions returns transactions visible to callerID, matching the filter, each with an embedded rating.
// Superusers see all transactions; ordinary callers are restricted to transactions where they are a party.
func (k *Kernel) ListTransactions(ctx context.Context, callerID string, filter TxFilter) ([]*TransactionView, error) {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return nil, err
	}
	if !k.isUserSuperuser(ctx, u) {
		filter.PartyUserID = callerID
	}
	txs, err := k.store.ListTransactions(ctx, filter)
	if err != nil {
		return nil, err
	}
	views := make([]*TransactionView, len(txs))
	for i, tx := range txs {
		views[i] = k.toTransactionView(ctx, tx)
	}
	return views, nil
}

// toTransactionView wraps a Transaction with its associated rating (if any).
func (k *Kernel) toTransactionView(ctx context.Context, tx *Transaction) *TransactionView {
	v := &TransactionView{Transaction: tx}
	if r, err := k.store.ReadRatingByTxID(ctx, tx.ID); err == nil && r != nil {
		v.Rating = &EmbeddedRating{Value: r.Rating, Note: r.Note}
	}
	return v
}

// RateTransaction submits a rating for a completed transaction.
// Only the direct buyer (the process owner who paid) may rate.
// Ratings are stored in a separate ratings table; the transaction row is never modified.
func (k *Kernel) RateTransaction(ctx context.Context, callerID, txID string, rating float64, note *string) (*Rating, error) {
	if rating != 0 && rating != 1 {
		return nil, ErrInvalidInput.Wrap("rating must be 0 or 1")
	}
	if _, err := k.requireActiveUser(ctx, callerID); err != nil {
		return nil, err
	}
	tx, err := k.store.ReadTransaction(ctx, txID)
	if err != nil {
		return nil, err
	}
	if tx == nil {
		return nil, ErrNotFound.Wrap("transaction not found")
	}
	// Only the direct buyer (process owner) may rate.
	if callerID != tx.OwnerUserID {
		return nil, ErrUnauthorized.Wrap("only the direct buyer may rate a transaction")
	}
	// Check for duplicate rating (transaction already has a rating record).
	if existing, _ := k.store.ReadRatingByTxID(ctx, txID); existing != nil {
		return nil, ErrInvalidInput.Wrap("transaction already rated")
	}
	// Look up receipt for this transaction (may be nil for old transactions).
	receipt, _ := k.store.ReadReceiptByTxID(ctx, txID)
	r := &Rating{
		ID:          uuid.New().String(),
		RatedTxID:   txID,
		RaterUserID: callerID,
		Rating:      rating,
		Note:        note,
		CreatedAt:   time.Now().UTC().Truncate(time.Second),
	}
	if receipt != nil {
		r.RatedReceiptID = &receipt.ID
	}
	sig, err := signRating(k.cfg.SigningKey, r)
	if err != nil {
		return nil, err
	}
	r.Signature = sig
	if err := k.store.CreateRatingAndUpdateStats(ctx, r, tx.ActionID, rating); err != nil {
		return nil, err
	}
	return r, nil
}

// ListRatings returns ratings for an action ordered by creation time descending.
func (k *Kernel) ListRatings(ctx context.Context, actionID string, limit, offset int) ([]*Rating, error) {
	return k.store.ListRatings(ctx, actionID, limit, offset)
}

// ---- Stats ----

// ReadStats returns statistics for an action.
func (k *Kernel) ReadStats(ctx context.Context, actionID string) (*Stats, error) {
	return k.store.ReadStats(ctx, actionID)
}

// ResetActionStats resets the statistics row for an action to zero counters.
func (k *Kernel) ResetActionStats(ctx context.Context, actionID string) error {
	return k.store.UpsertStats(ctx, &Stats{ActionID: actionID})
}

// ---- Lookup ----

// LookupRequest is a natural-language query for actions.
type LookupRequest struct {
	Query    string
	Limit    int
	CallerID string // authenticated caller
}

// LookupResult is a ranked action for a lookup query.
type LookupResult struct {
	Action      *Action
	OwnerHandle string
	Score       float32
}

// rrfK is the reciprocal-rank-fusion constant (standard default): score = Σ 1/(rrfK + rank).
const rrfK = 60

// Lookup ranks active actions the caller may call by a hybrid of lexical (BM25) and semantic
// (cosine) relevance, fused by reciprocal-rank fusion and weighted by observed quality (§9). The
// embedder is optional: with none configured (or on embed failure) ranking degrades to the lexical
// leg alone, so lookup still works on a kernel with no LLM. The formula is a tested baseline over
// replaceable storage (§9/§16); brute-force cosine is acceptable at this scale.
func (k *Kernel) Lookup(ctx context.Context, req LookupRequest) ([]*LookupResult, error) {
	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	oversample := limit * 10

	// Semantic leg: cosine over stored vectors, best first, capped at oversample. Skipped when no
	// embedder is configured; on embed failure, log and degrade rather than fail the query. A vector
	// whose dimension differs from the query's is skipped — a changed embed model can never panic
	// cosine or score across incompatible spaces.
	denseRank := map[string]int{}
	if k.llm != nil {
		if qvec, err := k.llm.Embed(ctx, req.Query); err != nil {
			k.log.With(ctx).Warn("lookup.embed_failed", "error", err.Error())
		} else if embeddings, err := k.store.ListEmbeddings(ctx); err != nil {
			return nil, err
		} else {
			type sc struct {
				id string
				s  float32
			}
			cand := make([]sc, 0, len(embeddings))
			for id, vec := range embeddings {
				if len(vec) != len(qvec) {
					continue
				}
				cand = append(cand, sc{id, cosine(qvec, vec)})
			}
			sort.Slice(cand, func(i, j int) bool { return cand[i].s > cand[j].s })
			for i, c := range cand {
				if i >= oversample {
					break
				}
				denseRank[c.id] = i
			}
		}
	}

	// Lexical leg: BM25-ranked action IDs (already active/non-deleted and capped at oversample).
	lexIDs, err := k.store.SearchActionsLexical(ctx, req.Query, oversample)
	if err != nil {
		return nil, err
	}

	// Reciprocal-rank fusion: scale-free (no normalization between cosine and BM25) and positive by
	// construction.
	fused := map[string]float64{}
	for id, rank := range denseRank {
		fused[id] += 1.0 / float64(rrfK+rank)
	}
	for rank, id := range lexIDs {
		fused[id] += 1.0 / float64(rrfK+rank)
	}

	// UNDER REVISION: the stats-based quality multiplier is temporarily removed. It multiplied each
	// action's fused relevance by a Laplace-smoothed success ratio (1+successes)/(2+uses) — with a
	// gossip prior on cold start — which is unbounded below, so a persistently-failing action could
	// sink far beneath weakly-relevant matches. Until the redesign lands (see ranking.md) the score
	// is relevance alone; fold the quality signal back in here when it does. gossipQualityPrior is
	// deliberately kept for that reinstatement.
	type scored struct {
		id    string
		score float64
	}
	ranked := make([]scored, 0, len(fused))
	for id, rel := range fused {
		ranked = append(ranked, scored{id, rel})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	// Hydrate and filter by CanCall BEFORE truncating, so a run of others' private actions cannot
	// starve the caller of results it may actually call. Visibility is caller-scoped (§4), so load
	// the caller once; a nil caller (anonymous lookup) sees public actions only.
	var caller *User
	if req.CallerID != "" {
		caller, _ = k.store.ReadUser(ctx, req.CallerID)
	}
	out := make([]*LookupResult, 0, limit)
	ownerHandles := map[string]string{}
	for _, r := range ranked {
		if len(out) >= limit {
			break
		}
		a, err := k.store.ReadAction(ctx, r.id)
		if err != nil || !canCall(caller, a) {
			continue
		}
		if _, cached := ownerHandles[a.OwnerUserID]; !cached {
			if u, err := k.store.ReadUser(ctx, a.OwnerUserID); err == nil {
				ownerHandles[a.OwnerUserID] = u.Handle
			}
		}
		out = append(out, &LookupResult{Action: a, OwnerHandle: ownerHandles[a.OwnerUserID], Score: float32(r.score)})
	}
	return out, nil
}

// gossipQualityPrior returns a quality prior in [0.5, 0.75] derived from gossip StatTags.
// It is dominated by local stats (only applied when uses==0 locally).
// Formula: 0.5 + 0.25*(clamp(gossip_rating,0,1)) using the first gossip_rating tag found.
func gossipQualityPrior(tags []*StatTag) float32 {
	for _, t := range tags {
		if t.Key == "gossip_rating" {
			var r float64
			if _, err := fmt.Sscanf(t.Value, "%f", &r); err == nil && r > 0 {
				if r > 1 {
					r = 1
				}
				return float32(0.5 + 0.25*r)
			}
		}
	}
	return 0.5
}

// indexForLookup keeps an action's lookup entries current: the lexical FTS text (always — it needs
// no LLM) and, when an embedder is configured, its description embedding. Best-effort: logs on
// failure, never fails the caller.
func (k *Kernel) indexForLookup(ctx context.Context, a *Action) {
	if err := k.store.UpsertLookupText(ctx, a.ID, lookupText(a)); err != nil {
		k.log.With(ctx).Warn("lookup.index_failed", "action_id", a.ID, "error", err.Error())
	}
	k.storeEmbedding(ctx, a.ID, a.Description)
}

// lookupText assembles an action's lexical-index text: owner handle, name, description, and the
// property names + descriptions from its input/output schemas (§3 requires those descriptions to be
// sufficient for lookup). The owner handle is included because an action's real name is @owner/name.
// Unknown schema shapes simply contribute nothing.
func lookupText(a *Action) string {
	var b strings.Builder
	if a.OwnerHandle != "" {
		b.WriteString(a.OwnerHandle)
		b.WriteByte(' ')
	}
	b.WriteString(a.Name)
	b.WriteByte(' ')
	b.WriteString(a.Description)
	schemaText(&b, a.InputSchema)
	schemaText(&b, a.OutputSchema)
	return b.String()
}

func schemaText(b *strings.Builder, schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	for name, p := range props {
		b.WriteByte(' ')
		b.WriteString(name)
		if pm, ok := p.(map[string]any); ok {
			if d, ok := pm["description"].(string); ok {
				b.WriteByte(' ')
				b.WriteString(d)
			}
		}
	}
}

// storeEmbedding embeds the description and persists the vector. Best-effort: logs on failure, never returns an error.
func (k *Kernel) storeEmbedding(ctx context.Context, actionID, description string) {
	if k.llm == nil || strings.TrimSpace(description) == "" {
		return
	}
	vec, err := k.llm.Embed(ctx, description)
	if err != nil {
		k.log.With(ctx).Warn("lookup.embed_failed", "action_id", actionID, "error", err.Error())
		return
	}
	if err := k.store.UpsertEmbedding(ctx, actionID, vec); err != nil {
		k.log.With(ctx).Warn("lookup.embed_store_failed", "action_id", actionID, "error", err.Error())
	}
}

func cosine(a, b []float32) float32 {
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

func sqrt32(x float32) float32 {
	return float32(math.Sqrt(float64(x)))
}

// ---- Helpers ----

// requireActiveUser rejects missing or suspended users.
func (k *Kernel) requireActiveUser(ctx context.Context, userID string) (*User, error) {
	u, err := k.store.ReadUser(ctx, userID)
	if err != nil {
		return nil, ErrUnauthenticated.Wrap("user not found")
	}
	if u.SuspendedAt != nil {
		return nil, ErrUnauthenticated.Wrap("account suspended")
	}
	return u, nil
}

// isUserSuperuser returns true if u is the platform superuser (@sys is fixed by the spec).
// SuperuserHandle is the bare handle of the single privileged system account (§12): signing-key
// owner, native-action owner, fee recipient, and gossip "about" source.
const SuperuserHandle = "sys"

func (k *Kernel) isUserSuperuser(_ context.Context, u *User) bool {
	return u.Handle == SuperuserHandle
}

// IsSuperuser reports whether userID is the configured superuser. Exported so the service
// layer can widen read scope for @sys (supervision is scope on the normal endpoints, §14).
func (k *Kernel) IsSuperuser(ctx context.Context, userID string) bool {
	u, err := k.store.ReadUser(ctx, userID)
	if err != nil || u == nil {
		return false
	}
	return k.isUserSuperuser(ctx, u)
}

// requireAdmin returns nil if callerID is authenticated, non-suspended, and is the owner
// of a or the platform superuser.
func (k *Kernel) requireAdmin(ctx context.Context, callerID string, a *Action) error {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return err
	}
	if a.OwnerUserID == callerID || k.isUserSuperuser(ctx, u) {
		return nil
	}
	return ErrUnauthorized.Wrap("owner or superuser required")
}

// requireSelf returns nil if callerID is authenticated, non-suspended, and equals ownerID
// or is the platform superuser.
func (k *Kernel) requireSelf(ctx context.Context, callerID, ownerID string) error {
	u, err := k.requireActiveUser(ctx, callerID)
	if err != nil {
		return err
	}
	if u.ID == ownerID || k.isUserSuperuser(ctx, u) {
		return nil
	}
	return ErrUnauthorized.Wrap("cannot act on behalf of another user")
}

// requireOpenAPIOwnershipIfVisible returns ErrUnauthorized if a is an OpenAPI action exposed beyond
// its owner (local or public) whose ownership has not been verified. This prevents exposing an
// unverified API import to any other caller.
func requireOpenAPIOwnershipIfVisible(a *Action) error {
	if a.Visibility == VisibilityPrivate {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(a.Source), "{") {
		return nil
	}
	var osrc HTTPSource
	if jsonErr := json.Unmarshal([]byte(a.Source), &osrc); jsonErr != nil || osrc.Type != "openapi" {
		return nil
	}
	if !osrc.OwnershipVerified {
		return ErrUnauthorized.Wrap("ownership not verified: add x-juice-owner to spec")
	}
	return nil
}

// ---- Stats helpers ----

// IncrementalMean updates a running mean with a new observation.
func IncrementalMean(mean float64, n int64, x float64) float64 {
	return mean + (x-mean)/float64(n+1)
}

// UpdateStats applies one completed transaction's outcome to stats.
func UpdateStats(s *Stats, tx *Transaction, latencySeconds float64) {
	s.Uses++
	s.LastUsedAt = time.Now().UTC()

	if tx.Status == TxSuccess {
		s.Successes++
	} else {
		s.Failures++
	}

	s.LatencyEstimate = IncrementalMean(s.LatencyEstimate, s.Uses-1, latencySeconds)
}

// DefaultStats returns a zeroed Stats struct for a newly activated action.
func DefaultStats(actionID string) *Stats {
	return &Stats{ActionID: actionID, LastUsedAt: time.Now().UTC()}
}

// ---- Receipts ----

// GetReceiptByID returns the receipt with the given ID.
func (k *Kernel) GetReceiptByID(ctx context.Context, id string) (*Receipt, error) {
	return k.store.ReadReceipt(ctx, id)
}

// GetIdempotencyRecord returns an unexpired idempotency record matching key + counterparty.
func (k *Kernel) GetIdempotencyRecord(ctx context.Context, key, counterpartyUserID string) (*IdempotencyRecord, error) {
	return k.store.ReadIdempotencyRecord(ctx, key, counterpartyUserID)
}

// InsertPendingIdempotencyRecord inserts a record with status="pending" before execution.
func (k *Kernel) InsertPendingIdempotencyRecord(ctx context.Context, r *IdempotencyRecord) error {
	return k.store.InsertPendingIdempotencyRecord(ctx, r)
}

// DeleteIdempotencyRecord removes a record to allow retry after execution failure.
func (k *Kernel) DeleteIdempotencyRecord(ctx context.Context, id string) error {
	return k.store.DeleteIdempotencyRecord(ctx, id)
}

// CompleteIdempotencyRecordIfPending transitions a pending idempotency record to complete.
// If the record is already complete (CommitFailedCall already ran), this is a no-op.
func (k *Kernel) CompleteIdempotencyRecordIfPending(ctx context.Context, id, resultJSON, receiptJSON string) error {
	return k.store.CompleteIdempotencyRecordIfPending(ctx, id, resultJSON, receiptJSON)
}

// ---- Receipt helpers ----

// buildReceipt constructs a signed Receipt from a committed transaction.
// charge is the amount actually drawn from the caller's funds (= gross on success,
// ≤ gross on failure, 0 on rejection). It must be pre-computed by the caller so that
// it is included in the JCS signature before the receipt is persisted.
// Returns ErrInvalidState if the kernel has not been bootstrapped (no issuer configured).
func (k *Kernel) buildReceipt(tx *Transaction, charge int64) (*Receipt, error) {
	if err := k.requireReceiptSigningReady(); err != nil {
		return nil, err
	}
	argsHash, err := jcsHashStr(string(tx.ArgsJSON))
	if err != nil {
		return nil, ErrInternal.Wrapf("hash args: %v", err)
	}
	replyHash, err := jcsHashStr(string(tx.ReplyJSON))
	if err != nil {
		return nil, ErrInternal.Wrapf("hash reply: %v", err)
	}
	r := &Receipt{
		ID:           uuid.New().String(),
		IssuerUserID: k.cfg.IssuerUserID,
		TxID:         tx.ID,
		TraceID:      tx.TraceID,
		ActionID:     tx.ActionID,
		CallerUserID: tx.CallerUserID,
		ProcessID:    tx.ProcessID,
		ArgsHash:     argsHash,
		ReplyHash:    replyHash,
		Status:       tx.Status,
		Gross:        tx.Gross,
		Net:          tx.Net,
		Fee:          tx.Fee,
		Charge:       charge,
		Reason:       tx.Reason,
		StartedAt:    tx.StartedAt,
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	sig, err := signReceipt(k.cfg.SigningKey, r)
	if err != nil {
		return nil, err
	}
	r.Signature = sig
	return r, nil
}

func (k *Kernel) requireReceiptSigningReady() error {
	if k.cfg.IssuerUserID == "" || len(k.cfg.SigningKey) != ed25519.PrivateKeySize {
		return ErrInvalidState.Wrap("kernel cannot issue signed receipts")
	}
	return nil
}

// signReceipt signs the canonical Receipt object (with Signature cleared) using JCS.
func signReceipt(key ed25519.PrivateKey, r *Receipt) (string, error) {
	cp := *r
	cp.Signature = ""
	return signJCS(key, cp)
}

// signRating signs the canonical Rating object (with Signature cleared) using JCS.
func signRating(key ed25519.PrivateKey, r *Rating) (string, error) {
	cp := *r
	cp.Signature = ""
	return signJCS(key, cp)
}

// ---- Import shared logic ----

// incomingOp describes one operation from an external source (OpenAPI or federation manifest).
type incomingOp struct {
	key   string        // unique identifier: operation_key (OpenAPI) or remote_action_id (federation)
	hash  string        // content hash for change detection
	apply func(*Action) // update mutable fields on an existing action
	new   func() *Action
}

// reconcileImport applies create/update/deactivate logic given existing actions (keyed by op key)
// and incoming operations. hashOf extracts the stored content hash from an existing action.
// resetStats controls whether changed or stale actions have their stats row zeroed:
// true for OpenAPI (contract change invalidates prior stats), false for remote (local usage stats are preserved).
// Used by both ImportOpenAPI and ImportRemoteAction.
func (k *Kernel) reconcileImport(ctx context.Context, existingByKey map[string]*Action, hashOf func(*Action) string, incoming []incomingOp, resetStats bool) (*ImportResult, error) {
	incomingKeys := make(map[string]struct{}, len(incoming))
	for _, op := range incoming {
		incomingKeys[op.key] = struct{}{}
	}

	var result ImportResult

	// Deactivate existing actions whose ops were removed from the spec.
	var stale []*Action
	for key, a := range existingByKey {
		if _, ok := incomingKeys[key]; ok {
			continue
		}
		stale = append(stale, a)
	}
	if err := k.deactivateImported(ctx, stale, resetStats); err != nil {
		return nil, err
	}
	result.Deactivated = append(result.Deactivated, stale...)

	// Process each incoming op.
	for _, op := range incoming {
		if ex, ok := existingByKey[op.key]; ok {
			if hashOf(ex) == op.hash {
				result.Unchanged = append(result.Unchanged, ex)
			} else {
				ex.Active = false
				ex.UpdatedAt = time.Now().UTC()
				op.apply(ex)
				if resetStats {
					if err := k.store.UpdateActionAndResetStats(ctx, ex); err != nil {
						return nil, err
					}
				} else {
					if err := k.store.UpdateAction(ctx, ex); err != nil {
						return nil, err
					}
				}
				result.Updated = append(result.Updated, ex)
			}
		} else {
			a := op.new()
			if err := k.store.CreateAction(ctx, a); err != nil {
				return nil, err
			}
			result.Created = append(result.Created, a)
		}
	}

	return &result, nil
}

// deactivateImported sets Active=false for each action and optionally resets its stats.
// Invariant: unimport ⇒ active=false ∧ history unchanged.
func (k *Kernel) deactivateImported(ctx context.Context, actions []*Action, resetStats bool) error {
	for _, a := range actions {
		a.Active = false
		a.UpdatedAt = time.Now().UTC()
		if resetStats {
			if err := k.store.UpdateActionAndResetStats(ctx, a); err != nil {
				return err
			}
		} else {
			if err := k.store.UpdateAction(ctx, a); err != nil {
				return err
			}
		}
	}
	return nil
}
