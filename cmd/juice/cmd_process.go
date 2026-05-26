package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Process lifecycle commands"}
	processCmd.AddCommand(processStartCmd(), processFundCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processStartCmd() *cobra.Command {
	var funds int64
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a new budgeted process",
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

			p, t, err := k.StartProcess(context.Background(), subjectID, funds)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{
					"process_id": p.ID,
					"trace_id":   t.ID,
					"available":  p.Available,
				})
			}
			fmt.Printf("Process started.\n  process_id: %s\n  trace_id:   %s\n  available:  %d\n",
				p.ID, t.ID, p.Available)
			return nil
		},
	}
	cmd.Flags().Int64Var(&funds, "funds", 0, "Initial credit allocation")
	return cmd
}

func processFundCmd() *cobra.Command {
	var processID string
	var funds int64
	cmd := &cobra.Command{
		Use:   "fund",
		Short: "Add credits to an open process",
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

			if err := k.FundProcess(context.Background(), subjectID, processID, funds); err != nil {
				return err
			}
			fmt.Printf("Added %d credits to process %s.\n", funds, processID)
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	cmd.Flags().Int64Var(&funds, "funds", 0, "Credits to add (required, > 0)")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("funds")
	return cmd
}

func processEndCmd() *cobra.Command {
	var processID string
	cmd := &cobra.Command{
		Use:   "end",
		Short: "End a process and return remaining funds",
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

			if err := k.EndProcess(context.Background(), subjectID, processID); err != nil {
				return err
			}
			fmt.Printf("Process %s ended.\n", processID)
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func processShowCmd() *cobra.Command {
	var processID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show process details",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			p, err := k.ReadProcess(context.Background(), processID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(p)
			}
			fmt.Printf("Process: %s\n  owner:     %s\n  status:    %s\n  available: %d\n  locked:    %d\n",
				p.ID, p.OwnerUserID, p.Status, p.Available, p.Locked)
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

