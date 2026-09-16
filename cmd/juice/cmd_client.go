// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
	bip39 "github.com/tyler-smith/go-bip39"
	"golang.org/x/term"
)

// The kernel noun: one verb runs a kernel from this installation, the rest are what this client
// knows about kernels it talks to — the same thing from either side, so one noun holds both.
func init() {
	kernelCmd := &cobra.Command{Use: "kernel", Short: "Run a kernel, or manage the ones this client knows"}
	kernelCmd.AddCommand(kernelServeCmd(), kernelAddCmd(), kernelListCmd(),
		kernelHealthCmd(), kernelForgetCmd())
	rootCmd.AddCommand(kernelCmd)
}

// kernelAddCmd registers a kernel by dialling it, and is also how a kernel that has moved is
// repointed: a key is what says which kernel this is, so the same key at a new address is the same
// kernel and keeps its logins. The name is the client's own label, defaulting to the nickname the
// kernel advertises — the name an operator has already seen is the one they will type — but a
// nickname is a label rather than proof, so a name held by another key is not taken.
func kernelAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add URL [NAME]",
		Short: "Register a kernel this client can talk to, or follow one that has moved",
		Long: "Register the kernel answering at URL, under NAME. Without NAME it is registered under the\n" +
			"nickname the kernel advertises. Adding does not log in and does not select anything:\n" +
			"`juice auth login USER@NAME` does that.\n\n" +
			"Adding a kernel already known under that name succeeds: the same kernel at the same\n" +
			"address changes nothing, and one that has moved has its address updated and keeps its\n" +
			"logins. A different kernel under a name already taken is refused; give it another name,\n" +
			"or `juice kernel forget NAME` first, which also removes that name's logins.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			name, k, outcome, err := registerKernel(context.Background(), name, args[0])
			if err != nil {
				return err
			}
			return emitKernel(name, k, outcome)
		},
	}
}

// kernelListCmd shows what this client knows, marking the kernel the selected login acts through.
func kernelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the kernels this client knows",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg := loadClientConfig()
			names := make([]string, 0, len(cfg.Kernels))
			for name := range cfg.Kernels {
				names = append(names, name)
			}
			sort.Strings(names)
			here, _ := cli.identity()
			rows := make([]map[string]any, 0, len(names))
			for _, name := range names {
				k := cfg.Kernels[name]
				rows = append(rows, map[string]any{"kernel": name, "endpoint": k.Endpoint,
					"network": k.Network, "public_key": k.PublicKey, "selected": name == here.Kernel})
			}
			body, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return emit(body, output{id: "kernel", human: func([]byte) error {
				fmt.Printf("  %-16s %-10s %-32s %s\n", "KERNEL", "NETWORK", "ADDRESS", "KEY")
				for _, name := range names {
					mark := " "
					if name == here.Kernel {
						mark = "*"
					}
					k := cfg.Kernels[name]
					fmt.Printf("%s %-16s %-10s %-32s %s\n", mark, name, k.Network, k.Endpoint, k.PublicKey)
				}
				return nil
			}})
		},
	}
}

// kernelHealthCmd reads a kernel's live banner: a registered kernel by name, else the one the
// selected login acts through. It needs no login, since what a server says about itself is public
// (§13) — which is why a client reads it before trusting an address.
func kernelHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health [NAME]",
		Short: "Check a kernel is up, and which kernel it is",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base := cli.resolve().base
			if len(args) == 1 {
				k := loadClientConfig().Kernels[args[0]]
				if k == nil {
					return kernel.ErrNotFound.Wrapf("no kernel named %s; add it with: juice kernel add URL %s", args[0], args[0])
				}
				base = k.Endpoint
			}
			if base == "" {
				return kernel.ErrInvalidInput.Wrap("name a kernel: juice kernel health NAME")
			}
			ctx := context.Background()
			body, status, err := doHTTP(ctx, "GET", base+"/health", nil, nil, 0, true)
			if err != nil {
				return errUnreachable(base, err)
			}
			if status != http.StatusOK {
				return kernel.ErrExecutionFailed.Wrapf("%s returned status %d", base, status)
			}
			h, err := decodeHealth(base, body)
			if err != nil {
				return err
			}
			return emit(body, output{id: "public_key", human: func([]byte) error {
				// The network comes first after the name: a kernel serves one for life, and it
				// decides what every balance and every signature here means (D23).
				fmt.Printf("ok  %s  network %s  %s\n", h.Handle, h.Network, h.PublicKey)
				return nil
			}})
		},
	}
}

// kernelForgetCmd removes what this client knows about a kernel, and the logins that only made
// sense there. It is `forget` rather than `delete` because nothing of the kernel's own is touched.
func kernelForgetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget NAME",
		Short: "Remove this client's record of a kernel, and its logins",
		Long: "Remove NAME from the kernels this client knows, along with the credentials of every\n" +
			"login on it. Nothing on the kernel itself is touched: accounts, actions and money are\n" +
			"its own, and it goes on serving whoever else knows it.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			cfg := loadClientConfig()
			if cfg.Kernels[name] == nil {
				return kernel.ErrNotFound.Wrapf("no kernel named %s", name)
			}
			if err := confirm(fmt.Sprintf("Forget %s and log out every login on it?", name), yes); err != nil {
				return err
			}
			delete(cfg.Kernels, name)
			if here, err := parseLogin(cfg.Current); err == nil && here.Kernel == name {
				cfg.Current = ""
			}
			forgetLogins(name)
			if err := saveClientConfig(cfg); err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{"kernel": name, "forgotten": true})
			if err != nil {
				return err
			}
			return emit(body, output{id: "kernel", human: func([]byte) error {
				fmt.Printf("Forgot %s\n", name)
				return nil
			}})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// registerKernel records a kernel after the server at that address has answered as itself, and
// returns what it did: added, already known, or moved. Nothing is written until the server has
// answered, so a name that cannot be dialled keeps whatever it meant before.
func registerKernel(ctx context.Context, name, url string) (string, *kernelRec, string, error) {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	h, err := cli.health(ctx, url)
	if err != nil {
		return "", nil, "", err
	}
	if name == "" {
		name = h.Handle
	}
	if err := validateLocalName("kernel", name); err != nil {
		return "", nil, "", err
	}
	cfg := loadClientConfig()
	existing := cfg.Kernels[name]
	outcome := "added"
	switch {
	case existing == nil:
	case sameKernel(name, existing, h) != nil:
		// A name is one kernel's here, on the network it was registered for. Taking it for another
		// would silently point every login and every reference made under it at a stranger, so the
		// operator says which they mean.
		return "", nil, "", sameKernel(name, existing, h)
	case existing.Endpoint == url:
		return name, existing, "already known", nil // nothing to do
	default:
		// The kernel answering is the one recorded — the same key, at whatever address it answers
		// on today — so it keeps its logins: a session belongs to the kernel that issued it, and
		// this is that kernel.
		outcome = "moved; existing logins kept"
	}
	k := &kernelRec{Endpoint: url, PublicKey: h.PublicKey, WorldDigest: h.Digest,
		Network: h.Network, Decimals: h.Decimals, Symbol: h.Symbol}
	cfg.Kernels[name] = k
	if err := saveClientConfig(cfg); err != nil {
		return "", nil, "", err
	}
	return name, k, outcome, nil
}

// forgetLogins removes the credentials of every login on one kernel, and unselects one that was
// selected. A session is only valid to the server that issued it, so a kernel this client no longer
// knows — or knows at another address — leaves nothing behind that could be sent anywhere.
func forgetLogins(kernelName string) {
	for _, l := range logins() {
		if l.Kernel != kernelName {
			continue
		}
		if path, err := credentialPath(l); err == nil {
			_ = os.Remove(path)
		}
	}
}

// emitKernel answers with the record that was registered, in the shape `kernel list` answers with,
// plus what registering did. The human line names the local name first, since that is the word
// every other command takes.
func emitKernel(name string, k *kernelRec, outcome string) error {
	body, err := json.Marshal(map[string]any{"kernel": name, "endpoint": k.Endpoint,
		"network": k.Network, "public_key": k.PublicKey, "outcome": outcome})
	if err != nil {
		return err
	}
	return emit(body, output{id: "kernel", human: func([]byte) error {
		fmt.Printf("%s  network %s  %s  %s  (%s)\n", name, k.Network, k.Endpoint, k.PublicKey, outcome)
		return nil
	}})
}

func init() {
	authCmd := &cobra.Command{Use: "auth", Short: "Log in, switch between logins, log out"}
	authCmd.AddCommand(loginCmd(), authUseCmd(), authListCmd(), logoutCmd(), recoverCmd())
	rootCmd.AddCommand(authCmd)
}

func loginCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "login USER@KERNEL",
		Short: "Log in on a kernel and act as that account",
		Long: "Log in as USER on KERNEL, and act as that login from now on. KERNEL is a kernel this\n" +
			"client knows — `juice kernel list` shows them, `juice kernel add URL` adds one.\n\n" +
			"A login is one account at one kernel. It says both who a command acts as and which\n" +
			"kernel it acts through, so nothing else has to be selected.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			_, c, err := namedClient(args[0])
			if err != nil {
				return err
			}
			// Before the password is asked for, let alone sent: a server answering at this address
			// that is not the kernel recorded here gets nothing.
			if err := c.verify(context.Background()); err != nil {
				return err
			}
			if password == "" {
				p, perr := promptPassword("Password: ")
				if perr != nil {
					return perr
				}
				password = p
			}
			return c.loginPKCE(password)
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	return cmd
}

// loginRecord is what the commands that make, switch or end a login answer with: the login, its
// halves, and whether the act reached the kernel. No token is ever in it — a credential is written
// to its 0600 file and nowhere else.
func loginRecord(l login, extra map[string]any) ([]byte, error) {
	row := map[string]any{"login": l.String(), "handle": l.Handle, "kernel": l.Kernel}
	for k, v := range extra {
		row[k] = v
	}
	return json.Marshal(row)
}

// authUseCmd switches to a login already held, without a password. The kernel is checked before
// the switch, so a server that is no longer the one that login was made at is refused rather than
// handed a credential.
func authUseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use USER@KERNEL",
		Short: "Act as a login you already hold",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			l, c, err := namedClient(args[0])
			if err != nil {
				return err
			}
			if c.creds.Token == "" && c.creds.RefreshToken == "" {
				return kernel.ErrUnauthenticated.Wrapf("no login %s here; log in with: juice auth login %s", l, l)
			}
			if err := c.selectLogin(context.Background()); err != nil {
				return err
			}
			return emitLogin(l, nil)
		},
	}
}

// emitLogin answers with one login under the one output policy: --quiet is the login, which is
// what a script pipes into --as, and the human line is the login alone.
func emitLogin(l login, extra map[string]any) error {
	body, err := loginRecord(l, extra)
	if err != nil {
		return err
	}
	return emit(body, output{id: "login", human: func([]byte) error { fmt.Println(l); return nil }})
}

// authListCmd lists the logins this client holds, marking the one in use. A login is its credential
// file, so this is what is on disk rather than a second list kept beside it.
func authListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the logins this client holds",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			here, _ := cli.identity()
			held := logins()
			rows := make([]map[string]any, 0, len(held))
			for _, l := range held {
				rows = append(rows, map[string]any{"login": l.String(), "handle": l.Handle, "kernel": l.Kernel,
					"principal_id": readCredentials(l).PrincipalID, "selected": l == here})
			}
			body, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return emit(body, output{id: "login", human: func([]byte) error {
				for _, l := range held {
					mark := " "
					if l == here {
						mark = "*"
					}
					fmt.Printf("%s %s\n", mark, l)
				}
				return nil
			}})
		},
	}
}

// loginPKCE performs the authorization code + PKCE flow against this client's kernel, as this
// client's login.
func (c *client) loginPKCE(password string) error {
	verifier, err := kernel.GenerateCodeVerifier()
	if err != nil {
		return err
	}
	challenge := kernel.CodeChallenge(verifier)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return kernel.ErrInternal.Wrapf("could not start loopback server: %v", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	codeCh := make(chan string, 1)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code := r.URL.Query().Get("code")
			if code != "" {
				codeCh <- code
				fmt.Fprintln(w, "Login successful. You may close this window.")
			} else {
				http.Error(w, "missing code", http.StatusBadRequest)
			}
		}),
	}
	go srv.Serve(ln)
	defer srv.Close()

	ctx := context.Background()
	if _, err := authPost(ctx, c.base, "/v1/auth/authorize", map[string]string{
		"handle":                c.login.Handle,
		"password":              password,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"redirect_uri":          redirectURI,
	}); err != nil {
		return err
	}

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(30 * time.Second):
		return kernel.ErrTimeout.Wrap("timed out waiting for authorization code")
	}

	tokens, err := authPost(ctx, c.base, "/v1/auth/token", map[string]string{
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	})
	if err != nil {
		return err
	}
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(tokens, &tokenResp); err != nil {
		return kernel.ErrExecutionFailed.Wrapf("decode token response: %v", err)
	}
	if tokenResp.AccessToken == "" {
		return kernel.ErrExecutionFailed.Wrap("server did not return an access token")
	}
	// The session is written whole, under the login just proved — never under whatever --as or
	// JUICE_AS happens to name — and only then selected: a selection pointing at a half-written
	// record, or at somebody else's tokens, is how one account ends up acting as another.
	c.creds.Token, c.creds.RefreshToken = tokenResp.AccessToken, tokenResp.RefreshToken
	if err := withCredentials(c.login, func(cr *credentials) (bool, error) {
		cr.Token, cr.RefreshToken = c.creds.Token, c.creds.RefreshToken
		return true, nil
	}); err != nil {
		return kernel.ErrInternal.Wrapf("could not save the session: %v", err)
	}
	if err := c.selectLogin(ctx); err != nil {
		return err
	}
	c.bindPrincipal()
	return emitLogin(c.login, nil)
}

// authPost sends one unauthenticated JSON request of the login exchange and returns the reply. It
// shares the path every other request takes, so the server's own refusal survives as a typed error
// rather than becoming a bare status number, and an unreachable server reads as one.
func authPost(ctx context.Context, server, path string, body map[string]string) ([]byte, error) {
	raw, _ := json.Marshal(body)
	respBody, status, err := doHTTP(ctx, "POST", strings.TrimRight(server, "/")+path,
		map[string]string{"Content-Type": "application/json"}, bytes.NewReader(raw), 0, true)
	if err != nil {
		return nil, errUnreachable(server, err)
	}
	if status != http.StatusOK && status != http.StatusFound {
		return nil, errorFromResponse(status, respBody)
	}
	return respBody, nil
}

// bindPrincipal records which account this login holds. The name on the file is a label — a handle
// can be renamed, and its old name taken by someone else — so the id the server reports is written
// beside the tokens, and it is the id, never the label, that says whose session this is (D15). It
// asks as this client, the login just proved: asking as whatever is selected would write one
// account's id beside another's tokens. Best effort: a login is complete without it.
func (c *client) bindPrincipal() {
	var me struct {
		ID string `json:"id"`
	}
	if err := c.call(context.Background(), "GET", "/v1/me", nil, &me); err != nil || me.ID == "" {
		return
	}
	_ = withCredentials(c.login, func(cr *credentials) (bool, error) {
		if cr.PrincipalID == me.ID {
			return false, nil
		}
		cr.PrincipalID = me.ID
		return true, nil
	})
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout [USER@KERNEL]",
		Short: "Log out, here or on a named login",
		Long: "End a session and forget its credentials. With no argument it is the login in use, and\n" +
			"nothing is selected afterwards — a command with no login says so rather than acting as\n" +
			"whoever else happens to be logged in.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			// The session belongs to the login being logged out, so everything here — the
			// revocation above all — acts as that login, on its own kernel, never as whichever
			// one happens to be selected.
			c := cli.resolve()
			if len(args) == 1 {
				var err error
				if _, c, err = namedClient(args[0]); err != nil {
					return err
				}
			}
			l, err := c.identity()
			if err != nil {
				return err
			}
			// Revoke at the server first, while the credentials are still here to prove who is
			// asking; then remove them locally whatever the server said, since a token this client
			// will not send again is one it should not keep.
			revoked := true
			if c.creds.RefreshToken != "" {
				revoked = c.atHome() && c.call(context.Background(), "POST", "/v1/auth/logout",
					map[string]string{"refresh_token": c.creds.RefreshToken}, nil) == nil
			}
			if path, perr := credentialPath(l); perr == nil {
				if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
					return rerr
				}
			}
			cfg := loadClientConfig()
			if cfg.Current == l.String() {
				cfg.Current = ""
				if serr := saveClientConfig(cfg); serr != nil {
					return serr
				}
			}
			body, berr := loginRecord(l, map[string]any{"revoked": revoked})
			if berr != nil {
				return berr
			}
			return emit(body, output{id: "login", human: func([]byte) error {
				if !revoked {
					// The credentials are gone from here either way, so saying "log out again"
					// would be advice this client can no longer take: say what is true instead.
					fmt.Printf("Removed %s from this computer. The kernel \"%s\" could not be reached, so the\n"+
						"session there could not be confirmed ended; it may remain valid until it expires.\n", l, l.Kernel)
					return nil
				}
				fmt.Printf("Logged out %s\n", l)
				return nil
			}})
		},
	}
}

// ---- Seed-phrase recovery (§12) ----

// promptMnemonic reads a recovery phrase (a full line, spaces included) from stdin. A package var so
// tests can script it.
var promptMnemonic = func(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// deriveRecoveryKey turns a BIP-39 mnemonic into the account's Ed25519 recovery keypair. The seed's
// first 32 bytes are the Ed25519 seed (no passphrase), so the same phrase always rederives the same
// key; the server only ever stores the public half (§12).
func deriveRecoveryKey(mnemonic string) (ed25519.PrivateKey, error) {
	mnemonic = strings.TrimSpace(mnemonic)
	if !bip39.IsMnemonicValid(mnemonic) {
		return nil, kernel.ErrInvalidInput.Wrap("invalid recovery phrase")
	}
	seed := bip39.NewSeed(mnemonic, "")
	return ed25519.NewKeyFromSeed(seed[:32]), nil
}

// generateRecovery creates a fresh 12-word mnemonic and returns it with the base64url public key to
// enroll. The mnemonic is the recovery credential; it never leaves the client (§12).
func generateRecovery() (mnemonic, recoveryPublicKey string, err error) {
	entropy, err := bip39.NewEntropy(128) // 128 bits of entropy => 12 words
	if err != nil {
		return "", "", kernel.ErrInternal.Wrapf("generate entropy: %v", err)
	}
	mnemonic, err = bip39.NewMnemonic(entropy)
	if err != nil {
		return "", "", kernel.ErrInternal.Wrapf("generate mnemonic: %v", err)
	}
	priv, err := deriveRecoveryKey(mnemonic)
	if err != nil {
		return "", "", err
	}
	pub := priv.Public().(ed25519.PublicKey)
	return mnemonic, base64.RawURLEncoding.EncodeToString(pub), nil
}

// runRecoveryCeremony is the whole enrollment sequence: generate the phrase, show it on out,
// wait for acknowledgment when ack is non-nil, then commit the public key. The phrase is shown
// BEFORE commit, so a crash between the two leaves an account without a recovery key — recoverable
// by password — rather than an enrolled key whose phrase nobody received; a failed commit is
// announced so the operator discards the phrase.
func runRecoveryCeremony(out io.Writer, ack io.Reader, label string, commit func(recoveryPublicKey string) error) error {
	mnemonic, recoveryPub, err := generateRecovery()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "%s (write this down; it is shown only once and cannot be recovered):\n", label)
	fmt.Fprintf(out, "  %s\n", mnemonic)
	if ack != nil {
		fmt.Fprint(out, "Press Enter once you have written it down: ")
		if _, err := bufio.NewReader(ack).ReadString('\n'); err != nil && err != io.EOF {
			return err
		}
	}
	if err := commit(recoveryPub); err != nil {
		fmt.Fprintln(out, "the recovery phrase shown above was NOT enrolled; discard it")
		return err
	}
	return nil
}

// enrollRecovery runs the ceremony on the controlling terminal when there is one, so the phrase
// is acknowledged before any further output and never enters a redirected stderr (log files).
// Headless (no terminal), the phrase goes to stderr without a pause: stdio is the only delivery
// channel a headless boot has, and the operator capturing it is receiving the phrase, not
// leaking it — a deliberate choice, not an oversight.
func enrollRecovery(label string, commit func(recoveryPublicKey string) error) error {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
			defer tty.Close()
			return runRecoveryCeremony(tty, tty, label, commit)
		}
	}
	return runRecoveryCeremony(os.Stderr, nil, label, commit)
}

// signRecoveryChallenge signs the recovery nonce with the phrase-derived key, matching the kernel's
// verification payload exactly (kernel.RecoveryChallenge, a disjoint signature domain). net carries
// the server's network digest, which the prefix binds the signature to (D23).
func signRecoveryChallenge(net kernel.Network, priv ed25519.PrivateKey, nonce string) (string, error) {
	payload, err := net.RecoveryChallengeSigningBytes(nonce)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)), nil
}

func recoverCmd() *cobra.Command {
	var phrase, newPassword string
	cmd := &cobra.Command{
		Use:   "recover USER@KERNEL",
		Short: "Reset a lost password using your recovery phrase",
		Long: "Reset a lost password using the 12-word recovery phrase printed when the account was\n" +
			"created. Recovering does not log you in: it sets a password, and `juice auth login`\n" +
			"then uses it.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			l, c, err := namedClient(args[0])
			if err != nil {
				return err
			}
			handle := l.Handle
			if phrase == "" {
				p, err := promptMnemonic("Recovery phrase: ")
				if err != nil {
					return err
				}
				phrase = p
			}
			priv, err := deriveRecoveryKey(phrase)
			if err != nil {
				return err
			}
			if newPassword == "" {
				p, err := promptNewPassword("New password: ")
				if err != nil {
					return err
				}
				newPassword = p
			}
			ctx := context.Background()
			var started struct {
				Nonce string `json:"nonce"`
			}
			if err := c.call(ctx, "POST", "/v1/auth/recover/start", map[string]any{"handle": handle}, &started); err != nil {
				return err
			}
			net, err := c.network(ctx)
			if err != nil {
				return err
			}
			sig, err := signRecoveryChallenge(net, priv, started.Nonce)
			if err != nil {
				return err
			}
			return c.emitCtx(ctx, "POST", "/v1/auth/recover/complete", map[string]any{
				"handle": handle, "nonce": started.Nonce, "signature": sig, "password": newPassword,
			}, output{})
		},
	}
	cmd.Flags().StringVar(&phrase, "phrase", "", "Recovery phrase (prompted if omitted)")
	cmd.Flags().StringVar(&newPassword, "password", "", "New password (prompted if omitted)")
	return cmd
}
