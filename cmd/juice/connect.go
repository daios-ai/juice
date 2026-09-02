package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// openBrowser best-effort launches the system browser at url (argv exec, no shell — no injection
// surface; url derives from the action's already-validated auth_url). The URL is always printed
// too, so this failing is harmless. Returns whether a launcher was invoked.
func openBrowser(url string) bool {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	if _, err := exec.LookPath(cmd); err != nil {
		return false
	}
	_ = exec.Command(cmd, args...).Start()
	return true
}

// ---- grant/consent wire shapes (§8) ----
//
// The consent plan is decoded straight into kernel.ConsentPlan (single source of truth); only the
// start/complete/attach responses, which the service layer returns as ad-hoc JSON, are local here.

type grantStartResp struct {
	Status                  string   `json:"status"` // "granted" on success, "pending" mid-device-flow
	Actions                 []string `json:"actions"`
	State                   string   `json:"state"`
	AuthorizeURL            string   `json:"authorize_url"`
	VerificationURI         string   `json:"verification_uri"`
	VerificationURIComplete string   `json:"verification_uri_complete"`
	UserCode                string   `json:"user_code"`
	Interval                int      `json:"interval"`
	ExpiresIn               int      `json:"expires_in"`
}

type grantCompleteResp struct {
	Status   string   `json:"status"`
	Provider string   `json:"provider"`
	Actions  []string `json:"actions"`
	Created  string   `json:"created_at"`
}

// userConnectCmd and userDisconnectCmd are registered under the `user` group in cmd.go, next to
// `user me` (which lists your connections). Connecting is also offered inline by `juice run`.
//
// connect walks a consent plan: one selector, one gesture per upstream account (§8). A trailing
// /* on the selector is accepted. --token connects a delegated_bearer group with a pasted token.
func userConnectCmd() *cobra.Command {
	var device bool
	var token string
	var yes bool
	cmd := &cobra.Command{
		Use:   "connect SELECTOR",
		Short: "Connect your account so actions can act on your behalf upstream (selector: owner, owner/dir, or owner/name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			selector := args[0]
			// --token: connect the selector's delegated_bearer group with a pasted static token
			// (prompt without echo if the flag is present but empty, keeping it out of argv/history).
			if cmd.Flags().Changed("token") {
				if token == "" {
					var err error
					if token, err = promptSecret("Paste token: "); err != nil {
						return err
					}
				}
				return connectToken(selector, "", token)
			}
			return connectSelector(selector, device, yes)
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "Use the device-code flow for OAuth groups (no local browser)")
	cmd.Flags().StringVar(&token, "token", "", "Store a static token for a delegated_bearer group; empty value prompts without echo")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt (accept the shown consent plan)")
	return cmd
}

// connectSelector fetches the consent plan, shows the delta, and covers each group needing work
// with one gesture: a token paste per bearer group, one browser consent per OAuth group (§8).
func connectSelector(selector string, device, yes bool) error {
	var plan kernel.ConsentPlan
	if err := apiCall(context.Background(), "GET", "/v1/grants/plan?selector="+url.QueryEscape(selector), nil, &plan); err != nil {
		return err
	}
	var todo []kernel.ConsentGroup
	for _, g := range plan.Groups {
		if groupNeedsWork(g) {
			todo = append(todo, g)
		}
	}
	if len(todo) == 0 {
		fmt.Println("Already connected.")
		return nil
	}
	printDelta(todo)
	if err := confirmProceed(yes); err != nil {
		return err
	}
	for _, g := range todo {
		if g.Scheme == kernel.AuthSchemeDelegatedBearer {
			tok := ""
			if !g.Connected {
				var err error
				if tok, err = promptSecret(fmt.Sprintf("Paste token for %s: ", g.Provider)); err != nil {
					return err
				}
			}
			if err := connectToken(selector, g.ProviderKey, tok); err != nil {
				return err
			}
			continue
		}
		if err := connectOAuthGroup(selector, g.ProviderKey, device); err != nil {
			return err
		}
	}
	return nil
}

// groupNeedsWork reports whether a plan group has anything to connect: an uncovered account, or a
// covered account with an action not yet granted (a later sibling to instant-grant).
func groupNeedsWork(g kernel.ConsentGroup) bool {
	if !g.Covered {
		return true
	}
	for _, a := range g.Actions {
		if !a.Granted {
			return true
		}
	}
	return false
}

func printDelta(todo []kernel.ConsentGroup) {
	fmt.Println("The following will be connected:")
	for _, g := range todo {
		how := "paste a token"
		if g.Scheme == kernel.AuthSchemeOAuthDelegated {
			how = "one browser sign-in"
		} else if g.Connected {
			how = "already connected"
		}
		fmt.Printf("  %s (%s):\n", g.Provider, how)
		if len(g.Destinations) > 0 {
			// The recipient of your credential — for OAuth this is the action's own upstream host,
			// which need not be the login provider. Shown so the destination is never hidden (§8).
			fmt.Printf("    → sends your credential to: %s\n", strings.Join(g.Destinations, ", "))
		}
		for _, a := range g.Actions {
			mark := " "
			if a.Granted {
				mark = "✓"
			}
			fmt.Printf("    [%s] %s\n", mark, a.Action)
		}
	}
}

// confirmProceed returns nil to proceed. With --yes it always proceeds; on a terminal it asks; off
// a terminal without --yes it refuses (the shown plan is the consent act — never auto-confirm).
func confirmProceed(yes bool) error {
	if yes {
		return nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return kernel.ErrInvalidInput.Wrap("re-run with --yes to accept the consent plan (no terminal to confirm)")
	}
	if !promptYesNo("Proceed?") {
		return kernel.ErrInvalidInput.Wrap("aborted")
	}
	return nil
}

// connectToken stores a static token for a delegated_bearer group via POST /v1/grants (§8). An
// empty token instant-grants against an already-connected account.
func connectToken(selector, provider, token string) error {
	body := map[string]string{"selector": selector, "token": token}
	if provider != "" {
		body["provider"] = provider
	}
	var done grantCompleteResp
	if err := apiCall(context.Background(), "POST", "/v1/grants", body, &done); err != nil {
		return err
	}
	fmt.Printf("Connected %s.\n", strings.Join(done.Actions, ", "))
	return nil
}

// promptSecret reads a secret from the terminal without echoing it; if stdin is not a terminal it
// reads one trimmed line (so `echo $PAT | juice user connect … --token` works in scripts).
func promptSecret(prompt string) (string, error) {
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, prompt)
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", kernel.ErrInvalidInput.Wrapf("could not read token: %v", err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	s := bufio.NewScanner(os.Stdin)
	if s.Scan() {
		return strings.TrimSpace(s.Text()), nil
	}
	return "", kernel.ErrInvalidInput.Wrap("no token provided")
}

// connectOAuthGroup connects one oauth_delegated provider group: an already-covered account grants
// instantly (no browser); otherwise the loopback code flow (or device flow) drives one consent.
func connectOAuthGroup(selector, provider string, device bool) error {
	if device {
		return connectOAuthDevice(selector, provider)
	}
	return connectOAuthCode(selector, provider)
}

// connectOAuthCode runs the authorization-code + PKCE flow, hosting the loopback redirect listener
// locally (the browser reaches it even behind NAT — the provider never contacts the kernel).
func connectOAuthCode(selector, provider string) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return kernel.ErrInternal.Wrapf("could not start loopback server: %v", err)
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	type cb struct{ code, state string }
	cbCh := make(chan cb, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			return
		}
		cbCh <- cb{code: code, state: r.URL.Query().Get("state")}
		fmt.Fprintln(w, "Authorization received. You may close this window.")
	})}
	go srv.Serve(ln)
	defer srv.Close()

	var start grantStartResp
	if err := apiCall(context.Background(), "POST", "/v1/grants/start", map[string]string{
		"selector": selector, "provider": provider, "redirect_uri": redirectURI, "flow": "code",
	}, &start); err != nil {
		return err
	}
	if start.Status == "granted" {
		fmt.Printf("Connected %s.\n", strings.Join(start.Actions, ", "))
		return nil
	}
	fmt.Printf("Open this URL to authorize:\n\n  %s\n\n", start.AuthorizeURL)
	openBrowser(start.AuthorizeURL)

	var got cb
	select {
	case got = <-cbCh:
	case <-time.After(5 * time.Minute):
		return kernel.ErrTimeout.Wrap("timed out waiting for authorization")
	}
	if got.state != start.State {
		return kernel.ErrUnauthenticated.Wrap("state mismatch — aborting")
	}

	var done grantCompleteResp
	if err := apiCall(context.Background(), "POST", "/v1/grants/complete", map[string]string{
		"state": start.State, "code": got.code,
	}, &done); err != nil {
		return err
	}
	fmt.Printf("Connected %s.\n", strings.Join(done.Actions, ", "))
	return nil
}

// connectOAuthDevice runs the device-code flow: the server polls the provider, the CLI polls the
// server's /complete until the connection lands.
func connectOAuthDevice(selector, provider string) error {
	var start grantStartResp
	if err := apiCall(context.Background(), "POST", "/v1/grants/start", map[string]string{
		"selector": selector, "provider": provider, "flow": "device",
	}, &start); err != nil {
		return err
	}
	if start.Status == "granted" {
		fmt.Printf("Connected %s.\n", strings.Join(start.Actions, ", "))
		return nil
	}
	target := start.VerificationURIComplete
	if target == "" {
		target = start.VerificationURI
	}
	fmt.Printf("Go to %s and enter code: %s\n", target, start.UserCode)

	interval := start.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)
		var done grantCompleteResp
		if err := apiCall(context.Background(), "POST", "/v1/grants/complete",
			map[string]string{"state": start.State}, &done); err != nil {
			return err
		}
		if done.Status == "granted" {
			fmt.Printf("Connected %s.\n", strings.Join(done.Actions, ", "))
			return nil
		}
	}
	return kernel.ErrTimeout.Wrap("timed out waiting for device authorization")
}

// userDisconnectCmd revokes by selector (grants only) or, with --account, a whole upstream account
// and all its grants (§8).
func userDisconnectCmd() *cobra.Command {
	var account string
	cmd := &cobra.Command{
		Use:   "disconnect [SELECTOR]",
		Short: "Disconnect actions by selector (owner, owner/dir, or owner/name), or a whole upstream account (--account)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if account != "" {
				return apiEmit("DELETE", "/v1/grants?account="+url.QueryEscape(account), nil)
			}
			if len(args) != 1 {
				return kernel.ErrInvalidInput.Wrap("a selector or --account is required")
			}
			return apiEmit("DELETE", "/v1/grants?selector="+url.QueryEscape(args[0]), nil)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "Disconnect a whole upstream account (provider) and all its grants")
	return cmd
}
