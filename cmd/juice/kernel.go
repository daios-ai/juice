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
	kernelCmd.AddCommand(kernelServeCmd(), kernelAddCmd(), kernelUpdateCmd(),
		kernelListCmd(), kernelHealthCmd(), kernelForgetCmd())
	rootCmd.AddCommand(kernelCmd)
}

// kernelAddCmd registers a kernel by dialling it. The name is the client's own label, defaulting to
// the nickname the kernel advertises — the name an operator has already seen is the one they will
// type — but a nickname is a label rather than proof, so a name held by another key is not taken.
func kernelAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add URL [NAME]",
		Short: "Register a kernel this client can talk to",
		Long: "Register the kernel answering at URL, under NAME. Without NAME it is registered under the\n" +
			"nickname the kernel advertises. Adding does not log in and does not select anything:\n" +
			"`juice auth login USER@NAME` does that.\n\n" +
			"Adding a kernel already known under that name succeeds: the same kernel at the same\n" +
			"address changes nothing, and one that has moved has its address updated, since a key is\n" +
			"what says which kernel this is. A different kernel under a name already taken is refused.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(_ *cobra.Command, args []string) error {
			name := ""
			if len(args) == 2 {
				name = args[1]
			}
			name, k, added, err := registerKernel(context.Background(), name, args[0], false)
			if err != nil {
				return err
			}
			printKernel(name, k, added)
			return nil
		},
	}
}

// kernelUpdateCmd repoints a kernel that has moved. It is its own verb because it is the one act
// here that destroys something: a session is only valid to the server that issued it, so every
// login on that kernel is logged out once the new address has answered.
func kernelUpdateCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "update NAME URL",
		Short: "Point a registered kernel at another address",
		Long: "Point NAME at URL, recording the identity that answers there.\n\n" +
			"Every login on that kernel is logged out: a session is only valid to the server that\n" +
			"issued it, so pointing a name somewhere else strands the ones made at the old address.",
		Args: cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			name, url := args[0], args[1]
			if _, err := kernelNamed(loadClientConfig(), name); err != nil {
				return err
			}
			if err := confirm(fmt.Sprintf("Point %s at %s? Every login on it is logged out.", name, url), yes); err != nil {
				return err
			}
			_, k, _, err := registerKernel(context.Background(), name, url, true)
			if err != nil {
				return err
			}
			printKernel(name, k, false)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// kernelListCmd shows what this client knows, marking the kernel the selected login acts through.
func kernelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the kernels this client knows",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg := loadClientConfig()
			if flagJSON {
				return printJSON(cfg)
			}
			names := make([]string, 0, len(cfg.Kernels))
			for name := range cfg.Kernels {
				names = append(names, name)
			}
			sort.Strings(names)
			here, _, _ := selected()
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
			resp, err := http.Get(base + "/health") //nolint:noctx
			if err != nil {
				return errUnreachable(base, err)
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if resp.StatusCode != http.StatusOK {
				return kernel.ErrExecutionFailed.Wrapf("server returned status %d", resp.StatusCode)
			}
			if flagJSON {
				return printJSON(body)
			}
			h, _ := body["handle"].(string)
			pk, _ := body["public_key"].(string)
			net, _ := body["network"].(string)
			// The network comes first after the name: a kernel serves one for life, and it decides
			// what every balance and every signature here means (D23).
			fmt.Printf("ok  %s  network %s  %s\n", h, net, pk)
			return nil
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
			fmt.Printf("Forgot %s\n", name)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// registerKernel records a kernel after the server at that address has answered as itself. Nothing
// is written until it has: a name that cannot be dialled keeps whatever it meant before. replace is
// what `kernel update` passes, and it is also what strands the logins made at the old address.
func registerKernel(ctx context.Context, name, url string, replace bool) (string, *kernelRec, bool, error) {
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	h, err := health(ctx, url)
	if err != nil {
		return "", nil, false, err
	}
	if name == "" {
		name = h.Handle
	}
	if err := validateLocalName("kernel", name); err != nil {
		return "", nil, false, err
	}
	cfg := loadClientConfig()
	existing := cfg.Kernels[name]
	switch {
	case existing == nil, replace:
		if replace {
			forgetLogins(name) // a repoint may be to another kernel entirely, so its logins go
		}
	case existing.PublicKey != h.PublicKey:
		return "", nil, false, kernel.ErrInvalidState.Wrapf(
			"%s is already the name of another kernel here (%s); add this one under another name, or "+
				"point %s at it deliberately: juice kernel update %s %s", name, existing.Endpoint, name, name, url)
	case existing.Endpoint == url:
		return name, existing, false, nil // already known, on the same terms: nothing to do
	}
	// Past that, either the name is new or the kernel answering is the one recorded — the same key,
	// at whatever address it answers on today. A kernel that has moved keeps its logins: a session
	// belongs to the kernel that issued it, and this is that kernel.
	k := &kernelRec{Endpoint: url, PublicKey: h.PublicKey, WorldDigest: h.Digest,
		Network: h.Network, Decimals: h.Decimals, Symbol: h.Symbol}
	cfg.Kernels[name] = k
	if err := saveClientConfig(cfg); err != nil {
		return "", nil, false, err
	}
	return name, k, true, nil
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

// printKernel names what was registered the way `kernel list` will show it: the local name first,
// since that is the word every other command takes.
func printKernel(name string, k *kernelRec, added bool) {
	verb := "already known"
	if added {
		verb = "added"
	}
	fmt.Printf("%s  network %s  %s  %s  (%s)\n", name, k.Network, k.Endpoint, k.PublicKey, verb)
}
