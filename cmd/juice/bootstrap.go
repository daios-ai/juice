package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"golang.org/x/term"
)

const (
	configKeySuperuser = "superuser_handle"
	superuserHandle    = "@sys"
)

// bootstrap runs idempotent startup tasks before the server accepts requests.
// On first boot (no superuser configured), it prompts for credentials interactively.
func bootstrap(k *kernel.Kernel) error {
	ctx := context.Background()

	// Reset any events that were left in-flight by a prior crash.
	if err := k.ResetInFlightEvents(ctx); err != nil {
		return fmt.Errorf("reset in-flight events: %w", err)
	}

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		// First boot: prompt for superuser credentials.
		handle, err = firstBoot(ctx, k)
		if err != nil {
			return err
		}
	}

	// Register /lookup native action if absent.
	if err := ensureSysLookup(ctx, k, handle); err != nil {
		return err
	}
	return nil
}

func firstBoot(ctx context.Context, k *kernel.Kernel) (string, error) {
	fmt.Println("First boot: no superuser configured.")

	fmt.Print("Superuser password: ")
	passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("reading password: %w", err)
	}
	password := strings.TrimSpace(string(passwordBytes))
	if password == "" {
		return "", fmt.Errorf("password cannot be empty")
	}

	_, err = k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle:   superuserHandle,
		Email:    "sys@sys",
		Password: password,
	}, configKeySuperuser)
	if err != nil {
		return "", fmt.Errorf("create superuser: %w", err)
	}

	fmt.Printf("Superuser %q created.\n", superuserHandle)
	return superuserHandle, nil
}

func ensureSysLookup(ctx context.Context, k *kernel.Kernel, superuserHandle string) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}

	actionName := "/lookup"
	a, err := k.ReadActionByOwnerName(ctx, su.ID, actionName)
	if err == nil && a != nil {
		// Already registered — ensure active and grant-all is set.
		if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
			return fmt.Errorf("activate @sys/lookup: %w", err)
		}
		_ = k.GrantAll(ctx, su.ID, a.ID)
		return nil
	}

	// Create the native lookup action.
	a, err = k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID:  su.ID,
		Name:         actionName,
		Kind:         kernel.KindNative,
		Price:        0,
		Description:  "Semantic search over active actions",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Semantic search query"},
				"limit": map[string]any{"type": "integer", "description": "Maximum number of results"},
			},
			"required": []string{"query"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"results": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action_id":    map[string]any{"type": "string"},
							"name":         map[string]any{"type": "string"},
							"owner_handle": map[string]any{"type": "string"},
							"description":  map[string]any{"type": "string"},
							"score":        map[string]any{"type": "number"},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create @sys/lookup: %w", err)
	}

	if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
		return fmt.Errorf("activate @sys/lookup: %w", err)
	}

	if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
		return fmt.Errorf("grant-all @sys/lookup: %w", err)
	}

	return nil
}
