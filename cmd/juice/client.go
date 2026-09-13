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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

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

// client is everything this invocation is: whom it acts as, which kernel it acts through, where
// that kernel answers, the credentials it will send, and the banners it has read. A command reads
// it and cannot make another — there is no second way to resolve an identity, so the token a
// request carries, the account a prompt names, and the login a retry refreshes are one answer
// rather than four agreeing ones.
type client struct {
	resolved bool
	login    login       // zero when nothing is selected
	kernel   *kernelRec  // nil when nothing is selected
	base     string      // where requests go: --server, else this login's kernel
	creds    credentials // this login's tokens; rotated under its lock, never re-read behind us
	err      error       // why there is no usable login, answered where one is needed
	named    bool        // a login was named, so err is a refusal rather than "no login given"
	banners  map[string]*serverHealth
}

// cli is this invocation's client. main replaces it before any command body runs; it resolves
// itself on first use, so a command that needs no identity (kernel serve) reads no records.
var cli = &client{}

// resolve fixes whom this command acts as and where it sends, once: --as, else JUICE_AS, else the
// recorded current login (§14). The first two never write, so naming a login for one command
// cannot move an agent onto another, and a name that is not a login here is an error rather than
// a fallback — a misspelled --as must not quietly send a credential somewhere nobody named.
// Nothing another process writes afterwards can change the answer, so a command finishes as the
// login it started with.
func (c *client) resolve() *client {
	if c.resolved {
		return c
	}
	c.resolved, c.banners = true, map[string]*serverHealth{}
	c.base = strings.TrimRight(strings.TrimSpace(flagServer), "/")
	cfg := loadClientConfig()
	name := strings.TrimSpace(flagAs)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("JUICE_AS"))
	}
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		// Nothing named: a request still goes out where --server says, as a stranger, which is how
		// a kernel is read before anyone has an account on it.
		c.err = kernel.ErrUnauthenticated.Wrap("no login selected; log in with: juice auth login USER@KERNEL")
		return c
	}
	c.named = true
	l, err := parseLogin(name)
	if err != nil {
		c.err = err
		return c
	}
	k, err := kernelNamed(cfg, l.Kernel)
	if err != nil {
		c.err = err
		return c
	}
	c.login, c.kernel, c.creds = l, k, readCredentials(l)
	if c.base == "" {
		c.base = strings.TrimRight(k.Endpoint, "/")
	}
	return c
}

// clientFor is the client of a login a command names rather than acts as: creating that account,
// logging in, recovering it, logging it out. It addresses that login's own kernel, so nothing
// about whatever is selected — or named by --as — reaches the request.
func clientFor(l login) (*client, error) {
	cfg := loadClientConfig()
	k, err := kernelNamed(cfg, l.Kernel)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(k.Endpoint, "/")
	if s := strings.TrimRight(strings.TrimSpace(flagServer), "/"); s != "" && !sameAddress(s, base) {
		return nil, kernel.ErrInvalidInput.Wrapf(
			"--server %s names a different address than kernel %s (%s); give one or the other", s, l.Kernel, base)
	}
	return &client{resolved: true, login: l, kernel: k, base: base,
		creds: readCredentials(l), banners: map[string]*serverHealth{}}, nil
}

// namedClient reads handle@kernel and returns the client of that login: the two steps every
// command that says where it acts takes before it acts.
func namedClient(name string) (login, *client, error) {
	l, err := parseLogin(name)
	if err != nil {
		return login{}, nil, err
	}
	c, err := clientFor(l)
	return l, c, err
}

// identity is the login this command acts as, and why there is none when there is not. A display
// that merely marks the current login ignores the error; anything that acts returns it.
func (c *client) identity() (login, error) {
	c.resolve()
	return c.login, c.err
}

// confirm gates an act that cannot be undone, naming the login it runs as — the same one the
// request will carry, so what a person reads and what the kernel receives are never two accounts.
func (c *client) confirm(act string, yes bool) error {
	l, err := c.identity()
	if err != nil {
		return err
	}
	return confirm(fmt.Sprintf("%s, acting as %s? This cannot be undone.", act, l), yes)
}

// atHome reports whether this client sends to its own login's kernel — the one place its
// credentials may go. Nothing a server says about itself can earn it a credential: an unsigned
// banner is a claim, not a proof, so any other address gets every request anonymously.
func (c *client) atHome() bool {
	return c.kernel != nil && sameAddress(c.base, c.kernel.Endpoint)
}

// token is the bearer this client sends, or why it sends none.
func (c *client) token() (string, error) {
	if c.err != nil {
		return "", c.err
	}
	if c.creds.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrapf("%s is not logged in; run: juice auth login %s", c.login, c.login)
	}
	if !c.atHome() {
		return "", kernel.ErrUnauthenticated.Wrapf(
			"%s is not the kernel %s belongs to, so the request was sent without your login", c.base, c.login)
	}
	return c.creds.Token, nil
}

// errUnreachable reports that the juice server/peer at url couldn't be reached, retaining
// the raw transport error as the cause (surfaced only with --verbose).
func errUnreachable(url string, cause error) error {
	return kernel.ErrInvalidState.
		Wrapf("cannot reach juice server at %s (is `juice kernel serve` running?)", url).
		Because(cause)
}

// apiCall sends an authenticated JSON request to the server and decodes a 2xx body into
// out (skipped when out is nil). The stored bearer token is attached when present; a 401
// triggers one refresh-and-retry. Non-2xx bodies are turned back into typed kernel errors
// so exit codes stay identical to the in-process path (exitCodeFor).
func (c *client) call(ctx context.Context, method, path string, body, out any) error {
	return c.resolve().do(ctx, method, path, body, out, true)
}

func (c *client) do(ctx context.Context, method, path string, body, out any, retry bool) error {
	if c.base == "" {
		// No login and no --server: there is no address to send this to, and localhost is a guess
		// that could reach a kernel the caller never named. A record with no address is the same
		// answer — never silence, which would read as a request that succeeded.
		if c.err != nil {
			return c.err
		}
		return kernel.ErrInvalidState.Wrapf("kernel %s has no address recorded; add it again: juice kernel add URL %s",
			c.login.Kernel, c.login.Kernel)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return kernel.ErrInvalidInput.Wrapf("encode request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	// A session whose access token is gone is renewed before the request, not after a stranger's
	// 401: a login that holds one never reads as nobody.
	if c.creds.Token == "" && c.creds.RefreshToken != "" && c.atHome() {
		if _, rerr := c.refresh(ctx, ""); rerr != nil {
			return rerr
		}
	}
	// Why no token is attached is a local answer, and the server can only say "missing bearer
	// token", so it is kept and replaces the 401 below rather than being discarded here.
	tok, tokErr := c.token()
	// A login that was named and holds no session at all is a refusal, never a quieter request:
	// `--as` on a login this client does not hold must not read public data as a stranger (D20).
	// Naming none, or addressing another kernel with --server, is the anonymous case.
	if tokErr != nil && c.named && c.creds.RefreshToken == "" && (c.kernel == nil || c.atHome()) {
		return tokErr
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if tokErr == nil {
		headers["Authorization"] = "Bearer " + tok
	}
	respBody, status, err := doHTTP(ctx, method, c.base+path, headers, rdr, 0, true)
	if err != nil && status == 0 {
		// The server was reachable but too slow (e.g. a slow upstream during OAuth consent) vs.
		// genuinely down — report each accurately rather than always blaming a missing server.
		if isTimeoutErr(err) {
			return kernel.ErrTimeout.Wrapf("juice server at %s did not respond in time (timed out)", c.base).Because(err)
		}
		return errUnreachable(c.base, err)
	}
	if err != nil {
		return err
	}
	// The refresh token is a credential too, and goes only where the access token may: an expired
	// login at home is refreshed, a stranger's 401 is not answered with anything.
	if status == 401 && retry && c.atHome() {
		rotated, rerr := c.refresh(ctx, tok)
		if rerr != nil {
			return rerr
		}
		if rotated {
			return c.do(ctx, method, path, body, out, false)
		}
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

// emit runs one request and prints the server's reply under the one output policy.
func (c *client) emit(method, path string, body any, o output) error {
	return c.emitCtx(context.Background(), method, path, body, o)
}

func (c *client) emitCtx(ctx context.Context, method, path string, body any, o output) error {
	if err := o.units(ctx, c); err != nil {
		return err
	}
	var out json.RawMessage
	if err := c.call(ctx, method, path, body, &out); err != nil {
		return err
	}
	return emit(out, o)
}

// refresh rotates this login's access token, returning true so the caller retries the request
// once. used is the token just refused, which is what makes this safe for two programs sharing one
// login: the whole read-rotate-write runs under the session's lock, so the second to arrive sees
// that the first already rotated and takes what it stored instead of spending a refresh token that
// no longer exists. Either way the client keeps what the lock yielded — the identity is fixed for
// the command, the tokens under it are not.
func (c *client) refresh(ctx context.Context, used string) (bool, error) {
	if c.kernel == nil {
		return false, nil
	}
	endpoint := strings.TrimRight(c.kernel.Endpoint, "/")
	var rotated *credentials
	err := withCredentials(c.login, func(cr *credentials) (bool, error) {
		if cr.Token != "" && cr.Token != used {
			rotated = cr // another process rotated while this one was in flight
			return false, nil
		}
		if cr.RefreshToken == "" {
			return false, nil
		}
		body, _ := json.Marshal(map[string]string{"refresh_token": cr.RefreshToken})
		respBody, status, err := doHTTP(ctx, "POST", endpoint+"/v1/auth/refresh",
			map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body), 0, true)
		if err != nil || status != 200 {
			return false, nil
		}
		var out struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if json.Unmarshal(respBody, &out) != nil || out.AccessToken == "" {
			return false, nil
		}
		cr.Token = out.AccessToken
		if out.RefreshToken != "" {
			cr.RefreshToken = out.RefreshToken
		}
		rotated = cr
		return true, nil
	})
	// A session this client holds and the file do not agree on is worse than an expired one: the
	// next command would spend a refresh token that is no longer the stored one. So the client
	// keeps only what the lock committed, and a failed write is the command's answer.
	if err != nil {
		return false, kernel.ErrInternal.Wrapf("your session was renewed but could not be saved: %v", err)
	}
	if rotated == nil {
		return false, nil
	}
	c.creds = *rotated
	return true, nil
}

// resolveActionID turns an action reference into an action id by asking the server to resolve it.
// The reference travels untouched — owner/name, a group root, owner@kernel/name, or a raw id — so
// the naming rules live in the kernel's one resolver and never here (§14). The stored token is
// attached by apiCall, so an owner resolving their own inactive/private action works too (§3).
func (c *client) resolveActionID(ctx context.Context, ref string) (string, error) {
	q := url.Values{"ref": {ref}}
	var actions []actionResp
	if err := c.call(ctx, "GET", "/v1/actions?"+q.Encode(), nil, &actions); err != nil {
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
	Symbol      string `json:"symbol"`
	RailAddress string `json:"rail_address"`
	base        string // the address it was read from: a banner is a claim about one place
}

// health reads a server's identity banner, at most once per address per command: deciding what a
// client may do must not multiply the requests a command makes. It reuses the one HTTP path every
// other request takes, so a proxy or timeout behaves identically here.
func (c *client) health(ctx context.Context, base string) (*serverHealth, error) {
	c.resolve()
	if h, ok := c.banners[base]; ok {
		if h == nil {
			return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s", base)
		}
		return h, nil
	}
	h, err := c.dialHealth(ctx, base)
	c.banners[base] = h // a failure is cached as nil, so one unreachable server is dialed once
	if err != nil {
		c.banners[base] = nil
		return nil, err
	}
	return h, nil
}

func (c *client) dialHealth(ctx context.Context, base string) (*serverHealth, error) {
	body, status, err := doHTTP(ctx, "GET", base+"/health", nil, nil, 0, true)
	if err != nil {
		return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s: %v", base, err)
	}
	if status != 200 {
		return nil, errorFromResponse(status, body)
	}
	return decodeHealth(base, body)
}

// decodeHealth reads an identity banner, refusing a body that is not one: a 200 from something
// else at that address must be an error rather than a blank "ok" line.
func decodeHealth(base string, body []byte) (*serverHealth, error) {
	var h serverHealth
	if err := json.Unmarshal(body, &h); err != nil || h.PublicKey == "" {
		return nil, kernel.ErrInvalidState.Wrapf("%s answered, but it is not a juice kernel", base)
	}
	h.base = strings.TrimRight(base, "/")
	return &h, nil
}

// sameAddress reports whether two base URLs name one address. Trailing slashes are a spelling,
// not a difference, and every decision about where a credential may go is made through here.
func sameAddress(a, b string) bool {
	return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
}

// banner is this client's own server's identity banner.
func (c *client) banner(ctx context.Context) (*serverHealth, error) {
	if c.resolve().base == "" {
		return nil, c.err
	}
	return c.health(ctx, c.base)
}

// network is the network of the server this client talks to. Signatures are bound to it and money
// is scaled by it, so it is read from the server rather than assumed, and an error is a refusal at
// the call site: a guessed zero would move a thousandth of what an operator typed, or a thousand
// times it.
func (c *client) network(ctx context.Context) (kernel.Network, error) {
	if c.resolve().base == "" {
		// No address — no login selected, none named — is not a money question: answer it the way
		// every request does, with the login to make, which is what the caller has to act on.
		return kernel.Network{}, c.err
	}
	h, err := c.health(ctx, c.base)
	if err != nil {
		return kernel.Network{}, kernel.ErrInvalidState.Wrapf(
			"cannot read this kernel's money units right now; nothing was sent — retry").Because(err)
	}
	return kernel.Network{Name: h.Network, Digest: h.Digest, Decimals: h.Decimals, Symbol: h.Symbol}, nil
}

// A client knows kernels and holds logins on them. A login is one account at one kernel, written
// handle@kernel the way an address names a person at a host, and one is selected: it says both who
// a command acts as and which kernel it acts through, so choosing where you are is one act rather
// than two. Credentials are not in the records — each login's tokens sit in their own 0600 file,
// rotated under a lock, because a session's lifecycle is its own.

// kernelRec is one kernel this client knows: where it answers, and the identity it reported there.
// The key and network are what a login checks before it is selected, so a command never lands on a
// kernel other than the one this record was made for.
type kernelRec struct {
	Endpoint    string `json:"endpoint"`
	PublicKey   string `json:"public_key,omitempty"`
	WorldDigest string `json:"world_digest,omitempty"`
	Network     string `json:"network,omitempty"`
	Decimals    uint8  `json:"decimals,omitempty"`
	Symbol      string `json:"symbol,omitempty"`
}

type clientConfig struct {
	Current string                `json:"current"`
	Kernels map[string]*kernelRec `json:"kernels"`
}

// login is one account at one kernel. Both halves are ordinary local names, so the pair is one file
// name under credentials/ and one word an operator types.
type login struct {
	Handle string
	Kernel string
}

func (l login) String() string { return l.Handle + "@" + l.Kernel }

// validate accepts a login whose halves are both ordinary local names, which is what keeps the pair
// a file name and nothing else — no separator, no leading dot, nothing that could address another
// file under credentials/ or outside it.
func (l login) validate() error {
	if err := validateLocalName("account", l.Handle); err != nil {
		return err
	}
	return validateLocalName("kernel", l.Kernel)
}

// parseLogin reads handle@kernel. The message names the form rather than the rule it broke, because
// the form is what the operator has to type.
func parseLogin(s string) (login, error) {
	handle, kernelName, found := strings.Cut(strings.TrimSpace(s), "@")
	if !found || strings.Contains(kernelName, "@") {
		return login{}, kernel.ErrInvalidInput.Wrap("name the account and its kernel: USER@KERNEL, as in alice@acme")
	}
	l := login{Handle: handle, Kernel: kernelName}
	return l, l.validate()
}

// clientHome is the client's own subdirectory, $JUICE_HOME/client. Kernel directories sit beside
// it and are never read here: every command is a TCP client (API.md C13).
func clientHome() string { return filepath.Join(juiceHome(), "client") }

func clientConfigPath() string { return filepath.Join(clientHome(), "config.json") }

func credentialsDir() string { return filepath.Join(clientHome(), "credentials") }

// credentialPath is where a login becomes a path, and the only place it does — so it is where a
// login that is not two names is refused, whether it was typed, recorded, or carried over from an
// older layout.
func credentialPath(l login) (string, error) {
	if err := l.validate(); err != nil {
		return "", err
	}
	return filepath.Join(credentialsDir(), l.String()+".json"), nil
}

// loadClientConfig reads the client's records, migrating a pre-login layout the first time it finds
// one. An unreadable file yields empty records rather than an error: a client that cannot parse its
// own notes must still be able to reach a server and log in again.
func loadClientConfig() *clientConfig {
	cfg := &clientConfig{}
	data, err := os.ReadFile(clientConfigPath())
	if err != nil {
		if migrated := migrateLegacyRecords(); migrated != nil {
			cfg = migrated
		}
	} else {
		_ = json.Unmarshal(data, cfg)
		if migrated := migrateContexts(data); migrated != nil {
			cfg = migrated
		}
	}
	if cfg.Kernels == nil {
		cfg.Kernels = map[string]*kernelRec{}
	}
	return cfg
}

// saveClientConfig writes the records atomically — temp file in the same directory, then rename —
// so an interrupted write never leaves a client without its bearings. The file holds no secret;
// credentials are their own files.
func saveClientConfig(cfg *clientConfig) error {
	if err := os.MkdirAll(clientHome(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(clientHome(), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), clientConfigPath())
}

// ---- selection ----

// kernelNamed is the one place a kernel name becomes a record, so an unknown one is refused the
// same way wherever it is met, with the command that would make it known.
func kernelNamed(cfg *clientConfig, name string) (*kernelRec, error) {
	if k := cfg.Kernels[name]; k != nil {
		return k, nil
	}
	return nil, kernel.ErrNotFound.Wrapf("no kernel named %s; add it with: juice kernel add URL %s", name, name)
}

// sameKernel is the one definition of "the kernel this record was made for": the key it answers
// with, and the network it serves. A key that changed is a different kernel on the same port; a
// network that changed means every balance and signature there now means something else (D23).
// Registering a kernel and selecting a login both ask this, and differ only in what they do with
// a mismatch — so neither can hold its own idea of the same kernel.
func sameKernel(name string, k *kernelRec, h *serverHealth) error {
	again := fmt.Sprintf("\nIf it was reinstalled, forget the old record and add it again:\n"+
		"  juice kernel forget %s\n  juice kernel add %s %s", name, h.base, name)
	switch {
	case k.PublicKey != "" && h.PublicKey != k.PublicKey:
		return kernel.ErrInvalidState.Wrapf(
			"the kernel at %s is not the one you registered as \"%s\". Nothing was sent.%s", h.base, name, again)
	case k.WorldDigest != "" && h.Digest != k.WorldDigest:
		return kernel.ErrInvalidState.Wrapf(
			"the kernel at %s now serves the %s network, not the one you registered as \"%s\". Nothing was "+
				"sent, because money and signatures mean something different there.%s", h.base, h.Network, name, again)
	}
	return nil
}

// verify refuses a server that is no longer the kernel this client's record was made for. Every
// command about to send something — a password above all — calls it before it sends, never after.
func (c *client) verify(ctx context.Context) error {
	if c.resolve().err != nil {
		return c.err
	}
	h, err := c.health(ctx, c.kernel.Endpoint)
	if err != nil {
		return err
	}
	return sameKernel(c.login.Kernel, c.kernel, h)
}

// selectLogin makes this client's login the current one, after its kernel has answered as itself.
func (c *client) selectLogin(ctx context.Context) error {
	if err := c.verify(ctx); err != nil {
		return err
	}
	cfg := loadClientConfig()
	cfg.Current = c.login.String()
	return saveClientConfig(cfg)
}

// logins are the logins this client holds, read from the credential directory itself: a login is
// its credential file, so there is no second list to keep in step with it.
func logins() []login {
	entries, err := os.ReadDir(credentialsDir())
	if err != nil {
		return nil
	}
	var out []login
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if l, err := parseLogin(strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// ---- credentials ----

// credentials is one login: the tokens it holds, and the principal they belong to. The file name is
// a label — a handle can be renamed, and its old name taken by someone else — so the principal id
// is recorded beside the tokens, and it is the token, never the label, that authenticates.
type credentials struct {
	Token        string `json:"token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	PrincipalID  string `json:"principal_id,omitempty"`
}

// withCredentials opens one login's credential file under an exclusive lock and hands it to fn. The
// lock is what makes successive and concurrent processes on one session safe: a refresh is a read,
// a round trip and a write, and two of them interleaved would leave one holding a rotated-away
// token. Everything fn needs to decide is inside the lock, so it can see that another process
// refreshed first and simply use what that one stored.
func withCredentials(l login, fn func(*credentials) (bool, error)) error {
	if err := os.MkdirAll(credentialsDir(), 0o700); err != nil {
		return err
	}
	path, err := credentialPath(l)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	var c credentials
	if data, rerr := os.ReadFile(path); rerr == nil && len(data) > 0 {
		_ = json.Unmarshal(data, &c)
	}
	write, err := fn(&c)
	if err != nil || !write {
		return err
	}
	data, err := json.MarshalIndent(&c, "", "  ")
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// readCredentials reads one login's tokens without holding the lock across anything else. Every
// caller that also writes goes through withCredentials instead.
func readCredentials(l login) credentials {
	var c credentials
	path, err := credentialPath(l)
	if err != nil {
		return c
	}
	if data, rerr := os.ReadFile(path); rerr == nil {
		_ = json.Unmarshal(data, &c)
	}
	return c
}

// ---- amounts ----

// parseUnits converts an amount as a person writes it into the whole base units the kernel counts
// in. The digits are shifted by hand: money never passes through floating point. Zero is a valid
// reading here — a price may be nothing — so a verb that must move money checks that itself.
func parseUnits(s string, decimals uint8) (int64, error) {
	bad := kernel.ErrInvalidInput.Wrap("amount must be a whole number")
	if decimals > 0 {
		bad = kernel.ErrInvalidInput.Wrapf("amount must have at most %d decimal places", decimals)
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if !allDigits(whole) || (frac != "" && !allDigits(frac)) || len(frac) > int(decimals) {
		return 0, bad
	}
	v, err := strconv.ParseInt(whole+frac+strings.Repeat("0", int(decimals)-len(frac)), 10, 64)
	if err != nil {
		return 0, bad
	}
	return v, nil
}

// parseAmount is parseUnits for the verbs that move money, where nothing to move is a mistake.
func parseAmount(s string, decimals uint8) (int64, error) {
	v, err := parseUnits(s, decimals)
	if err != nil || v <= 0 {
		return 0, kernel.ErrInvalidInput.Wrapf("amount must be positive, with at most %d decimal places", decimals)
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- migration from the pre-login layouts ----

// legacyRecords is what a client wrote before logins: kernels, and contexts naming one kernel and
// one login on it. profiles.json, older still, is read into the same shape — one profile was a
// kernel and a session at once — so there is one conversion rather than a chain of them.
type legacyRecords struct {
	Current  string                `json:"current"`
	Kernels  map[string]*kernelRec `json:"kernels"`
	Contexts map[string]struct {
		Kernel string `json:"kernel"`
		Handle string `json:"handle"`
	} `json:"contexts"`
}

// migrateContexts converts records that still name contexts. One that recorded whose login it held
// becomes that login and keeps its session; one that never logged in, or that would collide with a
// login already made, is kept aside. A session is never silently merged into another's name.
func migrateContexts(data []byte) *clientConfig {
	var old legacyRecords
	if json.Unmarshal(data, &old) != nil || len(old.Contexts) == 0 {
		return nil
	}
	cfg := &clientConfig{Kernels: old.Kernels}
	if cfg.Kernels == nil {
		cfg.Kernels = map[string]*kernelRec{}
	}
	// The selected context is converted first, so it is the one that keeps the name if two would
	// become one login.
	names := []string{old.Current}
	for name := range old.Contexts {
		if name != old.Current {
			names = append(names, name)
		}
	}
	sort.Strings(names[1:])
	taken := map[string]bool{}
	for _, name := range names {
		c, ok := old.Contexts[name]
		if !ok {
			continue
		}
		if c.Kernel == "" {
			c.Kernel = name
		}
		l := login{Handle: c.Handle, Kernel: c.Kernel}
		to, err := credentialPath(l)
		if c.Handle == "" || err != nil || taken[l.String()] {
			keepAside(name, nil)
			continue
		}
		taken[l.String()] = true
		if from := filepath.Join(credentialsDir(), name+".json"); from != to {
			_ = os.Rename(from, to)
		}
		if name == old.Current {
			cfg.Current = l.String()
		}
	}
	if saveClientConfig(cfg) != nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "client records now name logins (handle@kernel); see: juice auth list\n")
	return cfg
}

// keepAside is where a session whose account cannot be named is left: renamed when it has a file of
// its own, written when it never had one. Nothing is destroyed and nothing is guessed, and the name
// says why the file is there. The operator logs in again.
func keepAside(name string, c *credentials) {
	dir := credentialsDir()
	aside := filepath.Join(dir, name+".json.unmigrated")
	if c != nil {
		blob, _ := json.MarshalIndent(c, "", "  ")
		if os.MkdirAll(dir, 0o700) != nil || os.WriteFile(aside, append(blob, '\n'), 0o600) != nil {
			return
		}
	} else if os.Rename(filepath.Join(dir, name+".json"), aside) != nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s held a session whose account it never recorded; kept as %s — log in again\n", name, aside)
}

// migrateLegacyRecords converts a profiles.json written before contexts existed. Each profile was a
// kernel, a login and a selection at once, but never recorded whose login it was, so the kernels and
// their pinned keys survive and the sessions are kept aside. The old file is renamed rather than
// removed. It returns nil when there is nothing to migrate.
func migrateLegacyRecords() *clientConfig {
	legacy := filepath.Join(clientHome(), "profiles.json")
	data, err := os.ReadFile(legacy)
	if err != nil {
		return nil
	}
	var old struct {
		Active   string `json:"active"`
		Profiles map[string]struct {
			Endpoint     string `json:"endpoint"`
			PublicKey    string `json:"public_key"`
			WorldDigest  string `json:"world_digest"`
			Network      string `json:"network"`
			Decimals     uint8  `json:"decimals"`
			Token        string `json:"token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"profiles"`
	}
	if json.Unmarshal(data, &old) != nil {
		return nil
	}
	cfg := &clientConfig{Kernels: map[string]*kernelRec{}}
	for name, p := range old.Profiles {
		cfg.Kernels[name] = &kernelRec{
			Endpoint: p.Endpoint, PublicKey: p.PublicKey, WorldDigest: p.WorldDigest,
			Network: p.Network, Decimals: p.Decimals,
		}
		if p.Token != "" || p.RefreshToken != "" {
			keepAside(name, &credentials{Token: p.Token, RefreshToken: p.RefreshToken})
		}
	}
	if saveClientConfig(cfg) != nil {
		return nil
	}
	_ = os.Rename(legacy, legacy+".migrated")
	fmt.Fprintf(os.Stderr, "moved client profiles into %s (old file kept as %s.migrated)\n", clientConfigPath(), legacy)
	return cfg
}
