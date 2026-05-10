// Package logging configures structured JSON logging for the daemon.
package logging

import (
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/yuanmomo/tcp-commander/internal/config"
)

// Setup builds an slog.Logger that writes JSON to stdout, and optionally
// duplicates output into a configured file. When a file is set with a
// non-zero MaxSizeMB the file is rotated by lumberjack (rename-and-reopen
// in-process, no SIGHUP needed). The returned closer flushes any open
// file handle on shutdown.
func Setup(cfg config.Logging) (*slog.Logger, io.Closer, error) {
	var w io.Writer = os.Stdout
	var closer io.Closer = noopCloser{}

	if cfg.File != "" {
		fileWriter, c, err := openFile(cfg)
		if err != nil {
			return nil, nil, err
		}
		w = io.MultiWriter(os.Stdout, fileWriter)
		closer = c
	}

	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: cfg.Level()})
	return slog.New(h), closer, nil
}

func openFile(cfg config.Logging) (io.Writer, io.Closer, error) {
	if cfg.MaxSizeMB > 0 {
		// In-process rotation. lumberjack handles the rename-and-reopen
		// dance atomically, so we don't need to teach the daemon SIGHUP.
		lj := &lumberjack.Logger{
			Filename:   cfg.File,
			MaxSize:    cfg.MaxSizeMB,
			MaxBackups: cfg.MaxBackups,
			MaxAge:     cfg.MaxAgeDays,
			Compress:   cfg.Compress,
		}
		return lj, lj, nil
	}
	// No rotation: open append-only.
	f, err := os.OpenFile(cfg.File, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, nil, err
	}
	return f, f, nil
}

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
