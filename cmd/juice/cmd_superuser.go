// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
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
// performs: money (deposit), access (suspend/unsuspend), federation trust
// (peers/inspect), and the global roster (users/show). They are ordinary TCP
// clients like every other command (apiCall/apiEmit); the server gates the routes with
// requireSuperuserMW, so authority is the @sys bearer token (§14).
//
// Everything that is merely "the same operation with wider reach" is NOT here: a superuser
// sees all rows on `action/process/tx/step list` and may `action disable` any action, all
// over the normal TCP API (supervision is scope, not a separate surface).
func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Superuser commands"}
	userCmd := &cobra.Command{Use: "user", Short: "Accounts on this kernel"}
	userCmd.AddCommand(append(rosterCmds("user"), adminUserListCmd(), adminUserDepositCmd())...)
	peerCmd := &cobra.Command{Use: "peer", Short: "Kernels this one trades with"}
	peerCmd.AddCommand(append(rosterCmds("peer"), peerListCmd(), peerInspectCmd())...)
	kernelCmd := &cobra.Command{Use: "kernel", Short: "This kernel itself"}
	kernelCmd.AddCommand(identityCmd(), adminDepositsCmd())
	adminCmd.AddCommand(userCmd, peerCmd, kernelCmd)
	rootCmd.AddCommand(adminCmd)
}

// roster is what the two nouns an operator supervises have in common: an account here, named its
// own way, that can be read, suspended, restored and renamed. The verbs are identical but the
// nouns are not, so the kind travels to the server and a target of the other kind is refused
// there — which is the whole point of naming the noun rather than letting one command guess.
type roster struct {
	noun, target, named, renamed string
	id                           string // the field that names one of them, which --quiet prints
}

var rosters = map[string]roster{
	"user": {noun: "user", target: "USER", id: "address", named: "a user's address, handle@kernel, as `admin user list` shows it",
		renamed: "NEW_NAME is the new address, handle@kernel on this kernel; the old handle is freed"},
	"peer": {noun: "peer", target: "PEER", id: "public_key", named: "a peer kernel's petname or public key, as `admin peer list` shows it",
		renamed: "NEW_NAME becomes the peer's petname — the local name your commands use for it"},
}

// rosterCmds builds those four verbs for one noun. One constructor rather than eight commands: the
// difference between them is a word and a kind, and writing it once is what keeps them identical
// where they should be.
func rosterCmds(noun string) []*cobra.Command {
	r := rosters[noun]
	path := func(target, verb string) string {
		return "/v1/admin/" + r.noun + "s/" + url.PathEscape(target) + verb
	}
	// Suspending and restoring are one act and its undo: the same target, the same route, and a
	// word apart, so they are written once.
	flip := func(verb, done, short, long string) *cobra.Command {
		return &cobra.Command{
			Use:   verb + " " + r.target,
			Short: short,
			Long:  long + "\n\n" + r.target + " is " + r.named + ".",
			Args:  cobra.ExactArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				return cli.emit("POST", path(args[0], "/"+verb), nil, output{id: r.id, human: func([]byte) error {
					fmt.Printf("%s %s.\n", args[0], done)
					return nil
				}})
			},
		}
	}
	show := &cobra.Command{
		Use:   "show " + r.target,
		Short: "Show one " + r.noun + "'s account here",
		Long:  "Show one " + r.noun + "'s account on this kernel.\n\n" + r.target + " is " + r.named + ".",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cli.emit("GET", path(args[0], ""), nil, output{id: r.id, money: moneyAccount})
		},
	}
	rename := &cobra.Command{
		Use:   "rename " + r.target + " NEW_NAME",
		Short: "Rename a " + r.noun,
		Long:  "Rename a " + r.noun + ". " + r.renamed + ". A name already in use is refused.",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			body := map[string]any{"new_name": args[1]}
			return cli.emit("POST", path(args[0], "/rename"), body, output{id: r.id, human: func([]byte) error {
				fmt.Printf("%s renamed to %s.\n", args[0], strings.TrimSpace(args[1]))
				return nil
			}})
		},
	}
	return []*cobra.Command{show, rename,
		flip("suspend", "suspended", "Suspend a "+r.noun,
			"Suspend a "+r.noun+": a suspended user cannot log in, and a suspended peer's calls are refused. Reversible with `admin "+r.noun+" unsuspend`."),
		flip("unsuspend", "unsuspended", "Restore a suspended "+r.noun,
			"Restore a suspended "+r.noun+", lifting every refusal the suspension caused.")}
}

// identityCmd prints who this kernel is and where it stands. Federation exposes no .well-known
// document, so this is how an operator learns the key to share; it is also the one place the money
// picture is read whole — what the rail holds against what the books say, so an operator sees a
// disagreement here rather than in a user's failed withdrawal.
func identityCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show this kernel's identity, money position, and federation standing",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			return cli.emitCtx(ctx, "GET", "/v1/admin/kernel", nil, output{id: "public_key", human: func(b []byte) error {
				var out struct {
					Handle            string   `json:"handle"`
					PublicKey         string   `json:"public_key"`
					About             string   `json:"about"`
					Addrs             []string `json:"addrs"`
					Network           string   `json:"network"`
					BlockchainAddress string   `json:"blockchain_address"`
					Finalized         *struct {
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
					LotteryMax  int64 `json:"lottery_max"`
					CreditLimit int64 `json:"credit_limit"`
					Exposure    int64 `json:"exposure"`
					FeeBPS      int64 `json:"fee_bps"`
					RemoteBPS   int64 `json:"remote_bps"`
					ImportBPS   int64 `json:"import_bps"`
				}
				if err := json.Unmarshal(b, &out); err != nil {
					return err
				}
				fmt.Printf("Handle:     %s\n", out.Handle)
				fmt.Printf("Public key: %s\n", out.PublicKey)
				if out.About != "" {
					fmt.Printf("About:      %s\n", out.About)
				}
				if out.Network != "" {
					fmt.Printf("Network:    %s\n", out.Network)
				}
				if out.BlockchainAddress != "" {
					fmt.Printf("Paid at:    %s\n", out.BlockchainAddress)
				}
				if out.Finalized != nil {
					fmt.Printf("Holdings:   %s (gas %s) as of block %d\n",
						net.Amount(out.Finalized.Token), out.Finalized.Gas, out.Finalized.Block)
				}
				// What the operator's own account holds, split by what it is: money earned and spendable,
				// versus money merely passing through (owed out, unattributed, or locked for rail fees).
				fmt.Printf("Operator:   earned=%s paying-out=%s unclaimed=%s held-for-gas=%s\n",
					net.Amount(out.Sys.Earnings), net.Amount(out.Sys.PendingPayouts),
					net.Amount(out.Sys.HeldDeposits), net.Amount(out.Sys.RefillLocks))
				// The books add up when what users hold equals what came in: the ledger is backed by cash
				// alone, so there is nothing else in the identity.
				fmt.Printf("Solvency:   user-balances=%s money-in=%s difference=%s\n",
					net.Amount(out.Solvency.Liabilities), net.Amount(out.Solvency.Vault), net.Amount(out.Solvency.Gap))
				if out.Solvency.Gap != 0 {
					fmt.Printf("ALARM: the books do not add up — off by %s\n", net.Amount(out.Solvency.Gap))
				}
				// An audit that could not run says nothing either way; only a checked mismatch is an alarm.
				if out.Custody != nil && out.Custody.Checked {
					if out.Custody.OK {
						fmt.Println("Custody:    the money the rail holds matches the books")
					} else {
						fmt.Printf("ALARM: the money the rail holds differs from the books by %s\n", net.Amount(out.Custody.Difference))
					}
				}
				if out.Stop != nil {
					fmt.Printf("ALARM: outgoing payments are halted since %s: %s\n", out.Stop.Since, out.Stop.Reason)
				}
				// What this kernel is owed for work already delivered, and the ceiling it will carry.
				fmt.Printf("Credit:     owed-to-us=%s limit=%s\n", net.Amount(out.Exposure), net.Amount(out.CreditLimit))
				if out.Exposure > out.CreditLimit {
					fmt.Println("ALARM: more work has been delivered on credit than the limit allows")
				}
				// The money rules this kernel serves under, named by their configuration keys so an
				// operator can find them. A lottery of 0 pays every obligation exactly, and lottery_max
				// is the largest ticket this kernel accepts from a buyer.
				// In words, and in this world's own money: an operator reading what their kernel
				// charges should not have to convert basis points in their head (§14).
				fmt.Printf("Fees:       %s of each layer's margin here, %s on work served to another kernel, %s on work imported from one\n",
					percent(out.FeeBPS), percent(out.RemoteBPS), percent(out.ImportBPS))
				fmt.Printf("Tickets:    this kernel draws for %s, and accepts tickets up to %s\n",
					net.Amount(out.Lottery), net.Amount(out.LotteryMax))
				if len(out.Addrs) > 0 {
					fmt.Println("Listen addresses:")
					for _, a := range out.Addrs {
						fmt.Printf("  %s\n", a)
					}
				}
				return nil
			}})
		},
	}
}

func adminUserListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the accounts on this kernel",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			return cli.emit("GET", "/v1/admin/users?"+q.Encode(), nil, output{id: "address", human: list(
				column{"ACCOUNT", text("address")},
				column{"STATUS", func(row json.RawMessage) string {
					if strField(row, "suspended_at") != "" {
						return "suspended"
					}
					return "active"
				}},
				column{"ABOUT", text("description")},
			)})
		},
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// adminUserDepositCmd records money arriving from outside, once, against the fact that caused it.
// Only a user is ever credited: what a peer owes closes when it pays, which nobody records by hand.
// Repeating the same fact never moves money twice, and the same fact with a different amount is
// refused — by the kernel, which is where idempotency belongs (D23).
func adminUserDepositCmd() *cobra.Command {
	var reason, ref string
	var yes bool
	cmd := &cobra.Command{
		Use:   "deposit USER [AMOUNT]",
		Short: "Credit an account for a payment received from outside",
		Long: "Credit USER for a payment received from outside this kernel.\n\n" +
			"Two forms:\n" +
			"  admin user deposit USER AMOUNT --ref FACT   record an outside payment\n" +
			"  admin user deposit USER --ref TXHASH        assign a received payment\n\n" +
			"Crediting cannot be undone: there is no matching withdraw, and the money is the " +
			"account's once it is recorded.\n\n" +
			"FACT names the payment: your own record of it where this world has no chain, or the " +
			"transaction that carried it where it has.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			if ref == "" {
				return kernel.ErrInvalidInput.Wrap("name the payment this records (--ref)")
			}
			var amount int64
			what := fmt.Sprintf("Credit %s with payment %s", args[0], ref)
			if len(args) == 2 {
				net, err := cli.network(ctx)
				if err != nil {
					return err
				}
				if amount, err = parseAmount(args[1], net.Decimals); err != nil {
					return err
				}
				what = fmt.Sprintf("Credit %s to %s", net.Amount(amount), args[0])
			}
			if err := cli.confirm(what, yes); err != nil {
				return err
			}
			return cli.emitCtx(ctx, "POST", "/v1/admin/users/"+url.PathEscape(args[0])+"/deposit", map[string]any{
				"amount": amount, "reason": reason, "ref": ref,
			}, output{money: moneyLedger})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "The payment this records: your own record of it, or the transaction that carried it")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// adminDepositsCmd is what this kernel is waiting on: payments received whose sender nobody has
// registered, and the work it has delivered to foreign buyers and not been paid for. It reads —
// a payment is credited by `admin user deposit`, and an obligation closes when its buyer pays.
func adminDepositsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deposits",
		Short: "List money received that nobody has claimed, and work delivered unpaid",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			// Two real lists rather than one flattened model, so each prints as its own rows —
			// money included, since this is where an operator reads what is outstanding.
			return cli.emitCtx(ctx, "GET", "/v1/admin/kernel/deposits", nil, output{human: func(b []byte) error {
				var a struct {
					Deposits json.RawMessage `json:"deposits"`
					Owed     json.RawMessage `json:"owed"`
				}
				if err := json.Unmarshal(b, &a); err != nil {
					return err
				}
				fmt.Println("Payments received whose sender nobody has registered:")
				if err := printFields(a.Deposits, moneyRail, net); err != nil {
					return err
				}
				fmt.Println("Work delivered to foreign buyers and not yet paid for:")
				return printFields(a.Owed, moneyOwed, net)
			}})
		},
	}
}

func peerInspectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "inspect PEER",
		Short: "Inspect a remote kernel (by public key or bound petname)",
		Long: "Inspect a remote kernel: identity, public actions, retained trade evidence, and " +
			"reachability. The petname is the local name this kernel gave the peer (`admin rename`); " +
			"the nickname is what the peer calls itself, shown for recognition but never usable as a " +
			"name. An offline peer degrades to locally cached data.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			path := "/v1/admin/peers/" + url.PathEscape(args[0]) + "/inspect"
			return cli.emitCtx(ctx, "GET", path, nil, output{id: "public_key", human: func(b []byte) error {
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
					Evidence []*kernel.SubjectEvidenceRow `json:"evidence"`
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
					StepsTruncated bool `json:"steps_truncated"`
				}
				if err := json.Unmarshal(b, &out); err != nil {
					return err
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
						fmt.Printf("  %-30s  %s\n", a.Name, net.Amount(a.Price))
						if a.Description != "" {
							fmt.Printf("      %s\n", a.Description)
						}
					}
				}
				if len(out.Evidence) > 0 {
					subjectName := out.Petname
					if subjectName == "" {
						subjectName = out.Nickname
					}
					if subjectName == "" {
						subjectName = shortKey(out.PublicKey)
					}
					printEvidence(subjectName, out.PublicKey, out.Evidence)
				}
				if len(out.Steps) > 0 {
					fmt.Printf("\nSteps awaiting us (%d) — complete with: step complete ID --peer %s\n",
						len(out.Steps), args[0])
					for _, st := range out.Steps {
						fmt.Printf("  %s  price=%s  %s\n", st.ID, net.Amount(st.Price), st.CreatedAt.Format(time.RFC3339))
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
			}})
		},
	}
}

func peerListCmd() *cobra.Command {
	var showAll bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known kernels (counterparties and discovery-only), merged by key",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if showAll {
				q.Set("all", "1")
			}
			setLimitOffset(q, limit, offset)
			path := "/v1/admin/peers"
			if e := q.Encode(); e != "" {
				path += "?" + e
			}
			// PETNAME is the local name that resolves a reference; NICKNAME is what the kernel
			// calls itself and never resolves (§13). The public key always resolves, so an unbound
			// kernel is still callable — bind a petname with `admin peer rename <key> <name>`.
			return cli.emit("GET", path, nil, output{id: "public_key", human: list(
				column{"PETNAME", dash("petname")},
				column{"NICKNAME", dash("nickname")},
				column{"TRADED", func(row json.RawMessage) string {
					if strField(row, "has_account") == "true" {
						return "yes"
					}
					return "—"
				}},
				column{"LAST SEEN", when("last_seen")},
				column{"LAST FAILED", when("last_contact_failed_at")},
				column{"ACTIONS", text("actions")},
				column{"STATUS", func(row json.RawMessage) string {
					if strField(row, "suspended_at") != "" {
						return "suspended"
					}
					return ""
				}},
				column{"PUBLIC KEY", text("public_key")},
			)})
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

// dash is a name that may not be bound yet: an unbound one is shown as a dash rather than as a gap
// the reader has to interpret.
func dash(field string) func(json.RawMessage) string {
	return func(row json.RawMessage) string {
		if v := strField(row, field); v != "" {
			return v
		}
		return "—"
	}
}

// when is a timestamp a person reads as an age, and never as a claim about now: it is when this
// kernel last proved something about that peer, not whether the peer is up (§13).
func when(field string) func(json.RawMessage) string {
	return func(row json.RawMessage) string {
		v := strField(row, field)
		if v == "" {
			return "never"
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return v
		}
		return lastSeenStr(&t)
	}
}

// percent writes a rate the way it is read rather than the way it is stored: basis points are the
// integer the config holds, and nobody says "two thousand basis points" out loud.
func percent(bps int64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.2f", float64(bps)/100), "0"), ".") + "%"
}
