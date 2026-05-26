package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	adminCmd := &cobra.Command{Use: "admin", Short: "Admin management commands"}

	// admin user
	userCmd := &cobra.Command{Use: "user", Short: "User admin commands"}
	userCmd.AddCommand(
		adminUserListCmd(),
		adminUserShowCmd(),
		adminUserSuspendCmd(),
		adminUserUnsuspendCmd(),
	)

	// admin action
	actionCmd := &cobra.Command{Use: "action", Short: "Action admin commands"}
	actionCmd.AddCommand(
		adminActionListCmd(),
		adminActionDisableCmd(),
	)

	// admin process
	processCmd := &cobra.Command{Use: "process", Short: "Process admin commands"}
	processCmd.AddCommand(adminProcessListCmd())

	// admin tx
	txCmd := &cobra.Command{Use: "tx", Short: "Transaction admin commands"}
	txCmd.AddCommand(adminTxListCmd())

	adminCmd.AddCommand(userCmd, actionCmd, processCmd, txCmd)
	rootCmd.AddCommand(adminCmd)
}

func adminUserListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all users",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

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
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func adminUserShowCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show user details",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			u, err := k.ReadUser(context.Background(), userID)
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
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "User ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func adminUserSuspendCmd() *cobra.Command {
	var userID string
	cmd := &cobra.Command{
		Use:   "suspend",
		Short: "Suspend a user account",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			subjectID, err := requireSubjectID(k)
			if err != nil {
				return err
			}

			if err := k.SuspendUser(context.Background(), subjectID, userID); err != nil {
				return err
			}
			fmt.Printf("User %s suspended.\n", userID)
			return nil
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
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			subjectID, err := requireSubjectID(k)
			if err != nil {
				return err
			}

			if err := k.UnsuspendUser(context.Background(), subjectID, userID); err != nil {
				return err
			}
			fmt.Printf("User %s unsuspended.\n", userID)
			return nil
		},
	}
	cmd.Flags().StringVar(&userID, "id", "", "User ID to unsuspend (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func adminActionListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

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
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			subjectID, err := requireSubjectID(k)
			if err != nil {
				return err
			}

			if err := k.SetActive(context.Background(), subjectID, actionID, false); err != nil {
				return err
			}
			fmt.Printf("Action %s disabled.\n", actionID)
			return nil
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
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

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
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

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
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}
