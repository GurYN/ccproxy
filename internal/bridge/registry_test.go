package bridge

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeConn struct {
	principal string
	closed    atomic.Bool
	connected time.Time
}

func newFakeConn(p string) *fakeConn {
	return &fakeConn{principal: p, connected: time.Now()}
}

func (f *fakeConn) Principal() string                                  { return f.principal }
func (f *fakeConn) ConnectedAt() time.Time                             { return f.connected }
func (f *fakeConn) Root() string                                       { return "" }
func (f *fakeConn) Call(context.Context, string, any) (*Frame, error)  { return nil, nil }
func (f *fakeConn) Closed() bool                                       { return f.closed.Load() }
func (f *fakeConn) Close(string)                                       { f.closed.Store(true) }

func TestRegistry_RegisterLookupUnregister(t *testing.T) {
	r := NewRegistry()
	c := newFakeConn("alice")
	if err := r.Register("alice", c); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := r.Lookup("alice"); got != c {
		t.Errorf("lookup mismatch")
	}
	r.Unregister("alice", c)
	if got := r.Lookup("alice"); got != nil {
		t.Errorf("expected nil after unregister, got %v", got)
	}
}

func TestRegistry_RejectsDoubleRegister(t *testing.T) {
	r := NewRegistry()
	c1 := newFakeConn("alice")
	c2 := newFakeConn("alice")
	if err := r.Register("alice", c1); err != nil {
		t.Fatalf("register c1: %v", err)
	}
	if err := r.Register("alice", c2); err != ErrPrincipalAlreadyConnected {
		t.Errorf("want ErrPrincipalAlreadyConnected, got %v", err)
	}
}

func TestRegistry_ReplacesClosedConnection(t *testing.T) {
	r := NewRegistry()
	c1 := newFakeConn("alice")
	if err := r.Register("alice", c1); err != nil {
		t.Fatalf("register c1: %v", err)
	}
	c1.Close("test")
	c2 := newFakeConn("alice")
	if err := r.Register("alice", c2); err != nil {
		t.Errorf("expected closed conn to be replaceable, got %v", err)
	}
}

func TestRegistry_LookupSkipsClosed(t *testing.T) {
	r := NewRegistry()
	c := newFakeConn("alice")
	_ = r.Register("alice", c)
	c.Close("test")
	if got := r.Lookup("alice"); got != nil {
		t.Errorf("closed connection must not surface from Lookup, got %v", got)
	}
}

func TestRegistry_EmptyPrincipal(t *testing.T) {
	r := NewRegistry()
	if err := r.Register("", newFakeConn("")); err != ErrEmptyPrincipal {
		t.Errorf("want ErrEmptyPrincipal, got %v", err)
	}
}

func TestRegistry_UnregisterDoesNotEvictReplacement(t *testing.T) {
	// If Register replaced c1 with c2, calling Unregister(c1) later must
	// not remove c2 from the map.
	r := NewRegistry()
	c1 := newFakeConn("alice")
	_ = r.Register("alice", c1)
	c1.Close("replaced")
	c2 := newFakeConn("alice")
	_ = r.Register("alice", c2)
	r.Unregister("alice", c1)
	if got := r.Lookup("alice"); got != c2 {
		t.Errorf("replacement must survive stale unregister; got %v", got)
	}
}
