package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeYAML(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLoad_FromExplicitPath(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", `
listen: ":5151"
state_dir: "/tmp/cc-state"
claude_binary: "/usr/local/bin/claude"
default_verbosity: verbose
session_ttl: 15m
workspaces:
  vault:
    path: /tmp/vault
    description: notes
models:
  - id: claude-code
  - id: claude-code-vault
    workspace: vault
    system_prompt: "be terse"
`)
	cfg, src, err := Load(LoadOptions{ExplicitPath: p})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if src != p {
		t.Errorf("source = %q, want %q", src, p)
	}
	if cfg.Listen != ":5151" || cfg.SessionTTL.Duration() != 15*time.Minute {
		t.Errorf("top-level fields wrong: %+v", cfg)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("models = %d, want 2", len(cfg.Models))
	}
	m, ok := cfg.FindModel("claude-code-vault")
	if !ok || m.Workspace != "vault" || m.SystemPrompt != "be terse" {
		t.Errorf("FindModel: %+v ok=%v", m, ok)
	}
	if path, ok := cfg.WorkspacePath("vault"); !ok || path != "/tmp/vault" {
		t.Errorf("WorkspacePath: %q ok=%v", path, ok)
	}
}

func TestLoad_ExplicitPathMissing(t *testing.T) {
	_, _, err := Load(LoadOptions{ExplicitPath: filepath.Join(t.TempDir(), "nope.yaml")})
	if err == nil {
		t.Fatal("expected error for missing explicit path")
	}
}

func TestLoad_NoConfigFile(t *testing.T) {
	t.Setenv("CCPROXY_CONFIG", "")
	t.Setenv("CCPROXY_STATE", t.TempDir()) // empty state dir
	t.Chdir(t.TempDir())                   // no ./ccproxy.yaml
	_, _, err := Load(LoadOptions{})
	if err == nil {
		t.Fatal("expected error when no config file is found")
	}
	if !strings.Contains(err.Error(), "no config file found") {
		t.Errorf("err = %v, want it to mention missing config file", err)
	}
}

func TestLoad_PassthroughOnlyConfig(t *testing.T) {
	// Empty `models:` is valid when passthrough is enabled.
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", `
state_dir: /tmp
allow_passthrough_models: true
`)
	cfg, _, err := Load(LoadOptions{ExplicitPath: p})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Models) != 0 {
		t.Errorf("models = %+v, want empty", cfg.Models)
	}
	if !cfg.PassthroughEnabled() {
		t.Error("passthrough should be enabled")
	}
}

func TestLoad_EnvOverrides(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", `
listen: ":4141"
state_dir: /var/lib/ccproxy
claude_binary: claude
models:
  - id: claude-code
`)
	t.Setenv("CCPROXY_LISTEN", ":9999")
	cfg, _, err := Load(LoadOptions{ExplicitPath: p})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("listen = %q, want override to win", cfg.Listen)
	}
	// claude_binary is YAML-only now; env override removed.
	if cfg.ClaudeBinary != "claude" {
		t.Errorf("claude_binary = %q, want yaml value", cfg.ClaudeBinary)
	}
}

func TestValidate_RejectsUnknownWorkspaceRef(t *testing.T) {
	c := Defaults()
	c.Models = []Model{{ID: "m1", Workspace: "nope"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v, want unknown-workspace error", err)
	}
}

func TestValidate_RejectsRelativeWorkspacePath(t *testing.T) {
	c := Defaults()
	c.Workspaces = map[string]Workspace{"w": {Path: "./relative"}}
	c.Models = []Model{{ID: "m1"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("err = %v, want absolute-path error", err)
	}
}

func TestValidate_RejectsDuplicateModelID(t *testing.T) {
	c := Defaults()
	c.Models = []Model{{ID: "m1"}, {ID: "m1"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("err = %v, want duplicate-id error", err)
	}
}

func TestValidate_RejectsBadDefaultVerbosity(t *testing.T) {
	c := Defaults()
	c.DefaultVerbosity = "extreme"
	c.Models = []Model{{ID: "m1"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "default_verbosity") {
		t.Errorf("err = %v", err)
	}
}

func TestDuration_UnmarshalAcceptsStringAndInt(t *testing.T) {
	dir := t.TempDir()
	p := writeYAML(t, dir, "c.yaml", `
session_ttl: 90
request_timeout: 5m
models:
  - id: m1
`)
	cfg, _, err := Load(LoadOptions{ExplicitPath: p})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SessionTTL.Duration() != 90*time.Second {
		t.Errorf("session_ttl = %v, want 90s (bare int = seconds)", cfg.SessionTTL.Duration())
	}
	if cfg.RequestTimeout.Duration() != 5*time.Minute {
		t.Errorf("request_timeout = %v, want 5m", cfg.RequestTimeout.Duration())
	}
}
