package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/ratelimit"
)

func newPassthroughServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.StateDir = t.TempDir()
	if len(cfg.Models) == 0 {
		cfg.Models = []config.Model{{ID: "claude-code"}}
	}
	store, err := auth.Open(filepath.Join(cfg.StateDir, "tokens.db"))
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rl, err := ratelimit.New(store.DB())
	if err != nil {
		t.Fatalf("ratelimit.New: %v", err)
	}
	rt := RuntimeConfig{Cfg: &cfg, Auth: store, RateLimit: rl}
	s, err := New(rt, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestResolveModel_AliasFirstThenPassthrough(t *testing.T) {
	s := newPassthroughServer(t, config.Config{
		Models: []config.Model{{ID: "claude-code"}, {ID: "haiku", ClaudeModel: "custom"}},
	})
	// Configured "haiku" alias must win over the family-name passthrough.
	m, ok := s.resolveModel("haiku")
	if !ok || m.ClaudeModel != "custom" {
		t.Errorf("alias must beat passthrough, got %+v ok=%v", m, ok)
	}
}

func TestResolveModel_PassthroughFamilyUnpinned(t *testing.T) {
	s := newPassthroughServer(t, config.Config{Models: []config.Model{{ID: "claude-code"}}})
	m, ok := s.resolveModel("haiku")
	if !ok || m.ClaudeModel != "haiku" || m.ID != "haiku" {
		t.Errorf("unpinned passthrough = %+v", m)
	}
}

func TestResolveModel_PassthroughFamilyPinned(t *testing.T) {
	s := newPassthroughServer(t, config.Config{
		ModelVersions: map[string]string{"haiku": "claude-haiku-4-5"},
		Models:        []config.Model{{ID: "claude-code"}},
	})
	m, ok := s.resolveModel("haiku")
	if !ok || m.ClaudeModel != "claude-haiku-4-5" {
		t.Errorf("pinned passthrough = %+v", m)
	}
}

func TestResolveModel_PassthroughFullID(t *testing.T) {
	s := newPassthroughServer(t, config.Config{Models: []config.Model{{ID: "claude-code"}}})
	m, ok := s.resolveModel("claude-sonnet-4-6")
	if !ok || m.ClaudeModel != "claude-sonnet-4-6" {
		t.Errorf("full-id passthrough = %+v", m)
	}
}

func TestResolveModel_PassthroughDisabled(t *testing.T) {
	off := false
	s := newPassthroughServer(t, config.Config{
		AllowPassthroughModels: &off,
		Models:                 []config.Model{{ID: "claude-code"}},
	})
	if _, ok := s.resolveModel("haiku"); ok {
		t.Error("passthrough disabled should reject 'haiku'")
	}
	if _, ok := s.resolveModel("claude-code"); !ok {
		t.Error("aliases should still work when passthrough disabled")
	}
}

func TestResolveModel_UnknownStillRejected(t *testing.T) {
	s := newPassthroughServer(t, config.Config{Models: []config.Model{{ID: "claude-code"}}})
	if _, ok := s.resolveModel("gpt-4o"); ok {
		t.Error("non-claude id must not pass through")
	}
}

func TestModels_ListsPassthroughFamilies(t *testing.T) {
	cfg := config.Defaults()
	cfg.StateDir = t.TempDir()
	cfg.Models = []config.Model{{ID: "claude-code"}}
	cfg.ModelVersions = map[string]string{"haiku": "claude-haiku-4-5"}

	store, err := auth.Open(filepath.Join(cfg.StateDir, "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	bearer, _, _ := store.Create(t.Context(), auth.CreateOptions{Name: "t", Scopes: []string{"chat"}})
	rl, _ := ratelimit.New(store.DB())
	s, _ := New(RuntimeConfig{Cfg: &cfg, Auth: store, RateLimit: rl},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var list struct {
		Data []struct {
			ID      string
			OwnedBy string `json:"owned_by"`
		}
	}
	_ = json.NewDecoder(resp.Body).Decode(&list)

	ids := map[string]string{}
	for _, m := range list.Data {
		ids[m.ID] = m.OwnedBy
	}
	for _, want := range []string{"claude-code", "opus", "sonnet", "haiku"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("expected model %q in list, got %+v", want, ids)
		}
	}
	if got := ids["haiku"]; got != "claude-passthrough → claude-haiku-4-5" {
		t.Errorf("haiku owned_by = %q, want pinned-version annotation", got)
	}
}
