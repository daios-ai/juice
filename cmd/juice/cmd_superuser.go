package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

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

func adminUsersCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "users",
		Short: "List all users",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				users, err := k.ListUsers(context.Background(), limit, offset)
				if err != nil {
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
			})
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
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				u, err := resolveHandle(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				return emit(u)
			})
		},
	}
}

func adminSuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <user>",
		Short: "Suspend a user account (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withSuperuser(func(k *kernel.Kernel, operatorID string) error {
				u, err := resolveHandle(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := k.SuspendUser(context.Background(), operatorID, u.ID); err != nil {
					return err
				}
				fmt.Printf("User %s suspended.\n", u.Handle)
				return nil
			})
		},
	}
}

func adminUnsuspendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unsuspend <user>",
		Short: "Unsuspend a user account (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withSuperuser(func(k *kernel.Kernel, operatorID string) error {
				u, err := resolveHandle(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := k.UnsuspendUser(context.Background(), operatorID, u.ID); err != nil {
					return err
				}
				fmt.Printf("User %s unsuspended.\n", u.Handle)
				return nil
			})
		},
	}
}

func adminDepositCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "deposit <user> <amount>",
		Short: "Add credits to a user account (user is @handle)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			amount, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("amount must be a positive integer")
			}
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				u, err := resolveHandle(k, ctx, args[0])
				if err != nil {
					return err
				}
				d, err := k.Deposit(ctx, subjectID, u.ID, amount, reason)
				if err != nil {
					return err
				}
				return emit(d)
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	return cmd
}

func adminWithdrawCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "withdraw <user> <amount>",
		Short: "Deduct credits from a user account (user is @handle)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			amount, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return fmt.Errorf("amount must be a positive integer")
			}
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				u, err := resolveHandle(k, ctx, args[0])
				if err != nil {
					return err
				}
				w, err := k.Withdraw(ctx, subjectID, u.ID, amount, reason)
				if err != nil {
					return err
				}
				return emit(w)
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	return cmd
}

func adminActionsCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "actions",
		Short: "List all actions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				actions, err := k.ListAllActions(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagJSON {
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
					fmt.Printf("[%s%s] @%s/%s  %d credits\n", active, public, a.OwnerHandle, a.Name, a.Price)
				}
				return nil
			})
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
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				a, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := k.SetActive(context.Background(), subjectID, a.ID, false); err != nil {
					return err
				}
				fmt.Printf("Action %s disabled.\n", args[0])
				return nil
			})
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
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				processes, err := k.ListAllProcesses(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(processes)
				}
				for _, p := range processes {
					fmt.Printf("%s  owner=%s  status=%-6s  avail=%d\n",
						p.ID, p.OwnerUserID, p.Status, p.Available)
				}
				return nil
			})
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
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				txs, err := k.ListAllTransactions(context.Background(), limit, offset)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(txs)
				}
				for _, tx := range txs {
					fmt.Printf("%s  action=%s  status=%-7s  gross=%d\n",
						tx.ID, tx.ActionName, tx.Status, tx.Gross)
				}
				return nil
			})
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
			return withSuperuser(func(k *kernel.Kernel, superuserID string) error {
				steps, err := k.ListSteps(context.Background(), superuserID, processID, status)
				if err != nil {
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
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	return cmd
}

func peerInspectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inspect <url>",
		Short: "Show a remote kernel's identity and public actions (no auth, no DB write)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			base := strings.TrimRight(args[0], "/")
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
	return cmd
}

func peerFriendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "friend <url>",
		Short: "Befriend a remote kernel: register as peer and import all their active public actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				allow := allowLocalPeers()
				base := strings.TrimRight(args[0], "/")

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
	cmd := &cobra.Command{
		Use:   "unfriend <user>",
		Short: "Unfriend a peer: deny their calls and deactivate their proxy actions (user is @handle)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				handle := args[0]
				if !strings.HasPrefix(handle, "@") {
					handle = "@" + handle
				}
				if err := k.DenyPeer(context.Background(), subjectID, handle); err != nil {
					return err
				}
				fmt.Printf("Unfriended %s.\n", handle)
				return nil
			})
		},
	}
	return cmd
}

func peerListCmd() *cobra.Command {
	var showGossip bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List known remote kernel peers",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				ctx := context.Background()
				peers, err := k.ListPeers(ctx)
				if err != nil {
					return err
				}
				if flagJSON {
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
