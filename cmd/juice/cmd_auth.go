package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	authCmd := &cobra.Command{Use: "auth", Short: "Authentication commands"}
	authCmd.AddCommand(loginCmd(), logoutCmd(), refreshCmd())
	rootCmd.AddCommand(authCmd)
}

func loginCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "login <user>",
		Short: "Log in and store a bearer token (user is @handle)",
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
		"grant_type":    "authorization_code",
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
		Short: "Revoke the stored refresh token and remove local credentials",
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

func refreshCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Rotate the refresh token and get a new access token",
		RunE: func(_ *cobra.Command, _ []string) error {
			rt, err := loadRefreshToken()
			if err != nil {
				return kernel.ErrUnauthenticated.Wrap("no refresh token stored; run: juice auth login")
			}
			var out struct {
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
			}
			if err := apiCall(context.Background(), "POST", "/v1/auth/refresh",
				map[string]string{"refresh_token": rt}, &out); err != nil {
				return err
			}
			if err := saveToken(out.AccessToken); err != nil {
				return err
			}
			if err := saveRefreshToken(out.RefreshToken); err != nil {
				return err
			}
			fmt.Println("Token refreshed.")
			return nil
		},
	}
}
