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

// lastSeenStr renders one contact timestamp (§13) — last success or last failure: "never" when that
// contact has not happened yet, else a coarse relative age. Comparing the two is the reader's job.
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
	if err != nil || amount <= 0 {
		return 0, kernel.ErrInvalidInput.Wrap("amount must be a positive integer")
	}
	return amount, nil
}

// admin holds the superuser-only supervisory verbs — the operations no ordinary user ever
// performs: money (deposit/withdraw), access (suspend/unsuspend), federation trust
// (peers/inspect/settle), and the global roster (users/show). They are ordinary TCP
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
		peerListCmd(),
		peerInspectCmd(),
		identityCmd(),
	)
	rootCmd.AddCommand(adminCmd)
}

// identityCmd prints this kernel's own federation identity: its public key (which peers address
// it by), handle, and libp2p listen addresses. Federation no longer exposes a .well-known
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
			// Global exposure policy and current standing (§13), in operator words: how much
			// unsecured credit this kernel extends serving peers, how much peers owe right now,
			// and the two settlement thresholds from config.
			fmt.Printf("Credit:     serving-cap=%d owed-by-peers=%d settle-signal-at=%d small-debt-threshold=%d\n",
				out.ExposureMax, out.GrossReceivables, out.SettlementTrigger, out.SettlementQuantum)
			if out.SettlementDue {
				fmt.Println("Settlement: DUE (peers owe at least the settle signal)")
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
			var users []*kernel.Account
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			if err := apiCall(context.Background(), "GET", "/control/users?"+q.Encode(), nil, &users); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(users)
			}
			for _, u := range users {
				suspended := ""
				if u.SuspendedAt != nil {
					suspended = " [suspended]"
				}
				fmt.Printf("%-20s  %s%s\n", u.Handle, u.Description, suspended)
			}
			return nil
		},
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// targetHelp defines the shared TARGET placeholder of the mixed account/kernel admin commands.
const targetHelp = "TARGET is a local user's handle, or a peer kernel's local name (petname) or\npublic key; names are shown by `admin users` and `admin peers`."

func adminShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show TARGET",
		Short: "Show a local account or a remote kernel",
		Long:  "Show a local account or a remote kernel.\n\n" + targetHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/control/users/"+url.PathEscape(args[0]), nil)
		},
	}
}

func adminSuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suspend TARGET",
		Short: "Suspend a local account or a remote kernel",
		Long:  "Suspend a local account or a remote kernel: a suspended user cannot log in, and a\nsuspended peer's calls are refused. Reversible with `admin unsuspend`.\n\n" + targetHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/suspend", nil, nil); err != nil {
				return err
			}
			fmt.Printf("%s suspended.\n", kernel.NormalizeHandle(args[0]))
			return nil
		},
	}
}

func adminUnsuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsuspend TARGET",
		Short: "Unsuspend a local account or a remote kernel",
		Long:  "Unsuspend a local account or a remote kernel, restoring it fully.\n\n" + targetHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/unsuspend", nil, nil); err != nil {
				return err
			}
			fmt.Printf("%s unsuspended.\n", kernel.NormalizeHandle(args[0]))
			return nil
		},
	}
}

func adminRenameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename TARGET NEW_NAME",
		Short: "Rename a local account, or bind a kernel's petname",
		Long: "Rename a local account, or bind a kernel's petname.\n\n" + targetHelp + "\n\n" +
			"For a user target, NEW_NAME becomes its handle and the old handle is freed. For a\n" +
			"kernel target, NEW_NAME becomes its petname — the local name your commands use for\n" +
			"that peer. A name already in use is refused.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"new_name": args[1]}
			if err := apiCall(context.Background(), "POST", "/control/users/"+url.PathEscape(args[0])+"/rename", body, nil); err != nil {
				return err
			}
			fmt.Printf("%s renamed to %s.\n", args[0], kernel.NormalizeHandle(args[1]))
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
		Long:  short + ", reflecting a payment made outside the system.\n\n" + targetHelp,
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
	cmd.Flags().StringVar(&externalKey, "external-key", "", "Unique id of the outside payment; repeating the command with the same id never moves money twice")
	return cmd
}

func adminDepositCmd() *cobra.Command {
	return adjustCmd("deposit TARGET AMOUNT", "Add credits to an account or kernel", "/control/deposit")
}

func adminWithdrawCmd() *cobra.Command {
	return adjustCmd("withdraw TARGET AMOUNT", "Deduct credits from an account or kernel", "/control/withdraw")
}

func adminSettleCmd() *cobra.Command {
	var cash string
	cmd := &cobra.Command{
		Use:   "settle PEER",
		Short: "Settle what this kernel owes a peer kernel",
		Long: "Settle this kernel's debt to a peer. PEER is the peer's local name (petname) or its\n" +
			"public key — both are shown by `admin peers`.\n\n" +
			"The kernel only keeps the books; real money moves outside it, on whatever payment rail\n" +
			"the two operators share. If the debt is at least `settlement_quantum` (config), the\n" +
			"command prints the amount to pay and the exact `admin withdraw` command that records\n" +
			"the payment. A smaller debt is settled by a fair random draw with the peer: usually\n" +
			"the debt is cancelled outright and nothing is paid; with probability debt/quantum the\n" +
			"full quantum becomes payable instead. Over many settlements this averages out exactly,\n" +
			"so debts too small to pay economically still settle fairly.\n\n" +
			"When a draw ends payable, pay the quantum on the rail, then record it with\n" +
			"--cash SETTLEMENT_ID on both kernels.",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("POST", "/control/peers/settle", map[string]any{"handle": args[0], "settlement_id": cash})
		},
	}
	cmd.Flags().StringVar(&cash, "cash", "", "Record the rail payment for a payable draw (takes the settlement_id printed earlier)")
	return cmd
}

func peerInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect KEY|PETNAME",
		Short: "Inspect a remote kernel (by public key or bound petname)",
		Long: "Inspect a remote kernel: identity, public actions, retained trade evidence, and\n" +
			"reachability. The petname is the local name this kernel gave the peer (`admin rename`);\n" +
			"the nickname is what the peer calls itself, shown for recognition but never usable as a\n" +
			"name. An offline peer degrades to locally cached data.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var out struct {
				Petname   string `json:"petname"`
				Nickname  string `json:"nickname"`
				Handle    string `json:"handle"` // live pull: the kernel's own advertised name
				PublicKey string `json:"public_key"`
				About     string `json:"about"`
				Actions   []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Price       int64  `json:"price"`
				} `json:"actions"`
				Evidence []kernel.SubjectEvidenceRow `json:"evidence"`
				Account  *struct {
					Available int64 `json:"available"`
					Locked    int64 `json:"locked"`
					Suspended bool  `json:"suspended"`
				} `json:"account"`
				Reachability struct {
					Path      string `json:"path"`
					RTTmillis int64  `json:"rtt_millis"`
				} `json:"reachability"`
				Source string `json:"source"`
				Online bool   `json:"online"`
				// Steps this peer has parked for THIS kernel: work awaiting us, and the ids
				// `step complete ID --peer` takes (§13). A peer account holds no session token,
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
			// Petname is the name that resolves a reference here; the kernel's own label never
			// does (§13), so they print as separate lines rather than one ambiguous "handle".
			petname := out.Petname
			if petname == "" {
				petname = "— (unbound; call it by key)"
			}
			nickname := out.Nickname
			if nickname == "" {
				nickname = out.Handle
			}
			fmt.Printf("Petname:      %s\n", petname)
			fmt.Printf("Nickname:     %s\n", nickname)
			fmt.Printf("Public key:   %s\n", out.PublicKey)
			if out.About != "" {
				fmt.Printf("About:        %s\n", out.About)
			}
			reachLabel := out.Reachability.Path
			if !out.Online {
				reachLabel = "offline"
			}
			fmt.Printf("Reachability: %s (%dms)\n", reachLabel, out.Reachability.RTTmillis)
			if out.Account != nil {
				susp := ""
				if out.Account.Suspended {
					susp = " [suspended]"
				}
				fmt.Printf("Account:      available=%d locked=%d%s\n", out.Account.Available, out.Account.Locked, susp)
			}
			if out.Source == "none" {
				fmt.Println("This peer is offline and not known locally (no cached data).")
				return nil
			}
			if len(out.Actions) > 0 {
				label := "Public actions"
				if out.Source == "local" {
					label = "Actions (from discovery cache — peer offline)"
				}
				fmt.Printf("\n%s (%d):\n", label, len(out.Actions))
				for _, a := range out.Actions {
					fmt.Printf("  %-30s  %d credits\n", a.Name, a.Price)
					if a.Description != "" {
						fmt.Printf("      %s\n", a.Description)
					}
				}
			}
			if len(out.Evidence) > 0 {
				// Two views, never folded together: the subject's own execution summary (issuer ==
				// subject), then per-issuer counterparty experience (every other issuer's direct
				// interactions with the subject). A rating counts only when trade-backed (§13).
				subjectName := out.Petname
				if subjectName == "" {
					subjectName = out.Nickname
				}
				if subjectName == "" {
					subjectName = shortKey(out.PublicKey)
				}
				fmt.Printf("\nExecution reported by %s\n", subjectName)
				own := false
				for _, e := range out.Evidence {
					if e.IssuerPublicKey != out.PublicKey {
						continue
					}
					own = true
					fmt.Printf("  action %s: %d executions, %d successful  ~%.0fms\n",
						e.SubjectActionID, e.Uses, e.Successes, e.AvgLatencyMs)
				}
				if !own {
					fmt.Println("  (none)")
				}
				header := false
				for _, e := range out.Evidence {
					if e.IssuerPublicKey == out.PublicKey {
						continue
					}
					if !header {
						fmt.Printf("\nCounterparty experience\n")
						header = true
					}
					fmt.Printf("  From %s on %s: %d interactions, %d successful",
						shortKey(e.IssuerPublicKey), e.SubjectActionID, e.Uses, e.Successes)
					// Corroboration tag on the interactions themselves (§13): [verified] when the
					// two-kernel receipt link holds for all of them, a fraction when partial, else
					// [unverified] — an issuer's self-attested claim is never shown as fact.
					switch {
					case e.Uses > 0 && e.CorroboratedUses == e.Uses:
						fmt.Printf(" [verified]")
					case e.CorroboratedUses > 0:
						fmt.Printf(" [%d/%d verified]", e.CorroboratedUses, e.Uses)
					default:
						fmt.Printf(" [unverified]")
					}
					if e.RatingCount > 0 {
						fmt.Printf("  rating %.2f", e.RatingMean)
					}
					if e.UnverifiedRatings > 0 {
						fmt.Printf("  [+%d unverified rating]", e.UnverifiedRatings)
					}
					fmt.Println()
				}
			}
			if len(out.Steps) > 0 {
				fmt.Printf("\nSteps awaiting us (%d) — complete with: step complete ID --peer %s\n",
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

func peerListCmd() *cobra.Command {
	var showAll bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List known kernels (counterparties and discovery-only), merged by key",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if showAll {
				q.Set("all", "1")
			}
			setLimitOffset(q, limit, offset)
			path := "/control/peers"
			if e := q.Encode(); e != "" {
				path += "?" + e
			}
			var peers []*kernel.RemoteKernelView
			if err := apiCall(context.Background(), "GET", path, nil, &peers); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(peers)
			}
			if len(peers) == 0 {
				return nil
			}
			// PETNAME is the local name that resolves a reference; NICKNAME is what the kernel
			// calls itself and never resolves (§13). The public key always resolves, so an
			// unbound kernel is still callable — bind a petname with `admin rename <key> <name>`.
			fmt.Printf("%-16s %-16s %8s %8s %10s %12s %10s  %s\n",
				"PETNAME", "NICKNAME", "ACCOUNT", "BALANCE", "LAST SEEN", "LAST FAILED", "ACTIONS", "PUBLIC KEY")
			for _, p := range peers {
				flags := ""
				if p.SettlementDue {
					flags += " [settle_due]"
				}
				if p.SuspendedAt != nil {
					flags += " [suspended]"
				}
				petname, account, balance := "—", "—", "—"
				if p.Petname != "" {
					petname = p.Petname
				}
				if p.HasAccount {
					account = "yes"
					balance = fmt.Sprintf("%d", p.Available)
				}
				fmt.Printf("%-16s %-16s %8s %8s %10s %12s %10d  %s%s\n",
					petname, p.Nickname, account, balance, lastSeenStr(p.LastSeen),
					lastSeenStr(p.LastContactFailedAt), p.Actions, p.PublicKey, flags)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "Include suspended counterparties")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// shortKey abbreviates a base64url public key for display.
func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12] + "…"
	}
	return k
}
