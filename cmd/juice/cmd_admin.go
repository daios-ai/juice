package main

import (
	"context"
	"fmt"

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
	)

	actionCmd := &cobra.Command{Use: "action", Short: "Action admin commands"}
	actionCmd.AddCommand(adminActionListCmd(), adminActionDisableCmd())

	processCmd := &cobra.Command{Use: "process", Short: "Process admin commands"}
	processCmd.AddCommand(adminProcessListCmd())

	txCmd := &cobra.Command{Use: "tx", Short: "Transaction admin commands"}
	txCmd.AddCommand(adminTxListCmd())

	adminCmd.AddCommand(userCmd, actionCmd, processCmd, txCmd)
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
	if subject.Handle != superuserHandle {
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
