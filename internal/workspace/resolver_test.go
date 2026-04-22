package workspace

import (
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guryn/ccproxy/internal/config"
)

func newCfg(t *testing.T) *config.Config {
	t.Helper()
	c := config.Defaults()
	c.StateDir = t.TempDir()
	c.Workspaces = map[string]config.Workspace{
		"vault": {Path: "/tmp/vault-ws"},
	}
	c.Models = []config.Model{
		{ID: "m-bound", Workspace: "vault"},
		{ID: "m-free"},
	}
	return &c
}

func TestResolve_HeaderWins(t *testing.T) {
	r, err := New(newCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set(HeaderName, "vault")
	res, err := r.Resolve(req, config.Model{ID: "m-free"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/tmp/vault-ws" || res.Ephemeral {
		t.Errorf("res = %+v", res)
	}
}

func TestResolve_HeaderUnknown(t *testing.T) {
	r, err := New(newCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set(HeaderName, "no-such-thing")
	_, err = r.Resolve(req, config.Model{ID: "m-free"})
	if !errors.Is(err, ErrUnknownWorkspace) {
		t.Fatalf("err = %v, want ErrUnknownWorkspace", err)
	}
}

func TestResolve_ModelBindingFallback(t *testing.T) {
	r, err := New(newCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	res, err := r.Resolve(req, config.Model{ID: "m-bound", Workspace: "vault"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Path != "/tmp/vault-ws" || res.Name != "vault" {
		t.Errorf("res = %+v", res)
	}
}

func TestResolve_EphemeralDefault(t *testing.T) {
	cfg := newCfg(t)
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	res, err := r.Resolve(req, config.Model{ID: "m-free"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ephemeral {
		t.Fatalf("expected ephemeral, got %+v", res)
	}
	if !strings.HasPrefix(res.Path, filepath.Join(cfg.StateDir, "tmp")) {
		t.Errorf("ephemeral path %q not under StateDir/tmp", res.Path)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Errorf("ephemeral dir not created: %v", err)
	}
	res.Cleanup()
	if _, err := os.Stat(res.Path); !os.IsNotExist(err) {
		t.Errorf("ephemeral dir not cleaned up: %v", err)
	}
}

func TestResolveByName(t *testing.T) {
	r, err := New(newCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.ResolveByName("vault")
	if err != nil || res.Path != "/tmp/vault-ws" {
		t.Errorf("ResolveByName(vault) = %+v, %v", res, err)
	}
	res2, err := r.ResolveByName("")
	if err != nil || !res2.Ephemeral {
		t.Errorf("ResolveByName(\"\") should return ephemeral, got %+v %v", res2, err)
	}
	res2.Cleanup()
}
