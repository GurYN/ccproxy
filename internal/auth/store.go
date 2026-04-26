package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrInvalidToken is returned when the bearer can't be authenticated.
var ErrInvalidToken = errors.New("invalid token")

// ErrUnknownToken is returned by management calls when no row matches.
var ErrUnknownToken = errors.New("token not found")

// Store is the SQLite-backed token registry. It is safe for concurrent use.
type Store struct {
	db   *sql.DB
	now  func() time.Time
	mu   sync.RWMutex
	// authCache memoizes verified bearers to skip argon2 on hot paths.
	// Keyed by the prefix portion only — the value carries the expected hash
	// so we can still compare in constant time.
	authCache    map[string]*authCacheEntry
	authCacheTTL time.Duration
}

type authCacheEntry struct {
	hash      []byte
	token     *Token
	expiresAt time.Time
}

// Open opens (and creates if needed) the token DB at path. The directory
// must already exist.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("auth.Open: empty path")
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite + WAL: one writer is sufficient and avoids lock churn

	s := &Store{
		db:           db,
		now:          time.Now,
		authCache:    map[string]*authCacheEntry{},
		authCacheTTL: 2 * time.Minute,
	}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close releases the underlying DB handle.
func (s *Store) Close() error { return s.db.Close() }

// DB returns the underlying *sql.DB so other components (e.g. ratelimit)
// can share the same SQLite connection pool.
func (s *Store) DB() *sql.DB { return s.db }

// Path returns a likely default location for the token DB given a state dir.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "tokens.db")
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS tokens (
  id              TEXT PRIMARY KEY,
  name            TEXT NOT NULL,
  prefix          TEXT NOT NULL UNIQUE,
  hash            BLOB NOT NULL,
  scopes          TEXT NOT NULL,
  created_at      INTEGER NOT NULL,
  expires_at      INTEGER,
  last_used_at    INTEGER,
  rate_limit_rpm  INTEGER NOT NULL DEFAULT 0,
  rate_limit_tpd  INTEGER NOT NULL DEFAULT 0,
  debug_capture   INTEGER NOT NULL DEFAULT 0,
  default_verb    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_tokens_prefix ON tokens(prefix);
`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	// Additive migrations. Each ALTER is wrapped in a sniff so re-runs are
	// idempotent on databases that already have the column.
	if err := s.addColumnIfMissing(ctx, "tokens", "principal", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return fmt.Errorf("add tokens.principal: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_tokens_principal ON tokens(principal)`); err != nil {
		return err
	}
	return nil
}

// addColumnIfMissing is a tiny sqlite-specific helper for additive schema
// changes. SQLite has no IF NOT EXISTS for ALTER COLUMN, so we sniff
// PRAGMA table_info first.
func (s *Store) addColumnIfMissing(ctx context.Context, table, column, decl string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// CreateOptions describes a new token request.
type CreateOptions struct {
	Name             string
	Scopes           []string
	TTL              time.Duration // 0 = no expiry
	RateLimitRPM     int
	RateLimitTPD     int
	DebugCapture     bool
	DefaultVerbosity string
	// Principal opaquely groups tokens that should share a bridge
	// attachment. See Token.Principal.
	Principal string
}

// Create inserts a new token row and returns the bearer string (which
// MUST be displayed to the user once — it is not recoverable from the DB)
// and the persisted Token record.
func (s *Store) Create(ctx context.Context, opts CreateOptions) (string, *Token, error) {
	if strings.TrimSpace(opts.Name) == "" {
		return "", nil, errors.New("token name is required")
	}
	if len(opts.Scopes) == 0 {
		opts.Scopes = []string{string(ScopeChat)}
	}
	bearer, prefix, hash, err := generateBearer()
	if err != nil {
		return "", nil, err
	}
	id := newID()
	now := s.now().UTC()
	var expiresAt *time.Time
	if opts.TTL > 0 {
		t := now.Add(opts.TTL)
		expiresAt = &t
	}
	scopes := strings.Join(opts.Scopes, ",")

	_, err = s.db.ExecContext(ctx, `
INSERT INTO tokens (id, name, prefix, hash, scopes, created_at, expires_at,
                    rate_limit_rpm, rate_limit_tpd, debug_capture, default_verb,
                    principal)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, opts.Name, prefix, hash, scopes,
		now.Unix(), nullableUnix(expiresAt),
		opts.RateLimitRPM, opts.RateLimitTPD,
		boolToInt(opts.DebugCapture), opts.DefaultVerbosity,
		opts.Principal,
	)
	if err != nil {
		return "", nil, fmt.Errorf("insert: %w", err)
	}
	tok := &Token{
		ID:           id,
		Name:         opts.Name,
		Prefix:       prefix,
		Scopes:       opts.Scopes,
		CreatedAt:    now,
		ExpiresAt:    expiresAt,
		RateLimitRPM: opts.RateLimitRPM,
		RateLimitTPD: opts.RateLimitTPD,
		DebugCapture: opts.DebugCapture,
		DefaultVerb:  opts.DefaultVerbosity,
		Principal:    opts.Principal,
	}
	return bearer, tok, nil
}

// Authenticate resolves a bearer string to a Token. It first checks the
// in-memory cache; cache misses pay the argon2id cost.
func (s *Store) Authenticate(ctx context.Context, bearer string) (*Token, error) {
	prefix, secret, err := parseBearer(bearer)
	if err != nil {
		return nil, ErrInvalidToken
	}

	if entry := s.cacheGet(prefix); entry != nil {
		if !verifySecret(secret, entry.hash) {
			return nil, ErrInvalidToken
		}
		if entry.token.IsExpired(s.now()) {
			s.cacheDelete(prefix)
			return nil, ErrInvalidToken
		}
		s.touchAsync(entry.token.ID)
		return entry.token, nil
	}

	row := s.db.QueryRowContext(ctx, `
SELECT id, name, prefix, hash, scopes, created_at, expires_at, last_used_at,
       rate_limit_rpm, rate_limit_tpd, debug_capture, default_verb,
       principal
FROM tokens WHERE prefix = ?`, prefix)
	tok, hash, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if !verifySecret(secret, hash) {
		return nil, ErrInvalidToken
	}
	if tok.IsExpired(s.now()) {
		return nil, ErrInvalidToken
	}
	s.cachePut(prefix, hash, tok)
	s.touchAsync(tok.ID)
	return tok, nil
}

// List returns all tokens, ordered by creation time desc.
func (s *Store) List(ctx context.Context) ([]*Token, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, prefix, hash, scopes, created_at, expires_at, last_used_at,
       rate_limit_rpm, rate_limit_tpd, debug_capture, default_verb,
       principal
FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, _, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ResolveIDOrName returns the token id for a string that might already be
// an id or might be a unique name. Unknown or ambiguous values return
// ErrUnknownToken. Useful for CLI commands that take an identifier.
func (s *Store) ResolveIDOrName(ctx context.Context, idOrName string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM tokens WHERE id = ?`, idOrName).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM tokens WHERE name = ?`, idOrName)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return "", err
		}
		matches = append(matches, m)
	}
	switch len(matches) {
	case 0:
		return "", ErrUnknownToken
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous name %q matches %d tokens; use the id", idOrName, len(matches))
	}
}

// SetDebugCapture flips the debug_capture flag on an existing token row.
// Takes effect on the next auth after the in-memory cache entry expires
// (default TTL 2m) — or immediately for tokens not yet cached.
func (s *Store) SetDebugCapture(ctx context.Context, id string, enabled bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET debug_capture = ? WHERE id = ?`,
		boolToInt(enabled), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUnknownToken
	}
	s.cacheClear()
	return nil
}

// Revoke deletes the token row and drops it from the cache.
func (s *Store) Revoke(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrUnknownToken
	}
	s.cacheClear() // simpler than tracking which prefix matched id
	return nil
}

// Rotate generates a new bearer for an existing token id, invalidating
// the previous one. Scopes / limits / name are preserved.
func (s *Store) Rotate(ctx context.Context, id string) (string, *Token, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
SELECT id, name, prefix, hash, scopes, created_at, expires_at, last_used_at,
       rate_limit_rpm, rate_limit_tpd, debug_capture, default_verb,
       principal
FROM tokens WHERE id = ?`, id)
	tok, _, err := scanToken(row)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, ErrUnknownToken
	}
	if err != nil {
		return "", nil, err
	}
	bearer, prefix, hash, err := generateBearer()
	if err != nil {
		return "", nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET prefix = ?, hash = ? WHERE id = ?`, prefix, hash, id); err != nil {
		return "", nil, err
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	tok.Prefix = prefix
	s.cacheClear()
	return bearer, tok, nil
}

// SeedFromBearer is the M1→M2 migration path: if the DB has no tokens and
// a non-empty bearer is provided (e.g. from the legacy CCPROXY_TOKEN env),
// create one with chat scope and return it. Otherwise it's a no-op.
//
// SeedFromBearer can only seed the env value if it is in the new
// ccp_<prefix>_<secret> format; arbitrary strings can't be backfilled
// because the prefix-indexed lookup needs the structured shape.
// Returns the count of tokens after the call so callers can warn appropriately.
func (s *Store) SeedFromBearer(ctx context.Context, bearer string) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 || bearer == "" {
		return n, nil
	}
	prefix, secret, err := parseBearer(bearer)
	if err != nil {
		// Legacy env value isn't in our format — caller must rotate.
		return n, fmt.Errorf("CCPROXY_TOKEN is not in ccp_<prefix>_<secret> format; create a token via `ccproxy token create` instead: %w", err)
	}
	id := newID()
	now := s.now().UTC()
	hash := hashSecret(secret)
	_, err = s.db.ExecContext(ctx, `
INSERT INTO tokens (id, name, prefix, hash, scopes, created_at, default_verb)
VALUES (?, ?, ?, ?, ?, ?, '')`,
		id, "seed (CCPROXY_TOKEN)", prefix, hash, string(ScopeChat),
		now.Unix(),
	)
	if err != nil {
		return n, err
	}
	return n + 1, nil
}

// Count returns how many tokens are stored.
func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`).Scan(&n)
	return n, err
}

// touchAsync updates last_used_at without blocking the caller. Errors are
// swallowed — a missed touch is not worth failing a request.
func (s *Store) touchAsync(id string) {
	now := s.now().Unix()
	go func() {
		_, _ = s.db.Exec(`UPDATE tokens SET last_used_at = ? WHERE id = ?`, now, id)
	}()
}

// --- cache ----------------------------------------------------------------

func (s *Store) cacheGet(prefix string) *authCacheEntry {
	s.mu.RLock()
	e := s.authCache[prefix]
	s.mu.RUnlock()
	if e == nil {
		return nil
	}
	if s.now().After(e.expiresAt) {
		s.cacheDelete(prefix)
		return nil
	}
	return e
}

func (s *Store) cachePut(prefix string, hash []byte, t *Token) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authCache[prefix] = &authCacheEntry{
		hash:      hash,
		token:     t,
		expiresAt: s.now().Add(s.authCacheTTL),
	}
}

func (s *Store) cacheDelete(prefix string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.authCache, prefix)
}

func (s *Store) cacheClear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authCache = map[string]*authCacheEntry{}
}

// --- scan helpers ---------------------------------------------------------

type rowScanner interface {
	Scan(dest ...any) error
}

func scanToken(rs rowScanner) (*Token, []byte, error) {
	var (
		t              Token
		scopes         string
		createdAt      int64
		expiresAt      sql.NullInt64
		lastUsedAt     sql.NullInt64
		debugCapture   int
		hash           []byte
	)
	if err := rs.Scan(
		&t.ID, &t.Name, &t.Prefix, &hash, &scopes,
		&createdAt, &expiresAt, &lastUsedAt,
		&t.RateLimitRPM, &t.RateLimitTPD, &debugCapture, &t.DefaultVerb,
		&t.Principal,
	); err != nil {
		return nil, nil, err
	}
	t.Scopes = parseScopesCSV(scopes)
	t.CreatedAt = time.Unix(createdAt, 0).UTC()
	if expiresAt.Valid {
		ts := time.Unix(expiresAt.Int64, 0).UTC()
		t.ExpiresAt = &ts
	}
	if lastUsedAt.Valid {
		ts := time.Unix(lastUsedAt.Int64, 0).UTC()
		t.LastUsedAt = &ts
	}
	t.DebugCapture = debugCapture != 0
	return &t, hash, nil
}

func parseScopesCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func nullableUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
