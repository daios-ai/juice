package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// ---- output helpers ----

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// printText renders v as a complete, human-readable view of the SAME object the HTTP
// API serializes. It marshals v to JSON, then prints one "key: value" line per
// top-level field in declaration order; scalar values are printed plainly and
// object/array values as compact inline JSON. Because the field set is derived from
// the marshaled object, the text view can never silently drop a field the HTTP
// response carries (CLI/HTTP parity, §14).
func printText(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	// Non-object top levels (arrays, scalars) have no labeled fields; print as JSON.
	trimmed := b
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return printJSON(v)
	}
	// Re-decode preserving field order via the JSON object's marshaled byte order.
	dec := json.NewDecoder(bytes.NewReader(b))
	// consume opening '{'
	if _, err := dec.Token(); err != nil {
		return err
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := keyTok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		fmt.Printf("  %s: %s\n", key, renderValue(raw))
	}
	return nil
}

// renderValue formats one JSON value for text output: strings unquoted, objects and
// arrays as compact JSON, everything else as-is.
func renderValue(raw json.RawMessage) string {
	r := []byte(raw)
	for len(r) > 0 && (r[0] == ' ' || r[0] == '\n' || r[0] == '\t') {
		r = r[1:]
	}
	if len(r) == 0 {
		return ""
	}
	if r[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	if r[0] == '{' || r[0] == '[' {
		var buf interface{}
		if err := json.Unmarshal(raw, &buf); err == nil {
			if compact, err := json.Marshal(buf); err == nil {
				return string(compact)
			}
		}
	}
	return string(r)
}

// emit prints a single resource object: canonical JSON with --json, else the complete
// text view. Both render the same object, guaranteeing CLI/HTTP parity.
func emit(v any) error {
	if flagJSON {
		return printJSON(v)
	}
	return printText(v)
}

// ---- user ----

func init() {
	userCmd := &cobra.Command{Use: "user", Short: "User account commands"}
	userCmd.AddCommand(userCreateCmd(), userMeCmd(), userUpdateCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "create <user> <email>",
		Short: "Create a new user account",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			user, email := args[0], args[1]
			if password == "" {
				p, err := promptPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			return withKernel(func(k *kernel.Kernel) error {
				view, err := createUser(k, context.Background(), kernel.CreateUserRequest{
					Handle:   user,
					Email:    email,
					Password: password,
				})
				if err != nil {
					return err
				}
				return emit(view)
			})
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	return cmd
}

func userMeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Show the authenticated user's profile",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				view, err := getMe(k, context.Background(), callerID)
				if err != nil {
					return err
				}
				return emit(view)
			})
		},
	}
}

func userUpdateCmd() *cobra.Command {
	var email string
	var changePassword bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update your email or password",
		Args:  cobra.NoArgs,
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
				return emit(view)
			})
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "New email address")
	cmd.Flags().BoolVar(&changePassword, "password", false, "Change password (prompts for current and new)")
	return cmd
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
	var kind, source, description string
	var price int64
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new action owned by you (name e.g. /hello)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
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
				return emit(a)
			})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http, wasm, native")
	cmd.Flags().StringVar(&source, "source", "", "URL (http) or file path (wasm)")
	cmd.Flags().StringVar(&description, "description", "", "Human-readable description")
	cmd.Flags().Int64Var(&price, "price", 0, "Price in credits")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "JSON Schema for outputs")
	return cmd
}

func actionUpdateCmd() *cobra.Command {
	var description, source string
	var price int64
	var public bool
	var inputSchemaStr, outputSchemaStr string
	cmd := &cobra.Command{
		Use:   "update <action>",
		Short: "Update an action's metadata (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				a0, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				req := kernel.UpdateActionRequest{ID: a0.ID}
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
				return emit(a)
			})
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().Int64Var(&price, "price", 0, "New price in credits")
	cmd.Flags().BoolVar(&public, "public", false, "Make action public (true) or private (false)")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "New JSON Schema for inputs")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "New JSON Schema for outputs")
	return cmd
}

func actionEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <action>",
		Short: "Activate an action (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				a, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := enableAction(k, context.Background(), callerID, a.ID); err != nil {
					return err
				}
				if flagJSON {
					return printJSON(map[string]bool{"active": true})
				}
				fmt.Printf("Action %s enabled.\n", args[0])
				return nil
			})
		},
	}
}

func actionDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <action>",
		Short: "Deactivate an action (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				a, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := disableAction(k, context.Background(), callerID, a.ID); err != nil {
					return err
				}
				if flagJSON {
					return printJSON(map[string]bool{"active": false})
				}
				fmt.Printf("Action %s disabled.\n", args[0])
				return nil
			})
		},
	}
}

func actionListCmd() *cobra.Command {
	var all bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List actions",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if all {
				return withCaller(func(k *kernel.Kernel, callerID string) error {
					actions, err := listOwnedActions(k, context.Background(), callerID, limit, offset)
					if err != nil {
						return err
					}
					if flagJSON {
						return printJSON(actions)
					}
					for _, a := range actions {
						active := " "
						if a.Active {
							active = "*"
						}
						fmt.Printf("[%s] %s  %-30s  %d credits\n", active, a.ActionRef, a.Name, a.Price)
					}
					return nil
				})
			}
			return withKernel(func(k *kernel.Kernel) error {
				actions, err := listPublicActions(k, context.Background(), "", "", "", limit, offset)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(actions)
				}
				for _, a := range actions {
					fmt.Printf("  %-24s  %d credits\n", a.ActionRef, a.Price)
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
	return &cobra.Command{
		Use:   "show <action>",
		Short: "Show action details, including input/output schemas (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				a0, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				a, err := getAction(k, context.Background(), callerID, a0.ID)
				if err != nil {
					return err
				}
				return emit(a)
			})
		},
	}
}

func actionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <action>",
		Short: "Delete an action, preserving history (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				a, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				if err := deleteAction(k, context.Background(), callerID, a.ID); err != nil {
					return err
				}
				fmt.Printf("Action %s deleted.\n", args[0])
				return nil
			})
		},
	}
}

func actionImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <spec-url>",
		Short: "Import OpenAPI operations as inactive http actions (idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			specURL := args[0]
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
				if flagJSON {
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
	return cmd
}

func actionUnimportCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "unimport <spec-url>",
		Short: "Deactivate OpenAPI-imported actions without deleting history",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			specURL := args[0]
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				actions, err := k.UnimportOpenAPI(context.Background(), callerID, callerID, specURL, name)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(actions)
				}
				fmt.Printf("deactivated %d action(s)\n", len(actions))
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Deactivate only the action with this name or operation_key")
	return cmd
}

func actionStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats <action>",
		Short: "Show statistics for an action (action is @owner/name or an id)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withKernel(func(k *kernel.Kernel) error {
				a, err := resolveActionRef(k, context.Background(), args[0])
				if err != nil {
					return err
				}
				stats, err := actionStats(k, context.Background(), a.ID)
				if err != nil {
					return err
				}
				if stats == nil {
					if flagJSON {
						return printJSON(nil)
					}
					fmt.Println("No statistics yet.")
					return nil
				}
				return emit(stats)
			})
		},
	}
}

// ---- process ----

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Process lifecycle commands"}
	processCmd.AddCommand(processListCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List processes owned by the current user",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				processes, err := listProcesses(k, context.Background(), callerID, 100, 0)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(processes)
				}
				for _, p := range processes {
					fmt.Printf("%s  %-6s  available:%-6d  locked:%-6d\n",
						p.ID, p.Status, p.Available, p.Locked)
				}
				return nil
			})
		},
	}
}

func processEndCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "end <id>",
		Short: "End a process and return remaining funds",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				if err := endProcess(k, context.Background(), callerID, args[0]); err != nil {
					return err
				}
				fmt.Printf("Process %s ended.\n", args[0])
				return nil
			})
		},
	}
}

func processShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show process details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				p, err := getProcess(k, context.Background(), callerID, args[0])
				if err != nil {
					return err
				}
				return emit(p)
			})
		},
	}
}

// ---- step ----

func init() {
	stepCmd := &cobra.Command{Use: "step", Short: "Step management commands"}
	stepCmd.AddCommand(stepCreateCmd(), stepListCmd(), stepShowCmd(), stepCompleteCmd())
	rootCmd.AddCommand(stepCmd)
}

func stepCreateCmd() *cobra.Command {
	var traceID, requiredCaller string
	var partialArgs string
	cmd := &cobra.Command{
		Use:   "create <action>",
		Short: "Create a step (pause point for external completion; action is @owner/name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				ctx := context.Background()

				var pa json.RawMessage
				if partialArgs != "" {
					pa = json.RawMessage(partialArgs)
				}

				view, err := createStep(k, ctx, callerID, createStepParams{
					TraceID:        traceID,
					ActionRef:      args[0],
					RequiredCaller: requiredCaller,
					PartialArgs:    pa,
				})
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(view.ID)
					return nil
				}
				return emit(view)
			})
		},
	}
	cmd.Flags().StringVar(&traceID, "trace", "", "Trace ID (required)")
	cmd.Flags().StringVar(&requiredCaller, "required-caller", "", "User who must complete the step, e.g. @webhook (required)")
	cmd.Flags().StringVar(&partialArgs, "partial-args", "", "Partial args as JSON object")
	_ = cmd.MarkFlagRequired("trace")
	_ = cmd.MarkFlagRequired("required-caller")
	return cmd
}

func stepListCmd() *cobra.Command {
	var processID, status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List steps visible to the current user",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				steps, err := listSteps(k, context.Background(), callerID, processID, status)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(steps)
				}
				for _, s := range steps {
					fmt.Printf("%s  %-7s  %s\n", s.ID, s.Status, s.Action)
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
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show step details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				step, err := getStep(k, context.Background(), callerID, args[0])
				if err != nil {
					return err
				}
				return emit(step)
			})
		},
	}
}

func stepCompleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "complete <id> [json]",
		Short: "Complete a waiting step (json is the input object, default {})",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				input := json.RawMessage("{}")
				if len(args) == 2 && args[1] != "" {
					input = json.RawMessage(args[1])
				}
				reply, err := completeStep(k, context.Background(), callerID, args[0], input)
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(reply.TxID)
					return nil
				}
				return emit(reply)
			})
		},
	}
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
		Args:  cobra.NoArgs,
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
				if flagJSON {
					return printJSON(txs)
				}
				for _, tx := range txs {
					fmt.Printf("[%s] %s  status:%s  gross:%d\n",
						tx.StartedAt.Format("2006-01-02T15:04:05"),
						tx.ID, tx.Status, tx.Gross)
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
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a transaction",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				tv, err := getTransaction(k, context.Background(), callerID, args[0])
				if err != nil {
					return err
				}
				return emit(tv)
			})
		},
	}
}

func txVerifyReceiptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <id>",
		Short: "Verify the remote receipt for a transaction",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				v, err := verifyReceipt(k, context.Background(), callerID, args[0])
				if err != nil {
					return err
				}
				return emit(v)
			})
		},
	}
}

func txRateCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "rate <id> <0|1>",
		Short: "Rate a transaction (0 bad, 1 good)",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			rating, err := strconv.ParseFloat(args[1], 64)
			if err != nil {
				return fmt.Errorf("rating must be 0 or 1")
			}
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				var notePtr *string
				if note != "" {
					notePtr = &note
				}
				r, err := rateTransaction(k, context.Background(), callerID, args[0], rating, notePtr)
				if err != nil {
					return err
				}
				return emit(r)
			})
		},
	}
	cmd.Flags().StringVar(&note, "note", "", "Optional justification note")
	return cmd
}

// ---- run ----

func init() {
	rootCmd.AddCommand(runCmd())
}

func runCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run <action> [json]",
		Short: "Run an action (creates a process, calls the action, closes the process)",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, cmdArgs []string) error {
			return withCaller(func(k *kernel.Kernel, callerID string) error {
				argsStr := "{}"
				if len(cmdArgs) == 2 && cmdArgs[1] != "" {
					argsStr = cmdArgs[1]
				}
				args, err := readJSONArg(argsStr)
				if err != nil {
					return fmt.Errorf("invalid args: %w", err)
				}
				reply, err := run(k, context.Background(), callerID, cmdArgs[0], args)
				if err != nil {
					return err
				}
				if flagQuiet {
					fmt.Println(reply.TxID)
					return nil
				}
				return emit(reply)
			})
		},
	}
	return cmd
}
