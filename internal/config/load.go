package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LoadOptions controls how Load resolves the config source.
type LoadOptions struct {
	// ExplicitPath, if non-empty, is used verbatim. Missing → error.
	ExplicitPath string
}

// Load resolves and parses ccproxy's configuration. Resolution order:
//
//  1. opts.ExplicitPath (--config flag)
//  2. $CCPROXY_CONFIG
//  3. $CCPROXY_STATE/config.yaml
//  4. ./ccproxy.yaml (dev convenience)
//
// If none of the above exists, returns an error — ccproxy requires a YAML
// config file. The deploy/config.yaml.example file documents the schema;
// a minimal config can be just `models: []` when passthrough is enabled.
//
// After parsing, env-var overrides are applied and the result is validated.
func Load(opts LoadOptions) (*Config, string, error) {
	path, err := resolvePath(opts.ExplicitPath)
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		return nil, "", fmt.Errorf("no config file found (looked at $CCPROXY_CONFIG, %s, ./ccproxy.yaml). Pass --config <path> or create one — see deploy/config.yaml.example",
			filepath.Join(stateDirFromEnv(), "config.yaml"))
	}

	cfg := Defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse %s: %w", path, err)
	}
	applyDefaults(&cfg, Defaults())

	applyEnvOverrides(&cfg)

	if err := cfg.Validate(); err != nil {
		return nil, path, err
	}
	return &cfg, path, nil
}

func resolvePath(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("--config %s: %w", explicit, err)
		}
		return explicit, nil
	}
	candidates := []string{
		os.Getenv("CCPROXY_CONFIG"),
		filepath.Join(stateDirFromEnv(), "config.yaml"),
		"ccproxy.yaml",
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", nil
}

func stateDirFromEnv() string {
	if v := os.Getenv("CCPROXY_STATE"); v != "" {
		return v
	}
	return "/var/lib/ccproxy"
}

// applyEnvOverrides keeps a minimal set of deploy-time overrides so systemd
// drop-ins / Docker env can retarget bind addrs and the state dir without
// editing YAML. Behavior knobs (verbosity, effort, model, passthrough,
// claude binary) live in YAML only — edit config.yaml to change them.
func applyEnvOverrides(c *Config) {
	if v := os.Getenv("CCPROXY_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("CCPROXY_METRICS_LISTEN"); v != "" {
		c.MetricsListen = v
	}
	if v := os.Getenv("CCPROXY_STATE"); v != "" {
		c.StateDir = v
	}
}
