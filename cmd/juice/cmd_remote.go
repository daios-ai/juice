package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	remoteCmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage remote kernel peers",
	}

	remoteAddCmd := &cobra.Command{
		Use:   "add <url>",
		Short: "Register a remote kernel by fetching its well-known metadata",
		Args:  cobra.ExactArgs(1),
		RunE:  runRemoteAdd,
	}

	remoteListCmd := &cobra.Command{
		Use:   "list",
		Short: "List registered remote kernels",
		RunE:  runRemoteList,
	}

	remoteImportCmd := &cobra.Command{
		Use:   "import <handle> <action-name>",
		Short: "Import an action from a remote kernel as a local HTTP action",
		Args:  cobra.ExactArgs(2),
		RunE:  runRemoteImport,
	}

	remoteCmd.AddCommand(remoteAddCmd, remoteListCmd, remoteImportCmd)
	rootCmd.AddCommand(remoteCmd)
}

func runRemoteAdd(_ *cobra.Command, args []string) error {
	baseURL := strings.TrimRight(args[0], "/")
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := requireSuperuser(k); err != nil {
		return err
	}

	// Fetch well-known metadata from the remote kernel.
	resp, err := http.Get(baseURL + "/.well-known/juice-kernel.json")
	if err != nil {
		return fmt.Errorf("fetch well-known: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote kernel returned %d: %s", resp.StatusCode, body)
	}

	var meta struct {
		Handle    string `json:"handle"`
		PublicKey string `json:"public_key"`
		BaseURL   string `json:"base_url"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return fmt.Errorf("parse well-known response: %w", err)
	}
	if meta.Handle == "" || meta.PublicKey == "" {
		return fmt.Errorf("remote kernel response missing handle or public_key")
	}
	if meta.BaseURL == "" {
		meta.BaseURL = baseURL
	}

	// Derive a canonical local handle from the URL host (@<hostname> convention).
	parsed, err := url.Parse(meta.BaseURL)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("invalid base URL %q", meta.BaseURL)
	}
	localHandle := "@" + parsed.Host

	u, err := k.RegisterRemoteKernel(ctx, localHandle, meta.PublicKey, meta.BaseURL)
	if err != nil {
		return fmt.Errorf("register remote kernel: %w", err)
	}
	fmt.Printf("Registered remote kernel %s (id=%s, base_url=%s)\n", u.Handle, u.ID, u.RemoteBaseURL)
	return nil
}

func runRemoteList(_ *cobra.Command, _ []string) error {
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := requireSuperuser(k); err != nil {
		return err
	}

	remotes, err := k.ListRemoteKernels(ctx)
	if err != nil {
		return err
	}
	if len(remotes) == 0 {
		fmt.Println("No remote kernels registered.")
		return nil
	}
	fmt.Printf("%-20s %-36s %s\n", "HANDLE", "ID", "BASE_URL")
	for _, u := range remotes {
		fmt.Printf("%-20s %-36s %s\n", u.Handle, u.ID, u.RemoteBaseURL)
	}
	return nil
}

func runRemoteImport(_ *cobra.Command, args []string) error {
	remoteHandle, actionName := args[0], args[1]
	k, db, err := openKernel()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := requireSuperuser(k); err != nil {
		return err
	}

	// Resolve the remote kernel user.
	remoteUser, err := k.ReadUserByHandle(ctx, remoteHandle)
	if err != nil {
		return fmt.Errorf("remote kernel %q not found; run 'juice remote add' first", remoteHandle)
	}
	if remoteUser.RemoteBaseURL == "" {
		return fmt.Errorf("%q is not a remote kernel", remoteHandle)
	}

	// Fetch the action list to find the action by name.
	manifestURL := fmt.Sprintf("%s/v1/actions?owner=%s&name=%s",
		strings.TrimRight(remoteUser.RemoteBaseURL, "/"), remoteHandle, actionName)
	resp, err := http.Get(manifestURL)
	if err != nil {
		return fmt.Errorf("fetch action list: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote kernel returned %d: %s", resp.StatusCode, body)
	}

	// Parse the action list — remote kernel returns *kernel.Action objects (PascalCase JSON).
	var actions []struct {
		ID           string
		Name         string
		Description  string
		Price        int64
		Kind         string
		InputSchema  map[string]any
		OutputSchema map[string]any
		ArtifactHash string
	}
	if err := json.Unmarshal(body, &actions); err != nil {
		return fmt.Errorf("parse action list: %w", err)
	}
	idx := -1
	for i := range actions {
		if actions[i].Name == actionName {
			idx = i
			break
		}
	}
	if idx == -1 {
		return fmt.Errorf("action %q not found on remote kernel %q", actionName, remoteHandle)
	}
	a := actions[idx]

	m := kernel.ActionManifest{
		OwnerHandle:  remoteHandle,
		Name:         a.Name,
		Description:  a.Description,
		InputSchema:  a.InputSchema,
		OutputSchema: a.OutputSchema,
		Price:        a.Price,
		Kind:         kernel.ActionKind(a.Kind),
		ArtifactHash: a.ArtifactHash,
	}
	imported, err := k.ImportRemoteAction(ctx, remoteUser.ID, m)
	if err != nil {
		return fmt.Errorf("import action: %w", err)
	}
	fmt.Printf("Imported action %s/%s (id=%s)\n", remoteHandle, imported.Name, imported.ID)
	return nil
}
