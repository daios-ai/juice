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
	eventsCmd.AddCommand(eventsListenCmd(), eventsListCmd(), eventsUnlistenCmd(), eventsEmitCmd(), eventsPollCmd(), eventsConsumeCmd())
	rootCmd.AddCommand(eventsCmd)
}

func eventsListenCmd() *cobra.Command {
	var sourceHandle, eventName, actionID string
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

			sourceUser, err := k.ReadUserByHandle(context.Background(), sourceHandle)
			if err != nil {
				return fmt.Errorf("source user not found: %w", err)
			}

			l, err := k.CreateListener(context.Background(), kernel.CreateListenerRequest{
				OwnerUserID:    subjectID,
				SourceUserID:   sourceUser.ID,
				EventName:      eventName,
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
	cmd.Flags().StringVar(&actionID, "action", "", "Target action ID to call on event (required)")
	_ = cmd.MarkFlagRequired("source")
	_ = cmd.MarkFlagRequired("event")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func eventsListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List listeners owned by the current user",
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

			listeners, err := k.ListListeners(context.Background(), subjectID, 100, 0)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(listeners)
			}
			for _, l := range listeners {
				active := "active"
				if !l.Active {
					active = "inactive"
				}
				fmt.Printf("%s  %-8s  event:%-20s  action:%s\n",
					l.ID[:8], active, l.EventName, l.TargetActionID[:8])
			}
			return nil
		},
	}
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

			txIDs, err := k.EmitEvent(context.Background(), subjectID, eventName, args, "")
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{"event_ids": txIDs})
			}
			fmt.Printf("Event queued for %d listener(s)\n", len(txIDs))
			for _, id := range txIDs {
				fmt.Printf("  event: %s\n", id)
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

			events, err := k.PollListener(context.Background(), subjectID, listenerID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(map[string]any{"events": events})
			}
			fmt.Printf("Pending events: %d\n", len(events))
			for _, e := range events {
				fmt.Printf("  %s  args: %s\n", e.ID, e.ArgsJSON)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&listenerID, "id", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func eventsConsumeCmd() *cobra.Command {
	var eventID, processID, parentTraceID string
	cmd := &cobra.Command{
		Use:   "consume",
		Short: "Consume a pending event, calling its listener's target action",
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

			reply, err := k.ConsumeEvent(context.Background(), subjectID, eventID, processID, parentTraceID)
			if err != nil {
				return err
			}

			if flagOutput == "json" {
				return printJSON(reply)
			}
			fmt.Printf("Event consumed: tx=%s\n", reply.TxID)
			return nil
		},
	}
	cmd.Flags().StringVar(&eventID, "id", "", "Event ID (required)")
	cmd.Flags().StringVar(&processID, "process", "", "Process ID to fund the action call (required)")
	cmd.Flags().StringVar(&parentTraceID, "trace", "", "Parent trace ID (defaults to process root)")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("process")
	return cmd
}
