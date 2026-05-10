// Package command parses, validates, and executes a single command-line
// string against the configured allow list. No shell is ever invoked —
// the line is shell-tokenized into argv and passed straight to execve.
package command

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
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
	Truncated bool
	Argv      []string
	Prog      string
}

// StreamEvent is emitted by Run for streaming requests.
type StreamEvent struct {
	Channel string // "stdout" or "stderr"
	Data    string
}

// Options tunes a single Run. Zero values mean "use the executor's config".
type Options struct {
	// Timeout, if > 0, overrides the per-binary timeout for this run.
	Timeout time.Duration
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
//
// An optional Options value tunes this single run (e.g. per-request timeout).
func (e *Executor) Run(
	ctx context.Context,
	line string,
	onStream func(StreamEvent),
	opts ...Options,
) (*Result, error) {
	var opt Options
	if len(opts) > 0 {
		opt = opts[0]
	}

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
	if opt.Timeout > 0 {
		timeout = opt.Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	streaming := onStream != nil
	start := time.Now()
	cmd := exec.CommandContext(runCtx, resolved, argv[1:]...)

	// Graceful kill: send SIGTERM on context cancel and escalate to SIGKILL
	// after KillGrace if the child hasn't exited. This lets deploy scripts
	// clean up on timeout / shutdown / client disconnect.
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		return nil
	}
	cmd.WaitDelay = e.cfg.KillGrace()

	outBuf := newCappedBuffer(e.cfg.OutputCap())
	errBuf := newCappedBuffer(e.cfg.OutputCap())
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
		cmd.Stdout = outBuf
		cmd.Stderr = errBuf
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
		res.Truncated = outBuf.truncated || errBuf.truncated
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

	// Parent ctx cancelled (client disconnected / server shutdown).
	if errors.Is(runCtx.Err(), context.Canceled) {
		res.RC = -1
		if !streaming {
			if res.Stderr != "" && res.Stderr[len(res.Stderr)-1] != '\n' {
				res.Stderr += "\n"
			}
			res.Stderr += "tcp-commander: cancelled\n"
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

// cappedBuffer is an io.Writer that caps the total bytes it stores. Writes
// past the cap are dropped (the underlying program still gets EOF on its
// pipe only when we close it; in practice it just keeps writing into a
// no-op writer). When truncation occurs, the first byte after the cap is
// replaced by a marker line so callers can tell the output was clipped.
type cappedBuffer struct {
	cap       int64
	written   int64
	buf       []byte
	truncated bool
}

func newCappedBuffer(cap int64) *cappedBuffer {
	return &cappedBuffer{cap: cap}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if c.cap <= 0 {
		c.buf = append(c.buf, p...)
		c.written += int64(n)
		return n, nil
	}
	remaining := c.cap - c.written
	if remaining > 0 {
		take := int64(n)
		if take > remaining {
			take = remaining
		}
		c.buf = append(c.buf, p[:take]...)
		c.written += take
	}
	if c.written >= c.cap && !c.truncated {
		c.truncated = true
		c.buf = append(c.buf, []byte("\n[tcp-commander: output truncated]\n")...)
	}
	// Always claim we wrote everything so the child process is never
	// blocked by a backed-up pipe.
	return n, nil
}

func (c *cappedBuffer) String() string { return string(c.buf) }

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
