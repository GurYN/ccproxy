package config

import (
	"fmt"
	"time"
)

// Config is the validated, in-memory shape of $CCPROXY_STATE/config.yaml.
// PRD §6.7.
type Config struct {
	Listen                 string               `yaml:"listen"`
	MetricsListen          string               `yaml:"metrics_listen"`
	StateDir               string               `yaml:"state_dir"`
	ClaudeBinary           string               `yaml:"claude_binary"`
	DefaultVerbosity       string               `yaml:"default_verbosity"`
	DefaultEffort          string               `yaml:"default_effort"`       // low|medium|high|xhigh|max; empty = leave to claude
	DefaultClaudeModel     string               `yaml:"default_claude_model"` // e.g. "opus", "sonnet", "haiku" or a full model id
	// DefaultPermissionMode is passed to `claude --permission-mode` when an
	// alias does not override it. Claude's own default prompts interactively,
	// which is broken under -p (no TTY). Recommended values for a proxy:
	//   bypassPermissions — honor the trust decision already made by minting
	//                       the token. Best for trusted LAN/Tailscale deploys.
	//   acceptEdits       — auto-accept file writes but still gate shell etc.
	// Leave empty to inherit claude's default (will prompt and hang).
	DefaultPermissionMode  string               `yaml:"default_permission_mode"`
	AllowPassthroughModels *bool                `yaml:"allow_passthrough_models"`
	ModelVersions          map[string]string    `yaml:"model_versions"` // alias → full claude id (e.g. opus → claude-opus-4-7)
	SessionTTL             Duration             `yaml:"session_ttl"`
	RequestTimeout         Duration             `yaml:"request_timeout"`
	Workspaces             map[string]Workspace `yaml:"workspaces"`
	Models                 []Model              `yaml:"models"`
}

// PassthroughEnabled reports whether the request `model` field accepts
// well-known Claude family names (opus|sonnet|haiku) and full claude-*
// ids in addition to configured aliases. Default: true.
func (c *Config) PassthroughEnabled() bool {
	if c.AllowPassthroughModels == nil {
		return true
	}
	return *c.AllowPassthroughModels
}

// ResolvePassthroughModel returns the underlying claude id for a passthrough
// request — the pinned version from ModelVersions when present, else the
// raw id (which claude itself will resolve or reject).
//
// IsPassthrough reports whether id matches the passthrough allowlist:
// the bare family aliases opus/sonnet/haiku, or full ids of the form
// claude-{opus,sonnet,haiku}-*.
func (c *Config) ResolvePassthroughModel(id string) (resolved string, isPassthrough bool) {
	switch id {
	case "opus", "sonnet", "haiku":
		if v, ok := c.ModelVersions[id]; ok && v != "" {
			return v, true
		}
		return id, true
	}
	if isFullClaudeID(id) {
		return id, true
	}
	return "", false
}

// isFullClaudeID accepts ids like "claude-sonnet-4-6", "claude-opus-4-7",
// "claude-haiku-4-5" — anything starting with one of the family prefixes.
// Bare "claude-" or "claude-code-foo" do not match (those would either be
// a configured alias or a typo).
func isFullClaudeID(id string) bool {
	for _, prefix := range []string{"claude-opus-", "claude-sonnet-", "claude-haiku-"} {
		if len(id) > len(prefix) && id[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// Workspace is a named, server-side directory clients can target.
// Clients never supply free-form paths (PRD §6.4 last paragraph).
type Workspace struct {
	Path        string `yaml:"path"`
	Description string `yaml:"description,omitempty"`
}

// Model is a published model alias (PRD §6.7 example). The Workspace
// field, when non-empty, must reference a key in Config.Workspaces.
type Model struct {
	ID           string `yaml:"id"`
	Workspace    string `yaml:"workspace,omitempty"`
	SystemPrompt string `yaml:"system_prompt,omitempty"`
	// Effort, when set, becomes the per-alias default reasoning effort.
	// Overrides Config.DefaultEffort; itself overridden by the request.
	Effort string `yaml:"effort,omitempty"`
	// ClaudeModel pins the underlying Claude model passed to `claude --model`
	// (e.g. "opus", "sonnet", "haiku" or a full id like "claude-sonnet-4-6").
	// Overrides Config.DefaultClaudeModel; itself overridden by X-CC-Claude-Model.
	ClaudeModel string `yaml:"claude_model,omitempty"`
	// PermissionMode overrides Config.DefaultPermissionMode for this alias.
	// See Config.DefaultPermissionMode for accepted values.
	PermissionMode string `yaml:"permission_mode,omitempty"`
}

// FindModel returns the Model with the matching id and a bool ok flag.
func (c *Config) FindModel(id string) (Model, bool) {
	for _, m := range c.Models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}

// WorkspacePath returns the on-disk path for a workspace name. Empty name
// returns ("", false). Unknown name returns ("", false) — callers should
// translate that into an OpenAI invalid_request_error.
func (c *Config) WorkspacePath(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	w, ok := c.Workspaces[name]
	if !ok {
		return "", false
	}
	return w.Path, true
}

// Duration is a YAML-friendly wrapper around time.Duration that accepts
// strings like "30m" or "5s".
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case string:
		dur, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("parse duration %q: %w", v, err)
		}
		*d = Duration(dur)
	case int:
		*d = Duration(time.Duration(v) * time.Second)
	case int64:
		*d = Duration(time.Duration(v) * time.Second)
	case float64:
		*d = Duration(time.Duration(v) * time.Second)
	default:
		return fmt.Errorf("unsupported duration type %T", raw)
	}
	return nil
}
