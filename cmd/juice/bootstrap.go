package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"golang.org/x/term"
)

const (
	configKeySuperuser      = "superuser_handle"
	configKeySigningPublic  = "signing_public_key"
	configKeySigningPrivate = "signing_private_key"
	superuserHandle         = "@sys"
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
		handle, err = firstBoot(ctx, k)
		if err != nil {
			return err
		}
	}

	// Verify signing key is present (required after first boot).
	privKeyB64, _ := k.GetConfig(ctx, configKeySigningPrivate)
	if privKeyB64 == "" {
		return fmt.Errorf("signing_private_key missing from config; re-run on a fresh database or restore the key")
	}
	privKeyBytes, err := base64.RawURLEncoding.DecodeString(privKeyB64)
	if err != nil || len(privKeyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing_private_key in config is invalid")
	}

	// Load the signing key and issuer user ID into the kernel.
	su, err := k.ReadUserByHandle(ctx, handle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(privKeyBytes), su.ID, handle)

	// Register /lookup native action if absent.
	if err := ensureSysLookup(ctx, k, handle); err != nil {
		return err
	}

	// Register /llm/chat native action if absent.
	if err := ensureSysLLMChat(ctx, k, handle); err != nil {
		return err
	}

	return nil
}

func firstBoot(ctx context.Context, k *kernel.Kernel) (string, error) {
	fmt.Println("First boot: no superuser configured.")

	password := os.Getenv("JUICE_BOOTSTRAP_PASSWORD")
	if password == "" {
		fmt.Print("Superuser password: ")
		passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		password = strings.TrimSpace(string(passwordBytes))
	}
	if password == "" {
		return "", fmt.Errorf("password cannot be empty")
	}

	if _, err := k.BootstrapSuperuser(ctx, kernel.CreateUserRequest{
		Handle:   superuserHandle,
		Email:    "sys@sys",
		Password: password,
	}, configKeySuperuser); err != nil {
		return "", fmt.Errorf("create superuser: %w", err)
	}

	// Generate Ed25519 signing keypair atomically with first boot.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("generate signing key: %w", err)
	}
	if err := k.SetConfig(ctx, configKeySigningPublic, base64.RawURLEncoding.EncodeToString(pub)); err != nil {
		return "", fmt.Errorf("store signing public key: %w", err)
	}
	if err := k.SetConfig(ctx, configKeySigningPrivate, base64.RawURLEncoding.EncodeToString(priv)); err != nil {
		return "", fmt.Errorf("store signing private key: %w", err)
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
		if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
			return fmt.Errorf("grant-all @sys/lookup: %w", err)
		}
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

func ensureSysLLMChat(ctx context.Context, k *kernel.Kernel, superuserHandle string) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}

	actionName := "/llm/chat"
	a, err := k.ReadActionByOwnerName(ctx, su.ID, actionName)
	if err == nil && a != nil {
		if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
			return fmt.Errorf("activate @sys/llm/chat: %w", err)
		}
		if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
			return fmt.Errorf("grant-all @sys/llm/chat: %w", err)
		}
		return nil
	}

	msgItemSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"role":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		},
		"required": []string{"role", "content"},
	}
	a, err = k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
		OwnerUserID: su.ID,
		Name:        actionName,
		Kind:        kernel.KindNative,
		Price:       0,
		Description: "Chat completion via the configured language model",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{"type": "array", "items": msgItemSchema, "description": "Conversation history"},
				"system":   map[string]any{"type": "string", "description": "Optional system prompt"},
			},
			"required": []string{"messages"},
		},
		OutputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"role":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create @sys/llm/chat: %w", err)
	}

	if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
		return fmt.Errorf("activate @sys/llm/chat: %w", err)
	}

	if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
		return fmt.Errorf("grant-all @sys/llm/chat: %w", err)
	}

	return nil
}
