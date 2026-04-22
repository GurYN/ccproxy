package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Validate checks a Config for internal consistency. It does NOT touch the
// filesystem (workspace paths are not stat'd), so this is safe to run at
// startup before mounts/volumes are set up.
func (c *Config) Validate() error {
	var errs []string

	switch c.DefaultVerbosity {
	case "text-only", "verbose", "narrated":
	default:
		errs = append(errs, fmt.Sprintf("default_verbosity %q must be text-only, verbose, or narrated", c.DefaultVerbosity))
	}

	if !validEffortValue(c.DefaultEffort) {
		errs = append(errs, fmt.Sprintf("default_effort %q must be one of: low, medium, high, xhigh, max (or empty)", c.DefaultEffort))
	}

	for k, v := range c.ModelVersions {
		switch k {
		case "opus", "sonnet", "haiku":
		default:
			errs = append(errs, fmt.Sprintf("model_versions key %q must be one of: opus, sonnet, haiku", k))
			continue
		}
		if !isFullClaudeID(v) {
			errs = append(errs, fmt.Sprintf("model_versions[%q] = %q must be a full claude id like claude-%s-4-6", k, v, k))
		}
	}

	if len(c.Models) == 0 && !c.PassthroughEnabled() {
		errs = append(errs, "models[] is empty and allow_passthrough_models is false: define at least one alias or enable passthrough")
	}
	seenIDs := map[string]bool{}
	for i, m := range c.Models {
		if m.ID == "" {
			errs = append(errs, fmt.Sprintf("models[%d].id is empty", i))
		}
		if seenIDs[m.ID] {
			errs = append(errs, fmt.Sprintf("models[%d].id %q is duplicated", i, m.ID))
		}
		seenIDs[m.ID] = true
		if m.Workspace != "" {
			if _, ok := c.Workspaces[m.Workspace]; !ok {
				errs = append(errs, fmt.Sprintf("models[%d].workspace %q does not exist in workspaces", i, m.Workspace))
			}
		}
		if !validEffortValue(m.Effort) {
			errs = append(errs, fmt.Sprintf("models[%d].effort %q must be one of: low, medium, high, xhigh, max (or empty)", i, m.Effort))
		}
	}

	for name, w := range c.Workspaces {
		if w.Path == "" {
			errs = append(errs, fmt.Sprintf("workspaces[%q].path is empty", name))
			continue
		}
		if !filepath.IsAbs(w.Path) {
			errs = append(errs, fmt.Sprintf("workspaces[%q].path %q must be absolute", name, w.Path))
		}
	}

	if len(errs) > 0 {
		return errors.New("config invalid:\n  - " + strings.Join(errs, "\n  - "))
	}
	return nil
}

// validEffortValue mirrors the values accepted by `claude --effort`. Empty
// means "unset" and is always valid.
func validEffortValue(s string) bool {
	switch s {
	case "", "low", "medium", "high", "xhigh", "max":
		return true
	}
	return false
}
