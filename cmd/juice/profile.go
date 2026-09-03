package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

// defaultEndpoint is where a kernel listens when nothing says otherwise.
const defaultEndpoint = "http://localhost:4040"

// profile is how a client names a kernel: the address it is reached at, the identity that server
// must present, and the login held for it. Pinning the key is what makes a second address safe —
// a token is handed only to the kernel it was issued by, never to whatever answers a URL.
type profile struct {
	Endpoint     string `json:"endpoint"`
	PublicKey    string `json:"public_key,omitempty"`
	WorldDigest  string `json:"world_digest,omitempty"`
	Network      string `json:"network,omitempty"`
	Decimals     uint8  `json:"decimals,omitempty"`
	Token        string `json:"token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

type profileSet struct {
	Active   string              `json:"active"`
	Profiles map[string]*profile `json:"profiles"`
}

// clientHome is the client's own subdirectory, $JUICE_HOME/client. The kernel's directory sits
// beside it and is never read here: every command is a TCP client (API.md C13).
func clientHome() string { return filepath.Join(juiceHome(), "client") }

func profilesPath() string { return filepath.Join(clientHome(), "profiles.json") }

// loadProfiles reads the profile file, falling back to a single profile pointing at the local
// kernel. An unreadable file yields that same fallback rather than an error: a client that cannot
// parse its own notes must still be able to reach a server and log in again.
func loadProfiles() *profileSet {
	ps := &profileSet{}
	if data, err := os.ReadFile(profilesPath()); err == nil {
		_ = json.Unmarshal(data, ps)
	}
	if ps.Profiles == nil {
		ps.Profiles = map[string]*profile{}
	}
	if ps.Active == "" {
		ps.Active = "default"
	}
	return ps
}

// saveProfiles writes the file atomically — temp file in the same directory, then rename — so an
// interrupted write never leaves a client without its tokens. The file holds bearer tokens, so it
// is 0600 in a 0700 directory.
func saveProfiles(ps *profileSet) error {
	if err := os.MkdirAll(clientHome(), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(clientHome(), ".profiles-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), profilesPath())
}

// activeProfile returns the profile set and the profile this invocation addresses. JUICE_PROFILE
// overrides the stored active name, so one command can address another kernel without switching.
// A name with no entry yet gets one in memory, so an unconfigured client still reaches the local
// kernel; it is written only when something is stored in it.
func activeProfile() (*profileSet, string, *profile) {
	ps := loadProfiles()
	name := ps.Active
	if n := strings.TrimSpace(os.Getenv("JUICE_PROFILE")); n != "" {
		name = n
	}
	p := ps.Profiles[name]
	if p == nil {
		p = &profile{}
		ps.Profiles[name] = p
	}
	if p.Endpoint == "" {
		p.Endpoint = defaultEndpoint
	}
	return ps, name, p
}

// updateActive mutates the addressed profile and persists the whole file.
func updateActive(fn func(*profile)) error {
	ps, _, p := activeProfile()
	fn(p)
	return saveProfiles(ps)
}

// ---- tokens ----

func loadToken() (string, error) {
	_, _, p := activeProfile()
	if p.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrap("not logged in; run: juice auth login")
	}
	return p.Token, nil
}

// saveToken stores the access token in the addressed profile. A profile that has pinned no kernel
// also adopts the address the token came from, so that token travels back only there.
func saveToken(tok string) error {
	return updateActive(func(p *profile) {
		p.Token = tok
		if p.PublicKey == "" {
			p.Endpoint = serverBaseURL()
		}
	})
}

func removeToken() error { return updateActive(func(p *profile) { p.Token = "" }) }

func loadRefreshToken() (string, error) {
	_, _, p := activeProfile()
	if p.RefreshToken == "" {
		return "", os.ErrNotExist
	}
	return p.RefreshToken, nil
}

func saveRefreshToken(tok string) error {
	return updateActive(func(p *profile) { p.RefreshToken = tok })
}

func removeRefreshToken() error { return updateActive(func(p *profile) { p.RefreshToken = "" }) }

// atHome reports whether base is the active profile's own endpoint — the one place its credentials
// may go. Nothing a server says about itself can earn it a credential: an unsigned banner is a
// claim, not a proof, so any other address gets every request anonymously.
func atHome(base string) bool {
	_, _, p := activeProfile()
	return strings.TrimRight(base, "/") == strings.TrimRight(p.Endpoint, "/")
}

// tokenFor returns the bearer token to send to base, or why none is sent.
func tokenFor(base string) (string, error) {
	_, name, p := activeProfile()
	if p.Token == "" {
		return "", kernel.ErrUnauthenticated.Wrap("not logged in; run: juice auth login")
	}
	if !atHome(base) {
		return "", kernel.ErrUnauthenticated.Wrapf(
			"%s is not the kernel you are logged in to (profile %s), so the request was sent without your login", base, name)
	}
	return p.Token, nil
}

// probeHealth reads a server's identity banner at most once per address per run: deciding whether
// a token may travel must not multiply the requests a command makes.
var (
	healthMu    sync.Mutex
	healthCache = map[string]*serverHealth{}
)

func probeHealth(ctx context.Context, base string) (*serverHealth, error) {
	healthMu.Lock()
	defer healthMu.Unlock()
	if h, ok := healthCache[base]; ok {
		if h == nil {
			return nil, kernel.ErrPeerUnreachable.Wrapf("cannot reach %s", base)
		}
		return h, nil
	}
	h, err := health(ctx, base)
	healthCache[base] = h // a failure is cached as nil, so one unreachable server is dialed once
	if err != nil {
		healthCache[base] = nil
		return nil, err
	}
	return h, nil
}

// ---- amounts ----

// parseAmount converts an amount as a person writes it into the whole base units the kernel counts
// in. The digits are shifted by hand: money never passes through floating point.
func parseAmount(s string, decimals uint8) (int64, error) {
	bad := kernel.ErrInvalidInput.Wrap("amount must be a positive whole number")
	if decimals > 0 {
		bad = kernel.ErrInvalidInput.Wrapf("amount must be positive, with at most %d decimal places", decimals)
	}
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	if !allDigits(whole) || (frac != "" && !allDigits(frac)) || len(frac) > int(decimals) {
		return 0, bad
	}
	v, err := strconv.ParseInt(whole+frac+strings.Repeat("0", int(decimals)-len(frac)), 10, 64)
	if err != nil || v <= 0 {
		return 0, bad
	}
	return v, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// formatAmount renders base units the way a person reads them, exactly: the inverse of parseAmount.
func formatAmount(v int64, decimals uint8) string {
	s := strconv.FormatInt(v, 10)
	if decimals == 0 {
		return s
	}
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	for len(s) <= int(decimals) {
		s = "0" + s
	}
	return sign + s[:len(s)-int(decimals)] + "." + s[len(s)-int(decimals):]
}

// amountDecimals reports how many decimal places this world's money is written with: the pinned
// profile when it has one, else the server itself. Zero decimals — whole credits — is both the
// unpinned answer and a real one, and the same either way.
func amountDecimals(ctx context.Context) uint8 {
	if _, _, p := activeProfile(); p.Decimals > 0 {
		return p.Decimals
	}
	if h, err := probeHealth(ctx, serverBaseURL()); err == nil {
		return h.Decimals
	}
	return 0
}

// ---- use ----

func init() { rootCmd.AddCommand(useCmd()) }

func useCmd() *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:   "use [NAME]",
		Short: "Switch between the kernels this client knows",
		Long: "Switch between the kernels this client knows. A profile is a name for one kernel: the\n" +
			"address it answers on, the identity it must present, and your login there.\n\n" +
			"With no arguments, lists the profiles and marks the one in use. With NAME, switches to\n" +
			"that profile, refusing it if the server no longer presents the public key and network\n" +
			"the profile recorded. With --endpoint, adds NAME (or points it somewhere else) and\n" +
			"records the identity that server reports.\n\n" +
			"JUICE_PROFILE=NAME selects a profile for a single command without switching.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				if endpoint != "" {
					return kernel.ErrInvalidInput.Wrap("name the profile that address belongs to")
				}
				return listProfiles()
			}
			return useProfile(context.Background(), args[0], endpoint)
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", "", "Address this profile's kernel answers on, e.g. http://localhost:4040")
	return cmd
}

func listProfiles() error {
	ps, active, _ := activeProfile()
	if flagJSON {
		return printJSON(ps)
	}
	names := make([]string, 0, len(ps.Profiles))
	for name := range ps.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		mark := " "
		if name == active {
			mark = "*"
		}
		p := ps.Profiles[name]
		fmt.Printf("%s %-16s %-32s %s\n", mark, name, p.Endpoint, p.Network)
	}
	return nil
}

// useProfile switches to a profile, verifying it first. Without --endpoint the recorded identity
// is a promise the server must still keep; with it, the operator is naming a kernel for the first
// time and what that server reports is what gets recorded.
func useProfile(ctx context.Context, name, endpoint string) error {
	ps := loadProfiles()
	p := ps.Profiles[name]
	if p == nil {
		if endpoint == "" {
			return kernel.ErrNotFound.Wrapf("no profile named %s; add it with: juice use %s --endpoint URL", name, name)
		}
		p = &profile{}
		ps.Profiles[name] = p
	}
	if endpoint != "" {
		// A login belongs to the address it was made at. Whatever answers at the new one — even a
		// server presenting the pinned key, which any server could — starts with no credentials.
		p.Endpoint = strings.TrimRight(endpoint, "/")
		p.Token, p.RefreshToken = "", ""
	}
	h, err := health(ctx, p.Endpoint)
	if err != nil {
		return err
	}
	if endpoint == "" {
		if p.PublicKey != "" && h.PublicKey != p.PublicKey {
			return kernel.ErrInvalidState.Wrapf(
				"the server at %s is a different kernel than profile %s recorded; not switching", p.Endpoint, name)
		}
		if p.WorldDigest != "" && h.Digest != p.WorldDigest {
			return kernel.ErrInvalidState.Wrapf(
				"the server at %s now serves the %s network, not the one profile %s recorded; not switching", p.Endpoint, h.Network, name)
		}
	}
	p.PublicKey, p.WorldDigest, p.Network, p.Decimals = h.PublicKey, h.Digest, h.Network, h.Decimals
	ps.Active = name
	if err := saveProfiles(ps); err != nil {
		return err
	}
	fmt.Printf("Using %s\n  address: %s\n  kernel:  %s\n  network: %s\n", name, p.Endpoint, p.PublicKey, p.Network)
	return nil
}
