package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(runCmd())
}

func runCmd() *cobra.Command {
	var actionRef, argsStr string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run an action (creates a process, calls the action, closes the process)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				args, err := readJSONArg(argsStr)
				if err != nil {
					return fmt.Errorf("invalid --args: %w", err)
				}
				reply, err := k.Run(context.Background(), callerID, actionRef, args)
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
				resultJSON, _ := json.MarshalIndent(reply.Result, "", "  ")
				fmt.Printf("tx_id:    %s\ntrace_id: %s\nresult:\n%s\n",
					reply.TxID, reply.TraceID, string(resultJSON))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference as @owner/name (required)")
	cmd.Flags().StringVar(&argsStr, "args", "{}", "JSON-encoded arguments or @file.json")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}
