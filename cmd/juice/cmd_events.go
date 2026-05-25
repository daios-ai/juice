package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	eventsCmd := &cobra.Command{Use: "events", Short: "Event listener commands"}
	eventsCmd.AddCommand(eventsListenCmd(), eventsUnlistenCmd(), eventsEmitCmd(), eventsPollCmd())
	rootCmd.AddCommand(eventsCmd)
}

func eventsListenCmd() *cobra.Command {
	var sourceHandle, eventName, processID, traceID, actionID string
	cmd := &cobra.Command{
		Use:   "listen",
		Short: "Register a listener that calls an action when an event fires",
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

			// Resolve source user.
			sourceUser, err := k.ReadUserByHandle(context.Background(), sourceHandle)
			if err != nil {
				return fmt.Errorf("source user not found: %w", err)
			}

			l, err := k.CreateListener(context.Background(), kernel.CreateListenerRequest{
				OwnerUserID:    subjectID,
				SourceUserID:   sourceUser.ID,
				EventName:      eventName,
				ProcessID:      processID,
				TraceID:        traceID,
				TargetActionID: actionID,
			})
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(l)
			}
			fmt.Printf("Listener created: %s\n", l.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&sourceHandle, "source", "", "Source user handle to listen for (required)")
	cmd.Flags().StringVar(&eventName, "event", "", "Event name to listen for (required)")
	cmd.Flags().StringVar(&processID, "process", "", "Process ID to run under (required)")
	cmd.Flags().StringVar(&traceID, "trace", "", "Parent trace ID (defaults to process root)")
	cmd.Flags().StringVar(&actionID, "action", "", "Target action ID to call on event (required)")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("event")
	_ = cmd.MarkFlagRequired("process")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func eventsUnlistenCmd() *cobra.Command {
	var listenerID string
	cmd := &cobra.Command{
		Use:   "unlisten",
		Short: "Deactivate a listener",
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

			if err := k.DeleteListener(context.Background(), subjectID, listenerID); err != nil {
				return err
			}
			fmt.Println("Listener deactivated.")
			return nil
		},
	}
	cmd.Flags().StringVar(&listenerID, "id", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func eventsEmitCmd() *cobra.Command {
	var eventName, argsStr string
	cmd := &cobra.Command{
		Use:   "emit",
		Short: "Emit a named event, firing all matching listeners",
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

			txIDs, err := k.EmitEvent(context.Background(), subjectID, eventName, args)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{"tx_ids": txIDs})
			}
			fmt.Printf("Event emitted: %d listener(s) fired\n", len(txIDs))
			for _, id := range txIDs {
				fmt.Printf("  tx: %s\n", id)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&eventName, "event", "", "Event name (required)")
	cmd.Flags().StringVar(&argsStr, "args", "{}", "JSON-encoded arguments")
	_ = cmd.MarkFlagRequired("event")
	return cmd
}

func eventsPollCmd() *cobra.Command {
	var listenerID string
	cmd := &cobra.Command{
		Use:   "poll",
		Short: "Poll a listener's event queue",
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

			txIDs, err := k.PollListener(context.Background(), subjectID, listenerID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{"tx_ids": txIDs})
			}
			fmt.Printf("Queued transactions: %d\n", len(txIDs))
			for _, id := range txIDs {
				fmt.Println(" ", id)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listenerID, "id", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
