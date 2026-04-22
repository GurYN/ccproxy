package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/ratelimit"
)

// RuntimeConfig wraps the validated config, auth store, and rate limiter.
type RuntimeConfig struct {
	Cfg       *config.Config
	Auth      *auth.Store
	RateLimit *ratelimit.Limiter
	// LegacyBearer is the value of CCPROXY_TOKEN at boot, if any. Kept so
	// SeedAuth can offer a migration path; cleared after the seed call.
	LegacyBearer string
}

// LoadRuntime resolves the YAML config, opens the SQLite token store, and
// (if both the DB is empty and CCPROXY_TOKEN is set in the new format)
// seeds the bearer so the M1 → M2 upgrade is one restart.
func LoadRuntime(ctx context.Context, opts config.LoadOptions, logger *slog.Logger) (RuntimeConfig, string, error) {
	cfg, source, err := config.Load(opts)
	if err != nil {
		return RuntimeConfig{}, source, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return RuntimeConfig{}, source, fmt.Errorf("ensure state_dir %s: %w", cfg.StateDir, err)
	}
	store, err := auth.Open(auth.DefaultPath(cfg.StateDir))
	if err != nil {
		return RuntimeConfig{}, source, fmt.Errorf("open token store: %w", err)
	}
	limiter, err := ratelimit.New(store.DB())
	if err != nil {
		_ = store.Close()
		return RuntimeConfig{}, source, fmt.Errorf("init rate limiter: %w", err)
	}

	rt := RuntimeConfig{
		Cfg:          cfg,
		Auth:         store,
		RateLimit:    limiter,
		LegacyBearer: strings.TrimSpace(os.Getenv("CCPROXY_TOKEN")),
	}
	if err := rt.SeedAuth(ctx, logger); err != nil {
		return RuntimeConfig{}, source, err
	}
	return rt, source, nil
}

// SeedAuth ensures the token DB has at least one usable bearer on boot.
// Resolution order when the DB is empty:
//  1. CCPROXY_TOKEN is set → seed that value (ops/IaC path).
//  2. Otherwise → auto-mint a fresh token with chat scope and print it
//     once in a startup banner. Operator copies it; it cannot be recovered.
func (rt *RuntimeConfig) SeedAuth(ctx context.Context, logger *slog.Logger) error {
	count, err := rt.Auth.Count(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	if rt.LegacyBearer != "" {
		if _, err := rt.Auth.SeedFromBearer(ctx, rt.LegacyBearer); err != nil {
			logger.Warn("could not seed token DB from CCPROXY_TOKEN; create one with `ccproxy token create`",
				"err", err)
			return nil
		}
		logger.Info("seeded token DB from CCPROXY_TOKEN; rotate at your earliest convenience")
		return nil
	}

	bearer, tok, err := rt.Auth.Create(ctx, auth.CreateOptions{
		Name:   "auto-seed (first boot)",
		Scopes: []string{string(auth.ScopeChat)},
	})
	if err != nil {
		return fmt.Errorf("auto-mint first token: %w", err)
	}
	printFirstTokenBanner(logger, bearer, tok.Name)
	return nil
}

// printFirstTokenBanner writes the one-time bearer directly to stderr with
// a visually distinct banner so it is hard to miss. Intentionally bypasses
// the structured logger — slog would escape the newlines and collapse the
// banner into a single wall of text, defeating its purpose. The bearer
// cannot be recovered, so the operator must copy it now.
func printFirstTokenBanner(_ *slog.Logger, bearer, name string) {
	bar := strings.Repeat("=", 72)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintln(os.Stderr, "  ccproxy: no tokens in store — auto-minted a first bearer.")
	fmt.Fprintln(os.Stderr, "  name:   "+name)
	fmt.Fprintln(os.Stderr, "  bearer: "+bearer)
	fmt.Fprintln(os.Stderr, "  COPY THIS NOW — it is not recoverable. Rotate via `ccproxy token`.")
	fmt.Fprintln(os.Stderr, bar)
	fmt.Fprintln(os.Stderr)
}

// Close releases held resources. Safe to call multiple times.
func (rt *RuntimeConfig) Close() error {
	if rt.Auth != nil {
		err := rt.Auth.Close()
		rt.Auth = nil
		return err
	}
	return nil
}

var errTokenStoreUnset = errors.New("token store not initialized")
