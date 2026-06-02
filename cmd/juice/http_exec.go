package main

import (
	"context"
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

// validateResolvedIP returns an error if a DNS-resolved IP is loopback, private, or link-local.
func validateResolvedIP(ipStr string) error {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return fmt.Errorf("invalid resolved IP %q", ipStr)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return kernel.ErrInvalidInput.Wrap("resolved address is private or loopback")
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
// The response must be {"result": {...}, "receipt": <receipt-object>}.
// Returns (result, receiptJSONString, error).
func (e *httpActionExecutor) ExecuteFederation(ctx context.Context, source, idempotencyKey string, args map[string]any) (map[string]any, string, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, "", kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if idempotencyKey != "" {
		headers["X-Idempotency-Key"] = idempotencyKey
	}
	if e.signerFn != nil {
		actionParam := ""
		if u, err := url.Parse(source); err == nil {
			actionParam = u.Query().Get("action")
		}
		if sig, ts, err := e.signerFn(actionParam, idempotencyKey); err == nil {
			headers["X-Timestamp"] = ts
			headers["X-Signature"] = sig
		}
	}
	respBody, status, err := doHTTP(ctx, http.MethodPost, source, headers, strings.NewReader(string(body)), e.timeout, false)
	if err != nil {
		return nil, "", err
	}
	if status != http.StatusOK {
		return nil, "", kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", status, string(respBody))
	}
	var envelope struct {
		Result  map[string]any  `json:"result"`
		Receipt json.RawMessage `json:"receipt"`
	}
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		return nil, "", kernel.ErrExecutionFailed.Wrap("federation response is not valid JSON")
	}
	return envelope.Result, string(envelope.Receipt), nil
}

type httpActionExecutor struct {
	timeout    time.Duration
	allowLocal bool
	signerFn   func(action, idempotencyKey string) (sig, ts string, err error) // wired after bootstrap
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

func (e *httpActionExecutor) Execute(ctx context.Context, source string, args map[string]any) (map[string]any, error) {
	if strings.HasPrefix(strings.TrimSpace(source), "{") {
		return e.executeOpenAPI(ctx, source, args)
	}
	body, err := json.Marshal(args)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}
	headers := map[string]string{"Content-Type": "application/json"}
	respBody, status, err := doHTTP(ctx, http.MethodPost, source, headers, strings.NewReader(string(body)), e.timeout, e.allowLocal)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", status, string(respBody))
	}
	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("action response is not valid JSON")
	}
	return result, nil
}

func (e *httpActionExecutor) executeOpenAPI(ctx context.Context, source string, args map[string]any) (map[string]any, error) {
	var src kernel.OpenAPISource
	if err := json.Unmarshal([]byte(source), &src); err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("invalid OpenAPI source")
	}

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
	var headers map[string]string
	if len(bodyArgs) > 0 || (method != http.MethodGet && len(src.Params) > 0) {
		b, err := json.Marshal(bodyArgs)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
		}
		reqBody = strings.NewReader(string(b))
		headers = map[string]string{"Content-Type": "application/json"}
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
	u, err := url.Parse(specURL)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("invalid spec URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, kernel.ErrInvalidInput.Wrap("spec URL scheme must be http or https")
	}
	if !allowLocal {
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") || host == "" {
			return nil, kernel.ErrInvalidInput.Wrap("unsafe spec URL")
		}
		if ip := net.ParseIP(host); ip != nil {
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
				return nil, kernel.ErrInvalidInput.Wrap("unsafe spec URL: private/loopback host")
			}
		}
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
