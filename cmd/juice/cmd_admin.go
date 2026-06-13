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

	peerCmd := &cobra.Command{Use: "peer", Short: "Peer (federation) admin commands"}
	peerCmd.AddCommand(adminPeerListCmd(), adminPeerFriendCmd(), adminPeerUnfriendCmd(), adminPeerGossipCmd())

	adminCmd.AddCommand(userCmd, actionCmd, processCmd, txCmd, stepCmd, peerCmd)
	rootCmd.AddCommand(adminCmd)
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

func adminPeerListCmd() *cobra.Command {
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
						fmt.Println("\nDiscovered via gossip:")
						for _, d := range discovered {
							fmt.Printf("  %-20s %-50s (via %s)\n", d.Handle, d.BaseURL, d.IntroducedBy[:min(len(d.IntroducedBy), 16)])
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

func adminPeerFriendCmd() *cobra.Command {
	var peerURL string
	cmd := &cobra.Command{
		Use:   "friend",
		Short: "Friend a remote kernel (fetch well-known, register locally, send signed request)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				// Fetch /.well-known/juice-kernel.json from the peer.
				exec := &httpActionExecutor{timeout: 15 * time.Second, allowLocal: globalCfg.AllowLocalSources}
				wkBody, err := exec.FetchURL(ctx, strings.TrimRight(peerURL, "/")+"/.well-known/juice-kernel.json")
				if err != nil {
					return fmt.Errorf("fetch well-known: %w", err)
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
					sig, ts, serr := k.SignPeerRequestNow(localHandle, localPubKey, localBaseURL)
					if serr == nil {
						body, _ := json.Marshal(map[string]string{
							"handle":     localHandle,
							"public_key": localPubKey,
							"base_url":   localBaseURL,
							"timestamp":  ts,
							"signature":  sig,
						})
						_, _, _ = doHTTP(ctx, http.MethodPost, strings.TrimRight(wk.BaseURL, "/")+"/v1/peers",
							map[string]string{"Content-Type": "application/json"},
							strings.NewReader(string(body)), exec.timeout, exec.allowLocal)
					}
				}
				fmt.Printf("Friended peer %s (%s)\n", u.Handle, u.RemoteBaseURL)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&peerURL, "url", "", "Remote kernel base URL (required)")
	_ = cmd.MarkFlagRequired("url")
	return cmd
}

func adminPeerUnfriendCmd() *cobra.Command {
	var handle string
	cmd := &cobra.Command{
		Use:   "unfriend",
		Short: "Unfriend (deny) a peer: deactivate their proxy actions and cancel their steps",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				if err := k.DenyPeer(context.Background(), subjectID, handle); err != nil {
					return err
				}
				fmt.Printf("Peer %s unfriended.\n", handle)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&handle, "handle", "", "Peer handle (required)")
	_ = cmd.MarkFlagRequired("handle")
	return cmd
}

func adminPeerGossipCmd() *cobra.Command {
	var peerURL, peerHandle string
	cmd := &cobra.Command{
		Use:   "gossip",
		Short: "Fetch gossip from a peer and accumulate discovered kernels",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, _ string) error {
				ctx := context.Background()
				targetURL := peerURL
				if targetURL == "" && peerHandle != "" {
					peer, err := k.ReadUserByHandle(ctx, peerHandle)
					if err != nil || peer == nil {
						return fmt.Errorf("peer %q not found", peerHandle)
					}
					targetURL = peer.RemoteBaseURL
				}
				if targetURL == "" {
					return fmt.Errorf("--url or --handle required")
				}
				exec := &httpActionExecutor{timeout: 15 * time.Second, allowLocal: globalCfg.AllowLocalSources}
				body, err := exec.FetchURL(ctx, strings.TrimRight(targetURL, "/")+"/v1/gossip")
				if err != nil {
					return fmt.Errorf("fetch gossip: %w", err)
				}
				var gossip kernel.GossipResponse
				if err := json.Unmarshal(body, &gossip); err != nil {
					return fmt.Errorf("parse gossip: %w", err)
				}
				localPubKey, _ := k.GetConfig(ctx, configKeySigningPublic)
				if err := k.AccumulateGossip(ctx, &gossip, localPubKey); err != nil {
					return fmt.Errorf("accumulate gossip: %w", err)
				}
				fmt.Printf("Gossip from %s: %d actions, %d friends.\n", gossip.Handle, len(gossip.Actions), len(gossip.Friends))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&peerURL, "url", "", "Remote kernel base URL")
	cmd.Flags().StringVar(&peerHandle, "handle", "", "Registered peer handle")
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
	var actionID string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Disable an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
				if err := k.SetActive(context.Background(), subjectID, actionID, false); err != nil {
					return err
				}
				fmt.Printf("Action %s disabled.\n", actionID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("id")
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
					fmt.Printf("%s  process=%-8s  status=%-7s  tx=%s\n",
						s.ID[:8], s.ProcessID[:8], s.Status, txID)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	return cmd
}
