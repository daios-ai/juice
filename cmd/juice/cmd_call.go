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
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				args, err := readJSONArg(argsStr)
				if err != nil {
					return fmt.Errorf("invalid --args: %w", err)
				}
				reply, err := k.Call(context.Background(), kernel.CallRequest{
					CallerID:      callerID,
					ProcessID:     processID,
					ParentTraceID: parentTraceID,
					ActionRef:     actionRef,
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
