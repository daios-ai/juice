package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	stepCmd := &cobra.Command{Use: "step", Short: "Step management commands"}
	stepCmd.AddCommand(stepCreateCmd(), stepListCmd(), stepShowCmd(), stepCompleteCmd())
	rootCmd.AddCommand(stepCmd)
}

func stepCreateCmd() *cobra.Command {
	var processID, action, requiredCaller, parentTrace string
	var partialArgs, inputSchema string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a step (pause point for external completion)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				ctx := context.Background()

				// Resolve @owner/name → actionID.
				ownerHandle, actionName, err := kernel.ParseActionRef(action)
				if err != nil {
					return err
				}
				owner, err := k.ReadUserByHandle(ctx, ownerHandle)
				if err != nil {
					return fmt.Errorf("action owner not found: %w", err)
				}
				act, err := k.ReadActionByOwnerName(ctx, owner.ID, actionName)
				if err != nil {
					return fmt.Errorf("action not found: %w", err)
				}

				// Resolve @handle → userID for required_caller.
				callerUser, err := k.ReadUserByHandle(ctx, requiredCaller)
				if err != nil {
					return fmt.Errorf("required_caller not found: %w", err)
				}

				var pa json.RawMessage
				if partialArgs != "" {
					pa = json.RawMessage(partialArgs)
				}
				var is json.RawMessage
				if inputSchema != "" {
					is = json.RawMessage(inputSchema)
				}

				var pt *string
				if parentTrace != "" {
					pt = &parentTrace
				}

				step, err := k.CreateStep(ctx, callerID, processID, pt, act.ID, pa, is, callerUser.ID)
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(step.ID)
					return nil
				}
				if flagOutput == "json" {
					return printJSON(step)
				}
				fmt.Printf("Step created.\n  step_id:  %s\n  status:   %s\n  process:  %s\n  action:   %s\n",
					step.ID, step.Status, step.ProcessID, action)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Process ID (required)")
	cmd.Flags().StringVar(&action, "action", "", "Action reference @owner/name (required)")
	cmd.Flags().StringVar(&requiredCaller, "required-caller", "", "Handle of user who must complete the step, e.g. @webhook (required)")
	cmd.Flags().StringVar(&partialArgs, "partial-args", "", "Partial args as JSON object")
	cmd.Flags().StringVar(&inputSchema, "input-schema", "", "JSON Schema for completion input")
	cmd.Flags().StringVar(&parentTrace, "parent-trace", "", "Parent trace ID")
	_ = cmd.MarkFlagRequired("process")
	_ = cmd.MarkFlagRequired("action")
	_ = cmd.MarkFlagRequired("required-caller")
	return cmd
}

func stepListCmd() *cobra.Command {
	var processID, status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List steps visible to the current user",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				steps, err := k.ListSteps(context.Background(), callerID, processID, status)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(steps)
				}
				for _, s := range steps {
					fmt.Printf("%s  %-7s  process:%s\n", s.ID[:8], s.Status, s.ProcessID[:8])
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	return cmd
}

func stepShowCmd() *cobra.Command {
	var stepID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show step details",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				step, err := k.ReadStep(context.Background(), callerID, stepID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(step)
				}
				fmt.Printf("Step: %s\n  status:          %s\n  process:         %s\n  action_id:       %s\n  required_caller: %s\n",
					step.ID, step.Status, step.ProcessID, step.NextActionID, step.RequiredCallerUserID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "id", "", "Step ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func stepCompleteCmd() *cobra.Command {
	var stepID, args string
	cmd := &cobra.Command{
		Use:   "complete",
		Short: "Complete a waiting step",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				var input json.RawMessage
				if args != "" {
					input = json.RawMessage(args)
				} else {
					input = json.RawMessage("{}")
				}
				reply, err := k.CompleteStep(context.Background(), callerID, stepID, input)
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(reply.TxID)
					return nil
				}
				if flagOutput == "json" {
					return printJSON(reply)
				}
				fmt.Printf("Step completed.\n  step_id: %s\n  tx_id:   %s\n", reply.StepID, reply.TxID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&stepID, "id", "", "Step ID (required)")
	cmd.Flags().StringVar(&args, "args", "", "Input args as JSON object")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
