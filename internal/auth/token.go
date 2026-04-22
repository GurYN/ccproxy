package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// Bearer token format: ccp_<8 hex prefix>_<24 hex secret>.
// The prefix is the public lookup key (column-indexed in SQLite) so we
// avoid scanning every row on every request. The secret is what we hash.

const (
	tokenPrefixBytes = 4  // 8 hex chars
	tokenSecretBytes = 12 // 24 hex chars
	tokenSchemePfx   = "ccp_"
)

// Token is the persisted record minus the secret hash (which never leaves
// the store).
type Token struct {
	ID            string
	Name          string
	Prefix        string
	Scopes        []string
	CreatedAt     time.Time
	LastUsedAt    *time.Time
	ExpiresAt     *time.Time
	RateLimitRPM  int
	RateLimitTPD  int
	DebugCapture  bool
	DefaultVerb   string
}

// IsExpired reports whether t.ExpiresAt is set and in the past.
func (t *Token) IsExpired(now time.Time) bool {
	return t.ExpiresAt != nil && !t.ExpiresAt.After(now)
}

// HasScope is a convenience over Has(t.Scopes, ...).
func (t *Token) HasScope(s Scope) bool { return Has(t.Scopes, s) }

// generateBearer produces a fresh bearer token along with its prefix and the
// argon2id hash of its secret part.
func generateBearer() (bearer, prefix string, hash []byte, err error) {
	pb := make([]byte, tokenPrefixBytes)
	sb := make([]byte, tokenSecretBytes)
	if _, err := rand.Read(pb); err != nil {
		return "", "", nil, err
	}
	if _, err := rand.Read(sb); err != nil {
		return "", "", nil, err
	}
	prefix = hex.EncodeToString(pb)
	secret := hex.EncodeToString(sb)
	bearer = tokenSchemePfx + prefix + "_" + secret
	hash = hashSecret(secret)
	return bearer, prefix, hash, nil
}

// parseBearer splits a bearer token into its prefix and secret components.
// Anything that doesn't match the format yields an error so the auth
// middleware can fail fast and not waste an argon2 round.
func parseBearer(bearer string) (prefix, secret string, err error) {
	if !strings.HasPrefix(bearer, tokenSchemePfx) {
		return "", "", errors.New("token does not start with ccp_")
	}
	rest := bearer[len(tokenSchemePfx):]
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 {
		return "", "", errors.New("token missing prefix/secret separator")
	}
	prefix, secret = parts[0], parts[1]
	if len(prefix) != tokenPrefixBytes*2 || len(secret) != tokenSecretBytes*2 {
		return "", "", fmt.Errorf("token prefix/secret length wrong (%d/%d)", len(prefix), len(secret))
	}
	return prefix, secret, nil
}

// argon2id parameters. Per PRD M2.5: moderate cost so per-request auth
// stays under ~30 ms on a mid-range host. Combined with the in-memory cache
// in store.go, the steady-state cost is one hash per cache TTL window.
const (
	argonTime    = 1
	argonMemory  = 32 * 1024 // 32 MiB
	argonThreads = 2
	argonKeyLen  = 32
)

var argonSalt = []byte("ccproxy-argon2id-v1") // deterministic salt — fine for token verification (per-token randomness lives in the token itself)

func hashSecret(secret string) []byte {
	return argon2.IDKey([]byte(secret), argonSalt, argonTime, argonMemory, argonThreads, argonKeyLen)
}

func verifySecret(secret string, want []byte) bool {
	got := hashSecret(secret)
	return subtle.ConstantTimeCompare(got, want) == 1
}
