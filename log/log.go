package log

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/lmittmann/tint"
)

type contextKey int

const (
	keyRequestID contextKey = iota
	keyCallerUserID
	keyCallerHandle
	keyProcessID
	keyTraceID
	keyActionID
	keyTxID
)

// Logger wraps slog with juice-specific context fields.
type Logger struct {
	inner *slog.Logger
}

// Config controls log output and level.
type Config struct {
	Level    string // "debug", "info", "warn", "error"
	FilePath string // optional; when set, also writes JSON to this file
	Format   string // "text" or "json"
}

// New creates a Logger writing to terminal (and optionally a file).
func New(cfg Config) (*Logger, error) {
	level := parseLevel(cfg.Level)
	opts := &slog.HandlerOptions{Level: level}

	var terminal slog.Handler
	if cfg.Format == "json" {
		terminal = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		// Console: shorten IDs and prefer handles/names for readability. The optional
		// JSON file handler (below) is left unwrapped so it keeps full IDs for tooling.
		terminal = &readableHandler{inner: tint.NewHandler(os.Stderr, &tint.Options{
			Level:      level,
			TimeFormat: time.TimeOnly,
			NoColor:    false,
		})}
	}

	var handler slog.Handler = terminal

	if cfg.FilePath != "" {
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		handler = &multiHandler{
			terminal: terminal,
			file:     slog.NewJSONHandler(f, opts),
		}
	}

	return &Logger{inner: slog.New(handler)}, nil
}

// Default returns a logger that writes colored text to stdout at INFO level.
func Default() *Logger {
	l, _ := New(Config{Level: "info", Format: "text"})
	return l
}

func parseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// With returns a logger enriched with fields extracted from ctx.
func (l *Logger) With(ctx context.Context) *Logger {
	args := []any{}
	if v, ok := ctx.Value(keyRequestID).(string); ok && v != "" {
		args = append(args, "request_id", v)
	}
	if v, ok := ctx.Value(keyCallerUserID).(string); ok && v != "" {
		args = append(args, "caller_user_id", v)
	}
	if v, ok := ctx.Value(keyCallerHandle).(string); ok && v != "" {
		args = append(args, "caller_handle", v)
	}
	if v, ok := ctx.Value(keyProcessID).(string); ok && v != "" {
		args = append(args, "process_id", v)
	}
	if v, ok := ctx.Value(keyTraceID).(string); ok && v != "" {
		args = append(args, "trace_id", v)
	}
	if v, ok := ctx.Value(keyActionID).(string); ok && v != "" {
		args = append(args, "action_id", v)
	}
	if v, ok := ctx.Value(keyTxID).(string); ok && v != "" {
		args = append(args, "tx_id", v)
	}
	if len(args) == 0 {
		return l
	}
	return &Logger{inner: l.inner.With(args...)}
}

// Context setters — call these to enrich the context before passing it down.

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyRequestID, id)
}
func WithCallerUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyCallerUserID, id)
}
func WithCallerHandle(ctx context.Context, handle string) context.Context {
	return context.WithValue(ctx, keyCallerHandle, handle)
}
func WithProcessID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyProcessID, id)
}
func WithTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTraceID, id)
}
func WithActionID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyActionID, id)
}
func WithTxID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTxID, id)
}

// Logging methods.

func (l *Logger) Debug(event string, args ...any) { l.inner.Debug(event, args...) }
func (l *Logger) Info(event string, args ...any)  { l.inner.Info(event, args...) }
func (l *Logger) Warn(event string, args ...any)  { l.inner.Warn(event, args...) }
func (l *Logger) Error(event string, args ...any) { l.inner.Error(event, args...) }

// multiHandler fans out to two slog.Handlers.
type multiHandler struct {
	terminal slog.Handler
	file     slog.Handler
}

func (h *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.terminal.Enabled(ctx, level) || h.file.Enabled(ctx, level)
}
func (h *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = h.terminal.Handle(ctx, r)
	_ = h.file.Handle(ctx, r)
	return nil
}
func (h *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &multiHandler{
		terminal: h.terminal.WithAttrs(attrs),
		file:     h.file.WithAttrs(attrs),
	}
}
func (h *multiHandler) WithGroup(name string) slog.Handler {
	return &multiHandler{
		terminal: h.terminal.WithGroup(name),
		file:     h.file.WithGroup(name),
	}
}

// readableHandler wraps a console handler to make output scannable: it shortens UUID-ish
// ID values to their first 8 chars, renames the noisy context keys to short forms, prefers
// the caller's handle over its UUID, and drops action_id (the action name is already logged
// as action=…). It is applied ONLY to the console; the JSON file handler keeps full fidelity.
type readableHandler struct {
	inner slog.Handler
}

func (h *readableHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

func (h *readableHandler) Handle(ctx context.Context, r slog.Record) error {
	// Rebuild the record with transformed attrs (records are otherwise immutable here).
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	nr.AddAttrs(transformAttrs(attrs)...)
	return h.inner.Handle(ctx, nr)
}

func (h *readableHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &readableHandler{inner: h.inner.WithAttrs(transformAttrs(attrs))}
}

func (h *readableHandler) WithGroup(name string) slog.Handler {
	return &readableHandler{inner: h.inner.WithGroup(name)}
}

// idKeyRenames maps verbose context keys to short console-friendly ones.
var idKeyRenames = map[string]string{
	"request_id": "req",
	"process_id": "proc",
	"trace_id":   "trace",
	"tx_id":      "tx",
}

// transformAttrs applies the console readability rules to one batch of attributes.
// Caller handling is batch-aware: caller_user_id is dropped only when caller_handle is
// present in the same batch (context fields arrive together), else it falls back to a
// short caller= value so the caller is never lost.
func transformAttrs(attrs []slog.Attr) []slog.Attr {
	hasHandle := false
	for _, a := range attrs {
		if a.Key == "caller_handle" && a.Value.String() != "" {
			hasHandle = true
			break
		}
	}
	out := make([]slog.Attr, 0, len(attrs))
	for _, a := range attrs {
		switch {
		case a.Key == "action_id":
			// redundant with action=<name>
			continue
		case a.Key == "caller_handle":
			out = append(out, slog.String("caller", a.Value.String()))
		case a.Key == "caller_user_id":
			if hasHandle {
				continue
			}
			out = append(out, slog.String("caller", shortID(a.Value.String())))
		case idKeyRenames[a.Key] != "":
			out = append(out, slog.String(idKeyRenames[a.Key], shortID(a.Value.String())))
		case strings.HasSuffix(a.Key, "_id"):
			out = append(out, slog.String(a.Key, shortID(a.Value.String())))
		default:
			out = append(out, a)
		}
	}
	return out
}

// shortID returns the first 8 characters of an ID (UUID first group), or the whole
// string when it is already short.
func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// Discard returns a logger that drops all output (useful in tests).
func Discard() *Logger {
	return &Logger{inner: slog.New(slog.NewTextHandler(io.Discard, nil))}
}
