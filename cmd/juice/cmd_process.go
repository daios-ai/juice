package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Process lifecycle commands"}
	processCmd.AddCommand(processListCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
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
