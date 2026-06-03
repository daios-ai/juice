package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(lookupCmd())
}

func lookupCmd() *cobra.Command {
	var query string
	var limit int
	cmd := &cobra.Command{
		Use:   "lookup",
		Short: "Search for actions using a natural-language query",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				// Create an ephemeral free process for the lookup call.
				proc, _, err := k.StartProcess(context.Background(), subjectID, subjectID, 0)
				if err != nil {
					return fmt.Errorf("start process: %w", err)
				}
				defer k.EndProcess(context.Background(), subjectID, proc.ID)

				sys, err := k.ReadUserByHandle(context.Background(), "@sys")
				if err != nil {
					return fmt.Errorf("read @sys: %w", err)
				}

				reply, err := k.Call(context.Background(), kernel.CallRequest{
					SubjectID:    subjectID,
					ProcessID:    proc.ID,
					TargetUserID: sys.ID,
					ActionName:   "lookup",
					Args: map[string]any{
						"query": query,
						"limit": float64(limit),
					},
				})
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(reply.Result)
				}
				results, _ := reply.Result["results"].([]any)
				for _, item := range results {
					r, _ := item.(map[string]any)
					score, _ := r["score"].(float64)
					actionID, _ := r["action_id"].(string)
					name, _ := r["name"].(string)
					owner, _ := r["owner_handle"].(string)
					shortID := actionID
					if len(shortID) > 8 {
						shortID = shortID[:8]
					}
					fmt.Printf("%.4f  %s  %s/%s\n", score, shortID, owner, name)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "Natural-language query (required)")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	_ = cmd.MarkFlagRequired("query")
	return cmd
}
