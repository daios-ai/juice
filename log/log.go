package log

import (
	"context"
	"io"
	"log/slog"
	"os"
)

type contextKey int

const (
	keyRequestID contextKey = iota
	keySubjectUserID
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

	var writers []io.Writer
	writers = append(writers, os.Stdout)

	if cfg.FilePath != "" {
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		writers = append(writers, f)
	}

	var out io.Writer = os.Stdout
	var fileOut io.Writer
	if len(writers) > 1 {
		fileOut = writers[1]
	}

	var handler slog.Handler
	opts := &slog.HandlerOptions{Level: level}

	if cfg.Format == "json" {
		handler = slog.NewJSONHandler(out, opts)
	} else {
		handler = slog.NewTextHandler(out, opts)
	}

	// When a file is configured, multiplex: text to terminal, JSON to file.
	if fileOut != nil {
		handler = &multiHandler{
			terminal: slog.NewTextHandler(os.Stdout, opts),
			file:     slog.NewJSONHandler(fileOut, opts),
		}
	}

	return &Logger{inner: slog.New(handler)}, nil
}

// Default returns a logger that writes text to stdout at INFO level.
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
	if v, ok := ctx.Value(keySubjectUserID).(string); ok && v != "" {
		args = append(args, "subject_user_id", v)
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
func WithSubjectUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keySubjectUserID, id)
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

func (l *Logger) Debug(event string, args ...any) {
	l.inner.Debug(event, args...)
}
func (l *Logger) Info(event string, args ...any) {
	l.inner.Info(event, args...)
}
func (l *Logger) Warn(event string, args ...any) {
	l.inner.Warn(event, args...)
}
func (l *Logger) Error(event string, args ...any) {
	l.inner.Error(event, args...)
}

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
