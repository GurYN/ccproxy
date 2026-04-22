package auth

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "tokens.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateAndAuthenticate(t *testing.T) {
	s := newStore(t)
	bearer, tok, err := s.Create(t.Context(), CreateOptions{
		Name:   "n8n",
		Scopes: []string{"chat", "workspace:vault"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(bearer, "ccp_") {
		t.Errorf("bearer prefix = %q", bearer)
	}
	if tok.Name != "n8n" || len(tok.Scopes) != 2 {
		t.Errorf("token = %+v", tok)
	}

	got, err := s.Authenticate(t.Context(), bearer)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != tok.ID {
		t.Errorf("authenticated id = %q, want %q", got.ID, tok.ID)
	}
}

func TestAuthenticate_RejectsTampered(t *testing.T) {
	s := newStore(t)
	bearer, _, _ := s.Create(t.Context(), CreateOptions{Name: "x"})
	bad := bearer[:len(bearer)-1] + "0" // flip last hex char
	if bad == bearer {
		bad = bearer[:len(bearer)-1] + "1"
	}
	if _, err := s.Authenticate(t.Context(), bad); err != ErrInvalidToken {
		t.Errorf("err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticate_RejectsExpired(t *testing.T) {
	s := newStore(t)
	bearer, _, err := s.Create(t.Context(), CreateOptions{Name: "x", TTL: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := s.Authenticate(t.Context(), bearer); err != ErrInvalidToken {
		t.Errorf("err = %v, want ErrInvalidToken (TTL elapsed)", err)
	}
}

func TestRevoke(t *testing.T) {
	s := newStore(t)
	bearer, tok, _ := s.Create(t.Context(), CreateOptions{Name: "x"})
	if err := s.Revoke(t.Context(), tok.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), bearer); err != ErrInvalidToken {
		t.Errorf("revoked token still authenticates: err=%v", err)
	}
	if err := s.Revoke(t.Context(), tok.ID); err != ErrUnknownToken {
		t.Errorf("second Revoke err = %v, want ErrUnknownToken", err)
	}
}

func TestRotate(t *testing.T) {
	s := newStore(t)
	old, tok, _ := s.Create(t.Context(), CreateOptions{Name: "x"})
	bearer, _, err := s.Rotate(t.Context(), tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bearer == old {
		t.Error("rotated bearer should differ from old")
	}
	if _, err := s.Authenticate(t.Context(), old); err != ErrInvalidToken {
		t.Errorf("old bearer still authenticates after rotate: %v", err)
	}
	if _, err := s.Authenticate(t.Context(), bearer); err != nil {
		t.Errorf("new bearer should authenticate: %v", err)
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	for _, n := range []string{"a", "b", "c"} {
		if _, _, err := s.Create(t.Context(), CreateOptions{Name: n}); err != nil {
			t.Fatal(err)
		}
	}
	tokens, err := s.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 3 {
		t.Errorf("len = %d", len(tokens))
	}
}

func TestSeedFromBearer_OnlyOnEmptyDB(t *testing.T) {
	s := newStore(t)
	bearer, _, _, err := generateBearer()
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.SeedFromBearer(t.Context(), bearer)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("count after seed = %d, want 1", n)
	}
	if _, err := s.Authenticate(t.Context(), bearer); err != nil {
		t.Errorf("seeded bearer should authenticate: %v", err)
	}
	// Second seed must be a no-op since DB is non-empty.
	other, _, _, _ := generateBearer()
	n2, err := s.SeedFromBearer(t.Context(), other)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 1 {
		t.Errorf("second seed should not add a row, got count %d", n2)
	}
}

func TestSeedFromBearer_RejectsLegacyFormat(t *testing.T) {
	s := newStore(t)
	if _, err := s.SeedFromBearer(t.Context(), "dev-change-me"); err == nil {
		t.Error("expected error on legacy non-ccp_ format")
	}
}

func TestScopes_HasWildcard(t *testing.T) {
	scopes := []string{"chat", "workspace:*"}
	if !Has(scopes, ScopeChat) {
		t.Error("chat denied")
	}
	if !Has(scopes, WorkspaceScope("anything")) {
		t.Error("workspace:* should grant workspace:anything")
	}
	if Has([]string{"chat"}, ScopeAdmin) {
		t.Error("plain chat scope shouldn't grant admin")
	}
}

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes("chat, workspace:vault, session:persistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("got %v", got)
	}
	if _, err := ParseScopes("garbage"); err == nil {
		t.Error("expected error on unknown scope")
	}
}

func TestAuthCacheHitsBypassDB(t *testing.T) {
	s := newStore(t)
	bearer, _, _ := s.Create(t.Context(), CreateOptions{Name: "x"})
	if _, err := s.Authenticate(t.Context(), bearer); err != nil {
		t.Fatal(err)
	}
	// Close the underlying DB; cache hit should still succeed.
	_ = s.db.Close()
	if _, err := s.Authenticate(t.Context(), bearer); err != nil {
		t.Errorf("expected cache hit to bypass closed DB, got err=%v", err)
	}
}
