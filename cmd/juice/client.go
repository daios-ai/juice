package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"

	"github.com/daios-ai/juice/kernel"
)

// isTimeoutErr reports whether a transport error is a client-side timeout — the server was
// reachable but too slow to respond — rather than a hard connection failure (server down or
// refused). Used to give an accurate CLI message instead of "is serve running?".
func isTimeoutErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// serverBaseURL resolves the Juice server base URL for user-facing (client) commands: --server for
// one invocation, else the address the profile in use recorded (§14). A second address is not a
// second kernel identity — that is a public key, never a URL (§13) — which is why the token that
// travels there is decided by the pinned key, not by this address (tokenFor).
func serverBaseURL() string {
	if flagServer != "" {
		return strings.TrimRight(flagServer, "/")
	}
	_, _, p := activeProfile()
	return p.Endpoint
}

// errUnreachable reports that the juice server/peer at url couldn't be reached, retaining
// the raw transport error as the cause (surfaced only with --verbose).
func errUnreachable(url string, cause error) error {
	return kernel.ErrInvalidState.
		Wrapf("cannot reach juice server at %s (is `juice serve` running?)", url).
		Because(cause)
}

// apiCall sends an authenticated JSON request to the server and decodes a 2xx body into
// out (skipped when out is nil). The stored bearer token is attached when present; a 401
// triggers one refresh-and-retry. Non-2xx bodies are turned back into typed kernel errors
// so exit codes stay identical to the in-process path (exitCodeFor).
func apiCall(ctx context.Context, method, path string, body, out any) error {
	return apiDo(ctx, method, path, body, out, true)
}

func apiDo(ctx context.Context, method, path string, body, out any, retry bool) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return kernel.ErrInvalidInput.Wrapf("encode request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	base := serverBaseURL()
	// Remember why no token was attached: the server can only answer "missing bearer token", which
	// tells the user nothing about what to do. Whether they are not logged in or are addressing a
	// server their login does not belong to, the actionable answer is local, so it replaces the 401
	// below rather than being discarded here.
	tok, tokErr := tokenFor(base)
	if tokErr == nil {
		headers["Authorization"] = "Bearer " + tok
	}
	respBody, status, err := doHTTP(ctx, method, base+path, headers, rdr, 0, true)
	if err != nil && status == 0 {
		// The server was reachable but too slow (e.g. a slow upstream during OAuth consent) vs.
		// genuinely down — report each accurately rather than always blaming a missing server.
		if isTimeoutErr(err) {
			return kernel.ErrTimeout.Wrapf("juice server at %s did not respond in time (timed out)", base).Because(err)
		}
		return errUnreachable(base, err)
	}
	if err != nil {
		return err
	}
	// The refresh token is a credential too, and goes only where the access token may: an expired
	// login at home is refreshed, a stranger's 401 is not answered with anything.
	if status == 401 && retry && atHome(base) && refreshToken(ctx) {
		return apiDo(ctx, method, path, body, out, false)
	}
	if status == 401 && tokErr != nil {
		return tokErr // never logged in here: say so, instead of "missing bearer token"
	}
	if status < 200 || status >= 300 {
		return errorFromResponse(status, respBody)
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return kernel.ErrInternal.Wrapf("decode response: %v", err)
		}
	}
	return nil
}

// apiEmit runs the request and prints the server's JSON response via emitRaw, preserving
// field order. An empty body (e.g. 204) prints nothing.
func apiEmit(method, path string, body any) error {
	return apiEmitCtx(context.Background(), method, path, body)
}

func apiEmitCtx(ctx context.Context, method, path string, body any) error {
	var out json.RawMessage
	if err := apiCall(ctx, method, path, body, &out); err != nil {
		return err
	}
	if len(out) == 0 {
		return nil
	}
	return emitRaw(out)
}

// refreshToken rotates the stored access token using the stored refresh token, returning
// true on success so the caller can retry the original request once.
func refreshToken(ctx context.Context) bool {
	rt, err := loadRefreshToken()
	if err != nil {
		return false
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": rt})
	_, _, p := activeProfile()
	respBody, status, err := doHTTP(ctx, "POST", strings.TrimRight(p.Endpoint, "/")+"/v1/auth/refresh",
		map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body), 0, true)
	if err != nil || status != 200 {
		return false
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if json.Unmarshal(respBody, &out) != nil || out.AccessToken == "" {
		return false
	}
	_ = saveToken(out.AccessToken)
	if out.RefreshToken != "" {
		_ = saveRefreshToken(out.RefreshToken)
	}
	return true
}

// resolveActionID turns an action reference into an action id by asking the server to resolve it.
// The reference travels untouched — owner/name, a group root, owner@kernel/name, or a raw id — so
// the naming rules live in the kernel's one resolver and never here (§14). The stored token is
// attached by apiCall, so an owner resolving their own inactive/private action works too (§3).
func resolveActionID(ctx context.Context, ref string) (string, error) {
	q := url.Values{"ref": {ref}}
	var actions []actionResp
	if err := apiCall(ctx, "GET", "/v1/actions?"+q.Encode(), nil, &actions); err != nil {
		return "", err
	}
	if len(actions) == 0 {
		return "", kernel.ErrNotFound.Wrapf("action %s not found", ref)
	}
	return actions[0].ID, nil
}

// errorFromResponse reconstructs a typed KernelError from the server's {error, code} body
// so errors.Is checks and exit codes behave exactly as they did in-process.
func errorFromResponse(status int, body []byte) error {
	var e struct {
		Error string            `json:"error"`
		Code  string            `json:"code"`
		Meta  map[string]string `json:"meta"`
	}
	_ = json.Unmarshal(body, &e)
	if e.Code != "" {
		return &kernel.KernelError{Code: e.Code, HTTP: kernel.HTTPStatusFromCode(e.Code), Message: e.Error, Meta: e.Meta}
	}
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = fmt.Sprintf("server returned status %d", status)
	}
	return &kernel.KernelError{Code: "internal", HTTP: status, Message: msg}
}

// serverHealth is the open identity banner every client reads before it trusts a server: which
// kernel this is, and which network it serves (D23). It needs no token.
type serverHealth struct {
	Status      string `json:"status"`
	Handle      string `json:"handle"`
	PublicKey   string `json:"public_key"`
	Network     string `json:"network"`
	Digest      string `json:"network_digest"`
	Decimals    uint8  `json:"decimals"`
	RailAddress string `json:"rail_address"`
}

// health reads the banner from a specific base URL. It reuses the one HTTP path every other
// request takes, so a proxy or timeout behaves identically here.
func health(ctx context.Context, base string) (*serverHealth, error) {
	body, status, err := doHTTP(ctx, "GET", base+"/health", nil, nil, 0, true)
	if err != nil {
		return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s: %v", base, err)
	}
	if status != 200 {
		return nil, errorFromResponse(status, body)
	}
	var h serverHealth
	if err := json.Unmarshal(body, &h); err != nil || h.PublicKey == "" {
		return nil, kernel.ErrInvalidState.Wrapf("%s is not a juice server", base)
	}
	return &h, nil
}

// serverNetwork returns the network of the server this invocation is talking to. Signatures a
// client produces are bound to it, so it is read from the server rather than assumed.
func serverNetwork(ctx context.Context) (kernel.Network, error) {
	h, err := probeHealth(ctx, serverBaseURL())
	if err != nil {
		return kernel.Network{}, err
	}
	return kernel.Network{Name: h.Network, Digest: h.Digest, Decimals: h.Decimals}, nil
}
