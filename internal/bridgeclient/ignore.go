package bridgeclient

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// IgnoreList is a glob-pattern allowlist/denylist read from a
// .ccproxyignore file at the daemon's root. It uses the same simplified
// semantics as .gitignore: blank lines and lines starting with `#` are
// comments; trailing slashes mark directory-only patterns; patterns are
// matched with filepath.Match against each path segment.
//
// Defaults always denied even if no .ccproxyignore exists:
//   .ssh, .aws, .gnupg, id_rsa*, id_ed25519*, .env*
type IgnoreList struct {
	patterns []string
}

var builtinDenyPatterns = []string{
	".ssh", ".aws", ".gnupg",
	"id_rsa*", "id_ed25519*", "id_ecdsa*",
	".env", ".env.*",
}

// LoadIgnore reads .ccproxyignore from root (if present) and merges with
// the built-in deny list.
func LoadIgnore(root string) (*IgnoreList, error) {
	il := &IgnoreList{patterns: append([]string(nil), builtinDenyPatterns...)}
	f, err := os.Open(filepath.Join(root, ".ccproxyignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return il, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		il.patterns = append(il.patterns, line)
	}
	return il, sc.Err()
}

// Match reports whether rel (a path relative to root) hits any deny
// pattern. Hidden segments are matched independently so a pattern like
// `.ssh` denies `foo/.ssh/key`.
func (i *IgnoreList) Match(rel string) bool {
	if i == nil {
		return false
	}
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" || rel == "." {
		return false
	}
	parts := strings.Split(rel, "/")
	for _, pat := range i.patterns {
		dirOnly := strings.HasSuffix(pat, "/")
		p := strings.TrimSuffix(pat, "/")
		for _, seg := range parts {
			if ok, _ := filepath.Match(p, seg); ok {
				if dirOnly {
					// Approximation: we don't know if seg is a dir here;
					// treat dir-only patterns as matching any segment of
					// that name. Tighter matching belongs in B4.
				}
				return true
			}
		}
	}
	return false
}
