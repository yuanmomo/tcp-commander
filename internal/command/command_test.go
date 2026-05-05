package command

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yuanmomo/tcp-commander/internal/config"
)

func loadCfg(t *testing.T, body string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRun_Echo(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	res, err := e.Run(context.Background(), `echo hello world`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 0 || strings.TrimSpace(res.Stdout) != "hello world" {
		t.Fatalf("rc=%d out=%q", res.RC, res.Stdout)
	}
}

func TestRun_QuotingPreserved(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	// The quoted phrase should reach echo as a single argv element.
	res, err := e.Run(context.Background(), `echo "one two" three`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "one two three") {
		t.Fatalf("unexpected stdout: %q", res.Stdout)
	}
}

func TestRun_NoShellInterpolation(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	// `;` and `$(...)` must reach echo as literal characters since no
	// shell is invoked.
	res, err := e.Run(context.Background(), `echo "; rm -rf / && $(whoami)"`, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(res.Stdout)
	if !strings.Contains(out, "$(whoami)") || !strings.Contains(out, "rm -rf") {
		t.Fatalf("expected literal output, got %q", out)
	}
}

func TestRun_NotAllowed(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	_, err := e.Run(context.Background(), `rm -rf /tmp`, nil)
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
}

func TestRun_Empty(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	if _, err := e.Run(context.Background(), "   ", nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("want ErrEmpty, got %v", err)
	}
}

func TestRun_ParseError(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [echo]`)
	e := New(cfg)
	if _, err := e.Run(context.Background(), `echo "unterminated`, nil); !errors.Is(err, ErrParse) {
		t.Fatalf("want ErrParse, got %v", err)
	}
}

func TestRun_AbsolutePath(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: ["/bin/echo"]`)
	e := New(cfg)
	// Both absolute and bare basename should match an absolute-path entry.
	if _, err := e.Run(context.Background(), `/bin/echo abs`, nil); err != nil {
		t.Fatalf("absolute call: %v", err)
	}
	if _, err := e.Run(context.Background(), `echo bare`, nil); err != nil {
		t.Fatalf("bare-name call (basename match): %v", err)
	}
}

func TestRun_Timeout(t *testing.T) {
	cfg := loadCfg(t, `
allow_command_list: [sleep]
defaults:
  timeout: 200ms
`)
	e := New(cfg)
	res, err := e.Run(context.Background(), `sleep 5`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected timeout, rc=%d", res.RC)
	}
}

func TestRun_PerBinaryConcurrency(t *testing.T) {
	cfg := loadCfg(t, `
allow_command_list: [sleep]
defaults:
  timeout: 5s
limits:
  sleep:
    max_concurrent: 1
`)
	e := New(cfg)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = e.Run(context.Background(), `sleep 0.5`, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err := e.Run(context.Background(), `sleep 0.5`, nil); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("want ErrConcurrency, got %v", err)
	}
	wg.Wait()
}

func TestRun_BashScript(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hello.sh")
	if err := os.WriteFile(script, []byte("#!/bin/bash\necho \"hi $1 from $2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := loadCfg(t, `allow_command_list: [bash]`)
	e := New(cfg)
	res, err := e.Run(context.Background(),
		`bash `+script+` alice prod`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Stdout, "hi alice from prod") {
		t.Fatalf("unexpected: %q", res.Stdout)
	}
}

func TestRun_Streaming(t *testing.T) {
	cfg := loadCfg(t, `allow_command_list: [sh]`)
	e := New(cfg)
	var lines []string
	res, err := e.Run(context.Background(),
		`sh -c "echo one; echo two; echo three"`,
		func(ev StreamEvent) {
			lines = append(lines, ev.Channel+":"+strings.TrimRight(ev.Data, "\n"))
		})
	if err != nil {
		t.Fatal(err)
	}
	if res.RC != 0 || len(lines) != 3 || lines[0] != "stdout:one" {
		t.Fatalf("res=%+v lines=%v", res, lines)
	}
	if res.Stdout != "" {
		t.Fatalf("stream mode should not buffer stdout, got %q", res.Stdout)
	}
}
