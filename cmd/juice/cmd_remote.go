package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/spf13/cobra"
)

func init() {
	remoteCmd := &cobra.Command{
		Use:   "remote",
		Short: "Manage remote kernel peers",
	}

	var addURL string
	remoteAddCmd := &cobra.Command{
		Use:   "add",
		Short: "Register a remote kernel by fetching its well-known metadata",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRemoteAdd(addURL)
		},
	}
	remoteAddCmd.Flags().StringVar(&addURL, "url", "", "Remote kernel base URL (required)")
	_ = remoteAddCmd.MarkFlagRequired("url")

	remoteListCmd := &cobra.Command{
		Use:   "list",
		Short: "List registered remote kernels",
		RunE:  runRemoteList,
	}

	var importRemote, importAction string
	remoteImportCmd := &cobra.Command{
		Use:   "import",
		Short: "Import an action from a remote kernel as a local proxy action (idempotent)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRemoteImport(importRemote, importAction)
		},
	}
	remoteImportCmd.Flags().StringVar(&importRemote, "remote", "", "Remote kernel handle (required)")
	remoteImportCmd.Flags().StringVar(&importAction, "action", "", "Action name on the remote kernel (required)")
	_ = remoteImportCmd.MarkFlagRequired("remote")
	_ = remoteImportCmd.MarkFlagRequired("action")

	var unimportRemote, unimportAction string
	remoteUnimportCmd := &cobra.Command{
		Use:   "unimport",
		Short: "Deactivate a local proxy action without deleting history",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runRemoteUnimport(unimportRemote, unimportAction)
		},
	}
	remoteUnimportCmd.Flags().StringVar(&unimportRemote, "remote", "", "Remote kernel handle (required)")
	remoteUnimportCmd.Flags().StringVar(&unimportAction, "action", "", "Action name to unimport (required)")
	_ = remoteUnimportCmd.MarkFlagRequired("remote")
	_ = remoteUnimportCmd.MarkFlagRequired("action")

	remoteCmd.AddCommand(remoteAddCmd, remoteListCmd, remoteImportCmd, remoteUnimportCmd)
	rootCmd.AddCommand(remoteCmd)
}

func runRemoteAdd(baseURLArg string) error {
	baseURL := strings.TrimRight(baseURLArg, "/")
	return withSuperuser(func(k *kernel.Kernel, operatorID string) error {
		ctx := context.Background()

		wellKnownReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/.well-known/juice-kernel.json", nil)
		if err != nil {
			return fmt.Errorf("invalid remote URL: %w", err)
		}
		resp, err := newHTTPClient(30*time.Second, false).Do(wellKnownReq)
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

		parsed, err := url.Parse(meta.BaseURL)
		if err != nil || parsed.Host == "" {
			return fmt.Errorf("invalid base URL %q", meta.BaseURL)
		}
		localHandle := "@" + parsed.Host

		u, err := k.CreateOrUpdateProxyPeer(ctx, localHandle, meta.PublicKey, meta.BaseURL)
		if err != nil {
			return fmt.Errorf("register remote kernel: %w", err)
		}
		fmt.Printf("Registered remote kernel %s (id=%s, base_url=%s)\n", u.Handle, u.ID, u.RemoteBaseURL)
		return nil
	})
}

func runRemoteList(_ *cobra.Command, _ []string) error {
	return withSuperuser(func(k *kernel.Kernel, _ string) error {
		peers, err := k.ListPeers(context.Background())
		if err != nil {
			return err
		}
		if len(peers) == 0 {
			fmt.Println("No remote kernels registered.")
			return nil
		}
		fmt.Printf("%-20s %-36s %s\n", "HANDLE", "ID", "BASE_URL")
		for _, u := range peers {
			fmt.Printf("%-20s %-36s %s\n", u.Handle, u.ID, u.RemoteBaseURL)
		}
		return nil
	})
}

func runRemoteImport(remoteHandle, actionName string) error {
	ctx := context.Background()
	return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
		remoteUser, err := k.ReadUserByHandle(ctx, remoteHandle)
		if err != nil {
			return fmt.Errorf("remote kernel %q not found; run 'juice remote add' first", remoteHandle)
		}
		if remoteUser.RemoteBaseURL == "" {
			return fmt.Errorf("%q is not a remote kernel", remoteHandle)
		}

		base := strings.TrimRight(remoteUser.RemoteBaseURL, "/")

		// Discover the action ID by listing (filter by name only; owner on the remote
		// is the remote kernel's own @sys user, not the local alias we use for it).
		listReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/v1/actions?name=%s", base, url.QueryEscape(actionName)), nil)
		if err != nil {
			return fmt.Errorf("invalid remote URL: %w", err)
		}
		resp, err := newHTTPClient(30*time.Second, false).Do(listReq)
		if err != nil {
			return fmt.Errorf("fetch action list: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("remote kernel returned %d: %s", resp.StatusCode, body)
		}
		var actions []struct{ ID, Name string }
		if err := json.Unmarshal(body, &actions); err != nil {
			return fmt.Errorf("parse action list: %w", err)
		}
		actionID := ""
		for _, a := range actions {
			if a.Name == actionName {
				actionID = a.ID
				break
			}
		}
		if actionID == "" {
			result, err := k.ReconcileRemoteAction(ctx, subjectID, remoteHandle, actionName, nil)
			if err != nil {
				return fmt.Errorf("action %q not found on remote kernel %q and no local proxy to deactivate: %w", actionName, remoteHandle, err)
			}
			_ = result
			fmt.Printf("Remote action %q no longer available; deactivated local proxy\n", actionName)
			return nil
		}

		manifestReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
			fmt.Sprintf("%s/v1/actions/%s/manifest", base, actionID), nil)
		if err != nil {
			return fmt.Errorf("invalid manifest URL: %w", err)
		}
		resp2, err := newHTTPClient(30*time.Second, false).Do(manifestReq)
		if err != nil {
			return fmt.Errorf("fetch manifest: %w", err)
		}
		defer resp2.Body.Close()
		body2, _ := io.ReadAll(resp2.Body)

		// Non-200 means the action lost active/public state; deactivate local proxy.
		if resp2.StatusCode != http.StatusOK {
			result, reconcileErr := k.ReconcileRemoteAction(ctx, subjectID, remoteHandle, actionName, nil)
			if reconcileErr != nil {
				return fmt.Errorf("manifest unavailable (status %d) and could not deactivate local proxy: %w", resp2.StatusCode, reconcileErr)
			}
			_ = result
			fmt.Printf("Remote action %q manifest unavailable (status %d); deactivated local proxy\n", actionName, resp2.StatusCode)
			return nil
		}

		var m kernel.ActionManifest
		if err := json.Unmarshal(body2, &m); err != nil {
			return fmt.Errorf("parse manifest: %w", err)
		}
		if err := kernel.VerifyManifestSignature(remoteUser.PublicKey, &m); err != nil {
			return fmt.Errorf("manifest signature invalid: %w", err)
		}

		result, err := k.ReconcileRemoteAction(ctx, subjectID, remoteHandle, actionName, &m)
		if err != nil {
			return fmt.Errorf("import action: %w", err)
		}
		switch {
		case len(result.Created) > 0:
			fmt.Printf("Imported action %s (id=%s)\n", result.Created[0].Name, result.Created[0].ID)
		case len(result.Updated) > 0:
			fmt.Printf("Updated action %s (id=%s, deactivated for review)\n", result.Updated[0].Name, result.Updated[0].ID)
		case len(result.Unchanged) > 0:
			fmt.Printf("Action %s unchanged (id=%s)\n", result.Unchanged[0].Name, result.Unchanged[0].ID)
		}
		return nil
	})
}

func runRemoteUnimport(remoteHandle, actionName string) error {
	ctx := context.Background()
	return withSuperuser(func(k *kernel.Kernel, subjectID string) error {
		a, err := k.UnimportRemoteAction(ctx, subjectID, remoteHandle, actionName)
		if err != nil {
			return fmt.Errorf("unimport action: %w", err)
		}
		fmt.Printf("Deactivated action %s (id=%s)\n", a.Name, a.ID)
		return nil
	})
}
