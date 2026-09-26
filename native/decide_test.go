// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubDecideChatter struct {
	call *kernel.ToolCall
	msg  *kernel.ChatMessage
	err  error
}

func (s *stubDecideChatter) ChatDecide(_ context.Context, _ []kernel.DecideMessage, _ []kernel.ToolDefinition) (*kernel.ToolCall, *kernel.ChatMessage, error) {
	return s.call, s.msg, s.err
}

var (
	lookupOK = func(_ context.Context, _, _ string) (*kernel.Action, error) {
		return &kernel.Action{
			Description: "search actions",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{"type": "string"},
				},
				"required": []any{"query"},
			},
			Price: 0,
		}, nil
	}

	// resolveStub stands in for the kernel-qualified resolve path; bare-ref decide tests never call it.
	resolveStub = func(_ context.Context, _ string) (*kernel.Action, error) {
		return nil, kernel.ErrNotFound.Wrap("no remote resolve in test")
	}

	// remoteStub stands in for the kernel's own answer: an address on a kernel other than `k`.
	remoteStub = func(_ context.Context, ref string) bool {
		a, err := kernel.ParseAddress(ref)
		return err == nil && a.Kernel != "k"
	}

	validDecideArgs = map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "find something"}},
		"actions":  []any{"sys@k/lookup"},
	}
)

func TestExecuteDecide_NilChatter(t *testing.T) {
	_, err := executeDecide(context.Background(), validDecideArgs, nil, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestExecuteDecide_MissingMessages(t *testing.T) {
	_, err := executeDecide(context.Background(), map[string]any{
		"actions": validDecideArgs["actions"],
	}, &stubDecideChatter{}, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteDecide_MissingActions(t *testing.T) {
	_, err := executeDecide(context.Background(), map[string]any{
		"messages": validDecideArgs["messages"],
	}, &stubDecideChatter{}, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

// TestExecuteDecide_InvalidActionRef: decide holds no reference grammar of its own — a candidate
// travels whole to the resolver, and whatever the resolver says about a bad one aborts selection.
// A slashless candidate is no longer malformed: it names an owner's root (§13).
func TestExecuteDecide_InvalidActionRef(t *testing.T) {
	lookupBad := func(_ context.Context, _, _ string) (*kernel.Action, error) {
		return nil, kernel.ErrInvalidInput.Wrap("action ref must be owner[@kernel]/name")
	}
	_, err := executeDecide(context.Background(), map[string]any{
		"messages": validDecideArgs["messages"],
		"actions":  []any{"@sigil/greet"},
	}, &stubDecideChatter{}, lookupBad, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput, got %v", err)
	}
}

func TestExecuteDecide_UnknownAction(t *testing.T) {
	lookupErr := func(_ context.Context, _, _ string) (*kernel.Action, error) {
		return nil, kernel.ErrNotFound.Wrap("action not found")
	}
	_, err := executeDecide(context.Background(), validDecideArgs, &stubDecideChatter{}, lookupErr, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestExecuteDecide_NoSelection(t *testing.T) {
	_, err := executeDecide(context.Background(), validDecideArgs, &stubDecideChatter{call: nil}, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_UnknownActionReturned(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{Action: "sys/unknown", Args: map[string]any{"query": "x"}}}
	_, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_BadArgsReturned(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys@k/lookup",
		Args:   map[string]any{"query": 123}, // should be string
	}}
	_, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, remoteStub, "")
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}

func TestExecuteDecide_Success(t *testing.T) {
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys@k/lookup",
		Args:   map[string]any{"query": "test"},
	}}
	result, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, remoteStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["action"] != "sys@k/lookup" {
		t.Errorf("unexpected action: %v", result["action"])
	}
	args, ok := result["args"].(map[string]any)
	if !ok || args["query"] != "test" {
		t.Errorf("unexpected args: %v", result["args"])
	}
	if _, hasMsg := result["message"]; hasMsg {
		t.Errorf("expected no message in result")
	}
}

func TestExecuteDecide_MessageIncluded(t *testing.T) {
	chatter := &stubDecideChatter{
		call: &kernel.ToolCall{Action: "sys@k/lookup", Args: map[string]any{"query": "x"}},
		msg:  &kernel.ChatMessage{Role: "assistant", Content: "I'll look that up"},
	}
	result, err := executeDecide(context.Background(), validDecideArgs, chatter, lookupOK, resolveStub, remoteStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m, ok := result["message"].(map[string]any)
	if !ok || m["content"] != "I'll look that up" {
		t.Errorf("expected message in result, got %v", result["message"])
	}
}

func TestExecuteDecide_ToolTurn(t *testing.T) {
	args := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "find something"},
			map[string]any{
				"role": "assistant",
				"tool": map[string]any{"action": "sys@k/lookup", "args": map[string]any{"query": "x"}},
			},
			map[string]any{
				"role": "tool",
				"tool": map[string]any{"action": "sys@k/lookup", "result": map[string]any{"results": []any{}}},
			},
		},
		"actions": []any{"sys@k/lookup"},
	}
	chatter := &stubDecideChatter{call: &kernel.ToolCall{
		Action: "sys@k/lookup",
		Args:   map[string]any{"query": "refined"},
	}}
	result, err := executeDecide(context.Background(), args, chatter, lookupOK, resolveStub, remoteStub, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["action"] != "sys@k/lookup" {
		t.Errorf("unexpected action: %v", result["action"])
	}
}

// TestExecuteDecide_RootCandidates: a candidate travels whole to the resolver, so an owner root is
// a valid candidate at both depths — bob locally, bob@kernel remotely — and decide holds no
// grammar that could reject one before the resolver sees it (§13).
func TestExecuteDecide_RootCandidates(t *testing.T) {
	var seenLocal, seenRemote []string
	lookupRoot := func(_ context.Context, ref, _ string) (*kernel.Action, error) {
		seenLocal = append(seenLocal, ref)
		return &kernel.Action{Name: "index", Description: "the group", InputSchema: map[string]any{"type": "object"}}, nil
	}
	resolveRoot := func(_ context.Context, ref string) (*kernel.Action, error) {
		seenRemote = append(seenRemote, ref)
		return &kernel.Action{Name: "index", Description: "a remote group", InputSchema: map[string]any{"type": "object"}}, nil
	}
	chatter := &stubDecideChatter{call: &kernel.ToolCall{Action: "bob", Args: map[string]any{}}}

	if _, err := executeDecide(context.Background(), map[string]any{
		"messages": validDecideArgs["messages"],
		"actions":  []any{"bob", "carol@kernelkey/mail"},
	}, chatter, lookupRoot, resolveRoot, remoteStub, ""); err != nil {
		t.Fatalf("root candidates: %v", err)
	}
	if len(seenLocal) != 1 || seenLocal[0] != "bob" {
		t.Errorf("local candidate must travel whole: got %v", seenLocal)
	}
	if len(seenRemote) != 1 || seenRemote[0] != "carol@kernelkey/mail" {
		t.Errorf("kernel-qualified candidate must reach the resolver: got %v", seenRemote)
	}
}
