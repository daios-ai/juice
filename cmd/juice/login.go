package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/daios-ai/juice/kernel"
)

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

// selected returns the login this invocation acts as and the kernel it acts through: --as, else
// JUICE_AS, else the recorded current one. The first two never write, so naming a login for one
// command cannot move an agent onto another. A name that is not a login here is an error and never
// a fallback: a misspelled --as must not quietly send a credential somewhere nobody named.
func selected() (login, *kernelRec, error) {
	cfg := loadClientConfig()
	name := strings.TrimSpace(flagAs)
	if name == "" {
		name = strings.TrimSpace(os.Getenv("JUICE_AS"))
	}
	if name == "" {
		name = cfg.Current
	}
	if name == "" {
		return login{}, nil, kernel.ErrUnauthenticated.Wrap(
			"no login selected; log in with: juice auth login USER@KERNEL")
	}
	l, err := parseLogin(name)
	if err != nil {
		return login{}, nil, err
	}
	k, err := kernelNamed(cfg, l.Kernel)
	return l, k, err
}

// kernelNamed is the one place a kernel name becomes a record, so an unknown one is refused the
// same way wherever it is met, with the command that would make it known.
func kernelNamed(cfg *clientConfig, name string) (*kernelRec, error) {
	if k := cfg.Kernels[name]; k != nil {
		return k, nil
	}
	return nil, kernel.ErrNotFound.Wrapf("no kernel named %s; add it with: juice kernel add URL %s", name, name)
}

// selectLogin makes a login the current one, after the kernel it names has answered as the kernel
// its record was made for. Selecting is the moment a client commits to sending credentials to an
// address, so it is the moment the address is checked.
func selectLogin(ctx context.Context, l login) error {
	cfg := loadClientConfig()
	k, err := kernelNamed(cfg, l.Kernel)
	if err != nil {
		return err
	}
	if err := verifyKernel(ctx, l.Kernel, k); err != nil {
		return err
	}
	cfg.Current = l.String()
	return saveClientConfig(cfg)
}

// verifyKernel refuses a server that is no longer the kernel a record was made for. A key that
// changed is a different kernel on the same port; a network that changed means every balance and
// signature there now means something else (D23).
func verifyKernel(ctx context.Context, name string, k *kernelRec) error {
	h, err := health(ctx, k.Endpoint)
	if err != nil {
		return err
	}
	if k.PublicKey != "" && h.PublicKey != k.PublicKey {
		return kernel.ErrInvalidState.Wrapf(
			"the server at %s is a different kernel than %s recorded; not switching", k.Endpoint, name)
	}
	if k.WorldDigest != "" && h.Digest != k.WorldDigest {
		return kernel.ErrInvalidState.Wrapf(
			"the server at %s now serves the %s network, not the one %s recorded; not switching", k.Endpoint, h.Network, name)
	}
	return nil
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

// saveToken and saveRefreshToken store what a login was just issued. They act on the selected
// login, which `auth login` has already made the one it just proved, and they are the only writers
// of a session outside that command.
func saveToken(tok string) error {
	return onSelected(func(c *credentials) (bool, error) { c.Token = tok; return true, nil })
}

func saveRefreshToken(tok string) error {
	return onSelected(func(c *credentials) (bool, error) { c.RefreshToken = tok; return true, nil })
}

// onSelected runs fn on the selected login's credentials, under its lock.
func onSelected(fn func(*credentials) (bool, error)) error {
	l, _, err := selected()
	if err != nil {
		return err
	}
	return withCredentials(l, fn)
}

// ---- releasing a credential ----

// atHome reports whether base is the selected login's own kernel address — the one place its
// credentials may go. Nothing a server says about itself can earn it a credential: an unsigned
// banner is a claim, not a proof, so any other address gets every request anonymously.
func atHome(base string) bool {
	_, k, err := selected()
	return err == nil && strings.TrimRight(base, "/") == strings.TrimRight(k.Endpoint, "/")
}

// tokenFor returns the bearer token to send to base, or why none is sent.
func tokenFor(base string) (string, error) {
	l, _, err := selected()
	if err != nil {
		return "", err
	}
	c := readCredentials(l)
	if c.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrapf("%s is not logged in; run: juice auth login %s", l, l)
	}
	if !atHome(base) {
		return "", kernel.ErrUnauthenticated.Wrapf(
			"%s is not the kernel %s belongs to, so the request was sent without your login", base, l)
	}
	return c.Token, nil
}

// probeHealth reads a server's identity banner at most once per address per run: deciding what a
// client may do must not multiply the requests a command makes.
var (
	healthMu    sync.Mutex
	healthCache = map[string]*serverHealth{}
)

func probeHealth(ctx context.Context, base string) (*serverHealth, error) {
	healthMu.Lock()
	defer healthMu.Unlock()
	if h, ok := healthCache[base]; ok {
		if h == nil {
			return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s", base)
		}
		return h, nil
	}
	h, err := health(ctx, base)
	healthCache[base] = h // a failure is cached as nil, so one unreachable server is dialed once
	if err != nil {
		healthCache[base] = nil
		return nil, err
	}
	return h, nil
}

// ---- amounts ----

// parseAmount converts an amount as a person writes it into the whole base units the kernel counts
// in. The digits are shifted by hand: money never passes through floating point.
func parseAmount(s string, decimals uint8) (int64, error) {
	bad := kernel.ErrInvalidInput.Wrap("amount must be a positive whole number")
	if decimals > 0 {
		bad = kernel.ErrInvalidInput.Wrapf("amount must be positive, with at most %d decimal places", decimals)
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if !allDigits(whole) || (frac != "" && !allDigits(frac)) || len(frac) > int(decimals) {
		return 0, bad
	}
	v, err := strconv.ParseInt(whole+frac+strings.Repeat("0", int(decimals)-len(frac)), 10, 64)
	if err != nil || v <= 0 {
		return 0, bad
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
