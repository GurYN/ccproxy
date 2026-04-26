// ccproxy-bridge is the user-side daemon that exposes a local working
// directory to a ccproxy server over a single outbound websocket. While
// connected, chat requests authenticated with tokens that share the
// daemon's principal will see this directory as their workspace.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/guryn/ccproxy/internal/bridgeclient"
	"github.com/guryn/ccproxy/internal/buildver"
)

// version is overridable at build time via:
//
//	go build -ldflags "-X main.version=v0.3.0" ./cmd/ccproxy-bridge
var version = ""

func buildHandlers(root string, useConfirm bool, logger *slog.Logger) bridgeclient.Handlers {
	h := bridgeclient.NewFSHandlers(root)
	il, err := bridgeclient.LoadIgnore(root)
	if err != nil {
		logger.Warn("ignore list disabled", "err", err)
	} else {
		h.Ignore = il
	}
	if useConfirm {
		if t := bridgeclient.NewTTYConfirm(); t != nil {
			h.Confirm = t.Ask
		} else {
			logger.Warn("--confirm requested but stdin is not a TTY; auto-approving")
		}
	}
	return h
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ccproxy-bridge:", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("ccproxy-bridge", flag.ContinueOnError)
	server := fs.String("server", "", "Server URL, e.g. wss://ccproxy.lan/bridge (required)")
	root := fs.String("root", "", "Local directory to expose (required)")
	tokenEnv := fs.String("token-env", "CCPROXY_BRIDGE_TOKEN", "Env var holding the bridge token")
	allowWrite := fs.Bool("allow-write", true, "Permit write/create/mkdir/remove/rename/chmod RPCs")
	allowExec := fs.Bool("allow-exec", false, "Permit exec RPCs (DANGEROUS — grants shell access to anyone with a chat token sharing this principal)")
	watch := fs.Bool("watch", true, "Stream filesystem change notifications back to the server (FUSE cache invalidation)")
	auditPath := fs.String("audit", "", "Append one JSON line per RPC to this file. Empty disables auditing.")
	confirm := fs.Bool("confirm", false, "Prompt on stdin before every write/remove/exec RPC")
	verbose := fs.Bool("v", false, "Verbose logging")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if strings.TrimSpace(*server) == "" {
		fs.Usage()
		return fmt.Errorf("--server is required")
	}
	if strings.TrimSpace(*root) == "" {
		fs.Usage()
		return fmt.Errorf("--root is required")
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		return fmt.Errorf("resolve --root: %w", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("stat --root: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("--root %s is not a directory", abs)
	}
	bearer := os.Getenv(*tokenEnv)
	if bearer == "" {
		return fmt.Errorf("env %s is empty (set the bridge token there, or pass --token-env to point elsewhere)", *tokenEnv)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	log := func(lvl, msg string, kv ...any) {
		switch lvl {
		case "debug":
			logger.Debug(msg, kv...)
		case "warn":
			logger.Warn(msg, kv...)
		case "error":
			logger.Error(msg, kv...)
		default:
			logger.Info(msg, kv...)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	client := bridgeclient.New(bridgeclient.Options{
		ServerURL:    *server,
		BearerToken:  bearer,
		Root:         abs,
		AllowWrite:   *allowWrite,
		AllowExec:    *allowExec,
		Watch:        *watch,
		AuditPath:    *auditPath,
		PingInterval: 30 * time.Second,
		Version:      buildver.String("ccproxy-bridge", version),
	}, buildHandlers(abs, *confirm, logger), log)

	logger.Info("starting ccproxy-bridge",
		"server", *server,
		"root", abs,
		"allow_write", *allowWrite,
		"allow_exec", *allowExec)
	return client.Run(ctx)
}
