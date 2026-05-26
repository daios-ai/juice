package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"golang.org/x/term"
)

const configKeySuperuser = "superuser_handle"

// bootstrap runs idempotent startup tasks before the server accepts requests.
// On first boot (no superuser configured), it prompts for credentials interactively.
func bootstrap(k *kernel.Kernel) error {
	ctx := context.Background()

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		// First boot: prompt for superuser credentials.
		handle, err = firstBoot(ctx, k)
		if err != nil {
			return err
		}
	}

	// Register @sys/lookup native action if absent.
	if err := ensureSysLookup(ctx, k, handle); err != nil {
		return err
	}
	return nil
}

func firstBoot(ctx context.Context, k *kernel.Kernel) (string, error) {
	fmt.Println("First boot: no superuser configured.")

	reader := bufio.NewReader(os.Stdin)
	fmt.Print("Superuser handle: ")
	handle, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("reading handle: %w", err)
	}
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return "", fmt.Errorf("handle cannot be empty")
	}

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

	_, err = k.CreateUser(ctx, kernel.CreateUserRequest{
		Handle:   handle,
		Email:    handle + "@sys",
		Password: password,
	})
	if err != nil {
		return "", fmt.Errorf("create superuser: %w", err)
	}

	if err := k.SetConfig(ctx, configKeySuperuser, handle); err != nil {
		return "", fmt.Errorf("store superuser handle: %w", err)
	}

	fmt.Printf("Superuser %q created.\n", handle)
	return handle, nil
}

func ensureSysLookup(ctx context.Context, k *kernel.Kernel, superuserHandle string) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}

	actionName := "/lookup"
	a, err := k.ReadActionByOwnerName(ctx, su.ID, actionName)
	if err == nil && a != nil {
		// Already registered — ensure grant-all is set.
		_ = k.GrantAll(ctx, su.ID, a.ID)
		return nil
	}

	// Create the native lookup action.
	a, err = k.CreateAction(ctx, kernel.CreateActionRequest{
		OwnerUserID:  su.ID,
		Name:         actionName,
		Kind:         kernel.KindNative,
		Price:        0,
		Description:  "Semantic search over active actions",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}, "limit": map[string]any{"type": "number"}}},
		OutputSchema: map[string]any{"type": "object"},
	})
	if err != nil {
		return fmt.Errorf("create @sys/lookup: %w", err)
	}

	if err := k.SetActive(ctx, su.ID, a.ID, true); err != nil {
		return fmt.Errorf("activate @sys/lookup: %w", err)
	}

	if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
		return fmt.Errorf("grant-all @sys/lookup: %w", err)
	}

	return nil
}
