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
// addPagingFlags registers the --limit/--offset pair every list command shares.
func addPagingFlags(cmd *cobra.Command, limit, offset *int) {
	cmd.Flags().IntVar(limit, "limit", 50, "Maximum results")
	cmd.Flags().IntVar(offset, "offset", 0, "Pagination offset")
}

func grantActionRef(err error, fallback string) string {
	var ke *kernel.KernelError
	if errors.As(err, &ke) && ke.Meta["action"] != "" {
		return ke.Meta["action"]
	}
	return fallback
}

// prepareSourceArtifact reads the --source and --artifact values (each may be a file path or a
// literal), returning the source text and base64 artifact to send. A compiled WASM module is
// binary and cannot ride losslessly in a JSON string; only a binary wasm source is ever
// non-UTF-8 (URLs and TinyGo text are UTF-8), so such a source is routed into the base64
// wasm_artifact field. Shared by `action create` and `action update` for symmetric behavior.
func prepareSourceArtifact(source, artifact string) (srcData, artData string, err error) {
	srcData = source
	if source != "" {
		if _, statErr := os.Stat(source); statErr == nil {
			data, readErr := os.ReadFile(source)
			if readErr != nil {
				return "", "", kernel.ErrInvalidInput.Wrapf("reading source file: %v", readErr)
			}
			srcData = string(data)
		}
	}
	artData = artifact
	if artifact != "" {
		if _, statErr := os.Stat(artifact); statErr == nil {
			data, readErr := os.ReadFile(artifact)
			if readErr != nil {
				return "", "", kernel.ErrInvalidInput.Wrapf("reading artifact file: %v", readErr)
			}
			artData = strings.TrimSpace(string(data))
		}
	}
	if srcData != "" && !utf8.ValidString(srcData) {
		if artData == "" {
			artData = base64.StdEncoding.EncodeToString([]byte(srcData))
		}
		srcData = ""
	}
	return srcData, artData, nil
}

// directorySelector turns an action ref (@owner/name) into the selector that connects its whole
// directory in one gesture (§8): drop the last name segment when the name has ≥2 segments, else the
// ref itself. So @a/mail/send → @a/mail, and @a/send → @a/send.
func directorySelector(ref string) string {
	at := strings.Index(ref, "/")
	if at < 0 {
		return ref
	}
	owner, name := ref[:at], ref[at+1:]
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return owner + "/" + name[:i]
	}
	return ref
}

// peerMetaHandle returns the bare peer handle a peer_unreachable/peer_unfunded error names
// (§13), or "peer" when absent.
func peerMetaHandle(err error) string {
	var ke *kernel.KernelError
	if errors.As(err, &ke) && ke.Meta["peer"] != "" {
		return ke.Meta["peer"]
	}
	return "peer"
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

// ---- user ----

func init() {
	userCmd := &cobra.Command{Use: "user", Short: "Manage your account"}
	userCmd.AddCommand(userCreateCmd(), userMeCmd(), userUpdateCmd(), userTransferCmd(), userLedgerCmd(), userConnectCmd(), userDisconnectCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "create <user>",
		Short: "Create a user account",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			user := args[0]
			if password == "" {
				p, err := promptNewPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			// Enroll a recovery phrase (§12): generate it client-side, send only the public key,
			// and show the phrase once. It is the sole recovery credential — the server never sees it.
			mnemonic, recoveryPub, err := generateRecovery()
			if err != nil {
				return err
			}
			var view json.RawMessage
			if err := apiCall(context.Background(), "POST", "/v1/users", kernel.CreateUserRequest{
				Handle: user, Password: password, RecoveryPublicKey: recoveryPub,
			}, &view); err != nil {
				return err
			}
			fmt.Fprintln(os.Stderr, "Recovery phrase (write this down; it is shown only once and cannot be recovered):")
			fmt.Fprintln(os.Stderr, "  "+mnemonic)
			return emitRaw(view)
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
	var description string
	var setDescription bool
	var changePassword bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update your description or password",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			setDescription = cmd.Flags().Changed("description")
			if !setDescription && !changePassword {
				return kernel.ErrInvalidInput.Wrap("at least one of --description or --password is required")
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
			var req kernel.UpdateUserRequest
			if setDescription {
				req.Description = &description // may point at "" to clear
			}
			if changePassword {
				req.CurrentPassword, req.NewPassword = currentPassword, newPassword
			}
			return apiEmit("PUT", "/v1/me", req)
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "New profile description (about); pass empty to clear")
	cmd.Flags().BoolVar(&changePassword, "password", false, "Change your password")
	return cmd
}

func userTransferCmd() *cobra.Command {
	var reason, externalKey string
	cmd := &cobra.Command{
		Use:   "transfer <recipient> <amount>",
		Short: "Send credits to another user",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			amount, err := parseAmount(args[1])
			if err != nil {
				return err
			}
			return apiEmit("POST", "/v1/transfers", map[string]any{
				"recipient": args[0], "amount": amount, "reason": reason, "external_key": externalKey,
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().StringVar(&externalKey, "external-key", "", "Optional idempotency token")
	return cmd
}

func userLedgerCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "ledger",
		Short: "List your credit movements (deposits, withdrawals, transfers)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			var entries []*ledgerView
			if err := apiCall(context.Background(), "GET", "/v1/ledger?"+q.Encode(), nil, &entries); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(entries)
			}
			for _, e := range entries {
				from, to := e.FromHandle, e.ToHandle
				if from == "" {
					from = "—"
				}
				if to == "" {
					to = "—"
				}
				fmt.Printf("[%s] amount:%-6d  from:%-12s  to:%-12s  %s\n",
					e.CreatedAt.Format(time.RFC3339),
					e.Amount, from, to, e.Reason)
			}
			return nil
		},
	}
	addPagingFlags(cmd, &limit, &offset)
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
		actionRatingsCmd(),
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
			srcData, artData, err := prepareSourceArtifact(source, artifact)
			if err != nil {
				return err
			}
			httpParams, err := parseParams(params)
			if err != nil {
				return err
			}
			return apiEmit("POST", "/v1/actions", kernel.CreateActionRequest{
				Name: name, Kind: kernel.ActionKind(kind), Price: price, Description: description,
				InputSchema: inputSchema, OutputSchema: outputSchema,
				Source: srcData, WasmArtifact: artData,
				Method: method, Params: httpParams, Auth: auth,
			})
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
	var description, source, method, artifact string
	var params []string
	var price int64
	var visibility string
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
			// Pointer fields carry the absent/set distinction the contract defines (§14): a flag the
			// user did not pass stays nil, so the server leaves that term alone.
			var req kernel.UpdateActionRequest
			if c.Flags().Changed("description") {
				req.Description = &description
			}
			if c.Flags().Changed("source") || c.Flags().Changed("artifact") {
				srcData, artData, err := prepareSourceArtifact(source, artifact)
				if err != nil {
					return err
				}
				if srcData != "" {
					req.Source = &srcData
				}
				req.WasmArtifact = artData
			}
			if c.Flags().Changed("method") {
				req.Method = &method
			}
			if c.Flags().Changed("param") {
				httpParams, err := parseParams(params)
				if err != nil {
					return err
				}
				req.Params = &httpParams
			}
			if c.Flags().Changed("price") {
				req.Price = &price
			}
			if c.Flags().Changed("visibility") {
				v := kernel.ActionVisibility(visibility)
				req.Visibility = &v
			}
			if inputSchemaStr != "" {
				if err := unmarshalJSONArg(inputSchemaStr, &req.InputSchema); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --input-schema: %v", err)
				}
			}
			if outputSchemaStr != "" {
				if err := unmarshalJSONArg(outputSchemaStr, &req.OutputSchema); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --output-schema: %v", err)
				}
			}
			if c.Flags().Changed("auth") {
				req.Auth = &kernel.AuthInput{}
				if err := unmarshalJSONArg(authStr, req.Auth); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --auth: %v", err)
				}
			}
			return apiEmit("PUT", "/v1/actions/"+id, req)
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().StringVar(&artifact, "artifact", "", "New base64 WASM artifact or file path")
	cmd.Flags().StringVar(&method, "method", "", "New HTTP verb")
	cmd.Flags().StringArrayVar(&params, "param", nil, "HTTP field binding name:in (path|query|body); repeatable")
	cmd.Flags().Int64Var(&price, "price", 0, "New price in credits")
	cmd.Flags().StringVar(&visibility, "visibility", "", "Set visibility: private|local|public")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "New JSON Schema for inputs (or @file.json)")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "New JSON Schema for outputs (or @file.json)")
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json)")
	return cmd
}

func actionEnableCmd() *cobra.Command {
	return actionActiveCmd("enable <action>", "Enable an action", "enable", "enabled", true)
}

func actionDisableCmd() *cobra.Command {
	return actionActiveCmd("disable <action>", "Disable an action", "disable", "disabled", false)
}

// actionRunE adapts a command body that needs the resolved action id: every action subcommand
// takes one positional ref and resolves it the same way, so the preamble lives here once.
func actionRunE(fn func(ctx context.Context, id, ref string) error) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		ctx := context.Background()
		id, err := resolveActionID(ctx, args[0])
		if err != nil {
			return err
		}
		return fn(ctx, id, args[0])
	}
}

// actionActiveCmd builds the enable/disable action command; the two differ only in wording
// and the endpoint suffix.
func actionActiveCmd(use, short, suffix, pastTense string, active bool) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			if err := apiCall(ctx, "POST", "/v1/actions/"+id+"/"+suffix, nil, nil); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(map[string]bool{"active": active})
			}
			fmt.Printf("Action %s %s.\n", ref, pastTense)
			return nil
		}),
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
			// Default is active-only (like `docker ps`); --all includes inactive/private rows in
			// the caller's scope (own for a normal user, all owners for the superuser).
			if all {
				q.Set("all", "1")
			}
			var actions []actionResp
			if err := apiCall(ctx, "GET", "/v1/actions?"+q.Encode(), nil, &actions); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(actions)
			}
			for _, a := range actions {
				grant := ""
				if a.RequiresGrant {
					grant = " [grant]" // caller must connect their own credential first (§8)
				}
				if all {
					active := " "
					if a.Active {
						active = "*"
					}
					fmt.Printf("[%s] %s  %-30s  %d credits%s\n", active, a.ActionRef, a.Name, a.Price, grant)
				} else {
					fmt.Printf("  %-24s  %d credits%s\n", a.ActionRef, a.Price, grant)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include your inactive/private actions")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func actionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <action>",
		Short: "Show action details",
		Args:  cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			return apiEmit("GET", "/v1/actions/"+id, nil)
		}),
	}
}

func actionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <action>",
		Short: "Delete an action, preserving history",
		Args:  cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			if err := apiCall(ctx, "DELETE", "/v1/actions/"+id, nil, nil); err != nil {
				return err
			}
			fmt.Printf("Action %s deleted.\n", ref)
			return nil
		}),
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
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
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
		}),
	}
}

func actionRatingsCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "ratings <action>",
		Short: "Show an action's public ratings",
		Args:  cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			path := "/v1/actions/" + id + "/ratings"
			if e := q.Encode(); e != "" {
				path += "?" + e
			}
			var raw json.RawMessage
			if err := apiCall(ctx, "GET", path, nil, &raw); err != nil {
				return err
			}
			if flagJSON {
				return emitRaw(raw)
			}
			var ratings []struct {
				Value   int     `json:"value"`
				Note    *string `json:"note"`
				Created string  `json:"created_at"`
			}
			if err := json.Unmarshal(raw, &ratings); err != nil {
				return err
			}
			for _, rt := range ratings {
				note := ""
				if rt.Note != nil {
					note = "  " + *rt.Note
				}
				fmt.Printf("%d  %s%s\n", rt.Value, rt.Created, note)
			}
			return nil
		}),
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// ---- process ----

func init() {
	processCmd := &cobra.Command{Use: "process", Short: "Manage processes"}
	processCmd.AddCommand(processListCmd(), processEndCmd(), processShowCmd())
	rootCmd.AddCommand(processCmd)
}

func processListCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List processes",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			var processes []*processView
			if err := apiCall(context.Background(), "GET", "/v1/processes?"+q.Encode(), nil, &processes); err != nil {
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
	addPagingFlags(cmd, &limit, &offset)
	return cmd
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
			body := createStepParams{
				TraceID:        traceID,
				ActionRef:      args[0], // owner/name or id; the server resolves it
				RequiredCaller: requiredCaller,
				PartialArgs:    pa,
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
	var limit, offset int
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
			setLimitOffset(q, limit, offset)
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
				label := s.Action
				if s.CreatedBy != "" {
					label = s.CreatedBy + " → " + s.Action
				}
				fmt.Printf("%s  %-7s  %s%s\n", s.ID, s.Status, label, marker)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done)")
	addPagingFlags(cmd, &limit, &offset)
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
	var peer string
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
			body := map[string]any{"args": input}
			if peer != "" {
				body["peer"] = peer
			}
			if err := apiCall(context.Background(), "POST", "/v1/steps/"+args[0]+"/complete",
				body, &reply); err != nil {
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
	cmd.Flags().StringVar(&peer, "peer", "", "Complete a step held by this peer (handle or key), over federation")
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
					tx.StartedAt.Format(time.RFC3339),
					tx.ID, tx.Status, tx.Gross)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func txShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show transaction details",
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
		Long:  "Rate a transaction (0 bad, 1 good). The rating and note are visible wherever the action is visible.",
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
	var quoteHash string
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
			reqBody := kernel.RunRequest{ActionRef: cmdArgs[0], Args: args, QuoteHash: quoteHash}
			var raw json.RawMessage
			err = apiCall(context.Background(), "POST", "/v1/run", reqBody, &raw)
			// A delegated-OAuth action needs a one-time consent (§8). At an interactive terminal,
			// offer it inline and re-run once, so the user issues a single `juice run`. Non-TTY
			// callers (scripts, agents) get the structured error + hint instead — no browser.
			if errors.Is(err, kernel.ErrGrantRequired) {
				action := grantActionRef(err, cmdArgs[0])
				if interactiveTTY() && promptYesNo(fmt.Sprintf("This action needs your authorization. Authorize %s now?", action)) {
					if cerr := connectSelector(action, false, true); cerr != nil {
						return cerr
					}
					err = apiCall(context.Background(), "POST", "/v1/run", reqBody, &raw)
				} else {
					fmt.Fprintf(os.Stderr, "\nAuthorize with:\n  juice user connect %s\n", directorySelector(action))
					return err
				}
			}
			if err != nil {
				if errors.Is(err, kernel.ErrGrantRequired) {
					fmt.Fprintf(os.Stderr, "\nAuthorize with:\n  juice user connect %s\n", directorySelector(grantActionRef(err, cmdArgs[0])))
				}
				// Federation-relationship failures (§13): the caller's own balance is fine — say so,
				// and point at the operator remedy instead of a caller one.
				if errors.Is(err, kernel.ErrPeerUnreachable) {
					fmt.Fprintf(os.Stderr, "\nThe peer is offline; your funds were not charged. Try again when it is online.\n")
				}
				if errors.Is(err, kernel.ErrPeerUnfunded) {
					fmt.Fprintf(os.Stderr, "\nYour balance is fine; this kernel's credit with peer %s is exhausted.\nOperator remedy: pay the peer out of band and have its operator run `admin deposit`.\n", peerMetaHandle(err))
				}
				// A pinned run refused for changed terms: nothing was charged, and the current
				// number is what the caller must re-consent to (§4 precondition 7).
				if quoteHash != "" {
					if ke := (*kernel.KernelError)(nil); errors.As(err, &ke) && ke.Meta["quote_hash"] != "" {
						fmt.Fprintf(os.Stderr, "\nNothing was charged. It now costs %s; re-read the action and pin %s to accept.\n", ke.Meta["price"], ke.Meta["quote_hash"])
					}
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
	cmd.Flags().StringVar(&quoteHash, "quote-hash", "", "refuse before charging if the action's terms no longer match this quote")
	return cmd
}
