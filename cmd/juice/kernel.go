package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// The kernel noun: one verb runs a kernel from this installation, the rest are what this client
// knows about kernels it talks to — the same thing from either side, so one noun holds both.
func init() {
	kernelCmd := &cobra.Command{Use: "kernel", Short: "Run a kernel, or manage the ones this client knows"}
	kernelCmd.AddCommand(kernelServeCmd(), kernelAddCmd(), kernelListCmd(),
		kernelHealthCmd(), kernelForgetCmd())
	rootCmd.AddCommand(kernelCmd)
}

// kernelAddCmd registers a kernel by dialling it, and is also how a kernel that has moved is
// repointed: a key is what says which kernel this is, so the same key at a new address is the same
// kernel and keeps its logins. The name is the client's own label, defaulting to the nickname the
// kernel advertises — the name an operator has already seen is the one they will type — but a
// nickname is a label rather than proof, so a name held by another key is not taken.
func kernelAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add URL [NAME]",
		Short: "Register a kernel this client can talk to, or follow one that has moved",
		Long: "Register the kernel answering at URL, under NAME. Without NAME it is registered under the\n" +
			"nickname the kernel advertises. Adding does not log in and does not select anything:\n" +
			"`juice auth login USER@NAME` does that.\n\n" +
			"Adding a kernel already known under that name succeeds: the same kernel at the same\n" +
			"address changes nothing, and one that has moved has its address updated and keeps its\n" +
			"logins. A different kernel under a name already taken is refused; give it another name,\n" +
			"or `juice kernel forget NAME` first, which also removes that name's logins.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			name, k, outcome, err := registerKernel(context.Background(), name, args[0])
			if err != nil {
				return err
			}
			return emitKernel(name, k, outcome)
		},
	}
}

// kernelListCmd shows what this client knows, marking the kernel the selected login acts through.
func kernelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the kernels this client knows",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg := loadClientConfig()
			names := make([]string, 0, len(cfg.Kernels))
			for name := range cfg.Kernels {
				names = append(names, name)
			}
			sort.Strings(names)
			here, _, _ := selected()
			rows := make([]map[string]any, 0, len(names))
			for _, name := range names {
				k := cfg.Kernels[name]
				rows = append(rows, map[string]any{"kernel": name, "endpoint": k.Endpoint,
					"network": k.Network, "public_key": k.PublicKey, "selected": name == here.Kernel})
			}
			body, err := json.Marshal(rows)
			if err != nil {
				return err
			}
			return emit(body, output{id: "kernel", human: func([]byte) error {
				fmt.Printf("  %-16s %-10s %-32s %s\n", "KERNEL", "NETWORK", "ADDRESS", "KEY")
				for _, name := range names {
					mark := " "
					if name == here.Kernel {
						mark = "*"
					}
					k := cfg.Kernels[name]
					fmt.Printf("%s %-16s %-10s %-32s %s\n", mark, name, k.Network, k.Endpoint, k.PublicKey)
				}
				return nil
			}})
		},
	}
}

// kernelHealthCmd reads a kernel's live banner: a registered kernel by name, else the one the
// selected login acts through. It needs no login, since what a server says about itself is public
// (§13) — which is why a client reads it before trusting an address.
func kernelHealthCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "health [NAME]",
		Short: "Check a kernel is up, and which kernel it is",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			base := serverBaseURL()
			if len(args) == 1 {
				k := loadClientConfig().Kernels[args[0]]
				if k == nil {
					return kernel.ErrNotFound.Wrapf("no kernel named %s; add it with: juice kernel add URL %s", args[0], args[0])
				}
				base = k.Endpoint
			}
			if base == "" {
				return kernel.ErrInvalidInput.Wrap("name a kernel: juice kernel health NAME")
			}
			ctx := context.Background()
			body, status, err := doHTTP(ctx, "GET", base+"/health", nil, nil, 0, true)
			if err != nil {
				return errUnreachable(base, err)
			}
			if status != http.StatusOK {
				return kernel.ErrExecutionFailed.Wrapf("%s returned status %d", base, status)
			}
			h, err := decodeHealth(base, body)
			if err != nil {
				return err
			}
			return emit(body, output{id: "public_key", human: func([]byte) error {
				// The network comes first after the name: a kernel serves one for life, and it
				// decides what every balance and every signature here means (D23).
				fmt.Printf("ok  %s  network %s  %s\n", h.Handle, h.Network, h.PublicKey)
				return nil
			}})
		},
	}
}

// kernelForgetCmd removes what this client knows about a kernel, and the logins that only made
// sense there. It is `forget` rather than `delete` because nothing of the kernel's own is touched.
func kernelForgetCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "forget NAME",
		Short: "Remove this client's record of a kernel, and its logins",
		Long: "Remove NAME from the kernels this client knows, along with the credentials of every\n" +
			"login on it. Nothing on the kernel itself is touched: accounts, actions and money are\n" +
			"its own, and it goes on serving whoever else knows it.",
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			name := args[0]
			cfg := loadClientConfig()
			if cfg.Kernels[name] == nil {
				return kernel.ErrNotFound.Wrapf("no kernel named %s", name)
			}
			if err := confirm(fmt.Sprintf("Forget %s and log out every login on it?", name), yes); err != nil {
				return err
			}
			delete(cfg.Kernels, name)
			if here, err := parseLogin(cfg.Current); err == nil && here.Kernel == name {
				cfg.Current = ""
			}
			forgetLogins(name)
			if err := saveClientConfig(cfg); err != nil {
				return err
			}
			body, err := json.Marshal(map[string]any{"kernel": name, "forgotten": true})
			if err != nil {
				return err
			}
			return emit(body, output{id: "kernel", human: func([]byte) error {
				fmt.Printf("Forgot %s\n", name)
				return nil
			}})
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// registerKernel records a kernel after the server at that address has answered as itself, and
// returns what it did: added, already known, or moved. Nothing is written until the server has
// answered, so a name that cannot be dialled keeps whatever it meant before.
func registerKernel(ctx context.Context, name, url string) (string, *kernelRec, string, error) {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	h, err := health(ctx, url)
	if err != nil {
		return "", nil, "", err
	}
	if name == "" {
		name = h.Handle
	}
	if err := validateLocalName("kernel", name); err != nil {
		return "", nil, "", err
	}
	cfg := loadClientConfig()
	existing := cfg.Kernels[name]
	outcome := "added"
	switch {
	case existing == nil:
	case existing.PublicKey != h.PublicKey:
		// A name is one kernel's here. Taking it for another would silently point every login and
		// every reference made under it at a stranger, so the operator says which they mean.
		return "", nil, "", kernel.ErrInvalidState.Wrapf(
			"\"%s\" is already the name of a different kernel here (%s).\n"+
				"Add this one under another name, or first: juice kernel forget %s",
			name, existing.Endpoint, name)
	case existing.Endpoint == url:
		return name, existing, "already known", nil // nothing to do
	default:
		// The kernel answering is the one recorded — the same key, at whatever address it answers
		// on today — so it keeps its logins: a session belongs to the kernel that issued it, and
		// this is that kernel.
		outcome = "moved; existing logins kept"
	}
	k := &kernelRec{Endpoint: url, PublicKey: h.PublicKey, WorldDigest: h.Digest,
		Network: h.Network, Decimals: h.Decimals, Symbol: h.Symbol}
	cfg.Kernels[name] = k
	if err := saveClientConfig(cfg); err != nil {
		return "", nil, "", err
	}
	return name, k, outcome, nil
}

// forgetLogins removes the credentials of every login on one kernel, and unselects one that was
// selected. A session is only valid to the server that issued it, so a kernel this client no longer
// knows — or knows at another address — leaves nothing behind that could be sent anywhere.
func forgetLogins(kernelName string) {
	for _, l := range logins() {
		if l.Kernel != kernelName {
			continue
		}
		if path, err := credentialPath(l); err == nil {
			_ = os.Remove(path)
		}
	}
}

// emitKernel answers with the record that was registered, in the shape `kernel list` answers with,
// plus what registering did. The human line names the local name first, since that is the word
// every other command takes.
func emitKernel(name string, k *kernelRec, outcome string) error {
	body, err := json.Marshal(map[string]any{"kernel": name, "endpoint": k.Endpoint,
		"network": k.Network, "public_key": k.PublicKey, "outcome": outcome})
	if err != nil {
		return err
	}
	return emit(body, output{id: "kernel", human: func([]byte) error {
		fmt.Printf("%s  network %s  %s  %s  (%s)\n", name, k.Network, k.Endpoint, k.PublicKey, outcome)
		return nil
	}})
}
