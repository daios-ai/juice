package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Admin management commands"}

	userCmd := &cobra.Command{Use: "user", Short: "User admin commands"}
	userCmd.AddCommand(
		adminUserListCmd(),
		adminUserShowCmd(),
		adminUserSuspendCmd(),
		adminUserUnsuspendCmd(),
		adminUserDepositCmd(),
		adminUserWithdrawCmd(),
	)

	actionCmd := &cobra.Command{Use: "action", Short: "Action admin commands"}
	actionCmd.AddCommand(adminActionListCmd(), adminActionDisableCmd())

	processCmd := &cobra.Command{Use: "process", Short: "Process admin commands"}
	processCmd.AddCommand(adminProcessListCmd())

	txCmd := &cobra.Command{Use: "tx", Short: "Transaction admin commands"}
	txCmd.AddCommand(adminTxListCmd())

	stepCmd := &cobra.Command{Use: "step", Short: "Step admin commands"}
	stepCmd.AddCommand(adminStepListCmd())

	adminCmd.AddCommand(userCmd, actionCmd, processCmd, txCmd, stepCmd)
	rootCmd.AddCommand(adminCmd)
}

func init() {
	peerCmd := &cobra.Command{Use: "peer", Short: "Manage peer kernels and federation"}
	peerCmd.AddCommand(peerInspectCmd(), peerFriendCmd(), peerUnfriendCmd(), peerListCmd())
	rootCmd.AddCommand(peerCmd)
}

// allowLocalPeers returns true if peer federation HTTP calls may reach local/private addresses.
// allow_local_peer_urls targets peer traffic only; allow_local_sources enables everything.
func allowLocalPeers() bool {
	return globalCfg.AllowLocalPeerURLs || globalCfg.AllowLocalSources
}

func requireSuperuser(k *kernel.Kernel) (string, error) {
	subjectID, err := requireCallerID(k)
	if err != nil {
		return "", err
	}
	subject, err := k.ReadUser(context.Background(), subjectID)
	if err != nil {
		return "", err
	}
	expectedHandle, _ := k.GetConfig(context.Background(), configKeySuperuser)
	if expectedHandle == "" {
		expectedHandle = superuserHandle
	}
	if subject.Handle != expectedHandle {
		return "", kernel.ErrUnauthorized.Wrap("superuser required")
	}
	return subjectID, nil
}

func adminUserListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all users",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				users, err := k.ListUsers(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(users)
				}
				for _, u := range users {
					suspended := ""
					if u.SuspendedAt != nil {
						suspended = " [SUSPENDED]"
					}
					fmt.Printf("%s  %-20s  %s%s\n", u.ID[:8], u.Handle, u.Email, suspended)
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminUserShowCmd() *cobra.Command {
	var userID, handle string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show user details",
		RunE: func(c *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				ctx := context.Background()
				var u *kernel.User
				var err error
				if c.Flags().Changed("handle") {
					u, err = k.ReadUserByHandle(ctx, handle)
				} else if c.Flags().Changed("id") {
					u, err = k.ReadUser(ctx, userID)
				} else {
					return fmt.Errorf("either --id or --handle is required")
				}
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(u)
				}
				suspended := "no"
				if u.SuspendedAt != nil {
					suspended = u.SuspendedAt.String()
				}
				fmt.Printf("User: %s\n  handle:     %s\n  email:      %s\n  available:  %d\n  suspended:  %s\n",
					u.ID, u.Handle, u.Email, u.Available, suspended)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "User ID")
	cmd.Flags().StringVar(&handle, "handle", "", "User handle (e.g. @sys)")
	return cmd
}

func adminUserSuspendCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:   "suspend",
		Short: "Suspend a user account",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, operatorID string) error {
				if err := k.SuspendUser(context.Background(), operatorID, userID); err != nil {
					return err
				}
				fmt.Printf("User %s suspended.\n", userID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "User ID to suspend (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func adminUserUnsuspendCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:   "unsuspend",
		Short: "Unsuspend a user account",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, operatorID string) error {
				if err := k.UnsuspendUser(context.Background(), operatorID, userID); err != nil {
					return err
				}
				fmt.Printf("User %s unsuspended.\n", userID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "User ID to unsuspend (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func adminUserDepositCmd() *cobra.Command {
	var userID, handle, reason string
	var amount int64
	cmd := &cobra.Command{
		Use:   "deposit",
		Short: "Add credits to a user account",
		RunE: func(c *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				var targetID string
				if c.Flags().Changed("handle") {
					u, err := k.ReadUserByHandle(ctx, handle)
					if err != nil {
						return err
					}
					targetID = u.ID
				} else if c.Flags().Changed("id") {
					targetID = userID
				} else {
					return fmt.Errorf("either --id or --handle is required")
				}
				d, err := k.Deposit(ctx, subjectID, targetID, amount, reason)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(d)
				}
				fmt.Printf("Deposited %d credits to %s (deposit id: %s)\n", d.Amount, targetID, d.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "Target user ID")
	cmd.Flags().StringVar(&handle, "handle", "", "Target user handle (e.g. @alice)")
	cmd.Flags().Int64Var(&amount, "amount", 0, "Credits to deposit (required, > 0)")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	_ = cmd.MarkFlagRequired("amount")
	return cmd
}

func adminUserWithdrawCmd() *cobra.Command {
	var userID, handle, reason string
	var amount int64
	cmd := &cobra.Command{
		Use:   "withdraw",
		Short: "Deduct credits from a user account",
		RunE: func(c *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				var targetID string
				if c.Flags().Changed("handle") {
					u, err := k.ReadUserByHandle(ctx, handle)
					if err != nil {
						return err
					}
					targetID = u.ID
				} else if c.Flags().Changed("id") {
					targetID = userID
				} else {
					return fmt.Errorf("either --id or --handle is required")
				}
				w, err := k.Withdraw(ctx, subjectID, targetID, amount, reason)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(w)
				}
				fmt.Printf("Withdrew %d credits from %s (withdrawal id: %s)\n", w.Amount, targetID, w.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "Target user ID")
	cmd.Flags().StringVar(&handle, "handle", "", "Target user handle (e.g. @alice)")
	cmd.Flags().Int64Var(&amount, "amount", 0, "Credits to withdraw (required, > 0)")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	_ = cmd.MarkFlagRequired("amount")
	return cmd
}

func adminActionListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				actions, err := k.ListAllActions(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(actions)
				}
				for _, a := range actions {
					active := " "
					if a.Active {
						active = "*"
					}
					public := " "
					if a.Public {
						public = "P"
					}
					fmt.Printf("[%s%s] %s  %-30s  %d credits\n", active, public, a.ID[:8], a.Name, a.Price)
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminActionDisableCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				if err := k.SetActive(context.Background(), subjectID, id, false); err != nil {
					return err
				}
				fmt.Printf("Action %s disabled.\n", id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

func adminProcessListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all processes",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				processes, err := k.ListAllProcesses(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(processes)
				}
				for _, p := range processes {
					fmt.Printf("%s  owner=%-20s  status=%-6s  avail=%d\n",
						p.ID[:8], p.OwnerUserID[:8], p.Status, p.Available)
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminTxListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all transactions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				txs, err := k.ListAllTransactions(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(txs)
				}
				for _, tx := range txs {
					fmt.Printf("%s  action=%-20s  status=%-7s  gross=%d\n",
						tx.ID[:8], tx.ActionID[:8], tx.Status, tx.Gross)
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminStepListCmd() *cobra.Command {
	var processID, status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all steps",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, superuserID string) error {
				steps, err := k.ListSteps(context.Background(), superuserID, processID, status)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(steps)
				}
				for _, s := range steps {
					txID := "-"
					if s.TxID != nil {
						txID = (*s.TxID)[:8]
					}
					fmt.Printf("%s  status=%-7s  tx=%s\n",
						s.ID[:8], s.Status, txID)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	return cmd
}

func peerInspectCmd() *cobra.Command {
	var peerURL string
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Show a remote kernel's identity and public actions (no auth, no DB write)",
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			base := strings.TrimRight(peerURL, "/")
			allow := allowLocalPeers()

			// Fetch peer identity from well-known endpoint.
			wkBody, status, err := doHTTP(ctx, http.MethodGet, base+"/.well-known/juice-kernel.json", nil, nil, 15*time.Second, allow)
			if err != nil {
				return fmt.Errorf("fetch well-known: %w", err)
			}
			if status != http.StatusOK {
				return fmt.Errorf("fetch well-known: status %d", status)
			}
			var wk struct {
				Handle    string `json:"handle"`
				PublicKey string `json:"public_key"`
				BaseURL   string `json:"base_url"`
			}
			if err := json.Unmarshal(wkBody, &wk); err != nil {
				return fmt.Errorf("parse well-known: %w", err)
			}
			fp := wk.PublicKey
			if len(fp) > 16 {
				fp = fp[:16] + "…"
			}
			fmt.Printf("Handle:     %s\n", wk.Handle)
			fmt.Printf("Public key: %s\n", fp)
			fmt.Printf("Base URL:   %s\n", wk.BaseURL)

			// Fetch public gossip (actions + transacted friends).
			gossipBody, gStatus, gErr := doHTTP(ctx, http.MethodGet, base+"/v1/gossip", nil, nil, 15*time.Second, allow)
			if gErr != nil || gStatus != http.StatusOK {
				fmt.Printf("\n(gossip unavailable)\n")
				return nil
			}
			var gossip kernel.GossipResponse
			if err := json.Unmarshal(gossipBody, &gossip); err != nil {
				return nil
			}
			if len(gossip.Actions) > 0 {
				fmt.Printf("\nActive actions (%d):\n", len(gossip.Actions))
				for _, a := range gossip.Actions {
					fmt.Printf("  %-30s  %d credits  (uses: %d)\n", a.Name, a.Price, a.Uses)
				}
			}
			if len(gossip.Friends) > 0 {
				fmt.Printf("\nTransacted friends (%d):\n", len(gossip.Friends))
				for _, f := range gossip.Friends {
					fmt.Printf("  %s  %s\n", f.Handle, f.BaseURL)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&peerURL, "url", "", "Remote kernel base URL (required)")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

func peerFriendCmd() *cobra.Command {
	var peerURL string
	cmd := &cobra.Command{
		Use:   "friend",
		Short: "Befriend a remote kernel: register as peer and import all their active public actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				allow := allowLocalPeers()
				base := strings.TrimRight(peerURL, "/")

				// Fetch peer identity.
				wkBody, status, err := doHTTP(ctx, http.MethodGet, base+"/.well-known/juice-kernel.json", nil, nil, 15*time.Second, allow)
				if err != nil {
					return fmt.Errorf("fetch well-known: %w", err)
				}
				if status != http.StatusOK {
					return fmt.Errorf("fetch well-known: status %d", status)
				}
				var wk struct {
					Handle    string `json:"handle"`
					PublicKey string `json:"public_key"`
					BaseURL   string `json:"base_url"`
				}
				if err := json.Unmarshal(wkBody, &wk); err != nil {
					return fmt.Errorf("parse well-known: %w", err)
				}

				// Register peer locally (clears denial if previously denied).
				u, err := k.CreateOrUpdateProxyPeer(ctx, wk.Handle, wk.PublicKey, wk.BaseURL)
				if err != nil {
					return fmt.Errorf("register peer locally: %w", err)
				}

				// Send signed friend request to the peer's /v1/peers.
				localPubKey, _ := k.GetConfig(ctx, configKeySigningPublic)
				localHandle := globalCfg.PeerHandle
				if localHandle == "" {
					localHandle, _ = k.GetConfig(ctx, configKeySuperuser)
				}
				localBaseURL := globalCfg.ServerURL
				if localPubKey != "" && localBaseURL != "" {
					if sig, ts, serr := k.SignPeerRequestNow(localHandle, localPubKey, localBaseURL); serr == nil {
						body, _ := json.Marshal(map[string]string{
							"handle":     localHandle,
							"public_key": localPubKey,
							"base_url":   localBaseURL,
							"timestamp":  ts,
							"signature":  sig,
						})
						_, _, _ = doHTTP(ctx, http.MethodPost, strings.TrimRight(wk.BaseURL, "/")+"/v1/peers",
							map[string]string{"Content-Type": "application/json"},
							strings.NewReader(string(body)), 15*time.Second, allow)
					}
				}

				// Bulk-import all active public actions from the peer.
				imported, skipped := bulkImportPeerActions(ctx, k, subjectID, u, allow)

				// Accumulate gossip to discover the peer's friends.
				if gossipBody, gStatus, gErr := doHTTP(ctx, http.MethodGet, strings.TrimRight(wk.BaseURL, "/")+"/v1/gossip", nil, nil, 15*time.Second, allow); gErr == nil && gStatus == http.StatusOK {
					var gossip kernel.GossipResponse
					if json.Unmarshal(gossipBody, &gossip) == nil {
						localPubKey2, _ := k.GetConfig(ctx, configKeySigningPublic)
						_ = k.AccumulateGossip(ctx, &gossip, localPubKey2)
					}
				}

				msg := fmt.Sprintf("Friended %s", u.Handle)
				if imported > 0 {
					msg += fmt.Sprintf(" — %d action(s) available", imported)
				}
				if skipped > 0 {
					msg += fmt.Sprintf(", %d skipped", skipped)
				}
				fmt.Println(msg + ".")
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&peerURL, "url", "", "Remote kernel base URL (required)")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

// bulkImportPeerActions fetches all active public actions from a peer and imports them as
// enabled, public remote_proxy actions. Returns the number imported and skipped.
func bulkImportPeerActions(ctx context.Context, k *kernel.Kernel, subjectID string, peer *kernel.User, allowLocal bool) (imported, skipped int) {
	base := strings.TrimRight(peer.RemoteBaseURL, "/")

	listBody, status, err := doHTTP(ctx, http.MethodGet, base+"/v1/actions", nil, nil, 30*time.Second, allowLocal)
	if err != nil || status != http.StatusOK {
		return 0, 0
	}
	var actions []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(listBody, &actions) != nil {
		return 0, 0
	}

	for _, a := range actions {
		mBody, mStatus, mErr := doHTTP(ctx, http.MethodGet, fmt.Sprintf("%s/v1/actions/%s/manifest", base, a.ID), nil, nil, 30*time.Second, allowLocal)
		if mErr != nil || mStatus != http.StatusOK {
			skipped++
			continue
		}
		var m kernel.ActionManifest
		if json.Unmarshal(mBody, &m) != nil {
			skipped++
			continue
		}

		result, rErr := k.ReconcileRemoteAction(ctx, subjectID, peer.Handle, a.Name, &m)
		if rErr != nil {
			skipped++
			continue
		}

		// Enable and publish newly created or contract-changed actions.
		t := true
		for _, act := range append(result.Created, result.Updated...) {
			_ = enableAction(k, ctx, subjectID, act.ID)
			_, _ = k.UpdateAction(ctx, subjectID, kernel.UpdateActionRequest{ID: act.ID, Public: &t})
		}
		imported += len(result.Created) + len(result.Unchanged)
	}
	return imported, skipped
}

func peerUnfriendCmd() *cobra.Command {
	var handle string
	cmd := &cobra.Command{
		Use:   "unfriend",
		Short: "Unfriend a peer: deny their calls and deactivate their proxy actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				if err := k.DenyPeer(context.Background(), subjectID, handle); err != nil {
					return err
				}
				fmt.Printf("Unfriended %s.\n", handle)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&handle, "handle", "", "Peer handle (required)")
	_ = cmd.MarkFlagRequired("handle")
	return cmd
}

func peerListCmd() *cobra.Command {
	var showGossip bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known remote kernel peers",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				ctx := context.Background()
				peers, err := k.ListPeers(ctx)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(peers)
				}
				if len(peers) == 0 {
					fmt.Println("No peers registered.")
				} else {
					fmt.Printf("%-20s %-36s %s\n", "HANDLE", "ID", "BASE_URL")
					for _, p := range peers {
						denied := ""
						if p.DeniedAt != nil {
							denied = " [denied]"
						}
						fmt.Printf("%-20s %-36s %s%s\n", p.Handle, p.ID, p.RemoteBaseURL, denied)
					}
				}
				if showGossip {
					discovered, err := k.ListDiscoveredKernels(ctx)
					if err != nil {
						return err
					}
					if len(discovered) > 0 {
						peerHandle := map[string]string{}
						for _, p := range peers {
							if len(p.PublicKey) >= 16 {
								peerHandle[p.PublicKey[:16]] = p.Handle
							}
						}
						fmt.Println("\nDiscovered via gossip:")
						for _, d := range discovered {
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
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&showGossip, "gossip", false, "Also show gossip-discovered kernels")
	return cmd
}
