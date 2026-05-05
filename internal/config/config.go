// Package config loads and validates the daemon's YAML configuration.
package config

import (
	"fmt"
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
	Listen           string   `yaml:"listen"`
	AllowCIDR        []string `yaml:"allow_cidr,omitempty"`
	LogFile          string   `yaml:"log_file,omitempty"`
	AllowCommandList []string `yaml:"allow_command_list"`
	Defaults         Defaults `yaml:"defaults"`
	Limits           map[string]Limits `yaml:"limits,omitempty"`

	allowNets    []*net.IPNet    `yaml:"-"`
	allowedProgs map[string]bool `yaml:"-"`
}

// Defaults applied to every command unless overridden in `limits`.
type Defaults struct {
	TimeoutStr       string `yaml:"timeout"`
	MaxConcurrent    int    `yaml:"max_concurrent"`
	timeout          time.Duration
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
