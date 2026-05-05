package config

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_Defaults(t *testing.T) {
	p := writeTemp(t, `
allow_command_list: [echo, df]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen {
		t.Fatalf("default listen: %q", c.Listen)
	}
	if c.TimeoutFor("echo") != DefaultTimeout {
		t.Fatalf("default timeout: %v", c.TimeoutFor("echo"))
	}
	if !c.CommandAllowed("echo") {
		t.Fatal("echo should be allowed")
	}
	if !c.CommandAllowed("/bin/echo") {
		t.Fatal("absolute /bin/echo should match echo")
	}
	if c.CommandAllowed("rm") {
		t.Fatal("rm should not be allowed")
	}
}

func TestLoad_TimeoutsAndOverrides(t *testing.T) {
	p := writeTemp(t, `
defaults:
  timeout: 5s
  max_concurrent: 2
allow_command_list:
  - docker
  - df
limits:
  docker:
    timeout: 10m
    max_concurrent: 1
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeoutFor("df") != 5*time.Second {
		t.Fatalf("df timeout: %v", c.TimeoutFor("df"))
	}
	if c.TimeoutFor("docker") != 10*time.Minute {
		t.Fatalf("docker timeout: %v", c.TimeoutFor("docker"))
	}
	if c.MaxConcurrentFor("df") != 2 {
		t.Fatalf("df concurrency: %d", c.MaxConcurrentFor("df"))
	}
	if c.MaxConcurrentFor("docker") != 1 {
		t.Fatalf("docker concurrency: %d", c.MaxConcurrentFor("docker"))
	}
}

func TestLoad_AllowCIDR(t *testing.T) {
	p := writeTemp(t, `
allow_cidr: ["10.0.0.0/8"]
allow_command_list: [echo]
`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !c.IPAllowed(net.ParseIP("10.1.2.3")) {
		t.Fatal("10.1.2.3 should be allowed")
	}
	if c.IPAllowed(net.ParseIP("8.8.8.8")) {
		t.Fatal("8.8.8.8 should be denied")
	}
}

func TestLoad_RejectsUnknownField(t *testing.T) {
	p := writeTemp(t, `
typo_field: oops
allow_command_list: [echo]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error on unknown field")
	}
}

func TestLoad_RejectsBadTimeout(t *testing.T) {
	p := writeTemp(t, `
defaults:
  timeout: not-a-duration
allow_command_list: [echo]
`)
	if _, err := Load(p); err == nil {
		t.Fatal("expected error on bad timeout")
	}
}
