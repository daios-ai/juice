package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
)

// ExecuteFederation calls a remote kernel's federation endpoint with an idempotency key.
// The response must be {"result": {...}, "receipt": <receipt-object>}.
// Returns (result, receiptJSONString, error).
func (e *httpActionExecutor) ExecuteFederation(ctx context.Context, source, idempotencyKey string, args map[string]any) (map[string]any, string, error) {
	body, err := json.Marshal(args)
	if err != nil {
		return nil, "", kernel.ErrInvalidInput.Wrap("could not serialize args")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, source, strings.NewReader(string(body)))
	if err != nil {
		return nil, "", kernel.ErrInvalidInput.Wrapf("invalid action URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("X-Idempotency-Key", idempotencyKey)
	}

	// Sign the request so the remote kernel can verify our identity.
	if e.signerFn != nil {
		if key := e.signerFn(); len(key) == ed25519.PrivateKeySize {
			// Extract the "action" query param from source URL for the signed payload.
			actionParam := ""
			if u, err := url.Parse(source); err == nil {
				actionParam = u.Query().Get("action")
			}
			ts := time.Now().UTC().Format(time.RFC3339)
			sig, err := kernel.SignFederationPayload(key, actionParam, idempotencyKey, ts)
			if err == nil {
				req.Header.Set("X-Timestamp", ts)
				req.Header.Set("X-Signature", sig)
			}
		}
	}

	timeout := e.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, "", kernel.ErrExecutionFailed.Wrap("could not read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", resp.StatusCode, string(respBody))
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
	timeout  time.Duration
	signerFn func() ed25519.PrivateKey // wired after kernel bootstrap; nil if not yet available
}

func (e *httpActionExecutor) Execute(ctx context.Context, source string, args map[string]any) (map[string]any, error) {
	if strings.HasPrefix(strings.TrimSpace(source), "{") {
		return e.executeOpenAPI(ctx, source, args)
	}

	body, err := json.Marshal(args)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, source, strings.NewReader(string(body)))
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("invalid action URL: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	timeout := e.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("could not read response body")
	}

	if resp.StatusCode != http.StatusOK {
		return nil, kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", resp.StatusCode, string(respBody))
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

	// Substitute {param} placeholders in path from args.
	path := src.Path
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

	// Remaining args not consumed as path params.
	remaining := make(map[string]any, len(args))
	for k, v := range args {
		if _, isPath := pathParams[k]; !isPath {
			remaining[k] = v
		}
	}

	rawURL := strings.TrimRight(src.BaseURL, "/") + path
	method := strings.ToUpper(src.Method)

	var reqBody io.Reader
	if method == http.MethodGet {
		u, err := url.Parse(rawURL)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrapf("invalid URL: %v", err)
		}
		q := u.Query()
		for k, v := range remaining {
			q.Set(k, fmt.Sprintf("%v", v))
		}
		u.RawQuery = q.Encode()
		rawURL = u.String()
	} else {
		b, err := json.Marshal(remaining)
		if err != nil {
			return nil, kernel.ErrInvalidInput.Wrap("could not serialize args")
		}
		reqBody = strings.NewReader(string(b))
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		return nil, kernel.ErrInvalidInput.Wrapf("invalid action URL: %v", err)
	}
	if method != http.MethodGet {
		req.Header.Set("Content-Type", "application/json")
	}

	timeout := e.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrapf("HTTP call failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("could not read response body")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, kernel.ErrExecutionFailed.Wrapf("action returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, kernel.ErrExecutionFailed.Wrap("action response is not valid JSON")
	}
	return result, nil
}
