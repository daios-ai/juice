package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
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

// userConnectCmd and userDisconnectCmd are registered under the `user` group in cmd.go, next to
// `user me` (which lists your connections). Connecting is also offered inline by `juice run`.
func userConnectCmd() *cobra.Command {
	var device bool
	cmd := &cobra.Command{
		Use:   "connect <action>",
		Short: "Connect your account so an action can act on your behalf against its upstream API",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if device {
				return connectDevice(args[0])
			}
			return runConsentFlow(args[0])
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "Use the device-code flow (no local browser)")
	return cmd
}

// grantStartResp is the /v1/grants/start response (both flows).
type grantStartResp struct {
	State                   string `json:"state"`
	AuthorizeURL            string `json:"authorize_url"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	UserCode                string `json:"user_code"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type grantCompleteResp struct {
	Status  string `json:"status"`
	Action  string `json:"action"`
	Created string `json:"created_at"`
}

// runConsentFlow runs the authorization-code + PKCE flow, hosting the loopback redirect listener
// locally (the browser reaches it even behind NAT — the provider never contacts the kernel).
// Shared by `user connect` and by `run`'s inline consent offer.
func runConsentFlow(actionRef string) error {
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
		"action": actionRef, "redirect_uri": redirectURI, "flow": "code",
	}, &start); err != nil {
		return err
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
	fmt.Printf("Connected %s.\n", done.Action)
	return nil
}

// connectDevice runs the device-code flow: the server polls the provider, the CLI polls the
// server's /complete until the connection lands.
func connectDevice(actionRef string) error {
	var start grantStartResp
	if err := apiCall(context.Background(), "POST", "/v1/grants/start", map[string]string{
		"action": actionRef, "flow": "device",
	}, &start); err != nil {
		return err
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
		if done.Status == "complete" {
			fmt.Printf("Connected %s.\n", done.Action)
			return nil
		}
	}
	return kernel.ErrTimeout.Wrap("timed out waiting for device authorization")
}

func userDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect <action>",
		Short: "Disconnect your account from an action (revoke its delegated access)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("DELETE", "/v1/grants?action="+url.QueryEscape(args[0]), nil)
		},
	}
}
