package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// peerCreditStr renders the §13 sync cache "our credit on the peer": a dash when unsynced (nil).
func peerCreditStr(c *int64) string {
	if c == nil {
		return "-"
	}
	return strconv.FormatInt(*c, 10)
}

// lastSeenStr renders when a peer was last reached by peer sync (§13): "never" when unsynced,
// else a coarse relative age.
func lastSeenStr(t *time.Time) string {
	if t == nil {
		return "never"
	}
	d := time.Since(*t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func parseAmount(s string) (int64, error) {
	amount, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, kernel.ErrInvalidInput.Wrap("amount must be a positive integer")
	}
	return amount, nil
}

// admin holds the superuser-only supervisory verbs — the operations no ordinary user ever
// performs: money (deposit/withdraw), access (suspend/unsuspend), federation trust
// (subscribe/unsubscribe/peers/inspect), and the global roster (users/show). They are ordinary TCP
// clients like every other command (apiCall/apiEmit); the server gates the routes with
// requireSuperuserMW, so authority is the @sys bearer token (§14).
//
// Everything that is merely "the same operation with wider reach" is NOT here: a superuser
// sees all rows on `action/process/tx/step list` and may `action disable` any action, all
// over the normal TCP API (supervision is scope, not a separate surface).
func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Superuser commands"}
	adminCmd.AddCommand(
		adminUsersCmd(),
		adminShowCmd(),
		adminSuspendCmd(),
		adminUnsuspendCmd(),
		adminRenameCmd(),
		adminDepositCmd(),
		adminWithdrawCmd(),
		adminSettleCmd(),
		peerSubscribeCmd(),
		peerUnsubscribeCmd(),
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
				Handle            string   `json:"handle"`
				PublicKey         string   `json:"public_key"`
				About             string   `json:"about"`
				Addrs             []string `json:"addrs"`
				ExposureMax       int64    `json:"exposure_max"`
				SettlementTrigger int64    `json:"settlement_trigger"`
				SettlementQuantum int64    `json:"settlement_quantum"`
				GrossReceivables  int64    `json:"gross_receivables"`
				SettlementDue     bool     `json:"settlement_due"`
			}
			if err := apiCall(context.Background(), "GET", "/control/identity", nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("Handle:     %s\n", out.Handle)
			fmt.Printf("Public key: %s\n", out.PublicKey)
			if out.About != "" {
				fmt.Printf("About:      %s\n", out.About)
			}
			// Global exposure policy and current standing (§13).
			fmt.Printf("Exposure:   max=%d gross_receivables=%d trigger=%d quantum=%d\n",
				out.ExposureMax, out.GrossReceivables, out.SettlementTrigger, out.SettlementQuantum)
			if out.SettlementDue {
				fmt.Println("Settlement: DUE (gross receivables ≥ trigger)")
			}
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
				fmt.Printf("%-20s  %s%s\n", u.Handle, u.Description, suspended)
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

func adminRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <user> <new-handle>",
		Short: "Rename a user's handle (frees the old handle for reuse)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"new_handle": args[1]}
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/rename", body, nil); err != nil {
				return err
			}
			fmt.Printf("User %s renamed to %s.\n", kernel.NormalizeHandle(args[0]), kernel.NormalizeHandle(args[1]))
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

func adminSettleCmd() *cobra.Command {
	var cash string
	cmd := &cobra.Command{
		Use:   "settle <peer>",
		Short: "Settle the bilateral position with a peer: exact if debt ≥ Q, else the probabilistic residual protocol (§13)",
		Long:  "Settle the bilateral position with a peer.\n\nWith no flags: exact settlement if the debt ≥ Q, otherwise the two-party probabilistic\ncommit/reveal. A paid probabilistic outcome does NOT move money — it leaves a debt of Q pending.\n\nAfter paying that Q on your rail, record it with --cash <settlement_id> (run on both kernels).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("POST", "/control/peers/settle", map[string]any{"handle": args[0], "settlement_id": cash})
		},
	}
	cmd.Flags().StringVar(&cash, "cash", "", "record the rail payment for a paid probabilistic outcome (settlement_id)")
	return cmd
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
				About     string `json:"about"`
				Actions   []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Price       int64  `json:"price"`
					Uses        int64  `json:"uses"`
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
				Source string `json:"source"`
				Online bool   `json:"online"`
				// Steps this peer has parked for THIS kernel: work awaiting us, and the ids
				// `step complete <id> --peer` takes (§13). A peer account holds no session token,
				// so this is the only place an operator sees them.
				Steps []struct {
					ID           string          `json:"id"`
					Price        int64           `json:"price"`
					CreatedAt    time.Time       `json:"created_at"`
					PartialArgs  json.RawMessage `json:"partial_args"`
					AllowedInput json.RawMessage `json:"allowed_input"`
				} `json:"steps"`
			}
			if err := apiCall(context.Background(), "GET", "/control/peers/inspect?key="+url.QueryEscape(args[0]), nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			fmt.Printf("Handle:       %s\n", out.Handle)
			fmt.Printf("Public key:   %s\n", out.PublicKey)
			if out.About != "" {
				fmt.Printf("About:        %s\n", out.About)
			}
			reachLabel := out.Reachability.Path
			if !out.Online {
				reachLabel = "offline"
			}
			fmt.Printf("Reachability: %s (%dms)\n", reachLabel, out.Reachability.RTTmillis)
			if out.Source == "none" {
				fmt.Println("This peer is offline and not known locally (never friended).")
				return nil
			}
			if len(out.Actions) > 0 {
				label := "Active actions"
				if out.Source == "local" {
					label = "Actions (last imported — peer offline)"
				}
				fmt.Printf("\n%s (%d):\n", label, len(out.Actions))
				for _, a := range out.Actions {
					fmt.Printf("  %-30s  %d credits  (uses: %d)\n", a.Name, a.Price, a.Uses)
					if a.Description != "" {
						fmt.Printf("      %s\n", a.Description)
					}
				}
			}
			if len(out.Friends) > 0 {
				fmt.Printf("\nTransacted friends (%d):\n", len(out.Friends))
				for _, f := range out.Friends {
					fmt.Printf("  %-20s %s\n", f.Handle, f.PublicKey)
				}
			}
			if len(out.Steps) > 0 {
				fmt.Printf("\nSteps awaiting us (%d) — complete with: step complete <id> --peer %s\n",
					len(out.Steps), args[0])
				for _, st := range out.Steps {
					fmt.Printf("  %s  price=%d  %s\n", st.ID, st.Price, st.CreatedAt.Format(time.RFC3339))
					if len(st.PartialArgs) > 0 && string(st.PartialArgs) != "{}" {
						fmt.Printf("      %s\n", st.PartialArgs)
					}
					// The derived completion schema (§14): what this kernel may supply, without
					// having to read a target action it cannot see.
					if len(st.AllowedInput) > 0 {
						fmt.Printf("      allowed_input: %s\n", st.AllowedInput)
					}
				}
			}
			return nil
		},
	}
}

func peerSubscribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "subscribe <key>",
		Short: "Subscribe to a kernel and import its actions",
		Long:  "Subscribe to a kernel by public key and import its active public actions. The peer mounts under its self-reported handle (auto-suffixed on collision); rename the mount with `admin rename <key> <new-handle>`. Calls stay rejected until the peer holds credit here — fund it with `admin deposit <key> <amount>`.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"key": args[0]}
			var out struct {
				Handle   string `json:"handle"`
				Imported int    `json:"imported"`
				Skipped  int    `json:"skipped"`
			}
			if err := apiCall(context.Background(), "POST", "/control/peers/subscribe", body, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			msg := fmt.Sprintf("Subscribed to %s", out.Handle)
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

func peerUnsubscribeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsubscribe <user>",
		Short: "Unsubscribe from a peer (@handle or key), deactivating its imported actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Handle string `json:"handle"`
			}
			// Pass the identifier raw (a key must not become an @handle); the server resolves either.
			if err := apiCall(context.Background(), "POST", "/control/peers/unsubscribe",
				map[string]any{"handle": args[0]}, &out); err != nil {
				return err
			}
			fmt.Printf("Unsubscribed from %s.\n", out.Handle)
			return nil
		},
	}
}

func peerListCmd() *cobra.Command {
	var showGossip, showAll bool
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List peers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if showGossip {
				q.Set("gossip", "1")
			}
			if showAll {
				q.Set("all", "1")
			}
			path := "/control/peers"
			if e := q.Encode(); e != "" {
				path += "?" + e
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
				fmt.Printf("%-20s %10s %8s %12s %10s  %s\n", "HANDLE", "AVAILABLE", "LOCKED", "CREDIT_THERE", "LAST_SEEN", "PUBLIC_KEY")
				for _, p := range out.Peers {
					suspended := ""
					if p.SuspendedAt != nil {
						suspended = " [suspended]"
					}
					fmt.Printf("%-20s %10d %8d %12s %10s  %s%s\n",
						p.Handle, p.Available, p.Locked, peerCreditStr(p.PeerCredit), lastSeenStr(p.LastSeen), p.PublicKey, suspended)
				}
			}
			if showGossip {
				renderRoster(out.Roster, out.Peers)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showGossip, "gossip", false, "Also show the known-network directory (discovery)")
	cmd.Flags().BoolVar(&showAll, "all", false, "Include suspended peers")
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
