package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/config"
)

func runToken(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("ccproxy token: subcommand required (create | list | revoke | rotate | update)")
	}
	sub, rest := args[0], args[1:]

	store, err := openStoreForCLI(ctx)
	if err != nil {
		return err
	}
	defer store.Close()

	switch sub {
	case "create":
		return tokenCreate(ctx, store, rest)
	case "list":
		return tokenList(ctx, store, rest)
	case "revoke":
		return tokenRevoke(ctx, store, rest)
	case "rotate":
		return tokenRotate(ctx, store, rest)
	case "update":
		return tokenUpdate(ctx, store, rest)
	default:
		return fmt.Errorf("unknown token subcommand %q", sub)
	}
}

// openStoreForCLI reuses the env-file + config loading flow so the CLI
// finds the same SQLite file the server would. It does NOT seed from
// CCPROXY_TOKEN — that would create unintended tokens.
func openStoreForCLI(_ context.Context) (*auth.Store, error) {
	// Load .env from CWD so $CCPROXY_STATE / $CCPROXY_CONFIG resolve.
	_ = godotenv.Load(".env")

	cfg, _, err := config.Load(config.LoadOptions{})
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("ensure state_dir %s: %w", cfg.StateDir, err)
	}
	return auth.Open(auth.DefaultPath(cfg.StateDir))
}

func tokenCreate(ctx context.Context, store *auth.Store, args []string) error {
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	name := fs.String("name", "", "Human-readable label (required)")
	scopes := fs.String("scopes", "chat", "Comma-separated scopes (quote the value if it contains '*': --scopes 'chat,workspace:*')")
	ttl := fs.String("ttl", "", "Token lifetime, e.g. 90d, 24h. Empty = no expiry.")
	rpm := fs.Int("rpm", 0, "Per-token rate limit in requests/minute (0 = unlimited; M2.6)")
	tpd := fs.Int("tpd", 0, "Per-token rate limit in tokens/day (0 = unlimited; M2.6)")
	verb := fs.String("default-verbosity", "", "Per-token default verbosity (text-only|verbose|narrated)")
	debugCap := fs.Bool("debug-capture", false, "Tee every request lifecycle to $CCPROXY_STATE/captures/<req-id>.jsonl. Off by default.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("--name is required")
	}
	parsedScopes, err := auth.ParseScopes(*scopes)
	if err != nil {
		return err
	}
	dur, err := parseTTL(*ttl)
	if err != nil {
		return err
	}

	bearer, tok, err := store.Create(ctx, auth.CreateOptions{
		Name:             *name,
		Scopes:           parsedScopes,
		TTL:              dur,
		RateLimitRPM:     *rpm,
		RateLimitTPD:     *tpd,
		DefaultVerbosity: *verb,
		DebugCapture:     *debugCap,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Created token %s (%s)\n", tok.Name, tok.ID)
	fmt.Printf("  scopes:  %s\n", strings.Join(tok.Scopes, ", "))
	if tok.ExpiresAt != nil {
		fmt.Printf("  expires: %s\n", tok.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Println()
	fmt.Println("Bearer (will not be shown again):")
	fmt.Println(bearer)
	return nil
}

func tokenList(ctx context.Context, store *auth.Store, _ []string) error {
	tokens, err := store.List(ctx)
	if err != nil {
		return err
	}
	if len(tokens) == 0 {
		fmt.Println("(no tokens)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSCOPES\tCAPTURE\tCREATED\tEXPIRES\tLAST USED")
	for _, t := range tokens {
		exp := "-"
		if t.ExpiresAt != nil {
			exp = t.ExpiresAt.Format(time.RFC3339)
		}
		last := "-"
		if t.LastUsedAt != nil {
			last = t.LastUsedAt.Format(time.RFC3339)
		}
		cap := "off"
		if t.DebugCapture {
			cap = "on"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Name, strings.Join(t.Scopes, ","), cap,
			t.CreatedAt.Format(time.RFC3339), exp, last)
	}
	return tw.Flush()
}

func tokenRevoke(ctx context.Context, store *auth.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ccproxy token revoke <id-or-name>")
	}
	id, err := store.ResolveIDOrName(ctx, args[0])
	if err != nil {
		return err
	}
	if err := store.Revoke(ctx, id); err != nil {
		return err
	}
	slog.Default().Info("token revoked", "id", id)
	return nil
}

func tokenUpdate(ctx context.Context, store *auth.Store, args []string) error {
	fs := flag.NewFlagSet("token update", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: ccproxy token update <id> [flags]")
		fmt.Fprintln(fs.Output(), "  <id>  Token id (see `ccproxy token list`). Required.")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
	}
	debugCap := fs.String("debug-capture", "", "on|off to toggle per-request JSONL capture. Leave empty to not change it.")

	// Stdlib flag.Parse stops at the first non-flag token, so `<id>` has to
	// come first. Pull it off, then parse the remainder.
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		fs.Usage()
		return errors.New("missing <id> (must be the first argument)")
	}
	idOrName := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected extra arguments: %v", fs.Args())
	}
	id, err := store.ResolveIDOrName(ctx, idOrName)
	if err != nil {
		return err
	}

	changed := false
	switch strings.ToLower(strings.TrimSpace(*debugCap)) {
	case "":
		// no-op
	case "on", "true", "1", "yes":
		if err := store.SetDebugCapture(ctx, id, true); err != nil {
			return err
		}
		fmt.Printf("Token %s: debug_capture = on\n", id)
		changed = true
	case "off", "false", "0", "no":
		if err := store.SetDebugCapture(ctx, id, false); err != nil {
			return err
		}
		fmt.Printf("Token %s: debug_capture = off\n", id)
		changed = true
	default:
		return fmt.Errorf("invalid --debug-capture value %q (want on|off)", *debugCap)
	}
	if !changed {
		return errors.New("nothing to update (pass at least one flag, e.g. --debug-capture=on)")
	}
	return nil
}

func tokenRotate(ctx context.Context, store *auth.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: ccproxy token rotate <id-or-name>")
	}
	id, err := store.ResolveIDOrName(ctx, args[0])
	if err != nil {
		return err
	}
	bearer, tok, err := store.Rotate(ctx, id)
	if err != nil {
		return err
	}
	fmt.Printf("Rotated token %s (%s).\n", tok.Name, tok.ID)
	fmt.Println("New bearer (will not be shown again):")
	fmt.Println(bearer)
	return nil
}

// parseTTL accepts Go duration strings plus an extra "Nd" days form.
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var days int
		_, err := fmt.Sscanf(s, "%dd", &days)
		if err != nil {
			return 0, fmt.Errorf("invalid days TTL %q: %w", s, err)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}
