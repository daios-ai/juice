package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	txCmd := &cobra.Command{Use: "tx", Short: "Transaction commands"}
	txCmd.AddCommand(txListCmd(), txShowCmd(), txRateCmd(), txVerifyReceiptCmd())
	rootCmd.AddCommand(txCmd)
}

func txListCmd() *cobra.Command {
	var processID string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List transactions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				txs, err := k.ListTransactions(context.Background(), callerID, kernel.TxFilter{
					ProcessID: processID,
					Limit:     limit,
					Offset:    offset,
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
			})
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
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				tv, err := k.ReadTransaction(context.Background(), callerID, txID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(tv)
				}
				fmt.Printf("Transaction: %s\n  status:  %s\n  action:  %s\n  gross:   %d\n  net:     %d\n  fee:     %d\n  reason:  %s\n",
					tv.ID, tv.Status, tv.ActionID, tv.Gross, tv.Net, tv.Fee, tv.Reason)
				if tv.Rating != nil {
					if tv.Rating.Note != nil {
						fmt.Printf("  rating:  %.0f (%s)\n", tv.Rating.Value, *tv.Rating.Note)
					} else {
						fmt.Printf("  rating:  %.0f\n", tv.Rating.Value)
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func txVerifyReceiptCmd() *cobra.Command {
	var txID string
	cmd := &cobra.Command{
		Use:   "verify-receipt",
		Short: "Verify the remote receipt for a transaction",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				v, err := k.VerifyRemoteReceipt(context.Background(), callerID, txID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(v)
				}
				status := "PASS"
				if !v.Valid {
					status = "FAIL"
				}
				fmt.Printf("Receipt verification: %s  [%s]\n  remote: %s\n", txID[:8], status, v.RemoteKernelHandle)
				fmt.Printf("  receipt_hash:  %v\n  signature:     %v\n  action_id:     %v\n",
					v.Checks.ReceiptHash, v.Checks.Signature, v.Checks.ActionID)
				fmt.Printf("  status:        %v\n  gross:         %v\n  net:           %v\n  fee:           %v\n",
					v.Checks.Status, v.Checks.Gross, v.Checks.Net, v.Checks.Fee)
				fmt.Printf("  args_hash:     %v\n  reply_hash:    %v\n",
					v.Checks.ArgsHash, v.Checks.ReplyHash)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func txRateCmd() *cobra.Command {
	var txID string
	var rating float64
	var note string
	cmd := &cobra.Command{
		Use:   "rate",
		Short: "Rate a transaction (0 or 1)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				var notePtr *string
				if note != "" {
					notePtr = &note
				}
				if _, err := k.RateTransaction(context.Background(), callerID, txID, rating, notePtr); err != nil {
					return err
				}
				fmt.Printf("Transaction %s rated %.0f.\n", txID, rating)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	cmd.Flags().Float64Var(&rating, "rating", -1, "Rating: 0 (bad) or 1 (good) (required)")
	cmd.Flags().StringVar(&note, "note", "", "Optional justification note")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("rating")
	return cmd
}
