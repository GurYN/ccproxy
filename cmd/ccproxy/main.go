// Command ccproxy is the OpenAI-compatible proxy in front of Claude Code.
//
// Subcommands:
//
//	ccproxy serve     run the HTTP server
//	ccproxy version   print build info
//	ccproxy token     manage bearer tokens (M2)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/server"
)

func main() {
	logger := slog.New(newLogHandler(os.Stdout))
	slog.SetDefault(logger)

	if len(os.Args) < 2 {
		usage(os.Stdout)
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = runServe(ctx, args)
	case "version":
		err = runVersion(args)
	case "token":
		err = runToken(ctx, args)
	case "-h", "--help", "help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		// flag.ErrHelp is what stdlib returns when -h/--help is passed under
		// ContinueOnError. The usage text was already printed to stderr by
		// the flag package; don't log it as a failure.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logger.Error("command failed", "cmd", cmd, "err", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `ccproxy — OpenAI-compatible proxy in front of Claude Code

Usage:
  ccproxy <command> [flags]

Commands:
  serve     Run the HTTP server
  version   Print build info
  token     Manage bearer tokens (create | list | revoke | rotate | update)
  help      Show this message

Serve flags:
  --env-file <path>   Load KEY=VALUE pairs into env before reading config.
                      Default: .env in CWD (silently skipped if missing).
                      Existing env vars always win.
  --config <path>     YAML config. Defaults to $CCPROXY_CONFIG,
                      then $CCPROXY_STATE/config.yaml, then ./ccproxy.yaml.

Token subcommands:
  ccproxy token create --name <s> [--scopes 'chat,workspace:myproject'] [--ttl 90d] [--debug-capture]
  ccproxy token list
  ccproxy token revoke <id-or-name>
  ccproxy token rotate <id-or-name>
  ccproxy token update <id-or-name> [--debug-capture on|off]

  Quote the --scopes value when it contains '*' (zsh/bash glob workaround):
    ccproxy token create --name all --scopes 'chat,workspace:*,session:persistent'
`)
}

// version is overridable at build time via:
//
//	go build -ldflags "-X main.version=v0.3.0" ./cmd/ccproxy
//
// When unset, we fall back to the module's embedded version (which is
// (devel) for `go run` and untagged builds, or the release tag for
// `go install github.com/guryn/ccproxy/cmd/ccproxy@vX.Y.Z`).
var version = ""

func runVersion(_ []string) error {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("ccproxy (build info unavailable)")
		return nil
	}
	rev, dirty, when := "", "", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		case "vcs.time":
			when = s.Value
		}
	}

	v := version
	if v == "" {
		v = info.Main.Version
	}
	fmt.Printf("ccproxy %s%s\n", v, dirty)
	fmt.Printf("  go:   %s\n", info.GoVersion)
	// VCS info is only present when `go build` runs inside a git checkout.
	// For `go install …@vX.Y.Z` (module proxy) the tag itself is the id —
	// skip the rev/when lines rather than printing "unknown".
	if rev != "" {
		fmt.Printf("  rev:  %s%s\n", rev, dirty)
	}
	if when != "" {
		fmt.Printf("  when: %s\n", when)
	}
	return nil
}

func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	envFile := fs.String("env-file", ".env",
		"Path to a KEY=VALUE file loaded into the environment before reading config. Existing env vars win.")
	configPath := fs.String("config", "",
		"Path to a YAML config file. Defaults to $CCPROXY_CONFIG, then $CCPROXY_STATE/config.yaml, then ./ccproxy.yaml.")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if err := loadEnvFile(*envFile, fsExplicit(fs, "env-file")); err != nil {
		return err
	}

	rt, source, err := server.LoadRuntime(ctx, config.LoadOptions{
		ExplicitPath: *configPath,
	}, slog.Default())
	if err != nil {
		return err
	}
	defer func() { _ = rt.Close() }()
	slog.Default().Info("config loaded", "source", source,
		"models", len(rt.Cfg.Models), "workspaces", len(rt.Cfg.Workspaces))

	srv, err := server.New(rt, slog.Default())
	if err != nil {
		return err
	}
	return srv.Run(ctx)
}

// loadEnvFile reads path into the process environment via godotenv.Load
// (existing env vars always win). A missing file at the default path is
// silently skipped; a missing file at a user-specified path is fatal.
func loadEnvFile(path string, explicit bool) error {
	err := godotenv.Load(path)
	switch {
	case err == nil:
		slog.Default().Info("env file loaded", "path", path)
		return nil
	case errors.Is(err, os.ErrNotExist) && !explicit:
		return nil
	default:
		return fmt.Errorf("env-file %s: %w", path, err)
	}
}

// newLogHandler returns a slog.Handler tuned for where the output goes:
// TextHandler on a TTY (short, readable), JSONHandler otherwise (pipes,
// journald, Docker log drivers). Override by setting CCPROXY_LOG_FORMAT to
// `json` or `text` explicitly.
func newLogHandler(w *os.File) slog.Handler {
	opts := &slog.HandlerOptions{Level: slog.LevelInfo}
	format := strings.ToLower(os.Getenv("CCPROXY_LOG_FORMAT"))
	if format == "" {
		if isTerminal(w) {
			format = "text"
		} else {
			format = "json"
		}
	}
	if format == "text" {
		return slog.NewTextHandler(w, opts)
	}
	return slog.NewJSONHandler(w, opts)
}

// isTerminal reports whether f is a character device (stdin/stdout pointed
// at a real terminal). Works on linux/darwin without golang.org/x/term.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// fsExplicit reports whether the named flag was actually set on the command
// line (vs left at its default).
func fsExplicit(fs *flag.FlagSet, name string) bool {
	var seen bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}
