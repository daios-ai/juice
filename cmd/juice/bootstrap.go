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

	// Reset any steps that were left running by a prior crash.
	if err := k.ResetRunningSteps(ctx); err != nil {
		return fmt.Errorf("reset running steps: %w", err)
	}
	// Restore any process funds locked by calls that crashed before settlement.
	if err := k.ResetInFlightCalls(ctx); err != nil {
		return fmt.Errorf("reset in-flight calls: %w", err)
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
	k.SetSigningKey(ed25519.PrivateKey(privKeyBytes), su.ID)

	for _, spec := range sysNativeSpecs {
		if err := ensureSysNative(ctx, k, handle, spec); err != nil {
			return err
		}
	}

	return nil
}

func firstBoot(ctx context.Context, k *kernel.Kernel) (string, error) {
	fmt.Fprintln(os.Stderr, "First boot: no superuser configured.")

	password := os.Getenv("JUICE_BOOTSTRAP_PASSWORD")
	if password == "" {
		fmt.Fprint(os.Stderr, "Superuser password: ")
		passwordBytes, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
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

	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	return superuserHandle, nil
}

// sysNativeSpec describes one @sys native action to register during bootstrap.
type sysNativeSpec struct {
	name         string
	price        int64
	description  string
	inputSchema  map[string]any
	outputSchema map[string]any
}

// ensureSysNative idempotently registers, activates, and grants public call access to a
// @sys native action. If the action already exists, its description and schemas are always
// reconciled to the spec so that schema drift is corrected on every boot.
func ensureSysNative(ctx context.Context, k *kernel.Kernel, superuserHandle string, spec sysNativeSpec) error {
	su, err := k.ReadUserByHandle(ctx, superuserHandle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	a, err := k.ReadActionByOwnerName(ctx, su.ID, spec.name)
	if err != nil || a == nil {
		a, err = k.RegisterNativeAction(ctx, kernel.CreateActionRequest{
			OwnerUserID: su.ID,
			Name:        spec.name,
			Kind:        kernel.KindNative,
			Price:       spec.price,
		})
		if err != nil {
			return fmt.Errorf("create @sys/%s: %w", spec.name, err)
		}
	}
	if err := k.ActivateNativeAction(ctx, a.ID, spec.description, spec.inputSchema, spec.outputSchema); err != nil {
		return fmt.Errorf("activate @sys/%s: %w", spec.name, err)
	}
	return nil
}

var makeOutputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"status":      map[string]any{"type": "string", "description": "success or failure"},
		"action_id":   map[string]any{"type": "string", "description": "Registered action ID, present on success"},
		"action_name": map[string]any{"type": "string", "description": "Registered action name, present on success"},
		"diagnostics": map[string]any{"type": "array", "description": "Synthesis diagnostics", "items": map[string]any{"type": "string"}},
		"tests":       map[string]any{"type": "array", "description": "Test results from smoke-test execution", "items": map[string]any{"type": "object"}},
	},
	"required": []string{"status", "diagnostics"},
}

var msgItemSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"role":    map[string]any{"type": "string", "description": "Role of the message sender (user or assistant)"},
		"content": map[string]any{"type": "string", "description": "Text content of the message"},
	},
	"required": []string{"role", "content"},
}

var sysNativeSpecs = []sysNativeSpec{
	{
		name:        "lookup",
		price:       0,
		description: "Semantic search over active actions",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "Semantic search query"},
				"limit": map[string]any{"type": "integer", "description": "Maximum number of results"},
			},
			"required": []string{"query"},
		},
		outputSchema: map[string]any{
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
	},
	{
		name:        "llm/chat",
		price:       0,
		description: "Chat completion via the configured language model",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{"type": "array", "items": msgItemSchema, "description": "Conversation history"},
				"system":   map[string]any{"type": "string", "description": "Optional system prompt"},
			},
			"required": []string{"messages"},
		},
		outputSchema: map[string]any{
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
	},
	{
		name:         "make",
		price:        20,
		description:  "Generate a WASM action from a natural-language description",
		inputSchema:  map[string]any{"type": "object", "properties": map[string]any{"description": map[string]any{"type": "string", "description": "Natural-language description of the action to generate"}}, "required": []string{"description"}},
		outputSchema: makeOutputSchema,
	},
	{
		name:        "time",
		price:       0,
		description: "Returns the current UTC time",
		inputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		outputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"unix": map[string]any{"type": "integer", "description": "Seconds since UTC epoch"},
				"iso":  map[string]any{"type": "string", "description": "RFC 3339 timestamp"},
			},
		},
	},
	{
		name:        "message",
		price:       0,
		description: "Sends a message to another platform user and creates a Step they must complete",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":           map[string]any{"type": "string", "description": "Recipient handle (@owner)"},
				"message":      map[string]any{"type": "string", "description": "Message body"},
				"next_action":  map[string]any{"type": "string", "description": "Action ref (@owner/name) to call when the recipient completes the step"},
				"subject":      map[string]any{"type": "string", "description": "Optional notification subject"},
				"partial_args": map[string]any{"type": "object", "description": "Optional pre-filled args merged at step completion"},
			},
			"required": []string{"to", "message", "next_action"},
		},
		outputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step_id":   map[string]any{"type": "string", "description": "ID of the created step"},
				"delivered": map[string]any{"type": "boolean", "description": "Whether the notification was delivered"},
			},
		},
	},
}
