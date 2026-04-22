package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/workspace"
)

func newResolver(t *testing.T) *workspace.Resolver {
	t.Helper()
	c := config.Defaults()
	c.StateDir = t.TempDir()
	r, err := workspace.New(&c)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAcquireOrCreate_FirstCallCreatesSession(t *testing.T) {
	m := NewManager(time.Minute, newResolver(t))
	s, release, err := m.AcquireOrCreate("client-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ClientID != "client-1" || !s.Workspace.Ephemeral {
		t.Errorf("session = %+v", s)
	}
	release()

	if got := len(m.Snapshot()); got != 1 {
		t.Errorf("snapshot len = %d, want 1", got)
	}
}

func TestAcquireOrCreate_SecondCallReusesAndSerializes(t *testing.T) {
	m := NewManager(time.Minute, newResolver(t))

	s1, release1, _ := m.AcquireOrCreate("client-1", "")
	if release1 == nil {
		t.Fatal("nil release")
	}

	// A second goroutine calling AcquireOrCreate must block on the per-session
	// mutex until release1() runs.
	gotSecond := make(chan *Session, 1)
	go func() {
		s2, release2, err := m.AcquireOrCreate("client-1", "")
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		gotSecond <- s2
		release2()
	}()

	select {
	case <-gotSecond:
		t.Fatal("second AcquireOrCreate returned before first release()")
	case <-time.After(50 * time.Millisecond):
		// expected: blocked on the mutex
	}

	release1()

	select {
	case s2 := <-gotSecond:
		if s2 != s1 {
			t.Errorf("second call returned a different session pointer")
		}
	case <-time.After(time.Second):
		t.Fatal("second AcquireOrCreate never returned")
	}
}

func TestSetCCSessionID_OnlyFirstSticks(t *testing.T) {
	m := NewManager(time.Minute, newResolver(t))
	s, release, _ := m.AcquireOrCreate("c", "")
	defer release()

	m.SetCCSessionID(s, "first")
	m.SetCCSessionID(s, "second")
	if s.CCSessionID != "first" {
		t.Errorf("CCSessionID = %q, want first", s.CCSessionID)
	}
}

func TestEvictIdle_RemovesAndCleansUp(t *testing.T) {
	m := NewManager(20*time.Millisecond, newResolver(t))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)
	defer m.Stop()

	_, release, _ := m.AcquireOrCreate("c", "")
	release() // make eligible for eviction

	deadline := time.After(time.Second)
	for {
		if len(m.Snapshot()) == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("session not evicted after %v idle", 20*time.Millisecond)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestStop_EvictsActiveSessions(t *testing.T) {
	m := NewManager(time.Hour, newResolver(t)) // very long TTL
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.Start(ctx)

	_, release, _ := m.AcquireOrCreate("c", "")
	release()

	if len(m.Snapshot()) != 1 {
		t.Fatal("expected one session before stop")
	}
	m.Stop()
	if got := len(m.Snapshot()); got != 0 {
		t.Errorf("snapshot len after Stop = %d, want 0", got)
	}
}

func TestIDFromRequest(t *testing.T) {
	cases := []struct {
		header string
		model  string
		want   string
	}{
		{"abc", "anything", "abc"},
		{"", "claude-code:session=xyz", "xyz"},
		{"", "claude-code", ""},
		{"  spaces  ", "", "spaces"},
	}
	for _, c := range cases {
		if got := IDFromRequest(c.header, c.model); got != c.want {
			t.Errorf("IDFromRequest(%q,%q) = %q, want %q", c.header, c.model, got, c.want)
		}
	}
}

func TestStripSessionFromModel(t *testing.T) {
	if got := StripSessionFromModel("claude-code:session=xyz"); got != "claude-code" {
		t.Errorf("got %q", got)
	}
	if got := StripSessionFromModel("claude-code"); got != "claude-code" {
		t.Errorf("got %q", got)
	}
}

// Race regression: many concurrent AcquireOrCreate calls for distinct
// clients should not deadlock or panic.
func TestAcquireOrCreate_ManyClientsConcurrent(t *testing.T) {
	m := NewManager(time.Minute, newResolver(t))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a'+i%26)) + string(rune('0'+i/26))
			_, release, err := m.AcquireOrCreate(id, "")
			if err != nil {
				t.Errorf("acquire %s: %v", id, err)
				return
			}
			release()
		}(i)
	}
	wg.Wait()
}
