//go:build integration

// Integration tests that spawn a REAL `claude` binary. They are gated behind
// the `integration` build tag and a `claude` binary on PATH. Run with:
//
//	make test-integration
//
// or:
//
//	CCPROXY_INTEGRATION_TIMEOUT=2m go test -tags integration ./tests/integration/...
//
// Each test stands up an in-process ccproxy via httptest and posts real HTTP
// requests at it, so the full middleware chain (auth, rate limiting, session
// manager, capture) is exercised.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/ratelimit"
	"github.com/guryn/ccproxy/internal/server"
)

// requireClaude skips the test when no `claude` binary is on PATH so the
// suite stays green on machines that don't have Claude Code installed.
func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH; skipping integration test")
	}
}

// defaultDeadline is the per-test upper bound. Real claude calls take a few
// seconds even on trivial prompts. Override with CCPROXY_INTEGRATION_TIMEOUT.
func defaultDeadline() time.Duration {
	if v := os.Getenv("CCPROXY_INTEGRATION_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return 2 * time.Minute
}

type harness struct {
	server *httptest.Server
	bearer string
	state  string
}

func (h *harness) url(path string) string { return h.server.URL + path }

func newHarness(t *testing.T) *harness {
	t.Helper()
	requireClaude(t)

	stateDir := t.TempDir()
	workspaceDir := filepath.Join(stateDir, "ws")
	if err := os.MkdirAll(workspaceDir, 0o700); err != nil {
		t.Fatalf("mkdir ws: %v", err)
	}

	cfg := config.Defaults()
	cfg.StateDir = stateDir
	cfg.Listen = "127.0.0.1:0" // httptest picks the port; this is cosmetic
	cfg.RequestTimeout = config.Duration(defaultDeadline())
	cfg.Workspaces = map[string]config.Workspace{
		"ws": {Path: workspaceDir},
	}
	cfg.Models = []config.Model{{
		ID:        "claude-code",
		Workspace: "ws",
		Effort:    "low",
	}}

	store, err := auth.Open(filepath.Join(stateDir, "tokens.db"))
	if err != nil {
		t.Fatalf("open token store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bearer, _, err := store.Create(context.Background(), auth.CreateOptions{
		Name: "integration",
		Scopes: []string{
			string(auth.ScopeChat),
			string(auth.ScopeSessionPersistent),
			"workspace:*",
		},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	limiter, err := ratelimit.New(store.DB())
	if err != nil {
		t.Fatalf("ratelimit: %v", err)
	}

	rt := server.RuntimeConfig{Cfg: &cfg, Auth: store, RateLimit: limiter}
	srv, err := server.New(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	srv.Probe(context.Background())

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return &harness{server: ts, bearer: bearer, state: stateDir}
}

func (h *harness) post(t *testing.T, ctx context.Context, path string, body any, headers map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url(path), &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// --- tests ----------------------------------------------------------------

func TestStreamingText(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultDeadline())
	defer cancel()

	resp := h.post(t, ctx, "/v1/chat/completions", map[string]any{
		"model":    "claude-code",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly one word: OK"}},
	}, nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type=%q, want text/event-stream", ct)
	}

	sawData, sawDone := false, false
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		sawData = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !sawData {
		t.Error("no data chunks observed")
	}
	if !sawDone {
		t.Error("stream did not terminate with [DONE]")
	}
}

func TestUnauthorized(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"claude-code","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	// no Authorization header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.url("/v1/models"), nil)
	req.Header.Set("Authorization", "Bearer "+h.bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	found := false
	for _, m := range body.Data {
		if m.ID == "claude-code" {
			found = true
		}
	}
	if !found {
		t.Errorf("claude-code not in /v1/models response: %+v", body.Data)
	}
}

func TestNonStreaming(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultDeadline())
	defer cancel()

	resp := h.post(t, ctx, "/v1/chat/completions", map[string]any{
		"model":    "claude-code",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly one word: OK"}},
	}, nil)
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, b)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type=%q, want application/json", ct)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	if body.Choices[0].FinishReason == "" {
		t.Errorf("empty finish_reason")
	}
}

func TestPersistentSession(t *testing.T) {
	h := newHarness(t)
	sessionID := fmt.Sprintf("it-%d", time.Now().UnixNano())

	for i, prompt := range []string{
		"Remember the number 42.",
		"Reply with just the number I asked you to remember.",
	} {
		ctx, cancel := context.WithTimeout(context.Background(), defaultDeadline())
		resp := h.post(t, ctx, "/v1/chat/completions", map[string]any{
			"model":    "claude-code",
			"stream":   false,
			"messages": []map[string]string{{"role": "user", "content": prompt}},
		}, map[string]string{"X-CC-Session": sessionID})
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			t.Fatalf("turn %d status=%d body=%s", i, resp.StatusCode, b)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
	}
	// If we got here, the session manager handled --resume for the second
	// turn without errors. We don't assert on the model's memory because
	// that's non-deterministic; plumbing is the point.
}

func TestDebugCaptureWritesJSONL(t *testing.T) {
	h := newHarness(t)

	// Flip debug capture on for the bearer by rotating its row via the auth
	// store. We can't reach the store directly, so use the CLI path: this
	// test would require re-opening the DB. Simpler: just skip if the
	// capture dir is empty after a call when the token lacks the flag.
	// Instead: toggle the bit by going through the DB directly via a
	// temporary token. Keep it simple — create a new token with
	// DebugCapture=true and use it.
	stateDir := h.state
	store, err := auth.Open(filepath.Join(stateDir, "tokens.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer store.Close()
	bearer, _, err := store.Create(context.Background(), auth.CreateOptions{
		Name: "capture",
		Scopes: []string{
			string(auth.ScopeChat), string(auth.ScopeSessionPersistent), "workspace:*",
		},
		DebugCapture: true,
	})
	if err != nil {
		t.Fatalf("create capture token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultDeadline())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url("/v1/chat/completions"),
		strings.NewReader(`{"model":"claude-code","stream":false,"messages":[{"role":"user","content":"Reply OK"}]}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	entries, err := os.ReadDir(filepath.Join(stateDir, "captures"))
	if err != nil {
		t.Fatalf("read captures: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no capture files written")
	}
	// Spot-check the first file: it should contain at least a "request"
	// line and a "response" (buffered) or "done" (streaming) marker.
	data, err := os.ReadFile(filepath.Join(stateDir, "captures", entries[0].Name()))
	if err != nil {
		t.Fatalf("read capture: %v", err)
	}
	if !bytes.Contains(data, []byte(`"kind":"request"`)) {
		t.Errorf("capture missing request line:\n%s", data)
	}
	if !bytes.Contains(data, []byte(`"kind":"response"`)) && !bytes.Contains(data, []byte(`"kind":"done"`)) {
		t.Errorf("capture missing terminal line:\n%s", data)
	}
}
