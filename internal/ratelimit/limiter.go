package ratelimit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ErrRateLimited is returned when a request is denied. The chat handler
// turns this into a 429 with the OpenAI rate_limit_exceeded shape.
var ErrRateLimited = errors.New("rate limited")

// Limiter enforces per-token request and daily-token quotas.
//
// Requests/minute (rpm) is enforced by an in-memory token-bucket per token
// id (golang.org/x/time/rate). Tokens/day (tpd) is enforced by a SQLite
// table so the count survives restarts within the same UTC day.
//
// Either limit set to zero means "unlimited."
type Limiter struct {
	db  *sql.DB
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*tokenBucket
}

type tokenBucket struct {
	rl  *rate.Limiter
	rpm int // remembered so we can rebuild on rpm changes
}

// New returns a Limiter that uses db as its persistence layer for the
// daily counter. The Limiter owns no goroutines — callers do all work
// inline on the request path.
func New(db *sql.DB) (*Limiter, error) {
	l := &Limiter{
		db:      db,
		now:     time.Now,
		buckets: map[string]*tokenBucket{},
	}
	if err := l.migrate(context.Background()); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Limiter) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS token_usage_daily (
  token_id    TEXT NOT NULL,
  day         TEXT NOT NULL,  -- YYYY-MM-DD UTC
  total       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (token_id, day)
);`
	_, err := l.db.ExecContext(ctx, schema)
	return err
}

// AllowRequest checks the rpm bucket for tokenID and reserves a slot if
// allowed. Returns ErrRateLimited when over budget.
func (l *Limiter) AllowRequest(tokenID string, rpm int) error {
	if rpm <= 0 {
		return nil
	}
	bucket := l.bucketFor(tokenID, rpm)
	if !bucket.rl.Allow() {
		return ErrRateLimited
	}
	return nil
}

// CheckDailyTokens returns ErrRateLimited if the token's running daily
// count is already at or above tpd. Pre-flight check before letting a
// request proceed; the actual increment happens via RecordUsage.
func (l *Limiter) CheckDailyTokens(ctx context.Context, tokenID string, tpd int) error {
	if tpd <= 0 {
		return nil
	}
	day := l.now().UTC().Format("2006-01-02")
	row := l.db.QueryRowContext(ctx, `SELECT total FROM token_usage_daily WHERE token_id = ? AND day = ?`, tokenID, day)
	var total int
	switch err := row.Scan(&total); {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	if total >= tpd {
		return ErrRateLimited
	}
	return nil
}

// RecordUsage adds delta tokens to the running daily counter for tokenID.
// Called after a request completes (regardless of success).
func (l *Limiter) RecordUsage(ctx context.Context, tokenID string, delta int) error {
	if delta <= 0 || tokenID == "" {
		return nil
	}
	day := l.now().UTC().Format("2006-01-02")
	_, err := l.db.ExecContext(ctx, `
INSERT INTO token_usage_daily (token_id, day, total) VALUES (?, ?, ?)
ON CONFLICT(token_id, day) DO UPDATE SET total = total + excluded.total`,
		tokenID, day, delta)
	if err != nil {
		return fmt.Errorf("ratelimit record: %w", err)
	}
	return nil
}

// DailyUsage reports the current count for one token id (for /v1/sessions
// or admin use).
func (l *Limiter) DailyUsage(ctx context.Context, tokenID string) (int, error) {
	day := l.now().UTC().Format("2006-01-02")
	var n int
	err := l.db.QueryRowContext(ctx, `SELECT total FROM token_usage_daily WHERE token_id = ? AND day = ?`, tokenID, day).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func (l *Limiter) bucketFor(tokenID string, rpm int) *tokenBucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[tokenID]
	if !ok || b.rpm != rpm {
		// Convert rpm to a per-second rate; burst = full minute's worth so
		// a small burst at the start of a window is allowed.
		perSec := rate.Limit(float64(rpm) / 60.0)
		b = &tokenBucket{
			rl:  rate.NewLimiter(perSec, rpm),
			rpm: rpm,
		}
		l.buckets[tokenID] = b
	}
	return b
}
