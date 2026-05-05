// Package logging configures structured JSON logging for the daemon.
package logging

import (
	"io"
	"log/slog"
	"os"
)

// Setup returns an slog.Logger that writes JSON to stdout, and optionally
// duplicates output into the file at logFile. The returned closer should be
// called on shutdown to flush/close the file (no-op when no file).
func Setup(logFile string) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stdout
	var closer io.Closer = noopCloser{}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stdout, f)
		closer = f
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo})
	return slog.New(h), closer, nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
