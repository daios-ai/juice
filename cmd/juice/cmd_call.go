package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(callCmd())
}

func callCmd() *cobra.Command {
	var processID, parentTraceID, actionRef, argsStr string
	cmd := &cobra.Command{
		Use:   "call",
		Short: "Call an action within a process",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				ownerHandle, actionName, err := parseActionRefCLI(actionRef)
				if err != nil {
					return err
				}
				owner, err := k.ReadUserByHandle(context.Background(), ownerHandle)
				if err != nil {
					return fmt.Errorf("owner %s not found: %w", ownerHandle, err)
				}
				args, err := readJSONArg(argsStr)
				if err != nil {
					return fmt.Errorf("invalid --args: %w", err)
				}
				reply, err := k.Call(context.Background(), kernel.CallRequest{
					SubjectID:     subjectID,
					ProcessID:     processID,
					ParentTraceID: parentTraceID,
					TargetUserID:  owner.ID,
					ActionName:    actionName,
					Args:          args,
				})
				if err != nil {
					return err
				}
				if flagQuiet {
					printQuiet(reply.TxID)
					return nil
				}
				if flagOutput == "json" {
					return printJSON(reply)
				}
				resultJSON, _ := jsonMarshalIndent(reply.Result)
				fmt.Printf("tx_id:    %s\ntrace_id: %s\nresult:\n%s\n",
					reply.TxID, reply.TraceID, string(resultJSON))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Process ID (required)")
	cmd.Flags().StringVar(&parentTraceID, "trace", "", "Parent trace ID (defaults to process root)")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference as @owner/name (required)")
	cmd.Flags().StringVar(&argsStr, "args", "{}", "JSON-encoded arguments or @file.json")
	_ = cmd.MarkFlagRequired("process")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

// parseActionRefCLI parses "@owner/name" for CLI usage.
func parseActionRefCLI(ref string) (string, string, error) {
	if ref == "" {
		return "", "", fmt.Errorf("action must be @owner/name")
	}
	if ref[0] != '@' {
		return "", "", fmt.Errorf("action must be @owner/name")
	}
	idx := -1
	for i := 1; i < len(ref); i++ {
		if ref[i] == '/' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", "", fmt.Errorf("action must be @owner/name")
	}
	ownerHandle := ref[:idx]
	actionName := ref[idx+1:]
	if ownerHandle == "" || actionName == "" {
		return "", "", fmt.Errorf("action must be @owner/name")
	}
	return ownerHandle, actionName, nil
}
