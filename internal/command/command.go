// Package command parses, validates, and executes a single command-line
// string against the configured allow list. No shell is ever invoked —
// the line is shell-tokenized into argv and passed straight to execve.
package command

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/mattn/go-shellwords"

	"github.com/yuanmomo/tcp-commander/internal/config"
)

// Errors returned to clients verbatim.
var (
	ErrEmpty       = errors.New("empty command")
	ErrParse       = errors.New("parse error")
	ErrNotAllowed  = errors.New("command not in allow list")
	ErrNotFound    = errors.New("executable not found")
	ErrConcurrency = errors.New("max concurrent reached")
)

// Result is the outcome of a single command run.
type Result struct {
	RC        int
	Stdout    string
	Stderr    string
	ElapsedMs int64
	TimedOut  bool
	Argv      []string
	Prog      string
}

// StreamEvent is emitted by Run for streaming requests.
type StreamEvent struct {
	Channel string // "stdout" or "stderr"
	Data    string
}

// Executor parses and runs allow-listed command strings.
type Executor struct {
	cfg *config.Config

	mu   sync.Mutex
	sems map[string]chan struct{}
}

// New builds an Executor over the given config.
func New(cfg *config.Config) *Executor {
	return &Executor{cfg: cfg, sems: make(map[string]chan struct{})}
}

// Run parses line, checks the allow list, and executes the resulting argv.
// If onStream is non-nil the child's stdout/stderr are streamed line-by-line
// via onStream; in that case the returned Result has empty Stdout/Stderr.
func (e *Executor) Run(
	ctx context.Context,
	line string,
	onStream func(StreamEvent),
) (*Result, error) {
	argv, err := shellwords.Parse(line)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrParse, err)
	}
	if len(argv) == 0 {
		return nil, ErrEmpty
	}

	prog := argv[0]
	base := filepath.Base(prog)
	if !e.cfg.CommandAllowed(prog) {
		return nil, fmt.Errorf("%w: %s", ErrNotAllowed, base)
	}

	// Resolve to an absolute path. If user passed an absolute path that
	// matched, keep it; otherwise look it up on PATH.
	resolved := prog
	if !filepath.IsAbs(prog) {
		p, err := exec.LookPath(prog)
		if err != nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, prog)
		}
		resolved = p
	}

	if max := e.cfg.MaxConcurrentFor(base); max > 0 {
		sem := e.sem(base, max)
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			return nil, ErrConcurrency
		}
	}

	timeout := e.cfg.TimeoutFor(base)
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	streaming := onStream != nil
	start := time.Now()
	cmd := exec.CommandContext(runCtx, resolved, argv[1:]...)

	var outBuf, errBuf bytes.Buffer
	var wg sync.WaitGroup

	if streaming {
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, fmt.Errorf("stdout pipe: %w", err)
		}
		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			return nil, fmt.Errorf("stderr pipe: %w", err)
		}
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
		wg.Add(2)
		go streamLines(stdoutPipe, "stdout", onStream, &wg)
		go streamLines(stderrPipe, "stderr", onStream, &wg)
	} else {
		cmd.Stdout = &outBuf
		cmd.Stderr = &errBuf
		if err := cmd.Start(); err != nil {
			return nil, fmt.Errorf("start: %w", err)
		}
	}

	waitErr := cmd.Wait()
	if streaming {
		wg.Wait()
	}
	elapsed := time.Since(start).Milliseconds()

	res := &Result{ElapsedMs: elapsed, Argv: argv, Prog: base}
	if !streaming {
		res.Stdout = outBuf.String()
		res.Stderr = errBuf.String()
	}

	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.RC = -1
		if !streaming {
			if res.Stderr != "" && res.Stderr[len(res.Stderr)-1] != '\n' {
				res.Stderr += "\n"
			}
			res.Stderr += "tcp-commander: timeout after " + timeout.String() + "\n"
		}
		return res, nil
	}

	if waitErr == nil {
		res.RC = 0
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		res.RC = ee.ExitCode()
		return res, nil
	}
	return nil, fmt.Errorf("wait: %w", waitErr)
}

func (e *Executor) sem(name string, n int) chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sems[name]
	if !ok {
		s = make(chan struct{}, n)
		e.sems[name] = s
	}
	return s
}

func streamLines(r io.Reader, channel string, onStream func(StreamEvent), wg *sync.WaitGroup) {
	defer wg.Done()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			onStream(StreamEvent{Channel: channel, Data: line})
		}
		if err != nil {
			return
		}
	}
}
