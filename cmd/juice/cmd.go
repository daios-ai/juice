package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// ---- user ----

func init() {
	userCmd := &cobra.Command{Use: "user", Short: "User account commands"}
	userCmd.AddCommand(userCreateCmd(), userMeCmd(), userUpdateCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var handle, email, password string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new user account",
		RunE: func(_ *cobra.Command, _ []string) error {
			if password == "" {
				p, err := promptPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			return withKernel(func(k *kernel.Kernel) error {
				view, err := createUser(k, context.Background(), kernel.CreateUserRequest{
					Handle:   handle,
					Email:    email,
					Password: password,
				})
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(view)
				}
				fmt.Printf("User created: %s (id: %s)\n", view["handle"], view["id"])
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&handle, "handle", "", "Unique handle, e.g. @alice (required)")
	cmd.Flags().StringVar(&email, "email", "", "Email address (required)")
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	_ = cmd.MarkFlagRequired("handle")
	_ = cmd.MarkFlagRequired("email")
	return cmd
}

func userMeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Show the authenticated user's profile",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				view, err := getMe(k, context.Background(), callerID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(view)
				}
				fmt.Printf("id:        %s\nhandle:    %s\nemail:     %s\navailable: %d\nlocked:    %d\n",
					view["id"], view["handle"], view["email"], view["available"], view["locked"])
				return nil
			})
		},
	}
}

func userUpdateCmd() *cobra.Command {
	var email string
	var changePassword bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update email or password",
		RunE: func(_ *cobra.Command, _ []string) error {
			if email == "" && !changePassword {
				return fmt.Errorf("at least one of --email or --password must be specified")
			}
			var currentPassword, newPassword string
			if changePassword {
				var err error
				if currentPassword, err = promptPassword("Current password: "); err != nil {
					return err
				}
				if newPassword, err = promptPassword("New password: "); err != nil {
					return err
				}
			}
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				view, err := updateMe(k, context.Background(), callerID, email, currentPassword, newPassword)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(view)
				}
				fmt.Printf("id:        %s\nhandle:    %s\nemail:     %s\navailable: %d\nlocked:    %d\n",
					view["id"], view["handle"], view["email"], view["available"], view["locked"])
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "New email address")
	cmd.Flags().BoolVar(&changePassword, "password", false, "Change password (prompts for current and new)")
	return cmd
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// ---- action ----

func init() {
	actionCmd := &cobra.Command{Use: "action", Short: "Action management commands"}
	actionCmd.AddCommand(
		actionCreateCmd(),
		actionUpdateCmd(),
		actionEnableCmd(),
		actionDisableCmd(),
		actionListCmd(),
		actionShowCmd(),
		actionDeleteCmd(),
		actionImportCmd(),
		actionUnimportCmd(),
		actionStatsCmd(),
	)
	rootCmd.AddCommand(actionCmd)
}

func actionCreateCmd() *cobra.Command {
	var name, kind, source, description string
	var price int64
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				inputSchema := map[string]any{}
				if inputSchemaStr != "" {
					if err := json.Unmarshal([]byte(inputSchemaStr), &inputSchema); err != nil {
						return fmt.Errorf("invalid --input-schema: %w", err)
					}
				}
				outputSchema := map[string]any{}
				if outputSchemaStr != "" {
					if err := json.Unmarshal([]byte(outputSchemaStr), &outputSchema); err != nil {
						return fmt.Errorf("invalid --output-schema: %w", err)
					}
				}
				srcData := source
				if source != "" {
					if _, err := os.Stat(source); err == nil {
						data, err := os.ReadFile(source)
						if err != nil {
							return fmt.Errorf("reading source file: %w", err)
						}
						srcData = string(data)
					}
				}
				a, err := createAction(k, context.Background(), callerID, kernel.CreateActionRequest{
					OwnerUserID:  callerID,
					Name:         name,
					Kind:         kernel.ActionKind(kind),
					Price:        price,
					Description:  description,
					InputSchema:  inputSchema,
					OutputSchema: outputSchema,
					Source:       srcData,
				})
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(a)
				}
				fmt.Printf("Action created: %s (id: %s)\n", a.Name, a.ID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Action name, e.g. /hello (required)")
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http, wasm, native")
	cmd.Flags().StringVar(&source, "source", "", "URL (http) or file path (wasm)")
	cmd.Flags().StringVar(&description, "description", "", "Human-readable description")
	cmd.Flags().Int64Var(&price, "price", 0, "Price in credits")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "JSON Schema for outputs")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

func actionUpdateCmd() *cobra.Command {
	var actionID, actionRef, description, source string
	var price int64
	var public bool
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update an action's metadata",
		RunE: func(c *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				req := kernel.UpdateActionRequest{ID: id}
				if c.Flags().Changed("description") {
					req.Description = &description
				}
				if c.Flags().Changed("source") {
					req.Source = &source
				}
				if c.Flags().Changed("price") {
					req.Price = &price
				}
				if c.Flags().Changed("public") {
					req.Public = &public
				}
				if inputSchemaStr != "" {
					m := map[string]any{}
					if err := json.Unmarshal([]byte(inputSchemaStr), &m); err != nil {
						return fmt.Errorf("invalid --input-schema: %w", err)
					}
					req.InputSchema = m
				}
				if outputSchemaStr != "" {
					m := map[string]any{}
					if err := json.Unmarshal([]byte(outputSchemaStr), &m); err != nil {
						return fmt.Errorf("invalid --output-schema: %w", err)
					}
					req.OutputSchema = m
				}
				a, err := updateAction(k, context.Background(), callerID, req)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(a)
				}
				fmt.Printf("Action %s updated (active=%v, public=%v).\n", a.Name, a.Active, a.Public)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().Int64Var(&price, "price", 0, "New price in credits")
	cmd.Flags().BoolVar(&public, "public", false, "Make action public (true) or private (false)")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "New JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "New JSON Schema for outputs")
	return cmd
}

func actionEnableCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "enable",
		Short: "Activate an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				if err := enableAction(k, context.Background(), callerID, id); err != nil {
					return err
				}
				fmt.Printf("Action %s enabled.\n", id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

func actionDisableCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "disable",
		Short: "Deactivate an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				if err := disableAction(k, context.Background(), callerID, id); err != nil {
					return err
				}
				fmt.Printf("Action %s disabled.\n", id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

func actionListCmd() *cobra.Command {
	var all bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List actions",
		RunE: func(_ *cobra.Command, _ []string) error {
			if all {
				return withCaller(func(k *kernel.Kernel, callerID string) error {
					actions, err := listOwnedActions(k, context.Background(), callerID, limit, offset)
					if err != nil {
						return err
					}
					if flagOutput == "json" {
						return printJSON(actions)
					}
					for _, a := range actions {
						active := " "
						if a.Active {
							active = "*"
						}
						fmt.Printf("[%s] %s  %-30s  %d credits\n", active, a.ID[:8], a.Name, a.Price)
					}
					return nil
				})
			}
			return withKernel(func(k *kernel.Kernel) error {
				actions, err := listPublicActions(k, context.Background(), "", "", "", limit, offset)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(actions)
				}
				for _, a := range actions {
					fmt.Printf("  %s  %-30s  %d credits\n", a.ID[:8], a.Name, a.Price)
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include own inactive/private actions (requires auth)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func actionShowCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show action details",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				a, err := getAction(k, context.Background(), callerID, id)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(a)
				}
				active := "inactive"
				if a.Active {
					active = "active"
				}
				public := "private"
				if a.Public {
					public = "public"
				}
				fmt.Printf("Action: %s\n  name:        %s\n  kind:        %s\n  status:      %s  (%s)\n  price:       %d credits\n  owner:       %s\n  description: %s\n",
					a.ID, a.Name, a.Kind, active, public, a.Price, a.OwnerUserID, a.Description)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

func actionDeleteCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				if err := deleteAction(k, context.Background(), callerID, id); err != nil {
					return err
				}
				fmt.Printf("Action %s deleted.\n", id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

func actionImportCmd() *cobra.Command {
	var specURL string
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import OpenAPI operations as inactive http actions (idempotent)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				allowLocal := os.Getenv("JUICE_ALLOW_LOCAL_SOURCES") == "true"
				specBytes, err := fetchOpenAPISpec(context.Background(), specURL, allowLocal)
				if err != nil {
					return err
				}
				result, err := k.ImportOpenAPI(context.Background(), callerID, callerID, specURL, specBytes)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(result)
				}
				fmt.Printf("created=%d unchanged=%d updated=%d deactivated=%d rejected=%d\n",
					len(result.Created), len(result.Unchanged), len(result.Updated),
					len(result.Deactivated), len(result.Rejected))
				for _, r := range result.Rejected {
					fmt.Printf("  rejected %s: %s\n", r.Key, r.Reason)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&specURL, "openapi", "", "OpenAPI spec URL (required)")
	_ = cmd.MarkFlagRequired("openapi")
	return cmd
}

func actionUnimportCmd() *cobra.Command {
	var specURL, name string
	cmd := &cobra.Command{
		Use:   "unimport",
		Short: "Deactivate OpenAPI-imported actions without deleting history",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				actions, err := k.UnimportOpenAPI(context.Background(), callerID, callerID, specURL, name)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(actions)
				}
				fmt.Printf("deactivated %d action(s)\n", len(actions))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&specURL, "openapi", "", "OpenAPI spec URL (required)")
	cmd.Flags().StringVar(&name, "name", "", "Deactivate only the action with this name or operation_key")
	_ = cmd.MarkFlagRequired("openapi")
	return cmd
}

func actionStatsCmd() *cobra.Command {
	var actionID, actionRef string
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show statistics for an action",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withKernel(func(k *kernel.Kernel) error {
				id, err := resolveActionID(k, context.Background(), actionID, actionRef)
				if err != nil {
					return err
				}
				stats, err := actionStats(k, context.Background(), id)
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
				fmt.Printf("Stats for %s:\n  uses:             %d\n  successes:        %d\n  failures:         %d\n  latency_estimate: %.3fs\n  rating_estimate:  %.3f\n  last_used:        %s\n",
					stats.ActionID, stats.Uses, stats.Successes, stats.Failures,
					stats.LatencyEstimate, stats.RatingEstimate,
					stats.LastUsedAt.Format("2006-01-02T15:04:05"))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&actionID, "id", "", "Action ID")
	cmd.Flags().StringVar(&actionRef, "action", "", "Action reference (@owner/name)")
	return cmd
}

// ---- process ----

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Process lifecycle commands"}
	processCmd.AddCommand(processListCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List processes owned by the current user",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				processes, err := listProcesses(k, context.Background(), callerID, 100, 0)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(processes)
				}
				for _, p := range processes {
					fmt.Printf("%s  %-6s  available:%-6d  locked:%-6d\n",
						p.ID[:8], p.Status, p.Available, p.Locked)
				}
				return nil
			})
		},
	}
	return cmd
}

func processEndCmd() *cobra.Command {
	var processID string
	cmd := &cobra.Command{
		Use:   "end",
		Short: "End a process and return remaining funds",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				if err := endProcess(k, context.Background(), callerID, processID); err != nil {
					return err
				}
				fmt.Printf("Process %s ended.\n", processID)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func processShowCmd() *cobra.Command {
	var processID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show process details",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				p, err := getProcess(k, context.Background(), callerID, processID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(p)
				}
				fmt.Printf("Process: %s\n  owner:     %s\n  status:    %s\n  available: %d\n  locked:    %d\n",
					p.ID, p.OwnerUserID, p.Status, p.Available, p.Locked)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "id", "", "Process ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// ---- step ----

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

				var pa json.RawMessage
				if partialArgs != "" {
					pa = json.RawMessage(partialArgs)
				}
				var is json.RawMessage
				if inputSchema != "" {
					is = json.RawMessage(inputSchema)
				}

				view, err := createStep(k, ctx, callerID, createStepParams{
					ProcessID:      processID,
					ParentTraceID:  parentTrace,
					ActionRef:      action,
					RequiredCaller: requiredCaller,
					PartialArgs:    pa,
					InputSchema:    is,
				})
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(view.ID)
					return nil
				}
				if flagOutput == "json" {
					return printJSON(view)
				}
				fmt.Printf("Step created.\n  step_id:  %s\n  status:   %s\n  process:  %s\n  action:   %s\n",
					view.ID, view.Status, view.ProcessID, view.Action)
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
				steps, err := listSteps(k, context.Background(), callerID, processID, status)
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
				step, err := getStep(k, context.Background(), callerID, stepID)
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
				reply, err := completeStep(k, context.Background(), callerID, stepID, input)
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

// ---- tx ----

func init() {
	txCmd := &cobra.Command{Use: "tx", Short: "Transaction commands"}
	txCmd.AddCommand(txListCmd(), txShowCmd(), txRateCmd(), txVerifyReceiptCmd())
	rootCmd.AddCommand(txCmd)
}

func txListCmd() *cobra.Command {
	var processID string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List transactions",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				txs, err := listTransactions(k, context.Background(), callerID, kernel.TxFilter{
					ProcessID: processID,
					Limit:     limit,
					Offset:    offset,
				})
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(txs)
				}
				for _, tx := range txs {
					fmt.Printf("[%s] %s  action:%s  status:%s  gross:%d\n",
						tx.StartedAt.Format("2006-01-02T15:04:05"),
						tx.ID[:8], tx.ActionID[:8], tx.Status, tx.Gross)
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func txShowCmd() *cobra.Command {
	var txID string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show a transaction",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				tv, err := getTransaction(k, context.Background(), callerID, txID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(tv)
				}
				fmt.Printf("Transaction: %s\n  status:  %s\n  action:  %s\n  gross:   %d\n  net:     %d\n  fee:     %d\n  reason:  %s\n",
					tv.ID, tv.Status, tv.ActionID, tv.Gross, tv.Net, tv.Fee, tv.Reason)
				if tv.Rating != nil {
					if tv.Rating.Note != nil {
						fmt.Printf("  rating:  %.0f (%s)\n", tv.Rating.Value, *tv.Rating.Note)
					} else {
						fmt.Printf("  rating:  %.0f\n", tv.Rating.Value)
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func txVerifyReceiptCmd() *cobra.Command {
	var txID string
	cmd := &cobra.Command{
		Use:   "verify-receipt",
		Short: "Verify the remote receipt for a transaction",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				v, err := verifyReceipt(k, context.Background(), callerID, txID)
				if err != nil {
					return err
				}
				if flagOutput == "json" {
					return printJSON(v)
				}
				status := "PASS"
				if !v.Valid {
					status = "FAIL"
				}
				shortID := txID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}
				fmt.Printf("Receipt verification: %s  [%s]\n  remote: %s\n", shortID, status, v.RemoteKernelHandle)
				fmt.Printf("  receipt_hash:  %v\n  signature:     %v\n  action_id:     %v\n",
					v.Checks.ReceiptHash, v.Checks.Signature, v.Checks.ActionID)
				fmt.Printf("  status:        %v\n  charge:        %v\n  settlement_arith: %v\n",
					v.Checks.Status, v.Checks.Charge, v.Checks.SettlementArith)
				fmt.Printf("  args_hash:     %v\n  reply_hash:    %v\n",
					v.Checks.ArgsHash, v.Checks.ReplyHash)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

func txRateCmd() *cobra.Command {
	var txID string
	var rating float64
	var note string
	cmd := &cobra.Command{
		Use:   "rate",
		Short: "Rate a transaction (0 or 1)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				var notePtr *string
				if note != "" {
					notePtr = &note
				}
				if _, err := rateTransaction(k, context.Background(), callerID, txID, rating, notePtr); err != nil {
					return err
				}
				fmt.Printf("Transaction %s rated %.0f.\n", txID, rating)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&txID, "id", "", "Transaction ID (required)")
	cmd.Flags().Float64Var(&rating, "rating", -1, "Rating: 0 (bad) or 1 (good) (required)")
	cmd.Flags().StringVar(&note, "note", "", "Optional justification note")
	_ = cmd.MarkFlagRequired("id")
	_ = cmd.MarkFlagRequired("rating")
	return cmd
}

// ---- run ----

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
				reply, err := run(k, context.Background(), callerID, actionRef, args)
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

// ---- action ID resolution ----

// resolveActionID returns the action UUID for either a direct --id or a --action @owner/name reference.
// Exactly one of id or ref must be non-empty.
func resolveActionID(k *kernel.Kernel, ctx context.Context, id, ref string) (string, error) {
	if id != "" && ref != "" {
		return "", fmt.Errorf("specify --id or --action, not both")
	}
	if id != "" {
		return id, nil
	}
	if ref != "" {
		ownerHandle, name, err := kernel.ParseActionRef(ref)
		if err != nil {
			return "", err
		}
		owner, err := k.ReadUserByHandle(ctx, ownerHandle)
		if err != nil || owner == nil {
			return "", fmt.Errorf("action owner %q not found", ownerHandle)
		}
		action, err := k.ReadActionByOwnerName(ctx, owner.ID, name)
		if err != nil || action == nil {
			return "", fmt.Errorf("action %q not found", ref)
		}
		return action.ID, nil
	}
	return "", fmt.Errorf("--id or --action is required")
}

