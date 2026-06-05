package kernel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daios-ai/juice/kernel"
	"github.com/daios-ai/juice/llm"
	"github.com/google/uuid"
)

func newChatKernel(t *testing.T, chatter kernel.Chatter) (*kernel.Kernel, kernel.Store) {
	t.Helper()
	st := newTestStore(t)
	k := newTestKernel(st)
	if chatter != nil {
		cfg := kernel.DefaultConfig()
		cfg.TokenSecret = "test-secret"
		cfg.IssuerUserID = testIssuerUserID
		cfg.SigningKey = testSigningKey()
		k = kernel.New(st, nil, nil, nil, chatter, cfg, nil)
	}
	kernel.RegisterChatHandler(k)
	return k, st
}

func seedChatAction(t *testing.T, st kernel.Store, ownerID string) *kernel.Action {
	t.Helper()
	a := &kernel.Action{
		ID: uuid.New().String(), OwnerUserID: ownerID, Name: "llm/chat",
		Kind: kernel.KindNative, Active: true, Public: true, Price: 0, Source: "native",
		Description: "Chat completion",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"messages": map[string]any{"type": "array", "description": "messages", "items": map[string]any{"type": "object"}},
			},
			"required": []string{"messages"},
		},
		OutputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"message": map[string]any{"type": "object", "description": "reply"}},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := st.CreateAction(context.Background(), a); err != nil {
		t.Fatalf("seedChatAction: %v", err)
	}
	return a
}

func TestChatHandlerMissingMessages(t *testing.T) {
	k, st := newChatKernel(t, &llm.FakeChatter{})
	ctx := context.Background()
	sys := setupUser(t, st, "@sys", 0)
	seedChatAction(t, st, sys.ID)
	caller := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "llm/chat",
		Args: map[string]any{},
	})
	if !errors.Is(err, kernel.ErrSchemaViolation) && !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected schema/invalid-input error for missing messages, got %v", err)
	}
}

func TestChatHandlerNilChatter(t *testing.T) {
	k, st := newChatKernel(t, nil)
	ctx := context.Background()
	sys := setupUser(t, st, "@sys", 0)
	seedChatAction(t, st, sys.ID)
	caller := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "llm/chat",
		Args: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState with nil chatter, got %v", err)
	}
}

func TestChatHandlerSuccessfulReply(t *testing.T) {
	fakeChat := &llm.FakeChatter{Reply: kernel.ChatMessage{Role: "assistant", Content: "hello"}}
	k, st := newChatKernel(t, fakeChat)
	ctx := context.Background()
	sys := setupUser(t, st, "@sys", 0)
	seedChatAction(t, st, sys.ID)
	caller := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	reply, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "llm/chat",
		Args: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	msg, ok := reply.Result["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message object, got %T", reply.Result["message"])
	}
	if msg["role"] != "assistant" || msg["content"] != "hello" {
		t.Errorf("unexpected message: %v", msg)
	}
}

func TestChatHandlerWithSystemPrompt(t *testing.T) {
	fakeChat := &llm.FakeChatter{Reply: kernel.ChatMessage{Role: "assistant", Content: "ok"}}
	k, st := newChatKernel(t, fakeChat)
	ctx := context.Background()
	sys := setupUser(t, st, "@sys", 0)
	seedChatAction(t, st, sys.ID)
	caller := setupUser(t, st, "@alice", 100)
	p, root, _ := k.StartProcess(ctx, caller.ID, caller.ID, 100)

	_, err := k.Call(ctx, kernel.CallRequest{
		SubjectID: caller.ID, ProcessID: p.ID, ParentTraceID: root.ID,
		TargetUserID: sys.ID, ActionName: "llm/chat",
		Args: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			"system":   "you are helpful",
		},
	})
	if err != nil {
		t.Fatalf("Call with system prompt: %v", err)
	}
}
