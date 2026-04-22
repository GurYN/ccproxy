package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/ratelimit"
)

// The integration tests in this package re-invoke the test binary as a
// fake `claude` (TestHelperProcess pattern). Mode is selected via the
// CCPROXY_HELPER_MODE env var passed to the subprocess.
func TestMain(m *testing.M) {
	if os.Getenv("CCPROXY_HELPER") == "1" {
		runFakeClaude()
		return
	}
	os.Exit(m.Run())
}

func runFakeClaude() {
	switch os.Getenv("CCPROXY_HELPER_MODE") {
	case "version-only":
		// Triggered by /readyz probe with --version.
		fmt.Println("claude 0.0.0 (fake)")
		os.Exit(0)
	case "happy":
		if len(os.Args) > 1 && os.Args[1] == "--version" {
			fmt.Println("claude 0.0.0 (fake)")
			os.Exit(0)
		}
		fmt.Println(`{"type":"system","subtype":"init","cwd":"/tmp","model":"claude-opus-4-7","tools":["Bash"],"session_id":"sess-1"}`)
		fmt.Println(`{"type":"assistant","message":{"id":"msg-1","model":"claude-opus-4-7","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}},"session_id":"sess-1"}`)
		fmt.Println(`{"type":"result","subtype":"success","is_error":false,"duration_ms":12,"num_turns":1,"result":"pong","stop_reason":"end_turn","total_cost_usd":0.0001,"session_id":"sess-1","usage":{"input_tokens":3,"output_tokens":1}}`)
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "unknown helper mode")
	os.Exit(2)
}

// testServer bundles the httptest server with the bearer the test should use.
type testServer struct {
	*httptest.Server
	Bearer string
}

func newTestServer(t *testing.T, mode string) *testServer {
	t.Helper()
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	cfg.ClaudeBinary = os.Args[0]
	cfg.Models = []config.Model{{ID: "claude-code"}}

	store, err := auth.Open(filepath.Join(cfg.StateDir, "tokens.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	bearer, _, err := store.Create(t.Context(), auth.CreateOptions{
		Name:   "test",
		Scopes: []string{"chat", "session:persistent", "workspace:*"},
	})
	if err != nil {
		t.Fatalf("token create: %v", err)
	}

	rl, err := ratelimit.New(store.DB())
	if err != nil {
		t.Fatalf("ratelimit.New: %v", err)
	}
	rt := RuntimeConfig{Cfg: &cfg, Auth: store, RateLimit: rl}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(rt, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Setenv("CCPROXY_HELPER", "1")
	t.Setenv("CCPROXY_HELPER_MODE", "version-only")
	s.Probe(t.Context())
	t.Setenv("CCPROXY_HELPER_MODE", mode)
	return &testServer{Server: httptest.NewServer(s.Handler()), Bearer: bearer}
}

func doReq(t *testing.T, srv *httptest.Server, method, path, token string, body any, extraHeaders map[string]string) *http.Response {
	t.Helper()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, buf)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do req: %v", err)
	}
	return resp
}

func TestHealthz_AlwaysOK(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()
	resp := doReq(t, srv.Server, http.MethodGet, "/healthz", "", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestModels_RequiresAuth(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()

	resp := doReq(t, srv.Server, http.MethodGet, "/v1/models", "", nil, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", resp.StatusCode)
	}

	resp2 := doReq(t, srv.Server, http.MethodGet, "/v1/models", srv.Bearer, nil, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("with token: status = %d, want 200", resp2.StatusCode)
	}
	var list struct{ Data []struct{ ID string } }
	if err := json.NewDecoder(resp2.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	// Configured alias plus the three passthrough family aliases (default on).
	for _, want := range []string{"claude-code", "opus", "sonnet", "haiku"} {
		if !ids[want] {
			t.Errorf("models list missing %q, got %+v", want, list)
		}
	}
}

func TestChatCompletions_NonStreaming(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()

	body := map[string]any{
		"model": "claude-code",
		"messages": []map[string]any{
			{"role": "user", "content": "ping"},
		},
	}
	resp := doReq(t, srv.Server, http.MethodPost, "/v1/chat/completions", srv.Bearer, body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	var out struct {
		Object  string
		Choices []struct {
			Message struct {
				Role    string
				Content string
			}
			FinishReason string `json:"finish_reason"`
		}
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Object != "chat.completion" {
		t.Errorf("object = %q", out.Object)
	}
	if got := out.Choices[0].Message.Content; got != "pong" {
		t.Errorf("content = %q", got)
	}
	if out.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %q", out.Choices[0].FinishReason)
	}
	if out.Usage == nil || out.Usage.PromptTokens != 3 {
		t.Errorf("usage = %+v", out.Usage)
	}
}

func TestChatCompletions_Streaming(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()

	body := map[string]any{
		"model":    "claude-code",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	}
	resp := doReq(t, srv.Server, http.MethodPost, "/v1/chat/completions", srv.Bearer, body, nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body = %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	out := string(raw)
	if !strings.Contains(out, `"content":"pong"`) {
		t.Errorf("expected `pong` in stream, got %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("stream should end with [DONE], got tail %q", tail(out, 60))
	}
}

func TestChatCompletions_IgnoredParamsHeader(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()

	body := map[string]any{
		"model":       "claude-code",
		"temperature": 0.5,
		"max_tokens":  64,
		"messages":    []map[string]any{{"role": "user", "content": "hi"}},
	}
	resp := doReq(t, srv.Server, http.MethodPost, "/v1/chat/completions", srv.Bearer, body, nil)
	defer resp.Body.Close()

	hdr := resp.Header.Get("X-CC-Ignored-Params")
	if !strings.Contains(hdr, "temperature") || !strings.Contains(hdr, "max_tokens") {
		t.Errorf("X-CC-Ignored-Params = %q, want it to mention temperature and max_tokens", hdr)
	}
}

func TestChatCompletions_BadJSON(t *testing.T) {
	srv := newTestServer(t, "happy")
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/chat/completions", strings.NewReader("{not json"))
	req.Header.Set("Authorization", "Bearer "+srv.Bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	var body struct {
		Error struct{ Type, Message string }
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Type != "invalid_request_error" {
		t.Errorf("error.type = %q", body.Error.Type)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}
