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
func bootstrap(k *kernel.Kernel, nativeCfg NativeConfig) error {
	ctx := context.Background()

	handle, err := k.GetConfig(ctx, configKeySuperuser)
	if err != nil || handle == "" {
		handle, err = firstBoot(ctx, k)
		if err != nil {
			return err
		}
		if globalCfg.PeerHandle == "" {
			ph := os.Getenv("JUICE_BOOTSTRAP_PEER_HANDLE")
			if ph == "" && term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Fprint(os.Stderr, "Kernel peer handle (e.g. @myorg, or Enter to skip): ")
				var line string
				fmt.Fscanln(os.Stdin, &line)
				ph = strings.TrimSpace(line)
			}
			if ph != "" {
				globalCfg.PeerHandle = ph
				_ = writeConfig(resolvedConfigPath, globalCfg)
			}
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

	// Persist peer identity so GetGossip can serve them from the DB.
	if globalCfg.PeerHandle != "" {
		_ = k.SetConfig(ctx, "kernel_handle", globalCfg.PeerHandle)
	}
	if globalCfg.ServerURL != "" {
		_ = k.SetConfig(ctx, "kernel_base_url", globalCfg.ServerURL)
	}

	// Recover interrupted calls and re-park crashed step completions (after signing key is set).
	if err := k.Recover(ctx); err != nil {
		return fmt.Errorf("recover: %w", err)
	}

	// Retry any remote proxy calls that were pending at last shutdown.
	k.RetryPendingRemoteDispatches(ctx)

	for _, spec := range buildSysNativeSpecs(nativeCfg) {
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
	if err := k.ActivateNativeAction(ctx, a.ID, spec.description, spec.inputSchema, spec.outputSchema, spec.price); err != nil {
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

func buildSysNativeSpecs(cfg NativeConfig) []sysNativeSpec {
	return []sysNativeSpec{
	{
		name:        "lookup",
		price:       cfg.Lookup.Price,
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
		price:       cfg.LLM.Price,
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
		name:        "llm/json",
		price:       cfg.LLM.Price,
		description: "Structured JSON output from the configured language model, locally validated against a schema",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages":      map[string]any{"type": "array", "items": msgItemSchema, "description": "Conversation history"},
				"system":        map[string]any{"type": "string", "description": "Optional system prompt"},
				"output_schema": map[string]any{"type": "object", "description": "JSON Schema the model output must satisfy"},
			},
			"required": []string{"messages", "output_schema"},
		},
		outputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"value": map[string]any{"type": "object", "description": "JSON value conforming to output_schema"},
			},
		},
	},
	{
		name:        "llm/decide",
		price:       cfg.LLM.Price,
		description: "LLM-driven action selection; returns chosen action and args without executing",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Conversation turns (user/assistant/tool)"},
				"actions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Candidate actions as @owner/name strings"},
			},
			"required": []string{"messages", "actions"},
		},
		outputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":  map[string]any{"type": "string", "description": "Selected @owner/name"},
				"args":    map[string]any{"type": "object", "description": "Arguments for the selected action"},
				"message": map[string]any{"type": "object", "description": "Optional text message from the model"},
			},
			"required": []string{"action", "args"},
		},
	},
	{
		name:         "make",
		price:        cfg.Make.Price,
		description:  "Generate a WASM action from a natural-language description",
		inputSchema:  map[string]any{"type": "object", "properties": map[string]any{"description": map[string]any{"type": "string", "description": "Natural-language description of the action to generate"}}, "required": []string{"description"}},
		outputSchema: makeOutputSchema,
	},
	{
		name:        "time",
		price:       cfg.Time.Price,
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
		name:        "sink",
		price:       cfg.Sink.Price,
		description: "Universal no-op sink; accepts any input and returns {}",
		inputSchema:  map[string]any{"type": "object"},
		outputSchema: map[string]any{"type": "object"},
	},
	{
		name:        "llm/embed",
		price:       cfg.LLM.Price,
		description: "Returns a text embedding vector from the configured embedding model",
		inputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"text": map[string]any{"type": "string", "description": "Text to embed"},
		}, "required": []string{"text"}},
		outputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"embedding": map[string]any{"type": "array", "description": "Embedding vector", "items": map[string]any{"type": "number"}},
		}},
	},
	{
		name:         "random",
		price:        cfg.Random.Price,
		description:  "Returns a cryptographically secure random float in [0, 1)",
		inputSchema:  map[string]any{"type": "object", "properties": map[string]any{}},
		outputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"value": map[string]any{"type": "number", "description": "Random float in [0, 1)"},
		}},
	},
	{
		name:        "message",
		price:       cfg.Message.Price,
		description: "Sends a message to another platform user and creates a Step they must acknowledge",
		inputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":      map[string]any{"type": "string", "description": "Recipient handle (@owner)"},
				"message": map[string]any{"type": "string", "description": "Message body"},
			},
			"required": []string{"to", "message"},
		},
		outputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step_id": map[string]any{"type": "string", "description": "ID of the created step"},
			},
		},
	},
	}
}
