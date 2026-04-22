package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/openai"
	"github.com/guryn/ccproxy/internal/translate"
)

func newServer(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.StateDir = t.TempDir()
	if len(cfg.Models) == 0 {
		cfg.Models = []config.Model{{ID: "claude-code"}}
	}
	rt := RuntimeConfig{Cfg: &cfg}
	s, err := New(rt, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestResolveEffort_HeaderWins(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultEffort: "low",
		Models:        []config.Model{{ID: "m1", Effort: "medium"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set(translate.HeaderEffort, "high")

	got, err := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "max")
	if err != nil || got != "high" {
		t.Errorf("got (%q,%v), want (high,nil) — header should beat body+model+config", got, err)
	}
}

func TestResolveEffort_BodyBeatsAliasAndConfig(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultEffort: "low",
		Models:        []config.Model{{ID: "m1", Effort: "medium"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	got, err := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "high")
	if err != nil || got != "high" {
		t.Errorf("got (%q,%v), want body to win", got, err)
	}
}

func TestResolveEffort_AliasBeatsConfig(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultEffort: "low",
		Models:        []config.Model{{ID: "m1", Effort: "max"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	got, _ := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "")
	if got != "max" {
		t.Errorf("got %q, want alias value max", got)
	}
}

func TestResolveEffort_FallsBackToConfigDefault(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultEffort: "medium",
		Models:        []config.Model{{ID: "m1"}}, // no alias effort
	})
	req := httptest.NewRequest("POST", "/", nil)
	got, _ := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "")
	if got != "medium" {
		t.Errorf("got %q, want medium from config default", got)
	}
}

func TestResolveEffort_AllUnset(t *testing.T) {
	s := newServer(t, config.Config{Models: []config.Model{{ID: "m1"}}})
	req := httptest.NewRequest("POST", "/", nil)
	got, _ := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "")
	if got != "" {
		t.Errorf("got %q, want empty (claude default)", got)
	}
}

func TestResolveEffort_BadHeader(t *testing.T) {
	s := newServer(t, config.Config{Models: []config.Model{{ID: "m1"}}})
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set(translate.HeaderEffort, "turbo")
	_, err := s.resolveEffort(req, &openai.ChatRequest{}, s.rt.Cfg.Models[0], "")
	if err == nil || !strings.Contains(err.Error(), "turbo") {
		t.Errorf("expected validation error, got %v", err)
	}
}

func TestResolveClaudeModel_HeaderWins(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultClaudeModel: "haiku",
		Models:             []config.Model{{ID: "m1", ClaudeModel: "sonnet"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set(translate.HeaderClaudeModel, "opus")
	got := s.resolveClaudeModel(req, s.rt.Cfg.Models[0])
	if got != "opus" {
		t.Errorf("got %q, want opus", got)
	}
}

func TestResolveClaudeModel_AliasBeatsConfig(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultClaudeModel: "haiku",
		Models:             []config.Model{{ID: "m1", ClaudeModel: "sonnet"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	got := s.resolveClaudeModel(req, s.rt.Cfg.Models[0])
	if got != "sonnet" {
		t.Errorf("got %q, want sonnet", got)
	}
}

func TestResolveClaudeModel_FallsBackToConfig(t *testing.T) {
	s := newServer(t, config.Config{
		DefaultClaudeModel: "haiku",
		Models:             []config.Model{{ID: "m1"}},
	})
	req := httptest.NewRequest("POST", "/", nil)
	if got := s.resolveClaudeModel(req, s.rt.Cfg.Models[0]); got != "haiku" {
		t.Errorf("got %q, want haiku", got)
	}
}

func TestResolveClaudeModel_AllUnset(t *testing.T) {
	s := newServer(t, config.Config{Models: []config.Model{{ID: "m1"}}})
	req := httptest.NewRequest("POST", "/", nil)
	if got := s.resolveClaudeModel(req, s.rt.Cfg.Models[0]); got != "" {
		t.Errorf("got %q, want empty (claude default)", got)
	}
}
