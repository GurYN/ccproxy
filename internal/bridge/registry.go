package bridge

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrPrincipalAlreadyConnected is returned by Registry.Register when a
// bridge for the same principal is already connected. v1 policy is
// single-bridge-per-principal; relaxing this is an open question.
var ErrPrincipalAlreadyConnected = errors.New("bridge: principal already has an active bridge")

// ErrEmptyPrincipal is returned when a token without a Principal tries to
// open a bridge connection.
var ErrEmptyPrincipal = errors.New("bridge: token has no principal")

// Connection is the interface the registry exposes for dependents (the
// FUSE mount layer). It hides the websocket transport from callers so the
// registry can be tested without a real socket.
type Connection interface {
	// Principal is the owner identifier this bridge is registered under.
	Principal() string
	// ConnectedAt is when Register was called.
	ConnectedAt() time.Time
	// Root is the daemon's --root from the hello handshake. Empty until
	// hello completes.
	Root() string
	// Call issues an RPC to the daemon. Implementations must respect ctx
	// cancellation. The returned Frame is always Type=FrameResp; error
	// returns indicate transport failure (use frame.Error for protocol
	// errors).
	Call(ctx context.Context, method string, params any) (*Frame, error)
	// Closed reports whether the connection has terminated.
	Closed() bool
	// Close requests termination; idempotent.
	Close(reason string)
}

// Registry tracks connected bridges keyed by principal. Single-writer per
// principal: a second Register for the same principal returns
// ErrPrincipalAlreadyConnected unless the previous connection is already
// Closed (in which case the new one replaces it).
type Registry struct {
	mu sync.RWMutex
	m  map[string]Connection
}

// NewRegistry builds an empty registry.
func NewRegistry() *Registry {
	return &Registry{m: map[string]Connection{}}
}

// Register installs c under principal. Returns ErrPrincipalAlreadyConnected
// if a live connection already exists for that principal.
func (r *Registry) Register(principal string, c Connection) error {
	if principal == "" {
		return ErrEmptyPrincipal
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.m[principal]; ok && !existing.Closed() {
		return ErrPrincipalAlreadyConnected
	}
	r.m[principal] = c
	return nil
}

// Unregister removes c from the registry, but only if it is still the
// current entry for principal (a later Register may have replaced it).
func (r *Registry) Unregister(principal string, c Connection) {
	if principal == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.m[principal]; ok && existing == c {
		delete(r.m, principal)
	}
}

// Lookup returns the live connection for principal, if any. Returns nil if
// no connection is registered or the registered one is already Closed.
func (r *Registry) Lookup(principal string) Connection {
	if principal == "" {
		return nil
	}
	r.mu.RLock()
	c := r.m[principal]
	r.mu.RUnlock()
	if c == nil || c.Closed() {
		return nil
	}
	return c
}

// Snapshot returns a list of currently registered (live) principals.
// Useful for /healthz and metrics.
func (r *Registry) Snapshot() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.m))
	for p, c := range r.m {
		if !c.Closed() {
			out = append(out, p)
		}
	}
	return out
}
