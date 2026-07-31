package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/daios-ai/juice/fed"
	"github.com/daios-ai/juice/kernel"
)

// ---- AES-256-GCM SecretBox ----

// aesGCMBox implements kernel.SecretBox using AES-256-GCM.
// Each Seal call writes a fresh 12-byte random nonce prepended to the ciphertext, base64url-encoded.
// aad (action ID) is used as GCM additional data so ciphertexts can't be swapped between rows.
type aesGCMBox struct{ key [32]byte }

func newAESGCMBox(key []byte) (*aesGCMBox, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credentials key must be 32 bytes, got %d", len(key))
	}
	b := &aesGCMBox{}
	copy(b.key[:], key)
	return b, nil
}

func (b *aesGCMBox) gcm() (cipher.AEAD, error) {
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (b *aesGCMBox) Seal(aad, plaintext string) (string, error) {
	gcm, err := b.gcm()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return base64.RawURLEncoding.EncodeToString(ct), nil
}

func (b *aesGCMBox) Open(aad, ciphertext string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	gcm, err := b.gcm()
	if err != nil {
		return "", err
	}
	ns := gcm.NonceSize()
	if len(raw) < ns {
		return "", fmt.Errorf("ciphertext too short")
	}
	pt, err := gcm.Open(nil, raw[:ns], raw[ns:], []byte(aad))
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(pt), nil
}

// Upstream auth application (§9) lives in the authenticator — see authenticator.go. The executor
// holds one and calls Parse/Apply/Refreshable.

func sha256HexBytes(b []byte) string {
	h := sha256.Sum256(b)
	return fmt.Sprintf("%x", h)
}

// validateResolvedIP returns an error if a DNS-resolved IP is loopback, private, or link-local.
func validateResolvedIP(ipStr string) error {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("invalid resolved IP %q", ipStr)
	}
	if kernel.UnsafeIP(ip) {
		return kernel.ErrUnsafeSourceURL("resolved address is private or loopback")
	}
	return nil
}

// validatePublicURL rejects URLs unsafe for an outbound fetch: non-http(s) schemes and, unless
// allowLocal, RFC 1918 / link-local / reserved literal hosts (loopback is allowed by default).
// Hostname (non-literal-IP) targets are re-validated against DNS by newHTTPClient's dial guard at
// call time; this catches the literal-IP cases the dialer deliberately skips ("validated at URL
// parse time").
func validatePublicURL(rawURL string, allowLocal bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return kernel.ErrInvalidInput.Wrapf("invalid URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return kernel.ErrInvalidInput.Wrap("URL scheme must be http or https")
	}
	// Bracketless IPv6 (e.g. "::1") is malformed and may be an SSRF probe.
	if strings.Count(u.Host, ":") > 1 && !strings.HasPrefix(u.Host, "[") {
		return kernel.ErrUnsafeSourceURL("unsafe URL: private or reserved address")
	}
	host := u.Hostname()
	if host == "" {
		return kernel.ErrInvalidInput.Wrap("URL must have a host")
	}
	if allowLocal {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && kernel.UnsafeIP(ip) {
		return kernel.ErrUnsafeSourceURL("unsafe URL: private/reserved host")
	}
	return nil
}

// validateRedirectHost returns an error if hostname should not be followed as a redirect.
func validateRedirectHost(hostname string, allowLocal bool) error {
	if allowLocal {
		return nil
	}
	if kernel.UnsafeHost(hostname) {
		return kernel.ErrUnsafeSourceURL("unsafe redirect target: private/loopback host")
	}
	return nil
}

// newHTTPClient returns an HTTP client with the given timeout (defaulting to 30s).
// When allowLocal is false it installs a DialContext that resolves hostnames and
// rejects connections to loopback, RFC 1918, and link-local addresses.
func newHTTPClient(timeout time.Duration, allowLocal bool) *http.Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	var transport http.RoundTripper
	if !allowLocal {
		base := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				// Literal IPs were validated at URL parse time; skip re-resolution.
				if net.ParseIP(host) != nil {
					return base.DialContext(ctx, network, addr)
				}
				resolved, err := net.DefaultResolver.LookupHost(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range resolved {
					if err := validateResolvedIP(ip); err != nil {
						return nil, err
					}
				}
				return base.DialContext(ctx, network, net.JoinHostPort(resolved[0], port))
			},
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Never carry the composition capability across a host-changing redirect (§9):
			// Go strips Authorization automatically but not our custom headers.
			if len(via) > 0 && req.URL.Hostname() != via[len(via)-1].URL.Hostname() {
				req.Header.Del(capabilityHeader)
				req.Header.Del(callbackHeader)
			}
			return validateRedirectHost(req.URL.Hostname(), allowLocal)
		},
	}
}

// doHTTP executes one HTTP request and returns (body, statusCode, error).
// Enforces a 10 MiB response size limit.
func doHTTP(ctx context.Context, method, rawURL string, headers map[string]string, body io.Reader, timeout time.Duration, allowLocal bool) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, kernel.ErrInvalidInput.Wrapf("invalid URL: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := newHTTPClient(timeout, allowLocal).Do(req)
	if err != nil {
		// Keep the detailed message and attach the typed cause (via Because) so callers can tell
		// a client-side timeout apart from a connection failure (client.go isTimeoutErr).
		return nil, 0, kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err).Because(err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, resp.StatusCode, kernel.ErrExecutionFailed.Wrap("could not read response body")
	}
	return respBody, resp.StatusCode, nil
}

// signerFunc signs a federation request with the platform key, returning (signature, timestamp).
type signerFunc = func(action, counterparty, idempotencyKey, argsHash string) (sig, ts string, err error)

// ExecuteFederation sends a cross-kernel call over the libp2p federation transport (§13),
// addressing the peer by its Ed25519 public key. The routing (peer key, action ref) that used
// to live in a URL is now explicit arguments. A missing transport, or a transport error,
// returns a zero FederationResult so the kernel keeps the call pending for retry.
func (e *httpActionExecutor) ExecuteFederation(ctx context.Context, peerPublicKey, actionRef, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {
	if e.fedTransport == nil {
		// No transport at all: the request provably cannot have been sent (§13 never-dispatched).
		return kernel.FederationResult{NotDispatched: true}, nil
	}
	return executeFederationOverTransport(ctx, e.fedTransport, e.signerFn, e.localPubKey,
		peerPublicKey, actionRef, idempotencyKey, args)
}

// federationTransport is the outbound half of the libp2p transport this executor needs; *fed.Transport
// satisfies it. Keeping it an interface lets the fake in tests stand in without a real network.
type federationTransport interface {
	Call(ctx context.Context, peerKey string, req fed.CallRequest) (fed.CallResponse, error)
	Resolve(ctx context.Context, peerKey string, req fed.ResolveRequest) (fed.ResolveResponse, error)
}

// ResolveRemoteAction / ResolveRemoteUser implement kernel.RemoteResolver over the transport's
// /juice/fed/resolve/1 protocol (§13 subscription-free calls): fetch one signed manifest, or map a
// user reference to its stable id+handle on the peer. A missing transport is ErrPeerUnreachable so
// the kernel never treats "no network" as "action absent".
func (e *httpActionExecutor) ResolveRemoteAction(ctx context.Context, peerPublicKey, owner, name string) (*kernel.ActionManifest, error) {
	if e.fedTransport == nil {
		return nil, kernel.ErrPeerUnreachable.Wrap("federation transport not running")
	}
	resp, err := e.fedTransport.Resolve(ctx, peerPublicKey, fed.ResolveRequest{Kind: "action", Owner: owner, Name: name})
	if err != nil {
		return nil, kernel.ErrPeerUnreachable.Wrap("peer unreachable")
	}
	if resp.Status != 200 {
		return nil, kernel.ErrNotFound.Wrap("remote action not found")
	}
	var m kernel.ActionManifest
	if err := json.Unmarshal(resp.Body, &m); err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("invalid remote manifest")
	}
	return &m, nil
}

func (e *httpActionExecutor) ResolveRemoteUser(ctx context.Context, peerPublicKey, ref string) (string, string, error) {
	if e.fedTransport == nil {
		return "", "", kernel.ErrPeerUnreachable.Wrap("federation transport not running")
	}
	resp, err := e.fedTransport.Resolve(ctx, peerPublicKey, fed.ResolveRequest{Kind: "user", User: ref})
	if err != nil {
		return "", "", kernel.ErrPeerUnreachable.Wrap("peer unreachable")
	}
	if resp.Status != 200 {
		return "", "", kernel.ErrNotFound.Wrap("remote user not found")
	}
	var body struct {
		UserID string `json:"user_id"`
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return "", "", kernel.ErrInvalidInput.Wrap("invalid resolve response")
	}
	return body.UserID, body.Handle, nil
}

// executeFederationOverTransport is the transport-backed kernel.FederationExecutor. It signs the
// request as this kernel and sends the exact args bytes so the receiver's args_hash matches.
func executeFederationOverTransport(ctx context.Context, tr federationTransport, signerFn signerFunc,
	localPubKey, peerPublicKey, actionRef, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {

	body, err := json.Marshal(args)
	if err != nil {
		return kernel.FederationResult{}, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	argsHash := sha256HexBytes(body)
	req := fed.CallRequest{
		Action:         actionRef,
		Counterparty:   localPubKey,
		IdempotencyKey: idempotencyKey,
		Args:           json.RawMessage(body),
	}
	if signerFn != nil {
		if sig, ts, serr := signerFn(actionRef, localPubKey, idempotencyKey, argsHash); serr == nil {
			req.Signature = sig
			req.Timestamp = ts
		}
	}
	resp, err := tr.Call(ctx, peerPublicKey, req)
	if err != nil {
		// Provably-never-sent (resolve/connect failed) → NotDispatched, so a first dispatch may
		// fail fast (§13). Any other transport error stays pending: the request may have executed
		// remotely, so only a signed receipt (or the max-age bound) may settle it.
		return kernel.FederationResult{NotDispatched: errors.Is(err, fed.ErrNotDispatched)}, nil
	}
	var envelope struct {
		Result  map[string]any  `json:"result"`
		Receipt json.RawMessage `json:"receipt"`
	}
	var receiptJSON string
	if json.Unmarshal(resp.Body, &envelope) == nil && len(envelope.Receipt) > 0 && string(envelope.Receipt) != "null" {
		receiptJSON = string(envelope.Receipt)
	}
	return kernel.FederationResult{Result: envelope.Result, ReceiptJSON: receiptJSON, HTTPStatus: resp.Status}, nil
}

// Trace-scoped composition capability headers (§9): the token and the base URL the endpoint
// calls back on. Distinct from the user Authorization header so routes disambiguate the credential.
const (
	capabilityHeader = "X-Juice-Capability"
	callbackHeader   = "X-Juice-Callback"
)

type httpActionExecutor struct {
	timeout      time.Duration
	allowLocal   bool
	callbackURL  string              // §9 base URL advertised to dispatched endpoints for callbacks; "" disables composition
	auth         *authenticator      // §9 upstream-auth adapter; nil when no credentials box
	signerFn     signerFunc          // wired after bootstrap
	fedTransport federationTransport // libp2p federation carrier; nil off the serving path
	localPubKey  string              // this kernel's base64url Ed25519 public key
}

// FetchURL retrieves the body of a URL. Implements kernel.URLFetcher for ownership proof checks.
func (e *httpActionExecutor) FetchURL(ctx context.Context, rawURL string) ([]byte, error) {
	body, status, err := doHTTP(ctx, http.MethodGet, rawURL, nil, nil, 0, e.allowLocal)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", status, rawURL)
	}
	return body, nil
}

// fetchWeb performs a read-only GET for the @sys/web native action, returning the
// HTTP status, body, Content-Type, and the final URL fetched (after scheme
// resolution and redirects). A scheme-less URL is defaulted to https (HTTPS-first,
// like a browser); an explicit http/https scheme is respected as-is and never
// downgraded. It reuses the SSRF dial guard in newHTTPClient (loopback/RFC 1918/
// link-local rejected unless allowLocal) and sets the configured User-Agent. Non-2xx
// responses are returned with their status, not raised as errors, so callers and
// crawlers can react to them. Enforces a 10 MiB cap.
func (e *httpActionExecutor) fetchWeb(ctx context.Context, rawURL, userAgent string) (int, []byte, string, string, error) {
	rawURL = defaultScheme(rawURL)
	if err := validatePublicURL(rawURL, e.allowLocal); err != nil {
		return 0, nil, "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, "", "", kernel.ErrInvalidInput.Wrapf("invalid URL: %v", err)
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent)
	}
	resp, err := newHTTPClient(e.timeout, e.allowLocal).Do(req)
	if err != nil {
		return 0, nil, "", "", kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return resp.StatusCode, nil, "", "", kernel.ErrExecutionFailed.Wrap("could not read response body")
	}
	finalURL := rawURL
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String() // reflects any redirects followed
	}
	return resp.StatusCode, body, resp.Header.Get("Content-Type"), finalURL, nil
}

// defaultScheme prepends https:// to a scheme-less URL (HTTPS-first, like a browser).
// A URL that already carries a scheme (contains "://") is returned unchanged, so an
// explicit http:// is respected and never silently upgraded. A protocol-relative
// "//host" form is upgraded to https. Whitespace is trimmed first.
func defaultScheme(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || strings.Contains(rawURL, "://") {
		return rawURL
	}
	return "https://" + strings.TrimPrefix(rawURL, "//")
}

// Execute fires a kind=http action. The source is the canonical HTTPSource JSON
// (manual and OpenAPI-imported actions share one representation); executeHTTP
// applies its method, path templating, and parameter binding uniformly.
func (e *httpActionExecutor) Execute(ctx context.Context, action *kernel.Action, args map[string]any, ownerUserID, capability string) (map[string]any, error) {
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(action.Source), &src); err != nil {
		return nil, kernel.ErrInvalidState.Wrap("http action source is not valid HTTPSource JSON")
	}
	return e.executeHTTP(ctx, action, &src, args, ownerUserID, capability)
}

func (e *httpActionExecutor) executeHTTP(ctx context.Context, action *kernel.Action, src *kernel.HTTPSource, args map[string]any, ownerUserID, capability string) (map[string]any, error) {
	path := src.Path
	queryVals := url.Values{}
	bodyArgs := map[string]any{}

	if len(src.Params) > 0 {
		for _, p := range src.Params {
			v, ok := args[p.Name]
			if !ok {
				continue
			}
			switch p.In {
			case "path":
				path = strings.ReplaceAll(path, "{"+p.Name+"}", url.PathEscape(fmt.Sprintf("%v", v)))
			case "query":
				queryVals.Set(p.Name, fmt.Sprintf("%v", v))
			case "body":
				bodyArgs[p.Name] = v
			}
		}
	} else {
		pathParams := map[string]struct{}{}
		for {
			start := strings.Index(path, "{")
			if start < 0 {
				break
			}
			end := strings.Index(path[start:], "}")
			if end < 0 {
				break
			}
			param := path[start+1 : start+end]
			pathParams[param] = struct{}{}
			val := ""
			if v, ok := args[param]; ok {
				// Escape like the explicit-param branch (:365), so an arg value cannot inject extra
				// path segments or a query/fragment on the owner's upstream host.
				val = url.PathEscape(fmt.Sprintf("%v", v))
			}
			path = path[:start] + val + path[start+end+1:]
		}
		method := strings.ToUpper(src.Method)
		for k, v := range args {
			if _, isPath := pathParams[k]; isPath {
				continue
			}
			if method == http.MethodGet {
				queryVals.Set(k, fmt.Sprintf("%v", v))
			} else {
				bodyArgs[k] = v
			}
		}
	}

	rawURL := strings.TrimRight(src.BaseURL, "/") + path
	method := strings.ToUpper(src.Method)

	if len(queryVals) > 0 {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrapf("invalid URL: %v", err)
		}
		existing := u.Query()
		for k, vs := range queryVals {
			for _, v := range vs {
				existing.Set(k, v)
			}
		}
		u.RawQuery = existing.Encode()
		rawURL = u.String()
	}

	// Send a JSON body for body-bearing verbs (POST/PUT/PATCH always carry one,
	// even when empty, matching the original bare-URL POST behavior); GET/DELETE
	// only carry one when args were explicitly bound to the body.
	var bodyBytes []byte
	contentType := ""
	if len(bodyArgs) > 0 || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		b, err := json.Marshal(bodyArgs)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
		}
		bodyBytes = b
		contentType = "application/json"
	}

	// Parse the stored auth once, then send. Refreshable (OAuth) schemes fetch a bearer token (per
	// call, from the cache/grant) and are retried exactly once on a 401 after forcing a refresh.
	var auth *kernel.AuthInput
	if action.AuthJSON != "" {
		if e.auth == nil {
			return nil, kernel.ErrInvalidState.Wrap("upstream auth credentials present but credential encryption is not configured")
		}
		var perr error
		if auth, perr = e.auth.Parse(action); perr != nil {
			return nil, perr
		}
	}
	send := func(refresh bool) ([]byte, int, error) {
		reqHeaders := map[string]string{}
		if contentType != "" {
			reqHeaders["Content-Type"] = contentType
		}
		// Trace-scoped composition capability (§9): delivered as headers, never in the payload
		// (R9). Sent only when this kernel has a callback address; a leaf endpoint ignores them.
		if capability != "" && e.callbackURL != "" {
			reqHeaders[capabilityHeader] = capability
			reqHeaders[callbackHeader] = e.callbackURL
		}
		reqURL := rawURL
		if auth != nil {
			if err := e.auth.Apply(ctx, action, ownerUserID, auth, reqHeaders, &reqURL, refresh); err != nil {
				return nil, 0, err
			}
		}
		var body io.Reader
		if bodyBytes != nil {
			body = strings.NewReader(string(bodyBytes))
		}
		return doHTTP(ctx, method, reqURL, reqHeaders, body, e.timeout, e.allowLocal)
	}

	respBody, status, err := send(false)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized && auth != nil && e.auth.Refreshable(auth.Scheme) {
		respBody, status, err = send(true)
		if err != nil {
			return nil, err
		}
	}
	if status < 200 || status >= 300 {
		return nil, kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", status, string(respBody))
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("action response is not valid JSON")
	}
	return result, nil
}

// fetchOpenAPISpec fetches and returns the raw bytes of an OpenAPI spec at specURL.
// It rejects loopback, private, and link-local addresses unless allowLocal is true.
func fetchOpenAPISpec(ctx context.Context, specURL string, allowLocal bool) ([]byte, error) {
	if err := validatePublicURL(specURL, allowLocal); err != nil {
		return nil, err
	}
	respBody, status, err := doHTTP(ctx, http.MethodGet, specURL, nil, nil, 30*time.Second, allowLocal)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, kernel.ErrExecutionFailed.Wrapf("spec server returned %d", status)
	}
	return respBody, nil
}
