package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
)

// NewLoggerWithFile creates a structured logger writing to stderr and optionally
// to a file. format must be "text" or "json". If filePath is empty, only stderr
// is used. The file is opened in append mode so multiple runtime invocations
// accumulate in the same log file (matching containerd's expectations).
//
// The returned io.Closer owns the file handle (if any) and must be closed by
// the caller before process exit to release the descriptor; when filePath is
// empty, Close is a no-op.
func NewLoggerWithFile(level slog.Level, filePath, format string) (*slog.Logger, io.Closer, error) {
	opts := &slog.HandlerOptions{Level: level}
	w := io.Writer(os.Stderr)
	closer := io.Closer(noopCloser{})
	if filePath != "" {
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("opening log file %q: %w", filePath, err)
		}
		w = io.MultiWriter(os.Stderr, f)
		closer = f
	}
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	return slog.New(handler), closer, nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
