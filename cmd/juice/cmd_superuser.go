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

// admin holds the superuser-only supervisory verbs — the operations no ordinary user ever
// performs: money (deposit/withdraw), access (suspend/unsuspend), federation trust
// (friend/unfriend/peers/inspect), and the global roster (users/show). They are ordinary TCP
// clients like every other command (apiCall/apiEmit); the server gates the routes with
// requireSuperuserMW, so authority is the @sys bearer token (§14).
//
// Everything that is merely "the same operation with wider reach" is NOT here: a superuser
// sees all rows on `action/process/tx/step list` and may `action disable` any action, all
// over the normal TCP API (supervision is scope, not a separate surface).
func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Superuser supervision"}
	adminCmd.AddCommand(
		adminUsersCmd(),
		adminShowCmd(),
		adminSuspendCmd(),
		adminUnsuspendCmd(),
		adminDepositCmd(),
		adminWithdrawCmd(),
		peerFriendCmd(),
		peerUnfriendCmd(),
		peerListCmd(),
		peerInspectCmd(),
		identityCmd(),
	)
	rootCmd.AddCommand(adminCmd)
}

// identityCmd prints this kernel's own federation identity: its public key (which peers use to
// friend it), handle, and libp2p listen addresses. Federation no longer exposes a .well-known
// document, so this is how an operator learns the key to share.
func identityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "identity",
		Short: "Show this kernel's federation identity",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var out struct {
				Handle    string   `json:"handle"`
				PublicKey string   `json:"public_key"`
				Addrs     []string `json:"addrs"`
			}
			if err := apiCall(context.Background(), "GET", "/control/identity", nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("Handle:     %s\n", out.Handle)
			fmt.Printf("Public key: %s\n", out.PublicKey)
			if len(out.Addrs) > 0 {
				fmt.Println("Listen addresses:")
				for _, a := range out.Addrs {
					fmt.Printf("  %s\n", a)
				}
			}
			return nil
		},
	}
}

func adminUsersCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "users",
		Short: "List users",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var users []*kernel.User
			if err := apiCall(context.Background(), "GET", ctlPath("/control/users", limit, offset), nil, &users); err != nil {
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
		Short: "Show user details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/control/users/"+url.PathEscape(args[0]), nil)
		},
	}
}

func adminSuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <user>",
		Short: "Suspend a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/suspend", nil, nil); err != nil {
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
		Short: "Unsuspend a user",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/unsuspend", nil, nil); err != nil {
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
			return apiEmit("POST", path, map[string]any{
				"handle": args[0], "amount": amount, "reason": reason, "external_key": externalKey,
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().StringVar(&externalKey, "external-key", "", "Optional idempotency token")
	return cmd
}

func adminDepositCmd() *cobra.Command {
	return adjustCmd("deposit <user> <amount>", "Add credits to a user", "/control/deposit")
}

func adminWithdrawCmd() *cobra.Command {
	return adjustCmd("withdraw <user> <amount>", "Deduct credits from a user", "/control/withdraw")
}

func peerInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <key|handle>",
		Short: "Inspect a remote kernel (by key, or @handle if already friended)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle    string `json:"handle"`
				PublicKey string `json:"public_key"`
				Actions   []struct {
					Name  string `json:"name"`
					Price int64  `json:"price"`
					Uses  int64  `json:"uses"`
				} `json:"actions"`
				Friends []struct {
					Handle    string `json:"handle"`
					PublicKey string `json:"public_key"`
					Actions   []struct {
						Name string `json:"name"`
						Uses int64  `json:"uses"`
					} `json:"actions"`
				} `json:"friends"`
				Reachability struct {
					Path      string `json:"path"`
					RTTmillis int64  `json:"rtt_millis"`
				} `json:"reachability"`
			}
			if err := apiCall(context.Background(), "GET", "/control/peers/inspect?key="+url.QueryEscape(args[0]), nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("Handle:       %s\n", out.Handle)
			fmt.Printf("Public key:   %s\n", out.PublicKey)
			fmt.Printf("Reachability: %s (%dms)\n", out.Reachability.Path, out.Reachability.RTTmillis)
			if len(out.Actions) > 0 {
				fmt.Printf("\nActive actions (%d):\n", len(out.Actions))
				for _, a := range out.Actions {
					fmt.Printf("  %-30s  %d credits  (uses: %d)\n", a.Name, a.Price, a.Uses)
				}
			}
			if len(out.Friends) > 0 {
				fmt.Printf("\nTransacted friends (%d):\n", len(out.Friends))
				for _, f := range out.Friends {
					fmt.Printf("  %-20s %s\n", f.Handle, f.PublicKey)
				}
			}
			return nil
		},
	}
}

func peerFriendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "friend <key>",
		Short: "Befriend a kernel and import its actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle   string `json:"handle"`
				Imported int    `json:"imported"`
				Skipped  int    `json:"skipped"`
			}
			if err := apiCall(context.Background(), "POST", "/control/peers/friend",
				map[string]any{"key": args[0]}, &out); err != nil {
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
		Short: "Unfriend a peer (@handle or key)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle string `json:"handle"`
			}
			// Pass the identifier raw (a key must not become an @handle); the server resolves either.
			if err := apiCall(context.Background(), "POST", "/control/peers/unfriend",
				map[string]any{"handle": args[0]}, &out); err != nil {
				return err
			}
			fmt.Printf("Unfriended %s.\n", out.Handle)
			return nil
		},
	}
}

func peerListCmd() *cobra.Command {
	var showGossip bool
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List peers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path := "/control/peers"
			if showGossip {
				path += "?gossip=1"
			}
			var out struct {
				Peers  []*kernel.PeerView     `json:"peers"`
				Roster []*kernel.KernelRoster `json:"roster"`
			}
			if err := apiCall(context.Background(), "GET", path, nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			if len(out.Peers) == 0 {
				fmt.Println("No peers registered.")
			} else {
				fmt.Printf("%-20s %10s %8s  %s\n", "HANDLE", "AVAILABLE", "LOCKED", "PUBLIC_KEY")
				for _, p := range out.Peers {
					denied := ""
					if p.DeniedAt != nil {
						denied = " [denied]"
					}
					fmt.Printf("%-20s %10d %8d  %s%s\n", p.Handle, p.Available, p.Locked, p.PublicKey, denied)
				}
			}
			if showGossip {
				renderRoster(out.Roster, out.Peers)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showGossip, "gossip", false, "Also show the known-network directory (discovery)")
	return cmd
}

// renderRoster prints the known-network directory (§13) grouped kernel → introducer → action, with
// our own earned stats first (ground truth) and each introducer flagged self-reported or hearsay.
func renderRoster(roster []*kernel.KernelRoster, peers []*kernel.PeerView) {
	if len(roster) == 0 {
		fmt.Println("\nNo known kernels yet (discovery seeds from bootstrap peers).")
		return
	}
	friendByKey := map[string]string{}
	for _, p := range peers {
		if p.PublicKey != "" {
			friendByKey[p.PublicKey] = p.Handle
		}
	}
	printActions := func(as []kernel.GossipAction) {
		for _, a := range as {
			fmt.Printf("      %-24s uses %-5d rating %.2f  price %d\n", a.Name, a.Uses, a.Rating, a.Price)
		}
	}
	fmt.Println("\nKnown kernels (discovery):")
	for _, kr := range roster {
		fmt.Printf("\n%-20s %s\n", kr.Handle, kr.PublicKey)
		if len(kr.Own) > 0 {
			fmt.Println("  you:")
			printActions(kr.Own)
		}
		for _, src := range kr.Sources {
			label := "self-reported"
			if !src.SelfReported {
				if h, ok := friendByKey[src.IntroducedBy]; ok {
					label = "via " + h
				} else {
					label = "via " + src.IntroducedBy
				}
			}
			fmt.Printf("  %s:\n", label)
			printActions(src.Actions)
		}
	}
}
