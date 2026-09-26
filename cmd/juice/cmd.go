// SPDX-License-Identifier: AGPL-3.0-only

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
	"sync"
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

// directorySelector turns an action address (owner@kernel/name) into the selector that connects its whole
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

// interactiveTTY reports whether a human is driving: stdin readable and stderr a terminal.
// Prompts and progress go to stderr so stdout stays payload-only (§14).
var interactiveTTY = func() bool {
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// errCancelled is a user who said no. It is not a failure of the command and is not rendered as
// one — the exit code is non-zero so a script does not read silence as success, and the word is
// printed once, plainly (§14).
var errCancelled = errors.New("cancelled")

// confirm gates an act that cannot be undone. The default is no: a bare Enter on a prompt about
// money should not move it, and the one way to say yes is to say it. Off a terminal there is
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
	line, _ := promptLine()
	if l := strings.ToLower(strings.TrimSpace(line)); l == "y" || l == "yes" {
		return nil
	}
	return errCancelled
}

// promptLine reads one answer from whoever is at the terminal, through ONE buffered reader per
// input. A reader built per question reads ahead and swallows the answers behind it, so a command
// that asks twice — a price, then the authorization that price turns out to need — would see the
// second question answered by nobody.
var (
	promptMu     sync.Mutex
	promptSource *os.File
	promptReader *bufio.Reader
)

func promptLine() (string, error) {
	promptMu.Lock()
	defer promptMu.Unlock()
	if promptReader == nil || promptSource != os.Stdin {
		promptSource, promptReader = os.Stdin, bufio.NewReader(os.Stdin)
	}
	return promptReader.ReadString('\n')
}

// ---- output helpers ----

// printJSONBytes indents already-marshaled JSON in place. json.Indent preserves the
// source field order (unlike unmarshal-then-MarshalIndent, which would alphabetize map
// keys), so server responses print in their declared order.
func printJSONBytes(b []byte) error {
	if len(b) == 0 {
		return nil // a mutation that returns no resource has nothing to print
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, b, "", "  "); err != nil {
		fmt.Println(string(b)) // not an object/array; print verbatim
		return nil
	}
	fmt.Println(buf.String())
	return nil
}

// output is what one command adds to the single output policy: the field --quiet prints ("id"
// when empty), the top-level fields the field view writes as money, and the command's own human
// rendering (the field view when nil). A command that renders its own view formats its own money,
// so money and human are never both set.
type output struct {
	id    string
	rows  string // the field holding this reply's resources, when a reply wraps them in one
	money []string
	human func(body []byte) error
	net   kernel.Network // the world's unit, read before the request by units()
}

// units reads the world's money unit for a field view that will write money for a person. It runs
// before the request, never while printing its reply: a unit that cannot be read must refuse
// before anything is sent, rather than report "nothing was sent" about a write that committed.
// --json and --quiet carry base units, so they read nothing. emitCtx calls this for every request
// with the client that will make it; a command that emits without going through it calls this
// itself, or its money prints in whatever unit an unread world has.
func (o *output) units(ctx context.Context, c *client) error {
	if len(o.money) == 0 || flagJSON || flagQuiet {
		return nil
	}
	net, err := c.network(ctx)
	if err != nil {
		return err
	}
	o.net = net
	return nil
}

// humanUnits reads the world's money unit for a command that renders its own view. Under --json
// and --quiet it reads nothing and answers the zero world: those carry base units, and a read that
// prints no money must not turn a working reply into a failed /health.
func humanUnits(ctx context.Context) (kernel.Network, error) {
	if flagJSON || flagQuiet {
		return kernel.Network{}, nil
	}
	return cli.network(ctx)
}

// emit is the one output policy every command ends in (§14 C8): --json prints the server's body
// exactly as it arrived, --quiet prints the id of each resource one per line, and otherwise the
// command renders it — by default the field view, which shows every field the response carries.
func emit(body []byte, o output) error {
	body = bytes.TrimSpace(body)
	switch {
	case flagJSON:
		return printJSONBytes(body)
	case flagQuiet:
		return printIDs(o.resources(body), o.idField())
	case o.human != nil:
		return o.human(body)
	}
	return printFields(body, o.money, o.net)
}

func (o output) idField() string {
	if o.id == "" {
		return "id"
	}
	return o.id
}

// resources is the part of a reply that holds the resources it names. Most replies are a resource
// or a list of them and are their own; one that wraps a list beside something else — a page of a
// peer's steps beside whether more are waiting — names the field, rather than having every reply
// searched for one.
func (o output) resources(body []byte) []byte {
	if o.rows == "" {
		return body
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}
	return fields[o.rows]
}

// ---- the list view ----
//
// Every list a person reads is one shape: a header naming the columns, one row per item, aligned,
// and the same empty state — the header alone — whether the list is empty because nothing exists
// or because nothing matched. A command declares its columns; nothing else about a list is a
// command's to decide (§14).

// column is one column of a list: its heading, and what it reads from a row.
type column struct {
	head string
	cell func(row json.RawMessage) string
}

// text reads one field of a row as it stands.
func text(field string) func(json.RawMessage) string {
	return func(row json.RawMessage) string { return strField(row, field) }
}

// money reads one field of a row as an amount, in the unit a person reads.
func money(field string, net kernel.Network) func(json.RawMessage) string {
	return func(row json.RawMessage) string {
		var fields map[string]json.RawMessage
		if json.Unmarshal(row, &fields) != nil {
			return ""
		}
		var amount int64
		if json.Unmarshal(fields[field], &amount) != nil {
			return ""
		}
		return net.Amount(amount)
	}
}

// strField reads one string field of a row, rendering a non-string as it stands.
func strField(row json.RawMessage, field string) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(row, &fields) != nil {
		return ""
	}
	raw, ok := fields[field]
	if !ok {
		return ""
	}
	return renderValue(raw)
}

// list renders a reply of rows as the list view: the columns as declared, widths from the content,
// header always. rows() names the rows inside a reply that wraps them; nil means the reply is them.
func list(cols ...column) func(b []byte) error {
	return func(b []byte) error {
		var rows []json.RawMessage
		if len(bytes.TrimSpace(b)) > 0 && json.Unmarshal(b, &rows) != nil {
			var one json.RawMessage = b
			rows = []json.RawMessage{one}
		}
		cells := make([][]string, 0, len(rows))
		widths := make([]int, len(cols))
		for i, c := range cols {
			widths[i] = len(c.head)
		}
		for _, row := range rows {
			line := make([]string, len(cols))
			for i, c := range cols {
				line[i] = c.cell(row)
				if len(line[i]) > widths[i] {
					widths[i] = len(line[i])
				}
			}
			cells = append(cells, line)
		}
		print := func(vals []string) {
			var sb strings.Builder
			for i, v := range vals {
				if i > 0 {
					sb.WriteString("  ")
				}
				if i == len(vals)-1 {
					sb.WriteString(v)
					continue
				}
				sb.WriteString(v + strings.Repeat(" ", widths[i]-len(v)))
			}
			fmt.Println(strings.TrimRight(sb.String(), " "))
		}
		heads := make([]string, len(cols))
		for i, c := range cols {
			heads[i] = c.head
		}
		print(heads)
		for _, line := range cells {
			print(line)
		}
		return nil
	}
}

// printIDs prints the identifier of every resource a response names, one per line, so output
// pipes into the next command: a list yields one line per row, a single resource one line, and a
// response naming no resource nothing at all (§14 C8).
func printIDs(body []byte, field string) error {
	var rows []map[string]json.RawMessage
	if json.Unmarshal(body, &rows) != nil {
		var one map[string]json.RawMessage
		if json.Unmarshal(body, &one) != nil {
			return nil
		}
		rows = []map[string]json.RawMessage{one}
	}
	for _, row := range rows {
		var id string
		if json.Unmarshal(row[field], &id) == nil && id != "" {
			fmt.Println(id)
		}
	}
	return nil
}

// printFields renders a response as one "key: value" line per top-level field, in the order the
// server sent them — so the text view can never silently drop a field the HTTP response carries
// (CLI/HTTP parity, §14). The fields named as money are written the way this kernel writes money
// (D20); every other value prints as it arrived, because an action's own arguments and results
// ride inside these responses and are never reinterpreted.
func printFields(body []byte, money []string, net kernel.Network) error {
	if len(body) == 0 {
		return nil
	}
	// A list is its rows, one field view each: a reply of several resources reads like a reply of
	// one, money included. Anything else — a scalar, or a document that is not resources at all —
	// has no labeled fields and prints as JSON.
	if body[0] == '[' {
		var rows []json.RawMessage
		if json.Unmarshal(body, &rows) != nil {
			return printJSONBytes(body)
		}
		for i, row := range rows {
			if i > 0 {
				fmt.Println()
			}
			if err := printFields(row, money, net); err != nil {
				return err
			}
		}
		return nil
	}
	if body[0] != '{' {
		return printJSONBytes(body)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil { // the opening '{'
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
		fmt.Printf("  %s: %s\n", key, renderField(key, raw, money, net))
	}
	return nil
}

// renderField formats one field: money in the world's unit, a duration with the unit it is in, and
// everything else as renderValue does. A bare number a person cannot interpret is not an answer.
func renderField(key string, raw json.RawMessage, money []string, net kernel.Network) string {
	for _, m := range money {
		var amount int64
		if m == key && json.Unmarshal(raw, &amount) == nil {
			return net.Amount(amount)
		}
	}
	if strings.HasSuffix(key, "latency_estimate") {
		var seconds float64
		if json.Unmarshal(raw, &seconds) == nil {
			return fmt.Sprintf("%.3f seconds", seconds)
		}
	}
	return renderValue(raw)
}

// The money in each response the field view renders (D20). Named once because several responses
// carry the same fields, and never guessed from a field's name at print time.
var (
	moneyAccount = []string{"available", "locked"}
	moneyAction  = []string{"price", "base_price"}
	moneyTx      = []string{"gross", "net", "fee", "refund"}
	moneyRail    = []string{"amount", "credit"}
	moneyLedger  = []string{"amount"}
	moneyStep    = []string{"price"}
	moneyCall    = []string{"charge"}
	moneyOwed    = []string{"obligation", "amount"}
)

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
		userConnectCmd(), userDisconnectCmd(), userBlockchainAddressCmd(), userDepositCmd(),
		userWithdrawCmd(), userWithdrawalsCmd())
	rootCmd.AddCommand(userCmd)
}

func userCreateCmd() *cobra.Command {
	var password string
	cmd := &cobra.Command{
		Use:   "create USER@KERNEL",
		Short: "Create a user account on a kernel",
		Long: "Create the account USER on KERNEL, which is a kernel this client knows (`juice kernel " +
			"list` shows them). Creating an account does not log you in: `juice auth login USER@KERNEL` " +
			"does that.\n\n" +
			"Prints a one-time recovery phrase; write it down. It is the only way to reset a lost " +
			"password (`juice auth recover`).",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			l, c, err := namedClient(args[0])
			if err != nil {
				return err
			}
			user := l.String()
			if password == "" {
				p, err := promptNewPassword("Password: ")
				if err != nil {
					return err
				}
				password = p
			}
			ctx := context.Background()
			shown := output{money: moneyAccount}
			if err := shown.units(ctx, c); err != nil {
				return err
			}
			// Enroll a recovery phrase (§12): generated client-side, only the public key is
			// sent — it is the sole recovery credential, and the server never sees it. The
			// ceremony shows and acknowledges the phrase before committing.
			var created json.RawMessage
			if err := enrollRecovery("Recovery phrase", func(recoveryPub string) error {
				return c.call(ctx, "POST", "/v1/users", kernel.CreateUserRequest{
					Handle: user, Password: password, RecoveryPublicKey: recoveryPub,
				}, &created)
			}); err != nil {
				return err
			}
			return emit(created, shown)
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
			return cli.emit("GET", "/v1/me", nil, output{money: moneyAccount})
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
			return cli.emit("PUT", "/v1/me", req, output{money: moneyAccount})
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
		Long: "Send money to another user, directly and without fee. RECIPIENT is another user's " +
			"handle on this kernel (a public key also resolves a local account). AMOUNT is written " +
			"the way this kernel's money is written, for example 1.50.\n\n" +
			"A transfer cannot be undone: the recipient owns the money once it is sent.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			net, err := cli.network(ctx)
			if err != nil {
				return err
			}
			amount, err := parseAmount(args[1], net.Decimals)
			if err != nil {
				return err
			}
			if err := cli.confirm(fmt.Sprintf("Send %s to %s", net.Amount(amount), args[0]), yes); err != nil {
				return err
			}
			return cli.emitCtx(ctx, "POST", "/v1/transfers", map[string]any{
				"recipient": args[0], "amount": amount, "reason": reason, "external_key": externalKey,
			}, output{money: moneyLedger})
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
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			return cli.emitCtx(ctx, "GET", "/v1/ledger?"+q.Encode(), nil, output{human: list(
				column{"WHEN", text("created_at")},
				column{"AMOUNT", money("amount", net)},
				column{"FROM", party("from")},
				column{"TO", party("to")},
				column{"WHY", text("reason")},
			)})
		},
	}
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

// meView is the caller's own record, the only place a client reads its own id and payout address.
type meView struct {
	ID                string `json:"id"`
	Address           string `json:"address"`
	BlockchainAddress string `json:"blockchain_address"`
	Available         int64  `json:"available"`
}

func readMe(ctx context.Context) (*meView, error) {
	var me meView
	if err := cli.call(ctx, "GET", "/v1/me", nil, &me); err != nil {
		return nil, err
	}
	return &me, nil
}

// userBlockchainAddressCmd registers where the caller is paid. The kernel credits money to whoever finally
// sent it, so an account is paid out only to an address its holder has proved is theirs: the proof
// is a signature over a message naming this kernel, this account, and that address, and nothing
// else. The signing happens in the wallet, not here — this command composes the message and takes
// the signature back.
func userBlockchainAddressCmd() *cobra.Command {
	var signature string
	cmd := &cobra.Command{
		Use:   "blockchain-address ADDRESS",
		Short: "Register the blockchain address you pay from and are paid at",
		Long: "Register ADDRESS as the blockchain address you pay from and are paid at; `juice user me` shows the one " +
			"registered.\n\n" +
			"It is yours only once you prove it: this command prints a message naming this kernel, " +
			"your account, and the address; sign that message with the wallet that holds the address " +
			"and paste the signature back, or pass it with --signature. Registering also credits you " +
			"for payments already received from that address.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			me, err := readMe(ctx)
			if err != nil {
				return err
			}
			h, err := cli.banner(ctx)
			if err != nil {
				return err
			}
			if signature == "" {
				if !interactiveTTY() {
					return kernel.ErrInvalidInput.Wrap("--signature is required when nobody is at the terminal to sign")
				}
				fmt.Fprintf(os.Stderr, "Sign this message with the wallet holding %s:\n\n%s\n\n",
					args[0], string(kernel.BlockchainAddressMessage(h.PublicKey, me.ID, args[0])))
				fmt.Fprint(os.Stderr, "Signature: ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				signature = strings.TrimSpace(line)
			}
			return cli.emitCtx(ctx, "PUT", "/v1/me/blockchain-address", map[string]any{
				"blockchain_address": args[0], "signature": signature,
			}, output{id: "blockchain_address"})
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
			h, err := cli.banner(ctx)
			if err != nil {
				return err
			}
			me, err := readMe(ctx)
			if err != nil {
				return err
			}
			facts, err := json.Marshal(map[string]string{
				"network": h.Network, "kernel_address": h.BlockchainAddress, "your_address": me.BlockchainAddress,
				"token": h.Token})
			if err != nil {
				return err
			}
			return emit(facts, output{human: func([]byte) error {
				if h.BlockchainAddress == "" {
					fmt.Printf("Money on the %s network has no addresses to send to.\n", h.Network)
					fmt.Println("The operator of this kernel records payments here; there is nothing to send from your side.")
					return nil
				}
				fmt.Printf("Send %s to this kernel at:\n  %s\n\n", h.Network, h.BlockchainAddress)
				// The contract, not the symbol, is what says which money this is: one chain carries
				// several tokens called the same thing, and a payment in the wrong one is never
				// credited. An older kernel does not publish it, and a blank line under an
				// instruction to send money would be worse than none.
				if h.Token != "" {
					fmt.Printf("Send only this token, and nothing else:\n  %s", h.Token)
					if h.Symbol != "" {
						fmt.Printf("  (%s)", h.Symbol)
					}
					fmt.Print("\n\n")
				} else {
					fmt.Println("This kernel does not say which token it takes; its symbol alone does not name one.")
					fmt.Println("Ask the operator for the exact contract address before sending anything.")
					fmt.Println()
				}
				if me.BlockchainAddress == "" {
					fmt.Println("You have no address registered, so a payment from you cannot be recognized as yours.")
					fmt.Println("Register the address you will pay from first:  juice user address ADDRESS")
					return nil
				}
				fmt.Printf("Pay from your registered address:\n  %s\n\n", me.BlockchainAddress)
				fmt.Println("Money is credited to whoever finally sent it, so it must arrive from that address.")
				fmt.Println("A payment from any other address, an exchange paying on your behalf included, is held")
				fmt.Println("for the operator to assign by hand: withdraw to your own wallet first, then pay from there.")
				return nil
			}})
		},
	}
}

// userWithdrawCmd sends the caller's own credits back out, and bare lists what they have sent.
// The id names the withdrawal on the server, which returns the row it already made rather than
// making a second one — so a caller who repeats the command with the same id after a lost reply
// recovers the first withdrawal instead of sending twice (U51).
func userWithdrawCmd() *cobra.Command {
	var reason, id string
	var yes bool
	cmd := &cobra.Command{
		Use:   "withdraw AMOUNT",
		Short: "Withdraw your credits",
		Long: "Withdraw AMOUNT to the address you registered with `juice user address`; `juice user " +
			"withdrawals` lists the ones you have made and where each stands.\n\n" +
			"A withdrawal fixes its destination when it is made, so registering another address later " +
			"never redirects one already under way.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			ctx := context.Background()
			if id == "" {
				id = uuid.NewString()
			}
			net, err := cli.network(ctx)
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
			if me.BlockchainAddress != "" {
				where = " to " + me.BlockchainAddress
			}
			if err := cli.confirm(fmt.Sprintf("Withdraw %s on %s%s", net.Amount(amount), net.Name, where), yes); err != nil {
				return err
			}
			return cli.emitCtx(ctx, "POST", "/v1/withdrawals", map[string]any{
				"id": id, "amount": amount, "reason": reason,
			}, output{money: moneyRail})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	cmd.Flags().StringVar(&reason, "reason", "", "Optional reason for audit")
	cmd.Flags().StringVar(&id, "id", "", "An identifier for this withdrawal. Running the command again with the same --id does not withdraw twice. Chosen for you if omitted.")
	return cmd
}

// userWithdrawalsCmd lists what the caller has sent out and where each stands. A verb that reads
// and a verb that moves money are two words, never one word with and without an argument.
func userWithdrawalsCmd() *cobra.Command {
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "withdrawals",
		Short: "List the withdrawals you have made",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			return cli.emitCtx(context.Background(), "GET", "/v1/withdrawals?"+q.Encode(), nil,
				output{money: moneyRail})
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
		actionRatingsCmd(),
	)
	rootCmd.AddCommand(actionCmd)
}

// priceIn reads a price the way every other money input is written: in the world's own unit, so
// what a command takes is what it shows (D20). A price may be nothing, which is the one way it
// differs from an amount to move; a command that was given no price at all does not call here.
func priceIn(price string) (int64, error) {
	net, err := cli.network(context.Background())
	if err != nil {
		return 0, err
	}
	return parseUnits(price, net.Decimals)
}

// Shared placeholder definitions for the action commands' help.
const (
	actionPathHelp = "ACTION is an action id or owner@kernel/name; owner@kernel/path also matches every action beneath that path (bob@acme/mail covers bob/mail/send, never bob/mailer)."
	actionRefHelp  = "ACTION is an address, owner@kernel/name, or a raw action id. kernel is the kernel's name as this kernel knows it — its own name for its own actions, a peer's local name or public key for a peer's."
)

func actionCreateCmd() *cobra.Command {
	var kind, source, description, artifact, method, price string
	var params []string
	var inputSchemaStr, outputSchemaStr, authStr string
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create an action",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
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
			var amount int64
			if c.Flags().Changed("price") {
				if amount, err = priceIn(price); err != nil {
					return err
				}
			}
			return cli.emit("POST", "/v1/actions", kernel.CreateActionRequest{
				Name: name, Kind: kernel.ActionKind(kind), Price: amount, Description: description,
				InputSchema: inputSchema, OutputSchema: outputSchema,
				Source: srcData, WasmArtifact: artData,
				Method: method, Params: httpParams, Auth: auth,
			}, output{money: moneyAction})
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "http", "Action kind: http or wasm")
	cmd.Flags().StringVar(&source, "source", "", "URL (http) or file path (wasm)")
	cmd.Flags().StringVar(&method, "method", "", "HTTP verb (default POST)")
	cmd.Flags().StringArrayVar(&params, "param", nil, "HTTP field binding name:in (path|query|body); repeatable")
	cmd.Flags().StringVar(&artifact, "artifact", "", "Base64 WASM artifact or file path")
	cmd.Flags().StringVar(&description, "description", "", "Description")
	cmd.Flags().StringVar(&price, "price", "", "Price, written the way this kernel shows money (for example 1.50)")
	cmd.Flags().StringVar(&inputSchemaStr, "input-schema", "", "JSON Schema for inputs (or @file.json)")
	cmd.Flags().StringVar(&outputSchemaStr, "output-schema", "", "JSON Schema for outputs (or @file.json)")
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json)")
	return cmd
}

func actionUpdateCmd() *cobra.Command {
	var description, source, method, artifact, price string
	var params []string
	var visibility string
	var inputSchemaStr, outputSchemaStr, authStr string
	cmd := &cobra.Command{
		Use:   "update ACTION|PATH",
		Short: "Update an action or a path",
		Long:  "Update an action or a path.\n\n" + actionPathHelp + "\n\nVisibility, price, and auth may target a whole path; a description, schema, or source needs a target naming exactly one action. Changing source, schema, or price disables the action until re-enabled.",
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
				amount, err := priceIn(price)
				if err != nil {
					return err
				}
				req.Price = &amount
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
			return cli.emit("PUT", "/v1/actions", targetRequest{Target: args[0], UpdateActionRequest: req}, reportActions("updated"))
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "New description")
	cmd.Flags().StringVar(&source, "source", "", "New source URL or file path")
	cmd.Flags().StringVar(&artifact, "artifact", "", "New base64 WASM artifact or file path")
	cmd.Flags().StringVar(&method, "method", "", "New HTTP verb")
	cmd.Flags().StringArrayVar(&params, "param", nil, "HTTP field binding name:in (path|query|body); repeatable")
	cmd.Flags().StringVar(&price, "price", "", "New price, written the way this kernel shows money (for example 1.50)")
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

// reportActions names the rows a mutation wrote rather than counting them, in the list view every
// other set of rows is read in — the terms included, since a change to an action is most often a
// change to what it costs (§14).
func reportActions(verb string) output {
	return output{human: func(b []byte) error {
		var as []actionResp
		if err := json.Unmarshal(b, &as); err != nil {
			return err
		}
		net, err := humanUnits(context.Background())
		if err != nil {
			return err
		}
		if err := list(
			column{"CHANGE", func(json.RawMessage) string { return verb }},
			column{"ACTION", text("action")},
			column{"PRICE", money("price", net)},
			column{"ACTIVE", func(row json.RawMessage) string {
				if strField(row, "active") == "true" {
					return "yes"
				}
				return "no"
			}},
			column{"AUDIENCE", text("visibility")},
		)(b); err != nil {
			return err
		}
		warnUnfundedPublic(as)
		return nil
	}}
}

// warnUnfundedPublic tells a provider what only their own kernel can know: a public action is
// served abroad on the provider's own money (D14), so one priced above their balance is refused
// for every foreign buyer — who is told nothing except that this kernel declined. Said once, when
// the action becomes sellable, and never as an error: the action is published either way.
func warnUnfundedPublic(rows []actionResp) {
	var dearest int64
	for _, a := range rows {
		if a.Action != nil && a.Active && a.Visibility == kernel.VisibilityPublic && a.Price > dearest {
			dearest = a.Price
		}
	}
	if dearest == 0 {
		return
	}
	ctx := context.Background()
	me, err := readMe(ctx)
	if err != nil || me.Available >= dearest {
		return
	}
	net, nerr := humanUnits(ctx)
	if nerr != nil {
		return
	}
	fmt.Fprintf(os.Stderr,
		"Note: your balance is %s and this action costs %s. Your kernel pays for the work a buyer on\n"+
			"another kernel asks for, and is repaid when they settle, so it will decline their calls\n"+
			"until you hold at least the price. Calls from this kernel are unaffected.\n",
		net.Amount(me.Available), net.Amount(dearest))
}

// actionRunE adapts a command body that needs the resolved action id: every action subcommand
// takes one positional ref and resolves it the same way, so the preamble lives here once.
func actionRunE(fn func(ctx context.Context, id, ref string) error) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		ctx := context.Background()
		id, err := cli.resolveActionID(ctx, args[0])
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
			return cli.emit("POST", "/v1/actions/"+suffix,
				map[string]string{"target": args[0]}, reportActions(pastTense))
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
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
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
			// Whether a row is live and whether it needs the caller's own credential are columns
			// like any other, rather than marks the reader has to have been told about.
			cols := []column{{"ACTION", text("action")}, {"PRICE", money("price", net)}}
			if all {
				cols = append(cols, column{"ACTIVE", func(row json.RawMessage) string {
					if strField(row, "active") == "true" {
						return "yes"
					}
					return "no"
				}})
			}
			cols = append(cols, column{"AUTHORIZE", func(row json.RawMessage) string {
				if strField(row, "requires_grant") == "true" {
					return "your own login"
				}
				return ""
			}})
			return cli.emitCtx(ctx, "GET", "/v1/actions?"+q.Encode(), nil, output{human: list(cols...)})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Include inactive and private actions (a superuser sees every owner's)")
	cmd.Flags().StringVar(&owner, "owner", "", "Only actions owned by this user, handle@kernel")
	cmd.Flags().StringVar(&name, "name", "", "Only actions with this name")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func actionShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ACTION",
		Short: "Show action details",
		Long: "Show action details, and what this kernel knows about how the action has behaved: " +
			"its own calls, what the provider reports, and what other kernels report.\n\n" + actionRefHelp,
		Args: cobra.ExactArgs(1),
		RunE: actionRunE(func(ctx context.Context, id, ref string) error {
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			return cli.emitCtx(ctx, "GET", "/v1/actions/"+id, nil, output{money: moneyAction,
				human: func(b []byte) error { return printActionWithEvidence(b, net) }})
		}),
	}
}

// printEvidence writes what one subject's reporters say, kept in the two groups they belong to:
// the subject's own account of itself, then everyone else's account of trading with it. Used by
// `action show` for one action and by `admin peer inspect` for a whole kernel, so an operator and
// a buyer are never shown the same trades two different ways (§13).
func printEvidence(subjectName, subjectKey string, rows []*kernel.SubjectEvidenceRow) {
	fmt.Printf("\nExecution reported by %s\n", subjectName)
	own := false
	for _, e := range rows {
		if e.IssuerPublicKey == subjectKey {
			own = true
			printEvidenceRow("  ", e, false)
		}
	}
	if !own {
		fmt.Println("  (none)")
	}
	header := false
	for _, e := range rows {
		if e.IssuerPublicKey == subjectKey {
			continue
		}
		if !header {
			fmt.Printf("\nCounterparty experience\n")
			header = true
		}
		printEvidenceRow("  ", e, true)
	}
}

// printEvidenceRow writes one reporter's account. A reporter other than the subject is marked with
// how much of its account the subject's own record confirms, and with any trade the two describe
// differently — a claim nobody else corroborates is shown, never taken as fact (§13).
func printEvidenceRow(indent string, e *kernel.SubjectEvidenceRow, counterparty bool) {
	fmt.Printf("%sFrom %s on %s: %d interactions, %d successful  ~%.0fms",
		indent, shortKey(e.IssuerPublicKey), e.SubjectActionID, e.Uses, e.Successes, e.AvgLatencyMs)
	if counterparty {
		switch {
		case e.Uses > 0 && e.CorroboratedUses == e.Uses:
			fmt.Printf(" [verified]")
		case e.CorroboratedUses > 0:
			fmt.Printf(" [%d/%d verified]", e.CorroboratedUses, e.Uses)
		default:
			fmt.Printf(" [unverified]")
		}
	}
	if e.RatingCount > 0 {
		fmt.Printf("  rating %.2f", e.RatingMean)
	}
	if e.UnverifiedRatings > 0 {
		fmt.Printf("  [+%d unverified rating]", e.UnverifiedRatings)
	}
	if e.Contradictions > 0 {
		fmt.Printf("  [%d contradicted]", e.Contradictions)
	}
	if e.Equivocations > 0 {
		fmt.Printf("  [%d told two ways]", e.Equivocations)
	}
	if !e.Since.IsZero() {
		fmt.Printf("  (%s to %s)", e.Since.Format("2006-01-02"), e.Until.Format("2006-01-02"))
	}
	fmt.Println()
}

// printActionWithEvidence prints an action and then its record. The record is separated from the
// contract because it is a different kind of claim: the contract is what the provider promises,
// the record is what happened.
func printActionWithEvidence(b []byte, net kernel.Network) error {
	var resp struct {
		Evidence *struct {
			LocalExperience  *kernel.Stats                `json:"local_experience"`
			ProviderReported *kernel.SubjectEvidenceRow   `json:"provider_reported"`
			ObservedByOthers []*kernel.SubjectEvidenceRow `json:"observed_by_others"`
			RetainedCap      int                          `json:"retained_cap"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(b, &resp)
	if err := printFields(b, moneyAction, net); err != nil {
		return err
	}
	if resp.Evidence == nil {
		return nil
	}
	if s := resp.Evidence.LocalExperience; s != nil && s.Uses > 0 {
		fmt.Printf("\nThis kernel's own calls\n  %d calls, %d succeeded  ~%.0fms", s.Uses, s.Successes, s.LatencyEstimate*1000)
		if s.RatingCount > 0 {
			fmt.Printf("  rating %.2f from %d", s.RatingEstimate, s.RatingCount)
		}
		fmt.Println()
	}
	if p := resp.Evidence.ProviderReported; p != nil {
		fmt.Printf("\nReported by the provider\n")
		printEvidenceRow("  ", p, false)
	}
	if len(resp.Evidence.ObservedByOthers) > 0 {
		fmt.Printf("\nReported by other kernels\n")
		for _, e := range resp.Evidence.ObservedByOthers {
			printEvidenceRow("  ", e, true)
		}
	}
	return nil
}

func actionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete ACTION|PATH",
		Short: "Delete an action or a path, keeping history",
		Long:  "Delete an action or a path; transactions, receipts, and ratings survive.\n\n" + actionPathHelp,
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			q := url.Values{"target": {args[0]}}
			return cli.emit("DELETE", "/v1/actions?"+q.Encode(), nil, reportActions("deleted"))
		},
	}
}

func actionImportCmd() *cobra.Command {
	var authStr string
	cmd := &cobra.Command{
		Use:   "import NAME [SPEC_URL]",
		Short: "Import an OpenAPI document as one application",
		Long: "Import one OpenAPI document as the application at NAME, one action per operation. " +
			"SPEC_URL is the http(s) address of the document; give it on the first import — later " +
			"imports reuse the recorded one and reconcile changes, keeping each action's id, " +
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
			return cli.emit("POST", "/v1/actions/import", body, output{human: func(b []byte) error {
				var result importResp
				if err := json.Unmarshal(b, &result); err != nil {
					return err
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
			}})
		},
	}
	cmd.Flags().StringVar(&authStr, "auth", "", "Upstream auth config JSON (or @file.json), applied to every operation")
	return cmd
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
			return cli.emitCtx(ctx, "GET", path, nil, output{human: list(
				column{"RATING", func(row json.RawMessage) string {
					if strField(row, "value") == "1" {
						return "good"
					}
					return "bad"
				}},
				column{"WHEN", text("created_at")},
				column{"FROM", text("source")},
				column{"NOTE", text("note")},
			)})
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
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			q := url.Values{}
			setLimitOffset(q, limit, offset)
			return cli.emitCtx(ctx, "GET", "/v1/processes?"+q.Encode(), nil, output{human: list(
				column{"PROCESS", text("id")},
				column{"STATUS", text("status")},
				column{"AVAILABLE", money("available", net)},
				column{"LOCKED", money("locked", net)},
				// Not a liveness claim: the age of the oldest call this process is still waiting
				// on a receipt for (§14).
				column{"AWAITING SINCE", text("awaiting_receipt_since")},
			)})
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
			return cli.emit("POST", "/v1/processes/"+args[0]+"/end", nil, output{human: func([]byte) error {
				fmt.Printf("Process %s ended.\n", args[0])
				return nil
			}})
		},
	}
}

func processShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show process details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cli.emit("GET", "/v1/processes/"+args[0], nil, output{money: moneyAccount})
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
		Long:  "Create a step: a prepaid continuation of a running call, addressed to one user who later completes it with `step complete`. The step's price is reserved now, so completion needs no further funds.\n\n" + actionRefHelp,
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
				ActionRef:      args[0], // an address or id; the server resolves it
				RequiredCaller: requiredCaller,
				PartialArgs:    pa,
			}
			return cli.emit("POST", "/v1/steps", body, output{money: moneyStep})
		},
	}
	cmd.Flags().StringVar(&traceID, "trace", "", "Id of the funding call (the trace_id returned by run), whose budget pays for the step (required)")
	cmd.Flags().StringVar(&requiredCaller, "required-caller", "", "User who must complete the step, handle@kernel, here or on a peer (required)")
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
				ctx := context.Background()
				net, err := humanUnits(ctx)
				if err != nil {
					return err
				}
				return cli.emitCtx(ctx, "GET", "/v1/steps?"+q.Encode(), nil, output{rows: "steps", human: func(b []byte) error {
					var held kernel.PeerStepList
					if err := json.Unmarshal(b, &held); err != nil {
						return err
					}
					rows, _ := json.Marshal(held.Steps)
					if err := list(
						column{"STEP", text("id")},
						column{"PRICE", money("price", net)},
						column{"CREATED", text("created_at")},
						column{"SUPPLIED", text("partial_args")},
					)(rows); err != nil {
						return err
					}
					if held.Truncated {
						fmt.Println("more steps are waiting than one page carries; complete some and ask again")
					}
					return nil
				}})
			}
			if processID != "" {
				q.Set("process_id", processID)
			}
			if status != "" {
				q.Set("status", status)
			}
			setLimitOffset(q, limit, offset)
			return cli.emit("GET", "/v1/steps?"+q.Encode(), nil, output{human: list(
				column{"STEP", text("id")},
				column{"STATUS", text("status")},
				column{"CREATED BY", text("created_by")},
				column{"COMPLETES", text("action")},
				column{"CALLER", text("required_caller")},
			)})
		},
	}
	cmd.Flags().StringVar(&processID, "process", "", "Filter by process ID")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status (waiting, running, done, cancelled)")
	cmd.Flags().StringVar(&peer, "peer", "", "List steps this peer (its local name or key) is holding for you, over federation")
	addPagingFlags(cmd, &limit, &offset)
	return cmd
}

func stepShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show ID",
		Short: "Show step details",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cli.emit("GET", "/v1/steps/"+args[0], nil, output{money: moneyStep})
		},
	}
}

func stepCompleteCmd() *cobra.Command {
	var peer string
	cmd := &cobra.Command{
		Use:   "complete ID [JSON]",
		Short: "Complete a waiting step",
		Long:  "Complete a waiting step addressed to you, supplying what is missing.\n\n[JSON] is the completion input as a JSON object, default {}; @file.json reads it from a file. `step show` lists the fields still expected under allowed_input.",
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
			body := map[string]any{"args": input}
			if peer != "" {
				body["peer"] = peer
			}
			return cli.emit("POST", "/v1/steps/"+args[0]+"/complete", body, output{id: "tx_id"})
		},
	}
	cmd.Flags().StringVar(&peer, "peer", "", "Complete a step held by this peer (its local name or key), over federation")
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
			ctx := context.Background()
			net, err := humanUnits(ctx)
			if err != nil {
				return err
			}
			q := url.Values{}
			if processID != "" {
				q.Set("process_id", processID)
			}
			setLimitOffset(q, limit, offset)
			return cli.emitCtx(ctx, "GET", "/v1/transactions?"+q.Encode(), nil, output{human: list(
				column{"TRANSACTION", text("id")},
				column{"STARTED", text("started_at")},
				column{"ACTION", text("action")},
				column{"STATUS", text("status")},
				// What the call drew, which is what it locked less what came back: a failed call
				// refunds all of it unless work beneath it was already delivered (P5, U13).
				column{"CHARGED", func(row json.RawMessage) string {
					return net.Amount(numberField(row, "gross") - numberField(row, "refund"))
				}},
			)})
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
			return cli.emit("GET", "/v1/transactions/"+args[0], nil, output{money: moneyTx})
		},
	}
}

func txVerifyReceiptCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify ID",
		Short: "Verify a transaction's signed receipt offline",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return cli.emit("GET", "/v1/transactions/"+args[0]+"/receipt-verification", nil, output{})
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
			return cli.emit("POST", "/v1/transactions/"+args[0]+"/rate", body, output{})
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
	var yes bool
	cmd := &cobra.Command{
		Use:   "run ACTION [JSON]",
		Short: "Run an action",
		Long:  "Run an action and print its result. The advertised price is the most the whole call can cost you; a failed call refunds what was not consumed.\n\n" + actionRefHelp + "\n\n[JSON] is the arguments as a JSON object, default {}; @file.json reads it from a file.",
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
			// The unit is read before the call, never after: a /health that fails once the charge is
			// committed must not turn a run that happened into an error (the rule units() states).
			net, err := humanUnits(context.Background())
			if err != nil {
				return err
			}
			// A run pays the price it was shown. When the caller names no pin, the client reads the
			// action and pins what that read returned, so terms that moved between the read and
			// the run are refused rather than charged — the same guarantee for an action here and
			// one on another kernel (§4 precondition 7, U8). A pin the caller supplies is sent as
			// given: it stands for terms a person actually saw, which this read cannot replace.
			if quoteHash == "" {
				quoted, price, qerr := quotedTerms(context.Background(), cmdArgs[0])
				if qerr != nil {
					return qerr
				}
				quoteHash = quoted
				// The price goes to the person, not into the result: stdout carries what the
				// command returned and nothing else. At a terminal the person is asked before
				// their money moves, as every other spending command asks; off a terminal the
				// price is stated and the run proceeds, because a script has nobody to ask and
				// the pin already guarantees this is the price it read.
				if interactiveTTY() && !yes {
					if err := askYesNo(fmt.Sprintf("%s costs %s. Run it?", cmdArgs[0], net.Amount(price))); err != nil {
						return err
					}
				} else {
					fmt.Fprintf(os.Stderr, "%s costs %s.\n", cmdArgs[0], net.Amount(price))
				}
			}
			reqBody := kernel.RunRequest{ActionRef: cmdArgs[0], Args: args, QuoteHash: quoteHash}
			var raw json.RawMessage
			err = cli.call(context.Background(), "POST", "/v1/run", reqBody, &raw)
			// A delegated-OAuth action needs a one-time consent (§8). At an interactive terminal,
			// offer it inline and re-run once, so the user issues a single `juice run`. Non-TTY
			// callers (scripts, agents) get the structured error + hint instead — no browser.
			if errors.Is(err, kernel.ErrGrantRequired) {
				action := grantActionRef(err, cmdArgs[0])
				if interactiveTTY() && confirm(fmt.Sprintf("This action needs your authorization. Authorize %s now?", action), false) == nil {
					connected, cerr := connectSelector(action, false, true)
					if cerr != nil {
						return cerr
					}
					fmt.Fprintf(os.Stderr, "Connected %s.\n", strings.Join(connected, ", "))
					err = cli.call(context.Background(), "POST", "/v1/run", reqBody, &raw)
				}
			}
			if err != nil {
				return err
			}
			return emit(raw, output{id: "tx_id", money: moneyCall, net: net})
		},
	}
	cmd.Flags().StringVar(&quoteHash, "quote-hash", "", "Pin terms you read earlier (quote_hash on the action) instead of the ones this command reads")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// quotedTerms reads an action and returns the terms to pin and the price to show. One read, made
// by the same client that will run it, so what is pinned is what was quoted.
func quotedTerms(ctx context.Context, ref string) (string, int64, error) {
	var a struct {
		QuoteHash string `json:"quote_hash"`
		Price     int64  `json:"price"`
	}
	id, err := cli.resolveActionID(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	if err := cli.call(ctx, "GET", "/v1/actions/"+id, nil, &a); err != nil {
		return "", 0, err
	}
	return a.QuoteHash, a.Price, nil
}

// numberField reads one whole-number field of a row; a field that is absent or is not a number
// reads as zero, since every number these views show is an amount and an absent amount is none.
func numberField(row json.RawMessage, field string) int64 {
	var fields map[string]json.RawMessage
	if json.Unmarshal(row, &fields) != nil {
		return 0
	}
	var n int64
	if json.Unmarshal(fields[field], &n) != nil {
		return 0
	}
	return n
}

// party names one side of a ledger entry, where an absent side is the world outside this kernel:
// money that came from nowhere it knows, or left for somewhere it does not follow.
func party(field string) func(json.RawMessage) string {
	return func(row json.RawMessage) string {
		if v := strField(row, field); v != "" {
			return v
		}
		return "outside"
	}
}
