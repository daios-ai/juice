package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// grantActionRef returns the action a grant_required error names (from its structured Meta),
// falling back to fallback when absent.
func grantActionRef(err error, fallback string) string {
	var ke *kernel.KernelError
	if errors.As(err, &ke) && ke.Meta["action"] != "" {
		return ke.Meta["action"]
	}
	return fallback
}

// interactiveTTY reports whether a human is driving: stdin readable and stderr a terminal.
// Prompts and progress go to stderr so stdout stays payload-only (§14).
func interactiveTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// promptYesNo asks a yes/no question on stderr (default yes) and reads one line from stdin.
func promptYesNo(msg string) bool {
	fmt.Fprintf(os.Stderr, "%s [Y/n] ", msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

// ---- output helpers ----

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// printJSONBytes indents already-marshaled JSON in place. json.Indent preserves the
// source field order (unlike unmarshal-then-MarshalIndent, which would alphabetize map
// keys), so server responses print in their declared order.
func printJSONBytes(b []byte) error {
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		fmt.Println(string(b)) // not an object/array; print verbatim
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

// emitRaw prints a server JSON response, preserving field order: canonical indented
// JSON with --json, else the human field view. The client analogue of emit.
func emitRaw(b []byte) error {
	if flagJSON {
		return printJSONBytes(b)
	}
	return printTextBytes(b)
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
	return printTextBytes(b)
}

// printTextBytes renders already-marshaled JSON bytes as the human view, preserving
// the source field order (so server responses print in their declared order, not
// alphabetized). See printText for the field-view contract.
func printTextBytes(b []byte) error {
	// Non-object top levels (arrays, scalars) have no labeled fields; print as JSON.
	trimmed := b
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\n' || trimmed[0] == '\t') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return printJSONBytes(b)
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
// arrays as indented JSON, everything else as-is.
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
		// Indent nested objects/arrays; the "  " prefix keeps continuation lines and the
		// closing bracket aligned under the "  key:" label. json.Indent preserves key order.
		var buf bytes.Buffer
		if err := json.Indent(&buf, raw, "  ", "  "); err == nil {
			return buf.String()
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
	userCmd := &cobra.Command{Use: "user", Short: "Manage your account"}
	userCmd.AddCommand(userCreateCmd(), userMeCmd(), userUpdateCmd(), userConnectCmd(), userDisconnectCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "create <user> <email>",
		Short: "Create a user account",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			user, email := args[0], args[1]
			if password == "" {
				p, err := promptNewPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			return apiEmit("POST", "/v1/users", map[string]any{
				"handle": user, "email": email, "password": password,
			})
		},
	}
	cmd.Flags().StringVar(&password, "password", "", "Password (prompted if omitted)")
	return cmd
}

func userMeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Show your profile",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return apiEmit("GET", "/v1/me", nil)
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
				return kernel.ErrInvalidInput.Wrap("at least one of --email or --password must be specified")
			}
			var currentPassword, newPassword string
			if changePassword {
				var err error
				if currentPassword, err = promptPassword("Current password: "); err != nil {
					return err
				}
				if newPassword, err = promptNewPassword("New password: "); err != nil {
					return err
				}
			}
			body := map[string]any{}
			if email != "" {
				body["email"] = email
			}
			if changePassword {
				body["current_password"] = currentPassword
				body["password"] = newPassword
			}
			return apiEmit("PUT", "/v1/me", body)
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "New email address")
	cmd.Flags().BoolVar(&changePassword, "password", false, "Change your password")
	return cmd
}

// ---- action ----

func init() {
	actionCmd := &cobra.Command{Use: "action", Short: "Manage actions"}
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
	var kind, source, description, artifact, method string
	var params []string
	var price int64
	var inputSchemaStr, outputSchemaStr, authStr string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create an action",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			inputSchema := map[string]any{}
			if inputSchemaStr != "" {
				if err := unmarshalJSONArg(inputSchemaStr, &inputSchema); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --input-schema: %v", err)
				}
			}
			outputSchema := map[string]any{}
			if outputSchemaStr != "" {
				if err := unmarshalJSONArg(outputSchemaStr, &outputSchema); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --output-schema: %v", err)
				}
			}
			var auth *kernel.AuthInput
			if authStr != "" {
				auth = &kernel.AuthInput{}
				if err := unmarshalJSONArg(authStr, auth); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --auth: %v", err)
				}
			}
			srcData := source
			if source != "" {
				if _, err := os.Stat(source); err == nil {
					data, err := os.ReadFile(source)
					if err != nil {
						return kernel.ErrInvalidInput.Wrapf("reading source file: %v", err)
					}
					srcData = string(data)
				}
			}
			// --artifact carries a pre-compiled base64 WASM artifact (e.g. the
			// output of @sys/tinygo/compile); a file path is read for its contents.
			artData := artifact
			if artifact != "" {
				if _, err := os.Stat(artifact); err == nil {
					data, err := os.ReadFile(artifact)
					if err != nil {
						return kernel.ErrInvalidInput.Wrapf("reading artifact file: %v", err)
					}
					artData = strings.TrimSpace(string(data))
				}
			}
			httpParams, err := parseParams(params)
			if err != nil {
				return err
			}
			// A compiled WASM module is binary and cannot ride losslessly in a JSON string
			// (invalid UTF-8 is replaced with U+FFFD). Route a binary wasm source through the
			// base64 wasm_artifact field instead; TinyGo text source stays in source.
			if kind == "wasm" && srcData != "" && !utf8.ValidString(srcData) {
				if artData == "" {
					artData = base64.StdEncoding.EncodeToString([]byte(srcData))
				}
				srcData = ""
			}
			body := map[string]any{
				"name": name, "kind": kind, "price": price, "description": description,
				"input_schema": inputSchema, "output_schema": outputSchema,
				"source": srcData, "wasm_artifact": artData,
			}
			if method != "" {
				body["method"] = method
			}
			if len(httpParams) > 0 {
				body["params"] = httpParams
			}
			if auth != nil {
				body["auth"] = auth
			}
			return apiEmit("POST", "/v1/actions", body)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http, wasm, native")
	cmd.Flags().StringVar(&source, "source", "", "URL (http) or file path (wasm)")
	cmd.Flags().StringVar(&method, "method", "", "HTTP verb (default POST)")
	cmd.Flags().StringArrayVar(&params, "param", nil, "HTTP field binding name:in (path|query|body); repeatable")
	cmd.Flags().StringVar(&artifact, "artifact", "", "Base64 WASM artifact or file path")
	cmd.Flags().StringVar(&description, "description", "", "Description")
	cmd.Flags().Int64Var(&price, "price", 0, "Price in credits")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "JSON Schema for inputs (or @file.json)")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "JSON Schema for outputs (or @file.json)")
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json)")
	return cmd
}

func actionUpdateCmd() *cobra.Command {
	var description, source, method string
	var params []string
	var price int64
	var public bool
	var inputSchemaStr, outputSchemaStr, authStr string
	cmd := &cobra.Command{
		Use:   "update <action>",
		Short: "Update an action",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ctx := context.Background()
			id, err := resolveActionID(ctx, args[0])
			if err != nil {
				return err
			}
			req := map[string]any{}
			if c.Flags().Changed("description") {
				req["description"] = description
			}
			if c.Flags().Changed("source") {
				req["source"] = source
			}
			if c.Flags().Changed("method") {
				req["method"] = method
			}
			if c.Flags().Changed("param") {
				httpParams, err := parseParams(params)
				if err != nil {
					return err
				}
				req["params"] = httpParams
			}
			if c.Flags().Changed("price") {
				req["price"] = price
			}
			if c.Flags().Changed("public") {
				req["public"] = public
			}
			if inputSchemaStr != "" {
				m := map[string]any{}
				if err := unmarshalJSONArg(inputSchemaStr, &m); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --input-schema: %v", err)
				}
				req["input_schema"] = m
			}
			if outputSchemaStr != "" {
				m := map[string]any{}
				if err := unmarshalJSONArg(outputSchemaStr, &m); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --output-schema: %v", err)
				}
				req["output_schema"] = m
			}
			if c.Flags().Changed("auth") {
				auth := &kernel.AuthInput{}
				if err := unmarshalJSONArg(authStr, auth); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --auth: %v", err)
				}
				req["auth"] = auth
			}
			return apiEmit("PUT", "/v1/actions/"+id, req)
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().StringVar(&method, "method", "", "New HTTP verb")
	cmd.Flags().StringArrayVar(&params, "param", nil, "HTTP field binding name:in (path|query|body); repeatable")
	cmd.Flags().Int64Var(&price, "price", 0, "New price in credits")
	cmd.Flags().BoolVar(&public, "public", false, "Make action public or private")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "New JSON Schema for inputs (or @file.json)")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "New JSON Schema for outputs (or @file.json)")
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json)")
	return cmd
}

func actionEnableCmd() *cobra.Command {
	return actionActiveCmd("enable <action>", "Activate an action", "enable", "enabled", true)
}

func actionDisableCmd() *cobra.Command {
	return actionActiveCmd("disable <action>", "Deactivate an action", "disable", "disabled", false)
}

// actionActiveCmd builds the enable/disable action command; the two differ only in wording
// and the endpoint suffix.
func actionActiveCmd(use, short, suffix, pastTense string, active bool) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			id, err := resolveActionID(ctx, args[0])
			if err != nil {
				return err
			}
			if err := apiCall(ctx, "POST", "/v1/actions/"+id+"/"+suffix, nil, nil); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]bool{"active": active})
			}
			fmt.Printf("Action %s %s.\n", args[0], pastTense)
			return nil
		},
	}
}

// setLimitOffset adds the standard pagination params to a query when set (>0).
func setLimitOffset(q url.Values, limit, offset int) {
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
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
			ctx := context.Background()
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			// --all lists the caller's own actions regardless of active/public via the
			// self-owner filter (§3); it needs the caller's handle.
			if all {
				h, err := currentHandle(ctx)
				if err != nil {
					return err
				}
				q.Set("owner", h)
			}
			var actions []actionResp
			if err := apiCall(ctx, "GET", "/v1/actions?"+q.Encode(), nil, &actions); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(actions)
			}
			for _, a := range actions {
				if all {
					active := " "
					if a.Active {
						active = "*"
					}
					fmt.Printf("[%s] %s  %-30s  %d credits\n", active, a.ActionRef, a.Name, a.Price)
				} else {
					fmt.Printf("  %-24s  %d credits\n", a.ActionRef, a.Price)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include your inactive/private actions")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(&offset, "offset", 0, "Pagination offset")
	return cmd
}

func actionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <action>",
		Short: "Show an action's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			id, err := resolveActionID(ctx, args[0])
			if err != nil {
				return err
			}
			return apiEmit("GET", "/v1/actions/"+id, nil)
		},
	}
}

func actionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <action>",
		Short: "Delete an action, preserving history",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			id, err := resolveActionID(ctx, args[0])
			if err != nil {
				return err
			}
			if err := apiCall(ctx, "DELETE", "/v1/actions/"+id, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Action %s deleted.\n", args[0])
			return nil
		},
	}
}

func actionImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import <spec-url>",
		Short: "Import OpenAPI operations",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			specURL := args[0]
			var result kernel.ImportResult
			if err := apiCall(context.Background(), "POST", "/v1/actions/import",
				map[string]any{"spec_url": specURL}, &result); err != nil {
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
		},
	}
	return cmd
}

func actionUnimportCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "unimport <spec-url>",
		Short: "Deactivate OpenAPI-imported actions",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			specURL := args[0]
			body := map[string]any{"spec_url": specURL}
			if name != "" {
				body["name"] = name
			}
			var actions []actionResp
			if err := apiCall(context.Background(), "POST", "/v1/actions/unimport", body, &actions); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(actions)
			}
			fmt.Printf("deactivated %d action(s)\n", len(actions))
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Deactivate only this name or operation_key")
	return cmd
}

func actionStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats <action>",
		Short: "Show an action's statistics",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			id, err := resolveActionID(ctx, args[0])
			if err != nil {
				return err
			}
			var raw json.RawMessage
			if err := apiCall(ctx, "GET", "/v1/stats/"+id, nil, &raw); err != nil {
				return err
			}
			if len(raw) == 0 || string(raw) == "null" {
				if flagJSON {
					return printJSON(nil)
				}
				fmt.Println("No statistics yet.")
				return nil
			}
			return emitRaw(raw)
		},
	}
}

// ---- process ----

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Manage processes"}
	processCmd.AddCommand(processListCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List processes",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			var processes []*processView
			if err := apiCall(context.Background(), "GET", "/v1/processes", nil, &processes); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(processes)
			}
			for _, p := range processes {
				awaiting := ""
				if p.AwaitingReceipt && p.AwaitingReceiptSince != nil {
					awaiting = fmt.Sprintf("  awaiting-receipt since %s", p.AwaitingReceiptSince.Format(time.RFC3339))
				}
				fmt.Printf("%s  %-6s  available:%-6d  locked:%-6d%s\n",
					p.ID, p.Status, p.Available, p.Locked, awaiting)
			}
			return nil
		},
	}
}

func processEndCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "end <id>",
		Short: "End a process",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if err := apiCall(context.Background(), "POST", "/v1/processes/"+args[0]+"/end", nil, nil); err != nil {
				return err
			}
			fmt.Printf("Process %s ended.\n", args[0])
			return nil
		},
	}
}

func processShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show process details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/v1/processes/"+args[0], nil)
		},
	}
}

// ---- step ----

func init() {
	stepCmd := &cobra.Command{Use: "step", Short: "Manage steps"}
	stepCmd.AddCommand(stepCreateCmd(), stepListCmd(), stepShowCmd(), stepCompleteCmd())
	rootCmd.AddCommand(stepCmd)
}

func stepCreateCmd() *cobra.Command {
	var traceID, requiredCaller string
	var partialArgs string
	cmd := &cobra.Command{
		Use:   "create <action>",
		Short: "Create a step",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			pa := json.RawMessage("{}")
			if partialArgs != "" {
				var err error
				if pa, err = loadJSONArg(partialArgs); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --partial-args: %v", err)
				}
			}
			body := map[string]any{
				"trace_id":        traceID,
				"action_id":       args[0], // @owner/name or id; the server resolves it
				"required_caller": requiredCaller,
				"partial_args":    pa,
			}
			if flagQuiet {
				var view struct {
					ID string `json:"id"`
				}
				if err := apiCall(context.Background(), "POST", "/v1/steps", body, &view); err != nil {
					return err
				}
				fmt.Println(view.ID)
				return nil
			}
			return apiEmit("POST", "/v1/steps", body)
		},
	}
	cmd.Flags().StringVar(&traceID, "trace", "", "Trace ID (required)")
	cmd.Flags().StringVar(&requiredCaller, "required-caller", "", "User who must complete the step (required)")
	cmd.Flags().StringVar(&partialArgs, "partial-args", "", "Partial args as JSON object")
	_ = cmd.MarkFlagRequired("trace")
	_ = cmd.MarkFlagRequired("required-caller")
	return cmd
}

func stepListCmd() *cobra.Command {
	var processID, status string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List steps",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			if processID != "" {
				q.Set("process_id", processID)
			}
			if status != "" {
				q.Set("status", status)
			}
			var steps []stepWithAction
			if err := apiCall(context.Background(), "GET", "/v1/steps?"+q.Encode(), nil, &steps); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(steps)
			}
			for _, s := range steps {
				marker := ""
				if s.WaitingOnPeer {
					marker = "  waiting-on-peer"
				}
				fmt.Printf("%s  %-7s  %s%s\n", s.ID, s.Status, s.Action, marker)
			}
			return nil
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
			return apiEmit("GET", "/v1/steps/"+args[0], nil)
		},
	}
}

func stepCompleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "complete <id> [json]",
		Short: "Complete a waiting step",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			raw := ""
			if len(args) == 2 {
				raw = args[1]
			}
			input, err := loadJSONArg(raw)
			if err != nil {
				return kernel.ErrInvalidInput.Wrapf("invalid input: %v", err)
			}
			var reply json.RawMessage
			if err := apiCall(context.Background(), "POST", "/v1/steps/"+args[0]+"/complete",
				map[string]any{"args": input}, &reply); err != nil {
				return err
			}
			if flagQuiet {
				var r struct {
					TxID string `json:"tx_id"`
				}
				_ = json.Unmarshal(reply, &r)
				fmt.Println(r.TxID)
				return nil
			}
			return emitRaw(reply)
		},
	}
	return cmd
}

// ---- tx ----

func init() {
	txCmd := &cobra.Command{Use: "tx", Short: "Manage transactions"}
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
			q := url.Values{}
			if processID != "" {
				q.Set("process_id", processID)
			}
			setLimitOffset(q, limit, offset)
			var txs []*kernel.TransactionView
			if err := apiCall(context.Background(), "GET", "/v1/transactions?"+q.Encode(), nil, &txs); err != nil {
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
			return apiEmit("GET", "/v1/transactions/"+args[0], nil)
		},
	}
}

func txVerifyReceiptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify <id>",
		Short: "Verify a transaction's remote receipt",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/v1/transactions/"+args[0]+"/receipt-verification", nil)
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
				return kernel.ErrInvalidInput.Wrap("rating must be 0 or 1")
			}
			body := map[string]any{"rating": rating}
			if note != "" {
				body["note"] = note
			}
			return apiEmit("POST", "/v1/transactions/"+args[0]+"/rate", body)
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
		Short: "Run an action",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, cmdArgs []string) error {
			argsStr := "{}"
			if len(cmdArgs) == 2 && cmdArgs[1] != "" {
				argsStr = cmdArgs[1]
			}
			args, err := readJSONArg(argsStr)
			if err != nil {
				return kernel.ErrInvalidInput.Wrapf("invalid args: %v", err)
			}
			reqBody := map[string]any{"action": cmdArgs[0], "args": args}
			var raw json.RawMessage
			err = apiCall(context.Background(), "POST", "/v1/run", reqBody, &raw)
			// A delegated-OAuth action needs a one-time consent (§8). At an interactive terminal,
			// offer it inline and re-run once, so the user issues a single `juice run`. Non-TTY
			// callers (scripts, agents) get the structured error + hint instead — no browser.
			if errors.Is(err, kernel.ErrGrantRequired) {
				action := grantActionRef(err, cmdArgs[0])
				if interactiveTTY() && promptYesNo(fmt.Sprintf("This action needs your authorization. Authorize %s now?", action)) {
					if cerr := runConsentFlow(action); cerr != nil {
						return cerr
					}
					err = apiCall(context.Background(), "POST", "/v1/run", reqBody, &raw)
				} else {
					fmt.Fprintf(os.Stderr, "\nAuthorize with:\n  juice user connect %s\n", action)
					return err
				}
			}
			if err != nil {
				if errors.Is(err, kernel.ErrGrantRequired) {
					fmt.Fprintf(os.Stderr, "\nAuthorize with:\n  juice user connect %s\n", grantActionRef(err, cmdArgs[0]))
				}
				return err
			}
			if flagQuiet {
				var r struct {
					TxID string `json:"tx_id"`
				}
				_ = json.Unmarshal(raw, &r)
				fmt.Println(r.TxID)
				return nil
			}
			return emitRaw(raw)
		},
	}
	return cmd
}
