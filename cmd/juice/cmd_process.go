package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Process lifecycle commands"}
	processCmd.AddCommand(processStartCmd(), processListCmd(), processFundCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processStartCmd() *cobra.Command {
	var funds int64
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start a new budgeted process",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				p, t, err := k.StartProcess(context.Background(), callerID, callerID, funds)
				if err != nil {
					return err
				}
				if flagQuiet {
					printQuiet(p.ID)
					return nil
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
			})
		},
	}
	cmd.Flags().Int64Var(&funds, "funds", 0, "Initial credit allocation")
	return cmd
}

func processListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List processes owned by the current user",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				processes, err := k.ListProcesses(context.Background(), callerID, 100, 0)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(processes)
				}
				for _, p := range processes {
					fmt.Printf("%s  %-6s  available:%-6d  locked:%-6d\n",
						p.ID[:8], p.Status, p.Available, p.Locked)
				}
				return nil
			})
		},
	}
	return cmd
}

func processFundCmd() *cobra.Command {
	var processID string
	var funds int64
	cmd := &cobra.Command{
		Use:   "fund",
		Short: "Add credits to an open process",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				if err := k.FundProcess(context.Background(), callerID, processID, funds); err != nil {
					return err
				}
				fmt.Printf("Added %d credits to process %s.\n", funds, processID)
				return nil
			})
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
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				if err := k.EndProcess(context.Background(), callerID, processID); err != nil {
					return err
				}
				fmt.Printf("Process %s ended.\n", processID)
				return nil
			})
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
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				p, err := k.ReadProcess(context.Background(), callerID, processID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(p)
				}
				fmt.Printf("Process: %s\n  owner:     %s\n  status:    %s\n  available: %d\n  locked:    %d\n",
					p.ID, p.OwnerUserID, p.Status, p.Available, p.Locked)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
