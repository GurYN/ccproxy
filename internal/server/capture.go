package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Capture writes a per-request JSONL trace to disk. One file per request id,
// under $CCPROXY_STATE/captures/. Enabled only for tokens flagged with
// DebugCapture. File perm 0600, dir perm 0700.
type Capture struct {
	mu sync.Mutex
	f  *os.File
}

type captureLine struct {
	TS      time.Time   `json:"ts"`
	Kind    string      `json:"kind"`
	Payload interface{} `json:"payload,omitempty"`
}

func openCapture(stateDir, traceID string) (*Capture, error) {
	if stateDir == "" || traceID == "" {
		return nil, fmt.Errorf("capture: missing state dir or trace id")
	}
	safeID := filepath.Base(traceID)
	if safeID == "." || safeID == "/" || strings.ContainsAny(safeID, `/\`) {
		return nil, fmt.Errorf("capture: unsafe trace id %q", traceID)
	}
	dir := filepath.Join(stateDir, "captures")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir captures: %w", err)
	}
	path := filepath.Join(dir, safeID+".jsonl")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create capture: %w", err)
	}
	return &Capture{f: f}, nil
}

// Write appends one JSONL line. Safe to call with a nil receiver — no-op.
// Errors are intentionally swallowed: capture is observability, it must not
// fail the request path.
func (c *Capture) Write(kind string, payload interface{}) {
	if c == nil {
		return
	}
	b, err := json.Marshal(captureLine{TS: time.Now().UTC(), Kind: kind, Payload: payload})
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _ = c.f.Write(append(b, '\n'))
}

func (c *Capture) Close() error {
	if c == nil {
		return nil
	}
	return c.f.Close()
}

func captureFromCtx(ctx context.Context) *Capture {
	v, _ := ctx.Value(ctxKeyCapture).(*Capture)
	return v
}

// withDebugCapture opens a capture file when the authenticated token has
// DebugCapture=true, buffers the request body so both the capture and the
// handler can read it, and attaches the capture to the request context.
// Must run AFTER authed so the token is already on the context.
func (s *Server) withDebugCapture(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := tokenFromCtx(r.Context())
		if tok == nil || !tok.DebugCapture {
			next.ServeHTTP(w, r)
			return
		}
		cap, err := openCapture(s.rt.Cfg.StateDir, traceID(r.Context()))
		if err != nil {
			s.logger.Warn("debug capture open failed", "err", err)
			next.ServeHTTP(w, r)
			return
		}
		defer func() { _ = cap.Close() }()

		body, berr := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
		if berr == nil {
			var parsed interface{}
			if json.Unmarshal(body, &parsed) != nil {
				parsed = string(body)
			}
			cap.Write("request", map[string]interface{}{
				"method":  r.Method,
				"path":    r.URL.Path,
				"query":   r.URL.RawQuery,
				"headers": redactHeaders(r.Header),
				"body":    parsed,
			})
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		ctx := context.WithValue(r.Context(), ctxKeyCapture, cap)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// redactHeaders copies headers into a flat map and scrubs secrets.
func redactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if strings.EqualFold(k, "Authorization") {
			out[k] = "[redacted]"
			continue
		}
		out[k] = strings.Join(vs, ", ")
	}
	return out
}
