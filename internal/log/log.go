package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// NewLogger creates a structured logger writing to stderr.
func NewLogger(level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	handler := slog.NewTextHandler(os.Stderr, opts)
	return slog.New(handler)
}

// NewLoggerWithFile creates a structured logger writing to stderr and optionally
// to a file. format must be "text" or "json". If filePath is empty, only stderr
// is used. The file is opened in append mode so multiple runtime invocations
// accumulate in the same log file (matching containerd's expectations).
func NewLoggerWithFile(level slog.Level, filePath, format string) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: level}
	w := io.Writer(os.Stderr)
	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, fmt.Errorf("opening log file %q: %w", filePath, err)
		}
		w = io.MultiWriter(os.Stderr, f)
	}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), nil
}
