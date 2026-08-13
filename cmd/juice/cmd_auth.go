package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
	bip39 "github.com/tyler-smith/go-bip39"
)

func init() {
	authCmd := &cobra.Command{Use: "auth", Short: "Manage authentication"}
	authCmd.AddCommand(loginCmd(), logoutCmd(), recoverCmd())
	rootCmd.AddCommand(authCmd)
}

func loginCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "login <user>",
		Short: "Log in",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle := args[0]
			if password == "" {
				p, err := promptPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			return loginPKCE(handle, password, serverBaseURL())
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	return cmd
}

// loginPKCE performs the authorization code + PKCE flow against a running juice server.
func loginPKCE(handle, password, server string) error {
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
		"handle":                handle,
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
	if err := saveToken(tokenResp.AccessToken); err != nil {
		return kernel.ErrInternal.Wrapf("could not save token: %v", err)
	}
	if tokenResp.RefreshToken != "" {
		_ = saveRefreshToken(tokenResp.RefreshToken)
	}
	fmt.Println("Logged in.")
	return nil
}

func logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out",
		RunE: func(_ *cobra.Command, _ []string) error {
			if rt, err := loadRefreshToken(); err == nil {
				_ = apiCall(context.Background(), "POST", "/v1/auth/logout",
					map[string]string{"refresh_token": rt}, nil)
			}
			if err := removeToken(); err != nil && !os.IsNotExist(err) {
				return err
			}
			_ = removeRefreshToken()
			fmt.Println("Logged out.")
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
// enroll. The mnemonic is the master secret; it never leaves the client (§12).
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

// signRecoveryChallenge signs the recovery nonce with the phrase-derived key, matching the kernel's
// verification payload exactly (kernel.RecoveryChallenge, a disjoint signature domain).
func signRecoveryChallenge(priv ed25519.PrivateKey, nonce string) (string, error) {
	payload, err := kernel.RecoveryChallengeSigningBytes(nonce)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload)), nil
}

func recoverCmd() *cobra.Command {
	var phrase, newPassword string
	cmd := &cobra.Command{
		Use:   "recover <user>",
		Short: "Reset a lost password using your recovery phrase",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle := args[0]
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
			sig, err := signRecoveryChallenge(priv, started.Nonce)
			if err != nil {
				return err
			}
			return apiEmit("POST", "/v1/auth/recover/complete", map[string]any{
				"handle": handle, "nonce": started.Nonce, "signature": sig, "password": newPassword,
			})
		},
	}
	cmd.Flags().StringVar(&phrase, "phrase", "", "Recovery phrase (prompted if omitted)")
	cmd.Flags().StringVar(&newPassword, "password", "", "New password (prompted if omitted)")
	return cmd
}
