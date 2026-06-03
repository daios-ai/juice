package main

import (
	"context"
	"fmt"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	listenerCmd := &cobra.Command{Use: "listener", Short: "Manage event listeners"}
	listenerCmd.AddCommand(
		listenerCreateCmd(),
		listenerListCmd(),
		listenerShowCmd(),
		listenerDeleteCmd(),
	)
	rootCmd.AddCommand(listenerCmd)

	eventCmd := &cobra.Command{Use: "event", Short: "Emit and consume events"}
	eventCmd.AddCommand(
		eventEmitCmd(),
		eventListCmd(),
		eventConsumeCmd(),
	)
	rootCmd.AddCommand(eventCmd)
}

func listenerCreateCmd() *cobra.Command {
	var sourceHandle, eventName, actionRef string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Register a listener that calls an action when an event fires",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				ctx := context.Background()
				sourceUser, err := k.ReadUserByHandle(ctx, sourceHandle)
				if err != nil {
					return fmt.Errorf("source user not found: %w", err)
				}
				ownerHandle, actionName, err := parseActionRefCLI(actionRef)
				if err != nil {
					return err
				}
				owner, err := k.ReadUserByHandle(ctx, ownerHandle)
				if err != nil {
					return fmt.Errorf("action owner %s not found: %w", ownerHandle, err)
				}
				action, err := k.ReadActionByOwnerName(ctx, owner.ID, actionName)
				if err != nil {
					return fmt.Errorf("action %s not found: %w", actionRef, err)
				}
				l, err := k.CreateListener(ctx, subjectID, kernel.CreateListenerRequest{
					SourceUserID:   sourceUser.ID,
					EventName:      eventName,
					TargetActionID: action.ID,
				})
				if err != nil {
					return err
				}
				if flagQuiet {
					printQuiet(l.ID)
					return nil
				}
				if flagOutput == "json" {
					return printJSON(l)
				}
				fmt.Printf("listener: %s\n", l.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&sourceHandle, "source-user", "", "Source user handle to listen for (required)")
	cmd.Flags().StringVar(&eventName, "event", "", "Event name to listen for (required)")
	cmd.Flags().StringVar(&actionRef, "action", "", "Target action as @owner/name (required)")
	_ = cmd.MarkFlagRequired("source-user")
	_ = cmd.MarkFlagRequired("event")
	_ = cmd.MarkFlagRequired("action")
	return cmd
}

func listenerListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List listeners owned by the current user",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
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
			})
		},
	}
	return cmd
}

func listenerShowCmd() *cobra.Command {
	var listenerID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show a listener",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				l, err := k.GetListener(context.Background(), subjectID, listenerID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(l)
				}
				active := "active"
				if !l.Active {
					active = "inactive"
				}
				fmt.Printf("id:     %s\nstatus: %s\nevent:  %s\naction: %s\n",
					l.ID, active, l.EventName, l.TargetActionID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&listenerID, "id", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func listenerDeleteCmd() *cobra.Command {
	var listenerID string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete a listener and purge its pending events",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				if err := k.DeleteListener(context.Background(), subjectID, listenerID); err != nil {
					return err
				}
				if !flagQuiet {
					fmt.Println("deleted")
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&listenerID, "id", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func eventEmitCmd() *cobra.Command {
	var eventName, argsStr string
	cmd := &cobra.Command{
		Use:   "emit",
		Short: "Emit a named event, firing all matching listeners",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				args, err := readJSONArg(argsStr)
				if err != nil {
					return fmt.Errorf("invalid --args: %w", err)
				}
				eventIDs, err := k.EmitEvent(context.Background(), subjectID, subjectID, eventName, args, "")
				if err != nil {
					return err
				}
				if flagQuiet {
					for _, id := range eventIDs {
						printQuiet(id)
					}
					return nil
				}
				if flagOutput == "json" {
					return printJSON(map[string]any{"event_ids": eventIDs})
				}
				fmt.Printf("queued for %d listener(s)\n", len(eventIDs))
				for _, id := range eventIDs {
					fmt.Printf("  event: %s\n", id)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&eventName, "event", "", "Event name (required)")
	cmd.Flags().StringVar(&argsStr, "args", "{}", "JSON-encoded arguments or @file.json")
	_ = cmd.MarkFlagRequired("event")
	return cmd
}

func eventListCmd() *cobra.Command {
	var listenerID string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pending events for a listener",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				events, err := k.PollListener(context.Background(), subjectID, listenerID)
				if err != nil {
					return err
				}
				if events == nil {
					events = []*kernel.Event{}
				}
				if flagOutput == "json" {
					return printJSON(events)
				}
				for _, e := range events {
					fmt.Printf("  %s  args: %s\n", e.ID, e.ArgsJSON)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&listenerID, "listener", "", "Listener ID (required)")
	_ = cmd.MarkFlagRequired("listener")
	return cmd
}

func eventConsumeCmd() *cobra.Command {
	var eventID, processID, parentTraceID string
	cmd := &cobra.Command{
		Use:   "consume",
		Short: "Consume a pending event, calling its listener's target action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withSubject(func(k *kernel.Kernel, subjectID string) error {
				reply, err := k.ConsumeEvent(context.Background(), subjectID, eventID, processID, parentTraceID)
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
				fmt.Printf("consumed: tx=%s\n", reply.TxID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&eventID, "id", "", "Event ID (required)")
	cmd.Flags().StringVar(&processID, "process", "", "Process ID to fund the action call (required)")
	cmd.Flags().StringVar(&parentTraceID, "trace", "", "Parent trace ID (defaults to process root)")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("process")
	return cmd
}
