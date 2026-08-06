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
		adminTransferCmd(),
	)
	rootCmd.AddCommand(adminCmd)
}

// adminTransferCmd groups the operator surface for buyer-side pending value transfers (§13): a payment
// step whose completion is unresolved (pending) or whose receipt could not be validated (quarantined).
// list/show are read-only; retry is the ONLY mutation — it re-presents the SAME signed completion and
// settles strictly on receipt evidence (quarantine means "evidence insufficient", never "operator
// chooses"), so there is deliberately no refund/force-settle/edit/delete.
func adminTransferCmd() *cobra.Command {
	transferCmd := &cobra.Command{Use: "transfer", Short: "Inspect and retry pending value transfers"}
	transferCmd.AddCommand(transferListCmd(), transferShowCmd(), transferRetryCmd())
	return transferCmd
}

// transferRow is the CLI decode target for the list table (the server view carries more fields).
type transferRow struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	BuyerHandle string `json:"buyer_handle"`
	PeerHandle  string `json:"peer_handle"`
	Amount      int64  `json:"amount"`
	Reserve     int64  `json:"reserve"`
	CreatedAt   string `json:"created_at"`
}

func transferListCmd() *cobra.Command {
	var status string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending value transfers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if status != "" {
				q.Set("status", status)
			}
			setLimitOffset(q, limit, offset)
			var rows []transferRow
			if err := apiCall(context.Background(), "GET", "/control/transfers?"+q.Encode(), nil, &rows); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(rows)
			}
			for _, t := range rows {
				fmt.Printf("%-36s  %-11s  %-16s  %-16s  amount=%-6d reserve=%-6d  %s\n",
					t.ID, t.Status, t.BuyerHandle, t.PeerHandle, t.Amount, t.Reserve, t.CreatedAt)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter by status; default lists the unresolved ones (pending, quarantined)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func transferShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show transfer details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/control/transfers/"+url.PathEscape(args[0]), nil)
		},
	}
}

func transferRetryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "retry <id>",
		Short: "Re-present a pending transfer's completion and settle on the result",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("POST", "/control/transfers/"+url.PathEscape(args[0])+"/retry", nil)
		},
	}
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
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <target>",
		Short: "Show a local account or a remote kernel",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/control/users/"+url.PathEscape(args[0]), nil)
		},
	}
}

func adminSuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <target>",
		Short: "Suspend a local account or a remote kernel",
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
		Use:   "unsuspend <target>",
		Short: "Unsuspend a local account or a remote kernel",
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
		Use:   "rename <target> <new-name>",
		Short: "Rename a local account, or bind a kernel's petname",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"new_handle": args[1]}
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
	return adjustCmd("deposit <target> <amount>", "Add credits to an account or kernel", "/control/deposit")
}

func adminWithdrawCmd() *cobra.Command {
	return adjustCmd("withdraw <target> <amount>", "Deduct credits from an account or kernel", "/control/withdraw")
}

func adminSettleCmd() *cobra.Command {
	var cash string
	cmd := &cobra.Command{
		Use:   "settle <peer>",
		Short: "Settle the bilateral position with a peer",
		Long:  "Settle the bilateral position with a peer.\n\nWith no flags: exact settlement if the debt ≥ Q, otherwise the two-party probabilistic\ncommit/reveal. A paid probabilistic outcome does NOT move money — it leaves a debt of Q pending.\n\nAfter paying that Q on your rail, record it with --cash <settlement_id> (run on both kernels).",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("POST", "/control/peers/settle", map[string]any{"handle": args[0], "settlement_id": cash})
		},
	}
	cmd.Flags().StringVar(&cash, "cash", "", "Record the rail payment for a paid probabilistic outcome (settlement_id)")
	return cmd
}

func peerInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect <key|petname>",
		Short: "Inspect a remote kernel (by public key or bound petname)",
		Args:  cobra.ExactArgs(1),
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
				Evidence []struct {
					IssuerPublicKey   string   `json:"issuer_public_key"`
					SubjectActionID   string   `json:"subject_action_id"`
					Uses              int64    `json:"uses"`
					Successes         int64    `json:"successes"`
					Failures          int64    `json:"failures"`
					CorroboratedUses  int64    `json:"corroborated_uses"`
					AvgLatencyMs      float64  `json:"avg_latency_ms"`
					RatingCount       int64    `json:"rating_count"`
					RatingMean        float64  `json:"rating_mean"`
					UnverifiedRatings int64    `json:"unverified_ratings"`
					Notes             []string `json:"notes"`
				} `json:"evidence"`
				Account *struct {
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
			fmt.Printf("%-16s %-16s %8s %8s %10s %10s  %s\n",
				"PETNAME", "NICKNAME", "ACCOUNT", "BALANCE", "LAST SEEN", "ACTIONS", "PUBLIC KEY")
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
				fmt.Printf("%-16s %-16s %8s %8s %10s %10d  %s%s\n",
					petname, p.Nickname, account, balance, lastSeenStr(p.LastSeen), p.Actions, p.PublicKey, flags)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&showAll, "all", false, "Include suspended counterparties")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

// shortKey abbreviates a base64url public key for display.
func shortKey(k string) string {
	if len(k) > 12 {
		return k[:12] + "…"
	}
	return k
}
