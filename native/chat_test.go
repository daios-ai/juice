// SPDX-License-Identifier: AGPL-3.0-only

package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

type stubChatter struct {
	reply kernel.ChatMessage
	err   error
}

func (s *stubChatter) Chat(_ context.Context, _ []kernel.ChatMessage) (kernel.ChatMessage, error) {
	return s.reply, s.err
}

type capturingChatter struct {
	reply    kernel.ChatMessage
	captured []kernel.ChatMessage
}

func (c *capturingChatter) Chat(_ context.Context, msgs []kernel.ChatMessage) (kernel.ChatMessage, error) {
	c.captured = msgs
	return c.reply, nil
}

func TestExecuteChat_NilChatter(t *testing.T) {
	_, err := executeChat(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil)
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState with nil chatter, got %v", err)
	}
}

func TestExecuteChat_MissingMessages(t *testing.T) {
	_, err := executeChat(context.Background(), map[string]any{}, &stubChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing messages, got %v", err)
	}
}

func TestExecuteChat_MessagesNotArray(t *testing.T) {
	_, err := executeChat(context.Background(), map[string]any{"messages": "not-an-array"}, &stubChatter{})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for non-array messages, got %v", err)
	}
}

func TestExecuteChat_MessageMissingRoleOrContent(t *testing.T) {
	_, err := executeChat(context.Background(), map[string]any{
		"messages": []any{map[string]any{"content": "hi"}}, // no role
	}, &stubChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "ok"}})
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput for missing role, got %v", err)
	}
}

func TestExecuteChat_SuccessfulReply(t *testing.T) {
	c := &stubChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "hello"}}
	result, err := executeChat(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	msg, ok := result["message"].(map[string]any)
	if !ok {
		t.Fatalf("expected message map, got %T", result["message"])
	}
	if msg["role"] != "assistant" || msg["content"] != "hello" {
		t.Errorf("unexpected message: %v", msg)
	}
}

func TestExecuteChat_SystemPromptPrepended(t *testing.T) {
	c := &capturingChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "ok"}}
	_, err := executeChat(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"system":   "be helpful",
	}, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.captured) < 2 {
		t.Fatalf("expected at least 2 messages (system + user), got %d", len(c.captured))
	}
	if c.captured[0].Role != "system" || c.captured[0].Content != "be helpful" {
		t.Errorf("system message not prepended correctly: %+v", c.captured[0])
	}
}

func TestExecuteChat_EmptySystemPromptIgnored(t *testing.T) {
	c := &capturingChatter{reply: kernel.ChatMessage{Role: "assistant", Content: "ok"}}
	_, err := executeChat(context.Background(), map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"system":   "",
	}, c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.captured) != 1 || c.captured[0].Role != "user" {
		t.Errorf("empty system prompt should not be prepended: %v", c.captured)
	}
}
