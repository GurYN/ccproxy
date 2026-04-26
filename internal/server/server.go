package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"path/filepath"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/bridge"
	"github.com/guryn/ccproxy/internal/obs"
	"github.com/guryn/ccproxy/internal/session"
	"github.com/guryn/ccproxy/internal/workspace"
)

// Server owns HTTP wiring. Stateless across requests in M2.1; M2.3 adds
// the session registry, M2.5 adds the SQLite token store.
type Server struct {
	rt           RuntimeConfig
	logger       *slog.Logger
	workspace    *workspace.Resolver
	sessions     *session.Manager
	metrics      *obs.Metrics
	bridges      *bridge.Registry
	bridgeMounts *bridge.Manager

	// readiness state, populated at Probe.
	readyMu       sync.RWMutex
	claudeVersion string
	claudeReady   bool
	claudeErr     string
}

func New(rt RuntimeConfig, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	ws, err := workspace.New(rt.Cfg)
	if err != nil {
		return nil, fmt.Errorf("workspace resolver: %w", err)
	}
	metrics := obs.NewMetrics()
	mgr := session.NewManager(rt.Cfg.SessionTTL.Duration(), ws)
	mgr.SetObserver(metrics)
	mounts, err := bridge.NewManager(filepath.Join(rt.Cfg.StateDir, "bridges"))
	if err != nil {
		return nil, fmt.Errorf("bridge mount manager: %w", err)
	}
	registry := bridge.NewRegistry()
	ws.SetBridgeLookup(workspace.BridgeLookupFunc(func(principal string) (string, bool) {
		if principal == "" {
			return "", false
		}
		conn := registry.Lookup(principal)
		if conn == nil {
			return "", false
		}
		mt, err := mounts.Mount(conn)
		if err != nil || mt == nil {
			return "", false
		}
		return mt.Path, true
	}))
	return &Server{
		rt:           rt,
		logger:       logger,
		workspace:    ws,
		sessions:     mgr,
		metrics:      metrics,
		bridges:      registry,
		bridgeMounts: mounts,
	}, nil
}

// Probe runs `claude --version` once and caches the result for /readyz.
// Called from Run before the listener accepts traffic.
func (s *Server) Probe(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, s.rt.Cfg.ClaudeBinary, "--version").Output()
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if err != nil {
		s.claudeReady = false
		s.claudeErr = err.Error()
		return
	}
	s.claudeReady = true
	s.claudeVersion = strings.TrimSpace(string(out))
}

// Handler returns the configured http.Handler. Useful for tests; production
// code calls Run.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /v1/models", s.metrics.Instrument("/v1/models",
		s.authed(auth.ScopeChat, http.HandlerFunc(s.handleModels))))
	mux.Handle("POST /v1/chat/completions", s.metrics.Instrument("/v1/chat/completions",
		s.authed(auth.ScopeChat, s.withDebugCapture(http.HandlerFunc(s.handleChatCompletions)))))
	mux.Handle("GET /bridge", s.authed(auth.ScopeBridgeConnect, http.HandlerFunc(s.handleBridge)))

	return s.withTraceID(mux)
}

// Metrics returns the metrics registry so external entry points (e.g. the
// metrics listener) can build a /metrics handler.
func (s *Server) Metrics() *obs.Metrics { return s.metrics }

// Run starts the HTTP listener (and the metrics listener if configured)
// and blocks until ctx is cancelled. On shutdown it gives in-flight
// requests up to 30 s to finish.
func (s *Server) Run(ctx context.Context) error {
	s.Probe(ctx)
	s.sessions.Start(ctx)
	defer s.sessions.Stop()

	main := &http.Server{
		Addr:              s.rt.Cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	mainErr := serveAsync(main, s.logger.With("listener", "main"))

	var metricsErr <-chan error
	var metricsSrv *http.Server
	if addr := s.rt.Cfg.MetricsListen; addr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
		metricsSrv = &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		metricsErr = serveAsync(metricsSrv, s.logger.With("listener", "metrics"))
	}

	s.logger.Info("listening",
		"addr", s.rt.Cfg.Listen,
		"metrics_addr", s.rt.Cfg.MetricsListen,
		"models", len(s.rt.Cfg.Models),
		"workspaces", len(s.rt.Cfg.Workspaces),
		"claude_ready", s.isReady())

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = main.Shutdown(shutdownCtx)
		if metricsSrv != nil {
			_ = metricsSrv.Shutdown(shutdownCtx)
		}
		if err := <-mainErr; err != nil {
			return err
		}
		if metricsErr != nil {
			if err := <-metricsErr; err != nil {
				return err
			}
		}
		return nil
	case err := <-mainErr:
		return err
	case err := <-metricsErr:
		return err
	}
}

func serveAsync(srv *http.Server, logger *slog.Logger) <-chan error {
	out := make(chan error, 1)
	go func() {
		logger.Info("listener up", "addr", srv.Addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		out <- err
	}()
	return out
}

func (s *Server) isReady() bool {
	s.readyMu.RLock()
	defer s.readyMu.RUnlock()
	return s.claudeReady
}

func (s *Server) versionString() (version string, ready bool, errMsg string) {
	s.readyMu.RLock()
	defer s.readyMu.RUnlock()
	return s.claudeVersion, s.claudeReady, s.claudeErr
}
