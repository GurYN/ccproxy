package config

import "time"

// Defaults returns a Config populated with the values ccproxy uses when a
// field is missing from the YAML file. Workspaces and Models are left empty
// — those have no sensible defaults and must come from config or env.
func Defaults() Config {
	return Config{
		Listen:           ":4141",
		StateDir:         "/var/lib/ccproxy",
		ClaudeBinary:     "claude",
		DefaultVerbosity: "text-only",
		SessionTTL:       Duration(30 * time.Minute),
		RequestTimeout:   Duration(10 * time.Minute),
	}
}

// applyDefaults fills any unset top-level field on c with the value from d.
// Maps and slices are kept as-is.
func applyDefaults(c *Config, d Config) {
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.StateDir == "" {
		c.StateDir = d.StateDir
	}
	if c.ClaudeBinary == "" {
		c.ClaudeBinary = d.ClaudeBinary
	}
	if c.DefaultVerbosity == "" {
		c.DefaultVerbosity = d.DefaultVerbosity
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = d.RequestTimeout
	}
}
