package native

import (
	"context"
	"errors"
	"testing"

	"github.com/daios-ai/juice/kernel"
)

func TestExecuteWebSuccess(t *testing.T) {
	deps := WebDeps{Fetch: func(_ context.Context, url string) (int, []byte, string, error) {
		if url != "https://example.com/page" {
			t.Fatalf("unexpected url %q", url)
		}
		return 200, []byte("<html>hi</html>"), "text/html; charset=utf-8", nil
	}}

	result, err := executeWeb(context.Background(), map[string]any{"url": "https://example.com/page"}, deps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result["status"]; got != int64(200) {
		t.Errorf("status = %v, want 200", got)
	}
	if got := result["body"]; got != "<html>hi</html>" {
		t.Errorf("body = %q", got)
	}
	if got := result["content_type"]; got != "text/html; charset=utf-8" {
		t.Errorf("content_type = %q", got)
	}
}

func TestExecuteWebNon2xxReturnedNotError(t *testing.T) {
	deps := WebDeps{Fetch: func(_ context.Context, _ string) (int, []byte, string, error) {
		return 404, []byte("not found"), "text/plain", nil
	}}
	result, err := executeWeb(context.Background(), map[string]any{"url": "https://example.com/x"}, deps)
	if err != nil {
		t.Fatalf("non-2xx must not be an error, got %v", err)
	}
	if got := result["status"]; got != int64(404) {
		t.Errorf("status = %v, want 404", got)
	}
}

func TestExecuteWebMissingURL(t *testing.T) {
	deps := WebDeps{Fetch: func(_ context.Context, _ string) (int, []byte, string, error) {
		t.Fatal("fetch should not be called when url is missing")
		return 0, nil, "", nil
	}}
	for _, args := range []map[string]any{{}, {"url": ""}} {
		_, err := executeWeb(context.Background(), args, deps)
		if !errors.Is(err, kernel.ErrInvalidInput) {
			t.Errorf("args %v: expected ErrInvalidInput, got %v", args, err)
		}
	}
}

func TestExecuteWebUnconfiguredFetcher(t *testing.T) {
	_, err := executeWeb(context.Background(), map[string]any{"url": "https://example.com"}, WebDeps{})
	if !errors.Is(err, kernel.ErrInvalidState) {
		t.Errorf("expected ErrInvalidState, got %v", err)
	}
}

func TestExecuteWebPropagatesGuardError(t *testing.T) {
	// The fetcher rejects private/loopback hosts with ErrInvalidInput; the handler
	// must surface that typed error unchanged.
	deps := WebDeps{Fetch: func(_ context.Context, _ string) (int, []byte, string, error) {
		return 0, nil, "", kernel.ErrInvalidInput.Wrap("resolved address is private or loopback")
	}}
	_, err := executeWeb(context.Background(), map[string]any{"url": "http://127.0.0.1:11434"}, deps)
	if !errors.Is(err, kernel.ErrInvalidInput) {
		t.Errorf("expected ErrInvalidInput from guard, got %v", err)
	}
}

func TestExecuteWebPropagatesTransportError(t *testing.T) {
	deps := WebDeps{Fetch: func(_ context.Context, _ string) (int, []byte, string, error) {
		return 0, nil, "", kernel.ErrExecutionFailed.Wrap("HTTP call failed")
	}}
	_, err := executeWeb(context.Background(), map[string]any{"url": "https://example.com"}, deps)
	if !errors.Is(err, kernel.ErrExecutionFailed) {
		t.Errorf("expected ErrExecutionFailed, got %v", err)
	}
}
