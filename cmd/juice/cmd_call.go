package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(callCmd())
}

func callCmd() *cobra.Command {
	var processID, parentTraceID, target, actionName, argsStr string
	cmd := &cobra.Command{
		Use:   "call",
		Short: "Call an action within a process",
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

			args := map[string]any{}
			if argsStr != "" {
				if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
					return fmt.Errorf("invalid --args JSON: %w", err)
				}
			}

			// Resolve parent trace ID if not supplied.
			if parentTraceID == "" {
				p, err := k.ReadProcess(context.Background(), processID)
				if err != nil {
					return err
				}
				_ = p // root trace resolves inside kernel
			}

			reply, err := k.Call(context.Background(), kernel.CallRequest{
				SubjectID:     subjectID,
				ProcessID:     processID,
				ParentTraceID: parentTraceID,
				TargetUserID:  target,
				ActionName:    actionName,
				Args:          args,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(reply)
			}
			resultJSON, _ := json.MarshalIndent(reply.Result, "", "  ")
			fmt.Printf("tx_id:    %s\ntrace_id: %s\nresult:\n%s\n",
				reply.TxID, reply.TraceID, string(resultJSON))
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Process ID (required)")
	cmd.Flags().StringVar(&parentTraceID, "trace", "", "Parent trace ID (defaults to process root)")
	cmd.Flags().StringVar(&target, "target", "", "Target user handle (required)")
	cmd.Flags().StringVar(&actionName, "action", "", "Action name, e.g. /hello (required)")
	cmd.Flags().StringVar(&argsStr, "args", "{}", "JSON-encoded arguments")
	_ = cmd.MarkFlagRequired("process")
	_ = cmd.MarkFlagRequired("target")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}
