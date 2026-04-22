package ratelimit

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "rl.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestAllowRequest_RPMZeroIsUnlimited(t *testing.T) {
	l, err := New(newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		if err := l.AllowRequest("tok", 0); err != nil {
			t.Fatalf("rpm=0 should be unlimited, denied at #%d", i)
		}
	}
}

func TestAllowRequest_RPMBurstThenDeny(t *testing.T) {
	l, err := New(newDB(t))
	if err != nil {
		t.Fatal(err)
	}
	const rpm = 5
	// Burst should allow 5 immediately, the 6th should fail.
	for i := 0; i < rpm; i++ {
		if err := l.AllowRequest("tok", rpm); err != nil {
			t.Fatalf("burst slot #%d denied: %v", i, err)
		}
	}
	if err := l.AllowRequest("tok", rpm); err != ErrRateLimited {
		t.Errorf("expected ErrRateLimited after burst, got %v", err)
	}
}

func TestAllowRequest_DifferentTokensIndependent(t *testing.T) {
	l, _ := New(newDB(t))
	for i := 0; i < 5; i++ {
		_ = l.AllowRequest("a", 5)
	}
	if err := l.AllowRequest("b", 5); err != nil {
		t.Errorf("token b should not be affected by token a; got %v", err)
	}
}

func TestDailyTokens(t *testing.T) {
	l, _ := New(newDB(t))
	if err := l.CheckDailyTokens(t.Context(), "tok", 100); err != nil {
		t.Fatalf("empty count should pass: %v", err)
	}
	for i := 0; i < 4; i++ {
		if err := l.RecordUsage(t.Context(), "tok", 25); err != nil {
			t.Fatalf("RecordUsage #%d: %v", i, err)
		}
	}
	got, err := l.DailyUsage(t.Context(), "tok")
	if err != nil || got != 100 {
		t.Errorf("DailyUsage = %d (err=%v), want 100", got, err)
	}
	if err := l.CheckDailyTokens(t.Context(), "tok", 100); err != ErrRateLimited {
		t.Errorf("at-cap check should deny, got %v", err)
	}
	if err := l.CheckDailyTokens(t.Context(), "tok", 101); err != nil {
		t.Errorf("just-below-cap check should pass, got %v", err)
	}
}

func TestDailyTokens_DayRollover(t *testing.T) {
	l, _ := New(newDB(t))
	now := time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	_ = l.RecordUsage(t.Context(), "tok", 50)

	now = now.Add(48 * time.Hour) // jump two days
	got, _ := l.DailyUsage(t.Context(), "tok")
	if got != 0 {
		t.Errorf("DailyUsage after rollover = %d, want 0", got)
	}
}
