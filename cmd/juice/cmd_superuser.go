package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func parseAmount(s string) (int64, error) {
	amount, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, kernel.ErrInvalidInput.Wrap("amount must be a positive integer")
	}
	return amount, nil
}

// admin and peer are superuser supervision commands. They are clients of the local Unix
// control socket served by `juice serve` (ctlCall/ctlEmit route over it); the server
// enforces superuser and is the only process that touches SQLite (§14).

func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Admin management commands (superuser only)"}
	adminCmd.AddCommand(
		adminUsersCmd(),
		adminShowCmd(),
		adminSuspendCmd(),
		adminUnsuspendCmd(),
		adminDepositCmd(),
		adminWithdrawCmd(),
		adminActionsCmd(),
		adminDisableCmd(),
		adminProcessesCmd(),
		adminTxsCmd(),
		adminStepsCmd(),
	)
	rootCmd.AddCommand(adminCmd)
}

func init() {
	peerCmd := &cobra.Command{Use: "peer", Short: "Manage peer kernels and federation"}
	peerCmd.AddCommand(peerInspectCmd(), peerFriendCmd(), peerUnfriendCmd(), peerListCmd())
	rootCmd.AddCommand(peerCmd)
}

func adminUsersCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "users",
		Short: "List all users",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var users []*kernel.User
			if err := ctlCall(context.Background(), "GET", ctlPath("/control/users", limit, offset), nil, &users); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(users)
			}
			for _, u := range users {
				suspended := ""
				if u.SuspendedAt != nil {
					suspended = " [SUSPENDED]"
				}
				fmt.Printf("%-20s  %s%s\n", u.Handle, u.Email, suspended)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <user>",
		Short: "Show user details (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return ctlEmit("GET", "/control/users/"+url.PathEscape(args[0]), nil)
		},
	}
}

func adminSuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <user>",
		Short: "Suspend a user account (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := ctlCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/suspend", nil, nil); err != nil {
				return err
			}
			fmt.Printf("User %s suspended.\n", kernel.NormalizeHandle(args[0]))
			return nil
		},
	}
}

func adminUnsuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsuspend <user>",
		Short: "Unsuspend a user account (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := ctlCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/unsuspend", nil, nil); err != nil {
				return err
			}
			fmt.Printf("User %s unsuspended.\n", kernel.NormalizeHandle(args[0]))
			return nil
		},
	}
}

// adjustCmd builds the shared deposit/withdraw command (path is /control/deposit|withdraw).
func adjustCmd(use, short, path string) *cobra.Command {
	var reason, externalKey string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			amount, err := parseAmount(args[1])
			if err != nil {
				return err
			}
			return ctlEmit("POST", path, map[string]any{
				"handle": args[0], "amount": amount, "reason": reason, "external_key": externalKey,
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().StringVar(&externalKey, "external-key", "", "Optional idempotency token from the out-of-band payment system")
	return cmd
}

func adminDepositCmd() *cobra.Command {
	return adjustCmd("deposit <user> <amount>", "Add credits to a user account (user is @handle)", "/control/deposit")
}

func adminWithdrawCmd() *cobra.Command {
	return adjustCmd("withdraw <user> <amount>", "Deduct credits from a user account (user is @handle)", "/control/withdraw")
}

func adminActionsCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "actions",
		Short: "List all actions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var actions []*kernel.Action
			if err := ctlCall(context.Background(), "GET", ctlPath("/control/actions", limit, offset), nil, &actions); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(actions)
			}
			for _, a := range actions {
				active, public := " ", " "
				if a.Active {
					active = "*"
				}
				if a.Public {
					public = "P"
				}
				fmt.Printf("[%s%s] %s/%s  %d credits\n", active, public, a.OwnerHandle, a.Name, a.Price)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <action>",
		Short: "Disable any action (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := ctlCall(context.Background(), "POST", "/control/actions/disable",
				map[string]any{"ref": args[0]}, nil); err != nil {
				return err
			}
			fmt.Printf("Action %s disabled.\n", args[0])
			return nil
		},
	}
}

func adminProcessesCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "processes",
		Short: "List all processes",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var processes []*kernel.Process
			if err := ctlCall(context.Background(), "GET", ctlPath("/control/processes", limit, offset), nil, &processes); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(processes)
			}
			for _, p := range processes {
				fmt.Printf("%s  owner=%s  status=%-6s  avail=%d\n", p.ID, p.OwnerUserID, p.Status, p.Available)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminTxsCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "txs",
		Short: "List all transactions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var rows []adminTxRow
			if err := ctlCall(context.Background(), "GET", ctlPath("/control/txs", limit, offset), nil, &rows); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(rows)
			}
			for _, r := range rows {
				fmt.Printf("%s  %s  payer=%s  caller=%s  status=%-7s  gross=%d\n",
					r.ID, r.ActionRef, r.PayerHandle, r.CallerHandle, r.Status, r.Gross)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminStepsCmd() *cobra.Command {
	var processID, status string
	cmd := &cobra.Command{
		Use:   "steps",
		Short: "List all steps",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if processID != "" {
				q.Set("process", processID)
			}
			if status != "" {
				q.Set("status", status)
			}
			path := "/control/steps"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var steps []*kernel.Step
			if err := ctlCall(context.Background(), "GET", path, nil, &steps); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(steps)
			}
			for _, s := range steps {
				txID := "-"
				if s.TxID != nil {
					txID = *s.TxID
				}
				fmt.Printf("%s  status=%-7s  tx=%s\n", s.ID, s.Status, txID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	return cmd
}

func peerInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <url>",
		Short: "Show a remote kernel's identity and public actions (no auth, no DB write)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle    string `json:"handle"`
				PublicKey string `json:"public_key"`
				BaseURL   string `json:"base_url"`
				Actions   []struct {
					Name  string `json:"name"`
					Price int64  `json:"price"`
					Uses  int64  `json:"uses"`
				} `json:"actions"`
				Friends []struct {
					Handle  string `json:"handle"`
					BaseURL string `json:"base_url"`
				} `json:"friends"`
			}
			if err := ctlCall(context.Background(), "GET", "/control/peers/inspect?url="+url.QueryEscape(args[0]), nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fp := out.PublicKey
			if len(fp) > 16 {
				fp = fp[:16] + "…"
			}
			fmt.Printf("Handle:     %s\n", out.Handle)
			fmt.Printf("Public key: %s\n", fp)
			fmt.Printf("Base URL:   %s\n", out.BaseURL)
			if len(out.Actions) > 0 {
				fmt.Printf("\nActive actions (%d):\n", len(out.Actions))
				for _, a := range out.Actions {
					fmt.Printf("  %-30s  %d credits  (uses: %d)\n", a.Name, a.Price, a.Uses)
				}
			}
			if len(out.Friends) > 0 {
				fmt.Printf("\nTransacted friends (%d):\n", len(out.Friends))
				for _, f := range out.Friends {
					fmt.Printf("  %s  %s\n", f.Handle, f.BaseURL)
				}
			}
			return nil
		},
	}
}

func peerFriendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "friend <url>",
		Short: "Befriend a remote kernel: register as peer and import all their active public actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle   string `json:"handle"`
				Imported int    `json:"imported"`
				Skipped  int    `json:"skipped"`
			}
			if err := ctlCall(context.Background(), "POST", "/control/peers/friend",
				map[string]any{"url": args[0]}, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			msg := fmt.Sprintf("Friended %s", out.Handle)
			if out.Imported > 0 {
				msg += fmt.Sprintf(" — %d action(s) available", out.Imported)
			}
			if out.Skipped > 0 {
				msg += fmt.Sprintf(", %d skipped", out.Skipped)
			}
			fmt.Println(msg + ".")
			return nil
		},
	}
}

func peerUnfriendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unfriend <user>",
		Short: "Unfriend a peer: deny their calls and deactivate their proxy actions (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			handle := kernel.NormalizeHandle(args[0])
			if err := ctlCall(context.Background(), "POST", "/control/peers/unfriend",
				map[string]any{"handle": handle}, nil); err != nil {
				return err
			}
			fmt.Printf("Unfriended %s.\n", handle)
			return nil
		},
	}
}

func peerListCmd() *cobra.Command {
	var showGossip bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known remote kernel peers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := "/control/peers"
			if showGossip {
				path += "?gossip=1"
			}
			var out struct {
				Peers      []*kernel.User             `json:"peers"`
				Discovered []*kernel.DiscoveredKernel `json:"discovered"`
			}
			if err := ctlCall(context.Background(), "GET", path, nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out.Peers)
			}
			if len(out.Peers) == 0 {
				fmt.Println("No peers registered.")
			} else {
				fmt.Printf("%-20s %-36s %s\n", "HANDLE", "ID", "BASE_URL")
				for _, p := range out.Peers {
					denied := ""
					if p.DeniedAt != nil {
						denied = " [denied]"
					}
					fmt.Printf("%-20s %-36s %s%s\n", p.Handle, p.ID, p.RemoteBaseURL, denied)
				}
			}
			if showGossip && len(out.Discovered) > 0 {
				peerHandle := map[string]string{}
				for _, p := range out.Peers {
					if len(p.PublicKey) >= 16 {
						peerHandle[p.PublicKey[:16]] = p.Handle
					}
				}
				fmt.Println("\nDiscovered via gossip:")
				for _, d := range out.Discovered {
					fp := d.IntroducedBy
					if len(fp) > 16 {
						fp = fp[:16]
					}
					via := fp
					if h, ok := peerHandle[fp]; ok {
						via = fp + " (" + h + ")"
					}
					fmt.Printf("  %-20s %-50s (via %s)\n", d.Handle, d.BaseURL, via)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showGossip, "gossip", false, "Also show gossip-discovered kernels")
	return cmd
}
