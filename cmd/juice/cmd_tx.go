package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	txCmd := &cobra.Command{Use: "tx", Short: "Transaction commands"}
	txCmd.AddCommand(txListCmd(), txShowCmd())
	rootCmd.AddCommand(txCmd)
}

func txListCmd() *cobra.Command {
	var processID string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List transactions",
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

			txs, err := k.ListTransactions(context.Background(), kernel.TxFilter{
				OwnerUserID: subjectID,
				ProcessID:   processID,
				Limit:       limit,
				Offset:      offset,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(txs)
			}
			for _, tx := range txs {
				fmt.Printf("[%s] %s  action:%s  status:%s  gross:%d\n",
					tx.StartedAt.Format("2006-01-02T15:04:05"),
					tx.ID[:8], tx.ActionID[:8], tx.Status, tx.Gross)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func txShowCmd() *cobra.Command {
	var txID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show a transaction",
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

			tx, err := k.ReadTransaction(context.Background(), subjectID, txID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(tx)
			}
			fmt.Printf("Transaction: %s\n  status:  %s\n  action:  %s\n  gross:   %d\n  net:     %d\n  fee:     %d\n  reason:  %s\n",
				tx.ID, tx.Status, tx.ActionID, tx.Gross, tx.Net, tx.Fee, tx.Reason)
			return nil
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
