package main

import (
	"context"
	"fmt"

	"github.com/daios/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	statsCmd := &cobra.Command{Use: "stats", Short: "Action statistics"}
	statsCmd.AddCommand(statsShowCmd())
	rootCmd.AddCommand(statsCmd)

	rootCmd.AddCommand(lookupCmd())
}

func statsShowCmd() *cobra.Command {
	var actionID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show statistics for an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			stats, err := k.ReadStats(context.Background(), actionID)
			if err != nil {
				return err
			}
			if stats == nil {
				fmt.Println("No statistics yet.")
				return nil
			}

			if flagOutput == "json" {
				return printJSON(stats)
			}
			fmt.Printf("Stats for %s:\n  uses:         %d\n  successes:    %d\n  failures:     %d\n  price_mean:   %.2f\n  latency_mean: %.3fs\n  rating_mean:  %.3f\n  last_used:    %s\n",
				stats.ActionID, stats.Uses, stats.Successes, stats.Failures,
				stats.PriceMean, stats.LatencyMean, stats.RatingMean,
				stats.LastUsedAt.Format("2006-01-02T15:04:05"))
			return nil
		},
	}
	cmd.Flags().StringVar(&actionID, "action", "", "Action ID (required)")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func lookupCmd() *cobra.Command {
	var query string
	var limit int
	cmd := &cobra.Command{
		Use:   "lookup",
		Short: "Search for actions using a natural-language query",
		RunE: func(_ *cobra.Command, _ []string) error {
			k, db, err := openKernel()
			if err != nil {
				return err
			}
			defer db.Close()

			results, err := k.Lookup(context.Background(), kernel.LookupRequest{
				Query: query,
				Limit: limit,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(results)
			}
			for _, r := range results {
				fmt.Printf("%.4f  %s  %s\n", r.Score, r.Action.ID[:8], r.Action.Name)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&query, "query", "", "Natural-language query (required)")
	cmd.Flags().IntVar(&limit, "limit", 10, "Maximum results")
	_ = cmd.MarkFlagRequired("query")
	return cmd
}
