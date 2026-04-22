package auth

import (
	"fmt"
	"strings"
)

// Scope is a single permission grant. Scopes follow PRD §6.6.
type Scope string

const (
	ScopeChat              Scope = "chat"
	ScopeSessionPersistent Scope = "session:persistent"
	ScopeAdmin             Scope = "admin"

	// ScopeWorkspaceWildcard grants access to every configured workspace.
	ScopeWorkspaceWildcard Scope = "workspace:*"
)

// WorkspaceScope returns the scope value for one named workspace.
func WorkspaceScope(name string) Scope { return Scope("workspace:" + name) }

// Has reports whether scopes contains required, treating workspace:* as a
// supergrant over any workspace:<name>.
func Has(scopes []string, required Scope) bool {
	r := string(required)
	if strings.HasPrefix(r, "workspace:") {
		for _, s := range scopes {
			if s == r || s == string(ScopeWorkspaceWildcard) {
				return true
			}
		}
		return false
	}
	for _, s := range scopes {
		if s == r {
			return true
		}
	}
	return false
}

// ParseScopes splits a comma-separated list and validates each entry.
func ParseScopes(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	raw := strings.Split(s, ",")
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		v := strings.TrimSpace(r)
		if v == "" {
			continue
		}
		if !validScope(v) {
			return nil, fmt.Errorf("unknown scope %q", v)
		}
		out = append(out, v)
	}
	return out, nil
}

func validScope(s string) bool {
	switch s {
	case string(ScopeChat), string(ScopeSessionPersistent), string(ScopeAdmin), string(ScopeWorkspaceWildcard):
		return true
	}
	return strings.HasPrefix(s, "workspace:") && len(s) > len("workspace:")
}
