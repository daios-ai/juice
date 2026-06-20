package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

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

// ---- upstream auth application ----

// applyUpstreamAuth reads the decrypted auth JSON from action and applies it to headers/URL.
// headers must be non-nil; rawURL is modified in-place for "query" scheme.
// When box is nil, auth_json is treated as plaintext (dev/test mode with no encryption).
// Returns nil when no auth is configured (empty auth_json) or auth applied successfully.
// Fails closed: if configured credentials cannot be decrypted or parsed, the action is not
// executable as registered, so it returns an error instead of proceeding unauthenticated.
func applyUpstreamAuth(action *kernel.Action, headers map[string]string, rawURL *string, box kernel.SecretBox) error {
	if action.AuthJSON == "" {
		return nil
	}
	var plaintext string
	if box != nil {
		var err error
		plaintext, err = box.Open(action.ID, action.AuthJSON)
		if err != nil {
			return kernel.ErrInvalidState.Wrap("upstream auth credentials could not be decrypted")
		}
	} else {
		plaintext = action.AuthJSON // stored as plaintext when no SecretBox configured
	}
	var auth kernel.AuthInput
	if err := json.Unmarshal([]byte(plaintext), &auth); err != nil {
		return kernel.ErrInvalidState.Wrap("upstream auth credentials could not be parsed")
	}
	switch auth.Scheme {
	case "header":
		name, _ := auth.Config["name"].(string)
		value, _ := auth.Secrets["value"].(string)
		if name != "" {
			headers[name] = value
		}
	case "query":
		name, _ := auth.Config["name"].(string)
		value, _ := auth.Secrets["value"].(string)
		if name != "" && rawURL != nil {
			u, err := url.Parse(*rawURL)
			if err == nil {
				q := u.Query()
				q.Set(name, value)
				u.RawQuery = q.Encode()
				*rawURL = u.String()
			}
		}
	case "bearer":
		token, _ := auth.Secrets["token"].(string)
		headers["Authorization"] = "Bearer " + token
	case "basic":
		user, _ := auth.Secrets["username"].(string)
		pass, _ := auth.Secrets["password"].(string)
		headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	}
	return nil
}

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
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return kernel.ErrInvalidInput.Wrap("resolved address is private or loopback — for local/dev use, set allow_local_peer_urls or allow_local_sources in juice.json")
	}
	return nil
}

// validatePublicURL rejects URLs unsafe for an outbound fetch: non-http(s) schemes
// and, unless allowLocal, localhost / loopback / RFC 1918 / link-local literal hosts.
// Hostname (non-literal-IP) targets are re-validated against DNS by newHTTPClient's
// dial guard at call time; this catches the literal-IP and localhost cases the dialer
// deliberately skips ("validated at URL parse time").
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
		return kernel.ErrInvalidInput.Wrap("unsafe URL: private or reserved address")
	}
	if allowLocal {
		return nil
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") || host == "" {
		return kernel.ErrInvalidInput.Wrap("unsafe URL: localhost not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return kernel.ErrInvalidInput.Wrap("unsafe URL: private/loopback host")
		}
	}
	return nil
}

// validateRedirectHost returns an error if hostname should not be followed as a redirect.
func validateRedirectHost(hostname string, allowLocal bool) error {
	if allowLocal {
		return nil
	}
	if strings.EqualFold(hostname, "localhost") || hostname == "" {
		return kernel.ErrInvalidInput.Wrap("unsafe redirect target")
	}
	if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			return kernel.ErrInvalidInput.Wrap("unsafe redirect target: private/loopback host")
		}
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
		return nil, 0, kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, resp.StatusCode, kernel.ErrExecutionFailed.Wrap("could not read response body")
	}
	return respBody, resp.StatusCode, nil
}

// ExecuteFederation calls a remote kernel's federation endpoint with an idempotency key.
// Parses the {"result": {...}, "receipt": <receipt-object>} envelope at any HTTP status.
// A non-200 response with a valid receipt envelope is returned as a FederationResult so
// the kernel can settle the remote call locally (failure with charge from receipt.gross).
// Transport errors or responses without a parseable receipt return a zero FederationResult,
// causing the kernel to treat the call as pending for retry.
func (e *httpActionExecutor) ExecuteFederation(ctx context.Context, source, idempotencyKey string, args map[string]any) (kernel.FederationResult, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return kernel.FederationResult{}, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	argsHash := sha256HexBytes(body)
	headers := map[string]string{"Content-Type": "application/json"}
	if idempotencyKey != "" {
		headers["X-Idempotency-Key"] = idempotencyKey
	}
	if e.signerFn != nil {
		var actionParam, counterparty string
		if u, err := url.Parse(source); err == nil {
			actionParam = u.Query().Get("action")
			counterparty = u.Query().Get("counterparty")
		}
		if sig, ts, err := e.signerFn(actionParam, counterparty, idempotencyKey, argsHash); err == nil {
			headers["X-Timestamp"] = ts
			headers["X-Signature"] = sig
		}
	}
	respBody, status, err := doHTTP(ctx, http.MethodPost, source, headers, strings.NewReader(string(body)), e.timeout, e.allowLocal)
	if err != nil {
		// Transport error: no receipt → caller treats as pending.
		return kernel.FederationResult{HTTPStatus: 0}, nil
	}
	// Parse envelope at any status. Rejection/failure receipts arrive on non-200.
	var envelope struct {
		Result  map[string]any  `json:"result"`
		Receipt json.RawMessage `json:"receipt"`
	}
	var receiptJSON string
	if json.Unmarshal(respBody, &envelope) == nil && len(envelope.Receipt) > 0 && string(envelope.Receipt) != "null" {
		receiptJSON = string(envelope.Receipt)
	}
	return kernel.FederationResult{
		Result:      envelope.Result,
		ReceiptJSON: receiptJSON,
		HTTPStatus:  status,
	}, nil
}

type httpActionExecutor struct {
	timeout    time.Duration
	allowLocal bool
	secretBox  kernel.SecretBox
	signerFn   func(action, counterparty, idempotencyKey, argsHash string) (sig, ts string, err error) // wired after bootstrap
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
func (e *httpActionExecutor) Execute(ctx context.Context, action *kernel.Action, args map[string]any) (map[string]any, error) {
	var src kernel.HTTPSource
	if err := json.Unmarshal([]byte(action.Source), &src); err != nil {
		return nil, kernel.ErrInvalidState.Wrap("http action source is not valid HTTPSource JSON")
	}
	return e.executeHTTP(ctx, action, &src, args)
}

func (e *httpActionExecutor) executeHTTP(ctx context.Context, action *kernel.Action, src *kernel.HTTPSource, args map[string]any) (map[string]any, error) {
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
				val = fmt.Sprintf("%v", v)
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

	var reqBody io.Reader
	headers := map[string]string{}
	// Send a JSON body for body-bearing verbs (POST/PUT/PATCH always carry one,
	// even when empty, matching the original bare-URL POST behavior); GET/DELETE
	// only carry one when args were explicitly bound to the body.
	if len(bodyArgs) > 0 || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		b, err := json.Marshal(bodyArgs)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
		}
		reqBody = strings.NewReader(string(b))
		headers["Content-Type"] = "application/json"
	}
	if err := applyUpstreamAuth(action, headers, &rawURL, e.secretBox); err != nil {
		return nil, err
	}

	respBody, status, err := doHTTP(ctx, method, rawURL, headers, reqBody, e.timeout, e.allowLocal)
	if err != nil {
		return nil, err
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
