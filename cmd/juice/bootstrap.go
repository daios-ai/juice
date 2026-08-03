package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/daios-ai/juice/kernel"
	"golang.org/x/term"
)

const (
	configKeySuperuser      = "superuser_handle"
	configKeySigningPublic  = "signing_public_key"
	configKeySigningPrivate = "signing_private_key"
	superuserHandle         = "sys"
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
		// The kernel's network name is required at first boot — it's how the kernel presents itself
		// to peers. From JUICE_BOOTSTRAP_KERNEL_HANDLE, else prompt until a non-empty name is given.
		if globalCfg.KernelHandle == "" {
			ph := os.Getenv("JUICE_BOOTSTRAP_KERNEL_HANDLE")
			for ph == "" && term.IsTerminal(int(os.Stdin.Fd())) {
				fmt.Fprint(os.Stderr, "Kernel name — the @handle this kernel presents to the network (required): ")
				var line string
				fmt.Fscanln(os.Stdin, &line)
				ph = strings.TrimSpace(line)
			}
			if ph == "" {
				return fmt.Errorf("kernel name is required at first boot: run interactively or set JUICE_BOOTSTRAP_KERNEL_HANDLE")
			}
			globalCfg.KernelHandle = kernel.NormalizeHandle(ph)
			_ = writeConfig(resolvedConfigPath, globalCfg)
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

	// Fallback for a pre-existing DB that predates the required-name boot and still has no handle:
	// a distinct key-derived name, never the shared "@sys". Fresh boots always set a name above.
	if globalCfg.KernelHandle == "" {
		globalCfg.KernelHandle = "@k-" + pubKeyB64[:8]
		_ = writeConfig(resolvedConfigPath, globalCfg)
	}

	// Load the signing key and issuer user ID into the kernel.
	su, err := k.ReadUserByHandle(ctx, handle)
	if err != nil {
		return fmt.Errorf("read superuser: %w", err)
	}
	k.SetSigningKey(ed25519.PrivateKey(privKeyBytes), su.ID)

	// Persist the kernel handle so GetGossip serves it from the DB.
	_ = k.SetConfig(ctx, "kernel_handle", globalCfg.KernelHandle)

	// One-time migration to the Connection model (§8): re-home any legacy per-grant tokens onto
	// their derived connections. Idempotent — a fully-migrated store finds nothing to do.
	if err := k.BackfillGrantConnections(ctx); err != nil {
		return fmt.Errorf("backfill grant connections: %w", err)
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
	// Announce the location loudly: a first boot mints a NEW kernel identity and signing
	// key, so an operator who launched against the wrong DB path (a fresh, unintended
	// federation identity) sees it here — including in headless mode, before any prompt.
	loc := flagDB
	if abs, err := filepath.Abs(flagDB); err == nil {
		loc = abs
	}
	fmt.Fprintf(os.Stderr, "First boot: creating a NEW kernel — new identity and signing key — at %s\n", loc)

	password := os.Getenv("JUICE_BOOTSTRAP_PASSWORD")
	if password == "" {
		p, err := promptNewPassword("Superuser password: ")
		if err != nil {
			return "", fmt.Errorf("reading password: %w", err)
		}
		password = p
	}
	if password == "" {
		return "", fmt.Errorf("password cannot be empty")
	}

	// Enroll @sys's own recovery phrase (§12): generated here, only the public key is stored, so the
	// operator can reset the superuser password if it is lost. The phrase is shown once.
	mnemonic, recoveryPub, err := generateRecovery()
	if err != nil {
		return "", fmt.Errorf("generate recovery phrase: %w", err)
	}
	if err := k.FirstBoot(ctx, password, recoveryPub); err != nil {
		return "", fmt.Errorf("first boot: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Superuser %q created.\n", superuserHandle)
	fmt.Fprintln(os.Stderr, "@sys recovery phrase (write this down; it is shown only once and cannot be recovered):")
	fmt.Fprintf(os.Stderr, "  %s\n", mnemonic)
	return superuserHandle, nil
}

// sysNativeSpec describes one @sys native action to register during bootstrap.
type sysNativeSpec struct {
	name         string
	price        int64
	effect       string // privileged execution effect ("transfer"); empty for an ordinary native (§13)
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
			Effect:      spec.effect,
		})
		if err != nil {
			return fmt.Errorf("create @sys/%s: %w", spec.name, err)
		}
	}
	if err := k.ActivateNativeAction(ctx, a.ID, spec.description, spec.inputSchema, spec.outputSchema, spec.price, spec.effect); err != nil {
		return fmt.Errorf("activate @sys/%s: %w", spec.name, err)
	}
	return nil
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
								"action_id":     map[string]any{"type": "string", "description": "Unique action identifier"},
								"action":        map[string]any{"type": "string", "description": "Action reference as @owner/name"},
								"description":   map[string]any{"type": "string", "description": "Human-readable description of the action"},
								"score":         map[string]any{"type": "number", "description": "Relevance score between 0 and 1"},
								"input_schema":  map[string]any{"type": "object", "description": "JSON Schema for the action's input"},
								"output_schema": map[string]any{"type": "object", "description": "JSON Schema for the action's output"},
							},
						},
					},
				},
			},
		},
		{
			name:        "user-lookup",
			price:       cfg.UserLookup.Price,
			description: "Semantic search over local and discovered users",
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
						"description": "Ranked list of matching users",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"principal_id":      map[string]any{"type": "object", "description": "Stable identity: kernel_public_key + user_id"},
								"reference":         map[string]any{"type": "string", "description": "Display/use form: handle@<kernel-key> or a local handle"},
								"handle":            map[string]any{"type": "string", "description": "The user's handle on its home kernel"},
								"description":       map[string]any{"type": "string", "description": "The user's self-description"},
								"kernel_public_key": map[string]any{"type": "string", "description": "The home kernel's public key (empty for a local user)"},
								"score":             map[string]any{"type": "number", "description": "Relevance score"},
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
					"messages": map[string]any{"type": "array", "description": "Conversation turns (system/user/assistant/tool)", "items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"role":    map[string]any{"type": "string", "description": "Message role: system, user, assistant, or tool"},
							"content": map[string]any{"type": "string", "description": "Text content of the message"},
							"tool": map[string]any{
								"type":        "object",
								"description": "Tool action and result; present on assistant proposal and tool result turns",
								"properties": map[string]any{
									"action": map[string]any{"type": "string", "description": "Juice action reference (@owner/name)"},
									"args":   map[string]any{"type": "object", "description": "Arguments for the action"},
									"result": map[string]any{"type": "object", "description": "Result from the action execution"},
								},
							},
						},
						"required": []string{"role"},
					}},
					"actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Candidate actions as @owner/name strings"},
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
			name:         "sink",
			price:        cfg.Sink.Price,
			description:  "Universal no-op sink; accepts any input and returns {}",
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
			name:        "random",
			price:       cfg.Random.Price,
			description: "Returns a cryptographically secure random float in [0, 1)",
			inputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			outputSchema: map[string]any{"type": "object", "properties": map[string]any{
				"value": map[string]any{"type": "number", "description": "Random float in [0, 1)"},
			}},
		},
		{
			name:        "web",
			price:       cfg.Web.Price,
			description: "Fetch a public web page (read-only HTTP GET); returns status, body, content type, and final URL",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url": map[string]any{"type": "string", "description": "Public URL to fetch; a scheme-less URL defaults to https"},
				},
				"required": []string{"url"},
			},
			outputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":       map[string]any{"type": "integer", "description": "HTTP response status code"},
					"body":         map[string]any{"type": "string", "description": "Response body"},
					"content_type": map[string]any{"type": "string", "description": "Response Content-Type header"},
					"final_url":    map[string]any{"type": "string", "description": "Final URL fetched, after scheme defaulting and redirects"},
				},
			},
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
		{
			name:        "transfer",
			price:       cfg.Transfer.Price,
			effect:      "transfer",
			description: "Transfers credits from the caller to another user. The target may be local (a handle) or a remote transfer action (sys@<kernel>/transfer with a local target on that kernel); a cross-kernel transfer settles through the federation receipt/exposure system (§13). The value is funded from the immediate caller's own balance and delivered by a deferred, receipt-backed transfer effect.",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"target": map[string]any{"type": "string", "description": "Recipient: a local handle, or (when calling sys@<kernel>/transfer) a bare handle on that kernel"},
					"amount": map[string]any{"type": "integer", "description": "Amount of credits to transfer (positive integer)"},
				},
				"required": []string{"target", "amount"},
			},
			outputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"amount": map[string]any{"type": "integer", "description": "Amount transferred"},
				},
				"required": []string{"amount"},
			},
		},
		{
			name:        "tinygo/compile",
			price:       cfg.TinyGo.Price,
			description: "Compiles TinyGo source (a Handle function written against the Juice SDK) to a WASM artifact, ready to register with action create --kind wasm --artifact",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"source": map[string]any{"type": "string", "description": "TinyGo source: a func Handle(in map[string]any) (map[string]any, error) plus any private helpers; the SDK (package, imports, alloc, run, main) is prepended automatically"},
				},
				"required": []string{"source"},
			},
			outputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status":        map[string]any{"type": "string", "description": "success or failure"},
					"artifact":      map[string]any{"type": "string", "description": "Base64-encoded WASM artifact, present on success"},
					"artifact_hash": map[string]any{"type": "string", "description": "SHA-256 hex of the artifact, present on success"},
					"diagnostics":   map[string]any{"type": "array", "description": "Compile/validation diagnostics", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"status", "diagnostics"},
			},
		},
	}
}
