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
	"github.com/google/uuid"
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
			// A path names bytes, as it does for --source, so encode them. Never branch on whether
			// the bytes parse as UTF-8: a small WASM module is entirely below 0x80 (the header
			// alone is `\0asm\1\0\0\0`), so that test routes one module by its content and the next
			// one differently. Literal base64 is the non-file case below.
			artData = base64.StdEncoding.EncodeToString(data)
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
var interactiveTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// confirm gates an act that cannot be undone. The default is no: a bare Enter on a prompt about
// money should not move it, and the one way to say yes is to say it. Declining is an error, so the
// exit code says so too and a script does not read silence as success. Off a terminal there is
// nobody to ask, so --yes is required rather than assumed.
func confirm(msg string, yes bool) error {
	if yes {
		return nil
	}
	if !interactiveTTY() {
		return kernel.ErrInvalidInput.Wrap("re-run with --yes to confirm (no terminal to ask on)")
	}
	return askYesNo(msg)
}

// askYesNo puts the question to whoever is at the terminal. Callers that have no --yes flag to
// offer — first boot, where consent is a written configuration file rather than a flag — ask with
// this after their own check that somebody is there.
func askYesNo(msg string) error {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if l := strings.ToLower(strings.TrimSpace(line)); l == "y" || l == "yes" {
		return nil
	}
	fmt.Fprintln(os.Stderr, "cancelled")
	return kernel.ErrInvalidInput.Wrap("cancelled")
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
	// --quiet is one rule on every command, reads included (§14 C8): print the resource's id and
	// nothing else, so output pipes into the next command; a response naming no resource prints
	// nothing at all, rather than falling back to the full view the flag exists to suppress.
	if flagQuiet {
		var obj map[string]json.RawMessage
		if json.Unmarshal(b, &obj) == nil {
			var id string
			if raw, ok := obj["id"]; ok && json.Unmarshal(raw, &id) == nil && id != "" {
				fmt.Println(id)
			}
			return nil
		}
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
	userCmd.AddCommand(userCreateCmd(), userMeCmd(), userUpdateCmd(), userTransferCmd(), userLedgerCmd(),
		userConnectCmd(), userDisconnectCmd(), userAddressCmd(), userDepositCmd(), userWithdrawCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "create USER",
		Short: "Create a user account",
		Long:  "Create a user account. USER is a bare handle — letters and digits, no @ or /.\n\nPrints a one-time recovery phrase; write it down. It is the only way to reset a lost\npassword (`juice auth recover`).",
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
			// Enroll a recovery phrase (§12): generated client-side, only the public key is
			// sent — it is the sole recovery credential, and the server never sees it. The
			// ceremony shows and acknowledges the phrase before committing.
			var view json.RawMessage
			if err := enrollRecovery("Recovery phrase", func(recoveryPub string) error {
				return apiCall(context.Background(), "POST", "/v1/users", kernel.CreateUserRequest{
					Handle: user, Password: password, RecoveryPublicKey: recoveryPub,
				}, &view)
			}); err != nil {
				return err
			}
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
	var yes bool
	cmd := &cobra.Command{
		Use:   "transfer RECIPIENT AMOUNT",
		Short: "Send money to another user",
		Long: "Send money to another user, directly and without fee. RECIPIENT is another user's\n" +
			"handle on this kernel (a public key also resolves a local account). AMOUNT is written\n" +
			"the way this kernel's money is written, for example 1.50.\n\n" +
			"A transfer cannot be undone: the recipient owns the money once it is sent.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			net, err := serverNetwork(ctx)
			if err != nil {
				return err
			}
			amount, err := parseAmount(args[1], net.Decimals)
			if err != nil {
				return err
			}
			if err := confirm(fmt.Sprintf("Send %s to %s? This cannot be undone.", net.Amount(amount), args[0]), yes); err != nil {
				return err
			}
			return apiEmitCtx(ctx, "POST", "/v1/transfers", map[string]any{
				"recipient": args[0], "amount": amount, "reason": reason, "external_key": externalKey,
			})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().StringVar(&externalKey, "external-key", "", "Unique id for this transfer; repeating the command with the same id never moves money twice")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
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
			if flagQuiet {
				for _, e := range entries {
					fmt.Println(e.ID)
				}
				return nil
			}
			net, err := serverNetwork(context.Background())
			if err != nil {
				return err
			}
			for _, e := range entries {
				from, to := e.FromHandle, e.ToHandle
				if from == "" {
					from = "—"
				}
				if to == "" {
					to = "—"
				}
				fmt.Printf("[%s] amount:%-10s  from:%-12s  to:%-12s  %s\n",
					e.CreatedAt.Format(time.RFC3339),
					net.Amount(e.Amount), from, to, e.Reason)
			}
			return nil
		},
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// meView is the caller's own record, the only place a client reads its own id and payout address.
type meView struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	RailAddress string `json:"rail_address"`
}

func readMe(ctx context.Context) (*meView, error) {
	var me meView
	if err := apiCall(ctx, "GET", "/v1/me", nil, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// userAddressCmd registers where the caller is paid. The kernel credits money to whoever finally
// sent it, so an account is paid out only to an address its holder has proved is theirs: the proof
// is a signature over a message naming this kernel, this account, and that address, and nothing
// else. The signing happens in the wallet, not here — this command composes the message and takes
// the signature back.
func userAddressCmd() *cobra.Command {
	var signature string
	cmd := &cobra.Command{
		Use:   "address [ADDRESS]",
		Short: "Show or register the address you are paid at",
		Long: "Show or register the address you are paid at. With no arguments, shows the address\n" +
			"registered for your account.\n\n" +
			"ADDRESS registers that address. It is yours only once you prove it: this command prints a\n" +
			"message naming this kernel, your account, and the address; sign that message with the\n" +
			"wallet that holds the address and paste the signature back, or pass it with --signature.\n" +
			"Registering also credits you for payments already received from that address.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			me, err := readMe(ctx)
			if err != nil {
				return err
			}
			if len(args) == 0 {
				if me.RailAddress == "" {
					fmt.Println("No address registered.")
					return nil
				}
				fmt.Println(me.RailAddress)
				return nil
			}
			h, err := probeHealth(ctx, serverBaseURL())
			if err != nil {
				return err
			}
			if signature == "" {
				if !interactiveTTY() {
					return kernel.ErrInvalidInput.Wrap("--signature is required when nobody is at the terminal to sign")
				}
				fmt.Fprintf(os.Stderr, "Sign this message with the wallet holding %s:\n\n%s\n\n",
					args[0], string(kernel.RailAddressMessage(h.PublicKey, me.ID, args[0])))
				fmt.Fprint(os.Stderr, "Signature: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				signature = strings.TrimSpace(line)
			}
			return apiEmitCtx(ctx, "PUT", "/v1/me/address", map[string]any{
				"address": args[0], "signature": signature,
			})
		},
	}
	cmd.Flags().StringVar(&signature, "signature", "", "Signature of the registration message, produced by the wallet holding the address")
	return cmd
}

// userDepositCmd answers "how do I put money in?" and writes nothing. The answer depends on the
// world this kernel serves, so it is composed from what the server says about itself and about the
// caller rather than from anything stored here.
func userDepositCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deposit",
		Short: "Show how to put money into your account",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			ctx := context.Background()
			h, err := probeHealth(ctx, serverBaseURL())
			if err != nil {
				return err
			}
			me, err := readMe(ctx)
			if err != nil {
				return err
			}
			if h.RailAddress == "" {
				fmt.Printf("Money on the %s network has no addresses to send to.\n", h.Network)
				fmt.Println("The operator of this kernel records payments here; there is nothing to send from your side.")
				return nil
			}
			fmt.Printf("Send %s to this kernel at:\n  %s\n\n", h.Network, h.RailAddress)
			if me.RailAddress == "" {
				fmt.Println("You have no address registered, so a payment from you cannot be recognized as yours.")
				fmt.Println("Register the address you will pay from first:  juice user address ADDRESS")
				return nil
			}
			fmt.Printf("Pay from your registered address:\n  %s\n\n", me.RailAddress)
			fmt.Println("Money is credited to whoever finally sent it, so it must arrive from that address.")
			fmt.Println("An exchange paying this kernel on your behalf would be crediting itself, not you:")
			fmt.Println("withdraw to your own wallet first, then pay from there.")
			return nil
		},
	}
}

// userWithdrawCmd sends the caller's own credits back out, and bare lists what they have sent.
// The id is minted here and is the row's own, so a reply lost in transit is safe to ask for again.
func userWithdrawCmd() *cobra.Command {
	var reason string
	var yes bool
	cmd := &cobra.Command{
		Use:   "withdraw [AMOUNT]",
		Short: "Withdraw your credits, or list your withdrawals",
		Long: "Withdraw your credits to the address you registered with `juice user address`. With no\n" +
			"arguments, lists the withdrawals you have made and where each stands.\n\n" +
			"A withdrawal fixes its destination when it is made, so registering another address later\n" +
			"never redirects one already under way.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			if len(args) == 0 {
				return apiEmitCtx(ctx, "GET", "/v1/withdrawals", nil)
			}
			net, err := serverNetwork(ctx)
			if err != nil {
				return err
			}
			amount, err := parseAmount(args[0], net.Decimals)
			if err != nil {
				return err
			}
			me, err := readMe(ctx)
			if err != nil {
				return err
			}
			// Name the destination when there is one. Whether this world needs one is the rail's rule,
			// not the client's, so a withdrawal with nowhere to go is refused by the server that knows.
			where := ""
			if me.RailAddress != "" {
				where = " to " + me.RailAddress
			}
			if err := confirm(fmt.Sprintf("Withdraw %s on %s%s? This cannot be undone.",
				net.Amount(amount), net.Name, where), yes); err != nil {
				return err
			}
			return apiEmitCtx(ctx, "POST", "/v1/withdrawals", map[string]any{
				"id": uuid.NewString(), "amount": amount, "reason": reason,
			})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
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
		actionStatsCmd(),
		actionRatingsCmd(),
	)
	rootCmd.AddCommand(actionCmd)
}

// Shared placeholder definitions for the action commands' help.
const (
	actionPathHelp = "ACTION is an action id or owner/name; owner/path also matches every action beneath\nthat path (bob/mail covers bob/mail/send, never bob/mailer)."
	actionRefHelp  = "ACTION is owner/name on this kernel, owner@kernel/name on a peer (kernel = its local\nname or public key), or a raw action id."
)

func actionCreateCmd() *cobra.Command {
	var kind, source, description, artifact, method string
	var params []string
	var price int64
	var inputSchemaStr, outputSchemaStr, authStr string
	cmd := &cobra.Command{
		Use:   "create NAME",
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
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http or wasm")
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
		Use:   "update ACTION|PATH",
		Short: "Update an action or a path",
		Long:  "Update an action or a path.\n\n" + actionPathHelp + "\n\nVisibility, price, and auth may target a whole path; a description, schema, or source\nneeds a target naming exactly one action. Changing source, schema, or price disables the\naction until re-enabled.",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
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
			return apiEmit("PUT", "/v1/actions", targetRequest{Target: args[0], UpdateActionRequest: req})
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
	return actionActiveCmd("enable ACTION|PATH", "Enable an action or a path", "enable", "enabled", true)
}

func actionDisableCmd() *cobra.Command {
	return actionActiveCmd("disable ACTION|PATH", "Disable an action or a path", "disable", "disabled", false)
}

// targetRequest addresses a mutation: an action id names exactly one row, an owner/path names the
// action at that path and everything beneath it. The CLI passes what the user typed, unexamined —
// the two shapes are disjoint, and the server owns the resolution (§14).
type targetRequest struct {
	Target string `json:"target"`
	kernel.UpdateActionRequest
}

// reportActions prints the rows a mutation touched, named rather than counted.
func reportActions(as []actionResp, verb string) error {
	if flagJSON {
		return printJSON(as)
	}
	for _, a := range as {
		fmt.Printf("%s %s\n", verb, a.ActionRef)
	}
	return nil
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
		Long:  short + ".\n\n" + actionPathHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			var as []actionResp
			if err := apiCall(context.Background(), "POST", "/v1/actions/"+suffix,
				map[string]string{"target": args[0]}, &as); err != nil {
				return err
			}
			return reportActions(as, pastTense)
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
	var owner, name string
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
			if owner != "" {
				q.Set("owner", owner)
			}
			if name != "" {
				q.Set("name", name)
			}
			var actions []actionResp
			if err := apiCall(ctx, "GET", "/v1/actions?"+q.Encode(), nil, &actions); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(actions)
			}
			net, err := serverNetwork(context.Background())
			if err != nil {
				return err
			}
			for _, a := range actions {
				if flagQuiet {
					fmt.Println(a.ID) // ids only, one per line: pipeable (§14 C8)
					continue
				}
				grant := ""
				if a.RequiresGrant {
					grant = " [grant]" // caller must connect their own credential first (§8)
				}
				// The ref already contains the name; one padded reference column in both branches.
				if all {
					active := " "
					if a.Active {
						active = "*"
					}
					fmt.Printf("[%s] %-30s  %s%s\n", active, a.ActionRef, net.Amount(a.Price), grant)
				} else {
					fmt.Printf("  %-30s  %s%s\n", a.ActionRef, net.Amount(a.Price), grant)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include inactive and private actions (a superuser sees every owner's)")
	cmd.Flags().StringVar(&owner, "owner", "", "Only actions owned by this handle")
	cmd.Flags().StringVar(&name, "name", "", "Only actions with this name")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func actionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ACTION",
		Short: "Show action details",
		Long:  "Show action details.\n\n" + actionRefHelp,
		Args:  cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			return apiEmit("GET", "/v1/actions/"+id, nil)
		}),
	}
}

func actionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete ACTION|PATH",
		Short: "Delete an action or a path, keeping history",
		Long:  "Delete an action or a path; transactions, receipts, and ratings survive.\n\n" + actionPathHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			q := url.Values{"target": {args[0]}}
			var as []actionResp
			if err := apiCall(context.Background(), "DELETE", "/v1/actions?"+q.Encode(), nil, &as); err != nil {
				return err
			}
			return reportActions(as, "deleted")
		},
	}
}

func actionImportCmd() *cobra.Command {
	var authStr string
	cmd := &cobra.Command{
		Use:   "import NAME [SPEC_URL]",
		Short: "Import an OpenAPI document as one application",
		Long: "Import one OpenAPI document as the application at NAME, one action per operation.\n" +
			"SPEC_URL is the http(s) address of the document; give it on the first import — later\n" +
			"imports reuse the recorded one and reconcile changes, keeping each action's id,\n" +
			"history, credentials, and any price you set yourself.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			body := map[string]any{"name": args[0]}
			if len(args) == 2 {
				body["spec_url"] = args[1]
			}
			if c.Flags().Changed("auth") {
				auth := &kernel.AuthInput{}
				if err := unmarshalJSONArg(authStr, auth); err != nil {
					return kernel.ErrInvalidInput.Wrapf("invalid --auth: %v", err)
				}
				body["auth"] = auth
			}
			var result importResp
			if err := apiCall(context.Background(), "POST", "/v1/actions/import", body, &result); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(result)
			}
			var counts []string
			for _, group := range []struct {
				verb string
				rows []actionResp
			}{
				{"imported", result.Created},
				{"updated", result.Updated},
				{"unchanged", result.Unchanged},
				{"deactivated (no longer in the document)", result.Deactivated},
			} {
				for _, a := range group.rows {
					fmt.Printf("%s %s\n", group.verb, a.Name)
				}
				if len(group.rows) > 0 {
					counts = append(counts, fmt.Sprintf("%d %s", len(group.rows), group.verb))
				}
			}
			for _, r := range result.Rejected {
				fmt.Printf("skipped %s: %s\n", r.Key, r.Reason)
			}
			if len(result.Rejected) > 0 {
				counts = append(counts, fmt.Sprintf("%d skipped", len(result.Rejected)))
			}
			if len(counts) == 0 {
				fmt.Printf("%s: nothing to do.\n", args[0])
				return nil
			}
			fmt.Printf("%s: %s.\n", args[0], strings.Join(counts, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json), applied to every operation")
	return cmd
}

func actionStatsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stats ACTION",
		Short: "Show an action's statistics",
		Long:  "Show an action's statistics.\n\n" + actionRefHelp,
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
		Use:   "ratings ACTION",
		Short: "Show an action's public ratings",
		Long:  "Show an action's public ratings, one per line: value (0 bad, 1 good), date, note.\n\n" + actionRefHelp,
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
				Source  string  `json:"source"`
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
			if flagQuiet {
				for _, p := range processes {
					fmt.Println(p.ID)
				}
				return nil
			}
			net, err := serverNetwork(context.Background())
			if err != nil {
				return err
			}
			for _, p := range processes {
				awaiting := ""
				if p.AwaitingReceipt && p.AwaitingReceiptSince != nil {
					awaiting = fmt.Sprintf("  awaiting-receipt since %s", p.AwaitingReceiptSince.Format(time.RFC3339))
				}
				fmt.Printf("%s  %-6s  available:%-12s  locked:%-12s%s\n",
					p.ID, p.Status, net.Amount(p.Available), net.Amount(p.Locked), awaiting)
			}
			return nil
		},
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func processEndCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "end ID",
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
		Use:   "show ID",
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
		Use:   "create ACTION",
		Short: "Create a step",
		Long:  "Create a step: a prepaid continuation of a running call, addressed to one user who\nlater completes it with `step complete`. The step's price is reserved now, so completion\nneeds no further funds.\n\n" + actionRefHelp,
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
	cmd.Flags().StringVar(&traceID, "trace", "", "Id of the funding call (the trace_id returned by run), whose budget pays for the step (required)")
	cmd.Flags().StringVar(&requiredCaller, "required-caller", "", "User who must complete the step (required)")
	cmd.Flags().StringVar(&partialArgs, "partial-args", "", "Partial args as JSON object")
	_ = cmd.MarkFlagRequired("trace")
	_ = cmd.MarkFlagRequired("required-caller")
	return cmd
}

func stepListCmd() *cobra.Command {
	var processID, status, peer string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List steps",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			// A peer holds the step and answers for it, so the reply carries only what it may
			// disclose: the id to complete, what is already filled in, and what you may supply.
			if peer != "" {
				if cmd.Flags().Changed("process") || cmd.Flags().Changed("status") ||
					cmd.Flags().Changed("limit") || cmd.Flags().Changed("offset") {
					return fmt.Errorf("--peer lists the one page of steps a peer holds for you; it takes no filter or paging flag")
				}
				q.Set("peer", peer)
				var held kernel.PeerStepList
				if err := apiCall(context.Background(), "GET", "/v1/steps?"+q.Encode(), nil, &held); err != nil {
					return err
				}
				if flagJSON {
					return printJSON(held)
				}
				for _, h := range held.Steps {
					fmt.Printf("%s  price=%d  %s\n", h.ID, h.Price, h.CreatedAt.Format(time.RFC3339))
					if len(h.PartialArgs) > 0 && string(h.PartialArgs) != "{}" {
						fmt.Printf("      %s\n", h.PartialArgs)
					}
				}
				if held.Truncated {
					fmt.Println("more steps are waiting than one page carries; complete some and ask again")
				}
				return nil
			}
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
			if flagQuiet {
				for _, s := range steps {
					fmt.Println(s.ID)
				}
				return nil
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
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done, cancelled)")
	cmd.Flags().StringVar(&peer, "peer", "", "List steps this peer (handle or key) is holding for you, over federation")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func stepShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
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
		Use:   "complete ID [JSON]",
		Short: "Complete a waiting step",
		Long:  "Complete a waiting step addressed to you, supplying what is missing.\n\n[JSON] is the completion input as a JSON object, default {}; @file.json reads it from a\nfile. `step show` lists the fields still expected under allowed_input.",
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
			// Decoded into the server's own view so --json relays it faithfully: the kernel type
			// has no handle fields, and re-encoding through it would resurrect the raw user ids.
			var txs []*txSummary
			if err := apiCall(context.Background(), "GET", "/v1/transactions?"+q.Encode(), nil, &txs); err != nil {
				return err
			}
			if flagJSON {
				return printJSON(txs)
			}
			if flagQuiet {
				for _, tx := range txs {
					fmt.Println(tx.ID)
				}
				return nil
			}
			net, err := serverNetwork(context.Background())
			if err != nil {
				return err
			}
			for _, tx := range txs {
				fmt.Printf("[%s] %s  status:%s  gross:%s\n",
					tx.StartedAt.Format(time.RFC3339),
					tx.ID, tx.Status, net.Amount(tx.Gross))
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
		Use:   "show ID",
		Short: "Show transaction details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/v1/transactions/"+args[0], nil)
		},
	}
}

func txVerifyReceiptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify ID",
		Short: "Verify a transaction's signed receipt offline",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return apiEmit("GET", "/v1/transactions/"+args[0]+"/receipt-verification", nil)
		},
	}
}

func txRateCmd() *cobra.Command {
	var note string
	cmd := &cobra.Command{
		Use:   "rate ID 0|1",
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
		Use:   "run ACTION [JSON]",
		Short: "Run an action",
		Long:  "Run an action and print its result. The advertised price is the most the whole call can\ncost you; a failed call refunds what was not consumed.\n\n" + actionRefHelp + "\n\n[JSON] is the arguments as a JSON object, default {}; @file.json reads it from a file.",
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
				if interactiveTTY() && confirm(fmt.Sprintf("This action needs your authorization. Authorize %s now?", action), false) == nil {
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
				// A parked remote call: the money is reserved, not spent, and the process is the
				// handle to follow it by (§13). Say so — a bare "pending" reads as a lost charge.
				if ke := (*kernel.KernelError)(nil); errors.As(err, &ke) && ke.Meta["process_id"] != "" {
					fmt.Fprintf(os.Stderr, "\nYour funds are reserved, not spent, on process %s.\nFollow it with:\n  juice process show %s\n", ke.Meta["process_id"], ke.Meta["process_id"])
					if at := ke.Meta["refund_eligible_at"]; at != "" {
						fmt.Fprintf(os.Stderr, "It retries automatically. From %s it becomes eligible for an automatic refund, which a later retry pass applies; `juice process end` refunds it sooner.\n", at)
					} else {
						fmt.Fprintln(os.Stderr, "Work is still running beneath the call; the receipt follows when it settles. `juice process end` settles it now.")
					}
				}
				if errors.Is(err, kernel.ErrPeerUnfunded) {
					fmt.Fprintf(os.Stderr, "\nYour balance is fine; this kernel's credit with peer %s is exhausted.\nOperator remedy: pay the peer out of band and have its operator run `admin deposit`.\n", peerMetaHandle(err))
				}
				// A pinned run refused for changed terms: nothing was charged, and the current
				// number is what the caller must re-consent to (§4 precondition 7).
				if quoteHash != "" {
					if ke := (*kernel.KernelError)(nil); errors.As(err, &ke) && ke.Meta["quote_hash"] != "" {
						fmt.Fprintf(os.Stderr, "\nNothing was charged. The action's terms changed since you quoted them; its price is now %s.\nRe-read the action and pass --quote-hash %s to accept the new terms.\n", ke.Meta["price"], ke.Meta["quote_hash"])
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
	cmd.Flags().StringVar(&quoteHash, "quote-hash", "", "Fingerprint of the terms you saw (quote_hash on the action); the run is refused before any charge if the terms have changed since")
	return cmd
}
