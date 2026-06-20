package native

import (
	"context"

	"github.com/daios-ai/juice/kernel"
)

// WebDeps holds the injected dependencies for the @sys/web native action.
// Fetch performs a read-only GET of url and returns the HTTP status, response
// body, Content-Type, and the final URL actually fetched (after scheme
// resolution and redirects). It is supplied by cmd/juice so that the native
// package never imports net/http; the implementation defaults a scheme-less url
// to https, applies the SSRF guard, and sets the configured User-Agent (§7, §9).
type WebDeps struct {
	Fetch func(ctx context.Context, url string) (status int, body []byte, contentType, finalURL string, err error)
}

// RegisterWebHandler registers the @sys/web native action handler on k.
func RegisterWebHandler(k *kernel.Kernel, deps WebDeps) {
	k.RegisterNativeHandler("web", func(ctx context.Context, args map[string]any, _, _, _, _, _ string) (map[string]any, error) {
		return executeWeb(ctx, args, deps)
	})
}

func executeWeb(ctx context.Context, args map[string]any, deps WebDeps) (map[string]any, error) {
	url, _ := args["url"].(string)
	if url == "" {
		return nil, kernel.ErrInvalidInput.Wrap("web requires url")
	}
	if deps.Fetch == nil {
		return nil, kernel.ErrInvalidState.Wrap("web fetcher not configured")
	}

	status, body, contentType, finalURL, err := deps.Fetch(ctx, url)
	if err != nil {
		// SSRF rejection arrives as ErrInvalidInput from the guard; transport
		// failures as ErrExecutionFailed. Both are already typed by the fetcher.
		return nil, err
	}

	return map[string]any{
		"status":       int64(status),
		"body":         string(body),
		"content_type": contentType,
		"final_url":    finalURL,
	}, nil
}
