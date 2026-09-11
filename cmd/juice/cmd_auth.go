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
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
	bip39 "github.com/tyler-smith/go-bip39"
	"golang.org/x/term"
)

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
			l, err := namedLogin(args[0])
			if err != nil {
				return err
			}
			if password == "" {
				p, perr := promptPassword("Password: ")
				if perr != nil {
					return perr
				}
				password = p
			}
			return loginPKCE(l, password, serverBaseURL())
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	return cmd
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
			l, err := parseLogin(args[0])
			if err != nil {
				return err
			}
			if c := readCredentials(l); c.Token == "" && c.RefreshToken == "" {
				return kernel.ErrUnauthenticated.Wrapf("no login %s here; log in with: juice auth login %s", l, l)
			}
			if err := selectLogin(context.Background(), l); err != nil {
				return err
			}
			fmt.Println(l)
			return nil
		},
	}
}

// authListCmd lists the logins this client holds, marking the one in use. A login is its credential
// file, so this is what is on disk rather than a second list kept beside it.
func authListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the logins this client holds",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			here, _, _ := selected()
			held := logins()
			if flagJSON {
				out := make([]map[string]any, 0, len(held))
				for _, l := range held {
					out = append(out, map[string]any{"login": l.String(), "handle": l.Handle, "kernel": l.Kernel,
						"principal_id": readCredentials(l).PrincipalID, "selected": l == here})
				}
				return printJSON(out)
			}
			for _, l := range held {
				mark := " "
				if l == here {
					mark = "*"
				}
				fmt.Printf("%s %s\n", mark, l)
			}
			return nil
		},
	}
}

// loginPKCE performs the authorization code + PKCE flow against a running juice server.
func loginPKCE(l login, password, server string) error {
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

	authBody, _ := json.Marshal(map[string]string{
		"handle":                l.Handle,
		"password":              password,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"redirect_uri":          redirectURI,
	})
	resp, err := http.Post(strings.TrimRight(server, "/")+"/v1/auth/authorize", "application/json", bytes.NewReader(authBody))
	if err != nil {
		return errUnreachable(server, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return kernel.ErrExecutionFailed.Wrapf("authorize failed: status %d", resp.StatusCode)
	}

	var code string
	select {
	case code = <-codeCh:
	case <-time.After(30 * time.Second):
		return kernel.ErrTimeout.Wrap("timed out waiting for authorization code")
	}

	tokenBody, _ := json.Marshal(map[string]string{
		"code":          code,
		"code_verifier": verifier,
		"redirect_uri":  redirectURI,
	})
	resp2, err := http.Post(strings.TrimRight(server, "/")+"/v1/auth/token", "application/json", bytes.NewReader(tokenBody))
	if err != nil {
		return errUnreachable(server, err)
	}
	defer resp2.Body.Close()

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := decodeJSON(resp2.Body, &tokenResp); err != nil {
		return kernel.ErrExecutionFailed.Wrapf("decode token response: %v", err)
	}
	if tokenResp.AccessToken == "" {
		return kernel.ErrExecutionFailed.Wrap("server did not return an access token")
	}
	// Logging in is what makes a login, so it is also what selects it: the tokens are stored under
	// the name just proved, and every later command acts as it until another is chosen.
	if err := selectLogin(context.Background(), l); err != nil {
		return err
	}
	if err := saveToken(tokenResp.AccessToken); err != nil {
		return kernel.ErrInternal.Wrapf("could not save token: %v", err)
	}
	if tokenResp.RefreshToken != "" {
		_ = saveRefreshToken(tokenResp.RefreshToken)
	}
	bindPrincipal(l)
	fmt.Println(l)
	return nil
}

// bindPrincipal records which account this login holds. The name on the file is a label — a handle
// can be renamed, and its old name taken by someone else — so the id the server reports is written
// beside the tokens, and it is the id, never the label, that says whose session this is (D15).
// Best effort: a login is complete without it, and the next login records it.
func bindPrincipal(l login) {
	var me struct {
		ID string `json:"id"`
	}
	if err := apiCall(context.Background(), "GET", "/v1/me", nil, &me); err != nil || me.ID == "" {
		return
	}
	_ = withCredentials(l, func(c *credentials) (bool, error) {
		if c.PrincipalID == me.ID {
			return false, nil
		}
		c.PrincipalID = me.ID
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
			l, _, err := selected()
			if len(args) == 1 {
				l, err = namedLogin(args[0])
			}
			if err != nil {
				return err
			}
			// Revoke at the server first, while the credentials are still here to prove who is
			// asking; then remove them locally whatever the server said, since a token this client
			// will not send again is one it should not keep.
			if c := readCredentials(l); c.RefreshToken != "" && atHome(serverBaseURL()) {
				_ = apiCall(context.Background(), "POST", "/v1/auth/logout",
					map[string]string{"refresh_token": c.RefreshToken}, nil)
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
			fmt.Printf("Logged out %s\n", l)
			return nil
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
			l, err := namedLogin(args[0])
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
			if err := apiCall(ctx, "POST", "/v1/auth/recover/start", map[string]any{"handle": handle}, &started); err != nil {
				return err
			}
			net, err := serverNetwork(ctx)
			if err != nil {
				return err
			}
			sig, err := signRecoveryChallenge(net, priv, started.Nonce)
			if err != nil {
				return err
			}
			var view json.RawMessage
			if err := apiCall(ctx, "POST", "/v1/auth/recover/complete", map[string]any{
				"handle": handle, "nonce": started.Nonce, "signature": sig, "password": newPassword,
			}, &view); err != nil {
				return err
			}
			return emitRaw(view)
		},
	}
	cmd.Flags().StringVar(&phrase, "phrase", "", "Recovery phrase (prompted if omitted)")
	cmd.Flags().StringVar(&newPassword, "password", "", "New password (prompted if omitted)")
	return cmd
}
