package main

import (
	"context"
	"crypto/ed25519"
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

	// Verify both signing keys are present, valid, and consistent.
	privKeyB64, _ := k.GetConfig(ctx, configKeySigningPrivate)
	if privKeyB64 == "" {
		return fmt.Errorf("signing_private_key missing from config; re-run on a fresh database or restore the key")
	}
	privKeyBytes, err := base64.RawURLEncoding.DecodeString(privKeyB64)
	if err != nil || len(privKeyBytes) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing_private_key in config is invalid")
	}
	pubKeyB64, _ := k.GetConfig(ctx, configKeySigningPublic)
	if pubKeyB64 == "" {
		return fmt.Errorf("signing_public_key missing from config")
	}
	pubKeyBytes, err := base64.RawURLEncoding.DecodeString(pubKeyB64)
	if err != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
		return fmt.Errorf("signing_public_key in config is invalid")
	}
	derivedPub := ed25519.PrivateKey(privKeyBytes).Public().(ed25519.PublicKey)
	if !derivedPub.Equal(ed25519.PublicKey(pubKeyBytes)) {
		return fmt.Errorf("signing_public_key does not match signing_private_key")
	}

	// Load the signing key and issuer user ID into the kernel.
	su, err := k.ReadUserByHandle(ctx, handle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(privKeyBytes), su.ID, handle)

	// Register lookup native action if absent.
	if err := ensureSysLookup(ctx, k, handle); err != nil {
		return err
	}

	// Register llm-chat native action if absent.
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

	if err := k.FirstBoot(ctx, password); err != nil {
		return "", fmt.Errorf("first boot: %w", err)
	}

	fmt.Printf("Superuser %q created.\n", superuserHandle)
	return superuserHandle, nil
}

func ensureSysLookup(ctx context.Context, k *kernel.Kernel, superuserHandle string) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}

	actionName := "lookup"
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
		OwnerUserID: su.ID,
		Name:        actionName,
		Kind:        kernel.KindNative,
		Price:       0,
		Description: "Semantic search over active actions",
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
					"type":        "array",
					"description": "Ranked list of matching actions",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action_id":    map[string]any{"type": "string", "description": "Unique action identifier"},
							"name":         map[string]any{"type": "string", "description": "Action name"},
							"owner_handle": map[string]any{"type": "string", "description": "Handle of the action owner"},
							"description":  map[string]any{"type": "string", "description": "Human-readable description of the action"},
							"score":        map[string]any{"type": "number", "description": "Relevance score between 0 and 1"},
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

	actionName := "llm-chat"
	a, err := k.ReadActionByOwnerName(ctx, su.ID, actionName)
	if err == nil && a != nil {
		if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
			return fmt.Errorf("activate @sys/llm-chat: %w", err)
		}
		if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
			return fmt.Errorf("grant-all @sys/llm-chat: %w", err)
		}
		return nil
	}

	msgItemSchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"role":    map[string]any{"type": "string", "description": "Role of the message sender (user or assistant)"},
			"content": map[string]any{"type": "string", "description": "Text content of the message"},
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
					"type":        "object",
					"description": "Generated reply message",
					"properties": map[string]any{
						"role":    map[string]any{"type": "string", "description": "Role of the message sender (assistant)"},
						"content": map[string]any{"type": "string", "description": "Text content of the reply"},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create @sys/llm-chat: %w", err)
	}

	if err := k.ActivateNativeAction(ctx, a.ID); err != nil {
		return fmt.Errorf("activate @sys/llm-chat: %w", err)
	}

	if err := k.GrantAll(ctx, su.ID, a.ID); err != nil {
		return fmt.Errorf("grant-all @sys/llm-chat: %w", err)
	}

	return nil
}
