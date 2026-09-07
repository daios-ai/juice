package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
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

// admin holds the superuser-only supervisory verbs — the operations no ordinary user ever
// performs: money (deposit, settle), access (suspend/unsuspend), federation trust
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
		peerListCmd(),
		peerInspectCmd(),
		identityCmd(),
	)
	rootCmd.AddCommand(adminCmd)
}

// identityCmd prints who this kernel is and where it stands. Federation exposes no .well-known
// document, so this is how an operator learns the key to share; it is also the one place the money
// picture is read whole — what the rail holds against what the books say, so an operator sees a
// disagreement here rather than in a user's failed withdrawal.
func identityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "identity",
		Short: "Show this kernel's identity, money position, and federation standing",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var out struct {
				Handle      string   `json:"handle"`
				PublicKey   string   `json:"public_key"`
				About       string   `json:"about"`
				Addrs       []string `json:"addrs"`
				Network     string   `json:"network"`
				RailAddress string   `json:"rail_address"`
				Finalized   *struct {
					Token int64  `json:"token"`
					Gas   string `json:"gas"`
					Block uint64 `json:"block"`
				} `json:"finalized"`
				Sys struct {
					Earnings       int64 `json:"earnings"`
					PendingPayouts int64 `json:"pending_payouts"`
					HeldDeposits   int64 `json:"held_deposits"`
					RefillLocks    int64 `json:"refill_locks"`
				} `json:"sys"`
				Solvency struct {
					Liabilities int64 `json:"liabilities"`
					Vault       int64 `json:"vault"`
					Gap         int64 `json:"gap"`
				} `json:"solvency"`
				Custody *struct {
					Checked    bool  `json:"checked"`
					Difference int64 `json:"difference"`
					OK         bool  `json:"ok"`
				} `json:"custody"`
				Stop *struct {
					Reason string `json:"reason"`
					Since  string `json:"since"`
				} `json:"stop"`
				Lottery     int64 `json:"lottery"`
				CreditLimit int64 `json:"credit_limit"`
				Exposure    int64 `json:"exposure"`
				FeeBPS      int64 `json:"fee_bps"`
				RemoteBPS   int64 `json:"remote_bps"`
				ImportBPS   int64 `json:"import_bps"`
			}
			ctx := context.Background()
			if err := apiCall(ctx, "GET", "/control/identity", nil, &out); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(out)
			}
			decimals := amountDecimals(ctx)
			fmt.Printf("Handle:     %s\n", out.Handle)
			fmt.Printf("Public key: %s\n", out.PublicKey)
			if out.About != "" {
				fmt.Printf("About:      %s\n", out.About)
			}
			if out.Network != "" {
				fmt.Printf("Network:    %s\n", out.Network)
			}
			if out.RailAddress != "" {
				fmt.Printf("Paid at:    %s\n", out.RailAddress)
			}
			if out.Finalized != nil {
				fmt.Printf("Holdings:   %s (fee balance %s) as of block %d\n",
					formatAmount(out.Finalized.Token, decimals), out.Finalized.Gas, out.Finalized.Block)
			}
			// What the operator's own account holds, split by what it is: money earned and spendable,
			// versus money merely passing through (owed out, unattributed, or locked for rail fees).
			fmt.Printf("Operator:   earned=%d paying-out=%d unattributed=%d fee-locks=%d\n",
				out.Sys.Earnings, out.Sys.PendingPayouts, out.Sys.HeldDeposits, out.Sys.RefillLocks)
			// The books add up when what users hold equals what came in: the ledger is backed by cash
			// alone, so there is nothing else in the identity.
			fmt.Printf("Solvency:   user-credits=%d money-in=%d difference=%d\n",
				out.Solvency.Liabilities, out.Solvency.Vault, out.Solvency.Gap)
			if out.Solvency.Gap != 0 {
				fmt.Printf("ALARM: the books do not add up — off by %d\n", out.Solvency.Gap)
			}
			// An audit that could not run says nothing either way; only a checked mismatch is an alarm.
			if out.Custody != nil && out.Custody.Checked && !out.Custody.OK {
				fmt.Printf("ALARM: the money the rail holds differs from the books by %d\n", out.Custody.Difference)
			}
			if out.Stop != nil {
				fmt.Printf("ALARM: outgoing payments are halted since %s: %s\n", out.Stop.Since, out.Stop.Reason)
			}
			// What this kernel is owed for work already delivered, and the ceiling it will carry.
			fmt.Printf("Credit:     owed-to-us=%d limit=%d\n", out.Exposure, out.CreditLimit)
			if out.Exposure > out.CreditLimit {
				fmt.Println("ALARM: more work has been delivered on credit than the limit allows")
			}
			// The money rules this kernel serves under. A ticket of 0 pays every obligation exactly.
			fmt.Printf("Rates:      fee=%d bps serving=%d bps import=%d bps ticket=%d\n",
				out.FeeBPS, out.RemoteBPS, out.ImportBPS, out.Lottery)
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

// adminDepositCmd records money that came in from outside, and bare lists what is waiting to be
// recorded. Money is credited against a fact the rail witnesses, never on the operator's say-so
// alone, which is why --ref is required to credit anything. There is no matching withdraw: money
// leaves only by its owner's own `user withdraw`.
func adminDepositCmd() *cobra.Command {
	var reason, ref string
	cmd := &cobra.Command{
		Use:   "deposit [TARGET [AMOUNT]]",
		Short: "Credit an account or kernel for a payment received, or list payments awaiting it",
		Long: "Credit an account for a payment received from outside, or record the payment that\n" +
			"closes what a peer owes. A peer account holds no money of its own, so a peer can only be\n" +
			"named in that last form, and the money goes to the provider it is owed to.\n\n" + targetHelp + "\n\n" +
			"With no arguments, lists the money waiting to be recorded: payments whose sender nobody\n" +
			"has registered, and settlements a peer says it has paid.\n\n" +
			"Three forms:\n" +
			"  admin deposit USER AMOUNT --ref FACT   record a payment made outside the system\n" +
			"  admin deposit USER --ref TXHASH        assign a received payment to its sender\n" +
			"  admin deposit PEER [AMOUNT] --ref ID   record the payment closing what a peer owes\n\n" +
			"FACT names the payment: your own record of it where this world has no chain, or the\n" +
			"transaction that carried it where it has. Repeating the same fact never moves money\n" +
			"twice, and the same fact with a different amount is refused.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			if len(args) == 0 {
				return apiEmitCtx(ctx, "GET", "/control/deposits", nil)
			}
			var amount int64
			if len(args) == 2 {
				var err error
				if amount, err = parseAmount(args[1], amountDecimals(ctx)); err != nil {
					return err
				}
			}
			return apiEmitCtx(ctx, "POST", "/control/deposit", map[string]any{
				"handle": args[0], "amount": amount, "reason": reason, "ref": ref,
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "The payment this credit records: your own record of it, a transaction hash, or a settlement id")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
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
					Suspended bool `json:"suspended"`
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
				fmt.Printf("Traded here:  yes%s\n", susp)
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
			fmt.Printf("%-16s %-16s %8s %10s %12s %10s  %s\n",
				"PETNAME", "NICKNAME", "TRADED", "LAST SEEN", "LAST FAILED", "ACTIONS", "PUBLIC KEY")
			for _, p := range peers {
				flags := ""
				if p.SuspendedAt != nil {
					flags += " [suspended]"
				}
				petname, traded := "—", "—"
				if p.Petname != "" {
					petname = p.Petname
				}
				if p.HasAccount {
					traded = "yes"
				}
				fmt.Printf("%-16s %-16s %8s %10s %12s %10d  %s%s\n",
					petname, p.Nickname, traded, lastSeenStr(p.LastSeen),
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
