// Package config loads and validates the daemon's YAML configuration.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level daemon configuration.
//
// The daemon runs in "passthrough" mode: clients send a single command-line
// string, the daemon shell-tokenizes it (no shell is invoked), checks the
// program against AllowCommandList, and execs argv directly.
//
// There is intentionally no token auth and no TLS — this build is intended
// for personal use on a trusted network. Use AllowCIDR to restrict callers.
type Config struct {
	Listen           string            `yaml:"listen"`
	AllowCIDR        []string          `yaml:"allow_cidr,omitempty"`
	LogFile          string            `yaml:"log_file,omitempty"` // deprecated: use logging.file
	Logging          Logging           `yaml:"logging,omitempty"`
	AllowCommandList []string          `yaml:"allow_command_list"`
	Defaults         Defaults          `yaml:"defaults"`
	Limits           map[string]Limits `yaml:"limits,omitempty"`

	allowNets    []*net.IPNet    `yaml:"-"`
	allowedProgs map[string]bool `yaml:"-"`
}

// Logging configures structured-JSON output. The daemon always writes to
// stdout (so journald captures it under systemd); when File is set, output
// is duplicated to that file with optional size-based rotation.
type Logging struct {
	LevelStr   string `yaml:"level,omitempty"`        // debug, info, warn, error (default info)
	File       string `yaml:"file,omitempty"`         // log file path; empty disables file logging
	MaxSizeMB  int    `yaml:"max_size_mb,omitempty"`  // rotate when file exceeds this; 0 disables rotation
	MaxBackups int    `yaml:"max_backups,omitempty"`  // keep N rotated files (0 = keep all)
	MaxAgeDays int    `yaml:"max_age_days,omitempty"` // delete rotated files older than N days (0 = keep forever)
	Compress   bool   `yaml:"compress,omitempty"`     // gzip rotated files

	level slog.Level `yaml:"-"`
}

// Defaults applied to every command unless overridden in `limits`.
type Defaults struct {
	TimeoutStr       string `yaml:"timeout"`
	MaxConcurrent    int    `yaml:"max_concurrent"`
	MaxTimeoutStr    string `yaml:"max_timeout,omitempty"`
	HeartbeatStr     string `yaml:"heartbeat,omitempty"`
	KillGraceStr     string `yaml:"kill_grace,omitempty"`
	KeepAliveStr     string `yaml:"keepalive,omitempty"`
	OutputCapBytes   int64  `yaml:"output_cap_bytes,omitempty"`

	timeout      time.Duration
	maxTimeout   time.Duration
	heartbeat    time.Duration
	killGrace    time.Duration
	keepAlive    time.Duration
}

// Limits is the per-binary override for timeout / concurrency.
type Limits struct {
	TimeoutStr    string `yaml:"timeout"`
	MaxConcurrent *int   `yaml:"max_concurrent"`

	timeout time.Duration
	hasTimeout bool
}

// DefaultTimeout applies when neither defaults nor a per-binary limit set one.
const DefaultTimeout = 30 * time.Second

// DefaultListen is the default listen address.
const DefaultListen = ":9000"

// DefaultMaxTimeout caps the per-request timeout override that clients may
// pass in their request. 0 in config disables per-request overrides.
const DefaultMaxTimeout = 1 * time.Hour

// DefaultHeartbeat is the interval at which the daemon emits liveness frames
// on streaming requests. 0 disables heartbeats.
const DefaultHeartbeat = 30 * time.Second

// DefaultKillGrace is how long the daemon waits after sending SIGTERM before
// escalating to SIGKILL on timeout / cancellation.
const DefaultKillGrace = 10 * time.Second

// DefaultKeepAlive is the TCP keepalive period set on accepted connections.
// 0 disables keepalive.
const DefaultKeepAlive = 30 * time.Second

// DefaultOutputCapBytes caps the per-channel (stdout/stderr) output buffer
// for non-streaming requests. 0 disables the cap.
const DefaultOutputCapBytes = 4 * 1024 * 1024

// Load reads and validates a config file from disk.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) normalize() error {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}

	for _, cidr := range c.AllowCIDR {
		_, n, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid allow_cidr %q: %w", cidr, err)
		}
		c.allowNets = append(c.allowNets, n)
	}

	if c.Defaults.TimeoutStr == "" {
		c.Defaults.timeout = DefaultTimeout
	} else {
		d, err := time.ParseDuration(c.Defaults.TimeoutStr)
		if err != nil {
			return fmt.Errorf("invalid defaults.timeout %q: %w", c.Defaults.TimeoutStr, err)
		}
		if d <= 0 {
			return fmt.Errorf("defaults.timeout must be > 0")
		}
		c.Defaults.timeout = d
	}

	if err := parseOptionalDuration(c.Defaults.MaxTimeoutStr, "defaults.max_timeout", DefaultMaxTimeout, &c.Defaults.maxTimeout); err != nil {
		return err
	}
	if err := parseOptionalDuration(c.Defaults.HeartbeatStr, "defaults.heartbeat", DefaultHeartbeat, &c.Defaults.heartbeat); err != nil {
		return err
	}
	if err := parseOptionalDuration(c.Defaults.KillGraceStr, "defaults.kill_grace", DefaultKillGrace, &c.Defaults.killGrace); err != nil {
		return err
	}
	if err := parseOptionalDuration(c.Defaults.KeepAliveStr, "defaults.keepalive", DefaultKeepAlive, &c.Defaults.keepAlive); err != nil {
		return err
	}
	if c.Defaults.OutputCapBytes < 0 {
		return fmt.Errorf("defaults.output_cap_bytes must be >= 0")
	}
	if c.Defaults.OutputCapBytes == 0 {
		c.Defaults.OutputCapBytes = DefaultOutputCapBytes
	}

	if c.Limits == nil {
		c.Limits = map[string]Limits{}
	}
	for name, lim := range c.Limits {
		if lim.TimeoutStr != "" {
			d, err := time.ParseDuration(lim.TimeoutStr)
			if err != nil {
				return fmt.Errorf("limits[%q].timeout %q: %w", name, lim.TimeoutStr, err)
			}
			if d <= 0 {
				return fmt.Errorf("limits[%q].timeout must be > 0", name)
			}
			lim.timeout = d
			lim.hasTimeout = true
			c.Limits[name] = lim
		}
	}

	if err := c.normalizeLogging(); err != nil {
		return err
	}

	c.allowedProgs = make(map[string]bool, len(c.AllowCommandList))
	for _, p := range c.AllowCommandList {
		if p == "" {
			return fmt.Errorf("allow_command_list contains empty entry")
		}
		// Accept either a basename ("docker") or an absolute path
		// ("/usr/bin/docker"). Both are stored normalized as their basename
		// for lookup; absolute paths additionally get exact-path matching.
		c.allowedProgs[filepath.Base(p)] = true
		if filepath.IsAbs(p) {
			c.allowedProgs[p] = true
		}
	}
	return nil
}

// AllowNets returns parsed CIDRs (nil if no allowlist configured).
func (c *Config) AllowNets() []*net.IPNet { return c.allowNets }

// IPAllowed reports whether the given IP passes the allowlist.
// If no allowlist is configured, all IPs are allowed.
func (c *Config) IPAllowed(ip net.IP) bool {
	if len(c.allowNets) == 0 {
		return true
	}
	for _, n := range c.allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// CommandAllowed reports whether the given argv[0] is in the allow list.
// Both bare names ("docker") and absolute paths ("/usr/bin/docker") are
// accepted in config; matching is done by basename.
func (c *Config) CommandAllowed(prog string) bool {
	if c.allowedProgs[prog] {
		return true
	}
	return c.allowedProgs[filepath.Base(prog)]
}

// TimeoutFor returns the effective timeout for the given program basename.
func (c *Config) TimeoutFor(prog string) time.Duration {
	if lim, ok := c.Limits[prog]; ok && lim.hasTimeout {
		return lim.timeout
	}
	return c.Defaults.timeout
}

// MaxConcurrentFor returns the effective concurrency cap for the given
// program. 0 means unlimited.
func (c *Config) MaxConcurrentFor(prog string) int {
	if lim, ok := c.Limits[prog]; ok && lim.MaxConcurrent != nil {
		return *lim.MaxConcurrent
	}
	return c.Defaults.MaxConcurrent
}

// MaxTimeout returns the upper bound for per-request timeout overrides.
// A zero value means clients may not override the per-binary timeout.
func (c *Config) MaxTimeout() time.Duration { return c.Defaults.maxTimeout }

// Heartbeat returns the streaming-heartbeat interval. 0 disables.
func (c *Config) Heartbeat() time.Duration { return c.Defaults.heartbeat }

// KillGrace returns how long to wait between SIGTERM and SIGKILL.
func (c *Config) KillGrace() time.Duration { return c.Defaults.killGrace }

// KeepAlive returns the TCP keepalive period for accepted connections.
// 0 disables keepalive.
func (c *Config) KeepAlive() time.Duration { return c.Defaults.keepAlive }

// OutputCap returns the per-channel cap for non-streaming output buffers.
// 0 means unbounded.
func (c *Config) OutputCap() int64 { return c.Defaults.OutputCapBytes }

// LogConfig returns the resolved logging configuration. It honors the new
// `logging:` block, falling back to the legacy top-level `log_file:` field
// for the file path when `logging.file` is empty.
func (c *Config) LogConfig() Logging {
	out := c.Logging
	if out.File == "" && c.LogFile != "" {
		out.File = c.LogFile
	}
	return out
}

// Level returns the parsed slog.Level for this Logging config.
func (l Logging) Level() slog.Level { return l.level }

func (c *Config) normalizeLogging() error {
	switch strings.ToLower(strings.TrimSpace(c.Logging.LevelStr)) {
	case "", "info":
		c.Logging.level = slog.LevelInfo
	case "debug":
		c.Logging.level = slog.LevelDebug
	case "warn", "warning":
		c.Logging.level = slog.LevelWarn
	case "error", "err":
		c.Logging.level = slog.LevelError
	default:
		return fmt.Errorf("invalid logging.level %q (want debug|info|warn|error)", c.Logging.LevelStr)
	}
	if c.Logging.MaxSizeMB < 0 {
		return fmt.Errorf("logging.max_size_mb must be >= 0")
	}
	if c.Logging.MaxBackups < 0 {
		return fmt.Errorf("logging.max_backups must be >= 0")
	}
	if c.Logging.MaxAgeDays < 0 {
		return fmt.Errorf("logging.max_age_days must be >= 0")
	}
	return nil
}

func parseOptionalDuration(raw, label string, fallback time.Duration, dst *time.Duration) error {
	if raw == "" {
		*dst = fallback
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid %s %q: %w", label, raw, err)
	}
	if d < 0 {
		return fmt.Errorf("%s must be >= 0", label)
	}
	*dst = d
	return nil
}
