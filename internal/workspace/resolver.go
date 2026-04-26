package workspace

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/guryn/ccproxy/internal/config"
)

// HeaderName is the request header that lets a client pick a configured
// workspace by name. PRD §6.4 #1.
const HeaderName = "X-CC-Workspace"

// HeaderRequireBridge, when "true", makes the request fail with
// ErrBridgeUnavailable if no bridge is connected for the token's principal.
// Without it the resolver falls through to ephemeral as before.
const HeaderRequireBridge = "X-CC-Require-Bridge"

// ErrUnknownWorkspace is returned when a request asks for a workspace name
// that isn't configured. The caller turns this into HTTP 400.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// ErrBridgeUnavailable is returned when X-CC-Require-Bridge demands a
// bridge mount but no live connection exists for the request's principal.
var ErrBridgeUnavailable = errors.New("bridge not connected")

// BridgeLookupFunc resolves a principal to a live mount path. It returns
// (path, true) when a bridge is connected and mounted; (_, false) means
// the resolver should continue down the chain to ephemeral.
type BridgeLookupFunc func(principal string) (string, bool)

// Resolver implements the PRD §6.4 resolution chain. Existing-session
// reuse is applied by internal/session before calling Resolve. M4 adds
// a bridge step between model-bound workspace and ephemeral fallback.
type Resolver struct {
	cfg          *config.Config
	tmpRoot      string
	bridgeLookup BridgeLookupFunc
}

// SetBridgeLookup wires the optional bridge resolution step. May be
// called after construction (the server initializes both Resolver and
// the bridge subsystem and only then can hand the lookup over).
func (r *Resolver) SetBridgeLookup(fn BridgeLookupFunc) {
	r.bridgeLookup = fn
}

// New returns a Resolver that creates ephemeral dirs under
// $StateDir/tmp/<uuid>/ and validates header lookups against cfg.Workspaces.
func New(cfg *config.Config) (*Resolver, error) {
	tmpRoot := filepath.Join(cfg.StateDir, "tmp")
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return nil, fmt.Errorf("ensure %s: %w", tmpRoot, err)
	}
	return &Resolver{cfg: cfg, tmpRoot: tmpRoot}, nil
}

// Resolution is the result of resolving a request to a working directory.
// Cleanup must be called when the workspace is no longer needed; for
// non-ephemeral workspaces it is a no-op.
type Resolution struct {
	Path      string
	Name      string // configured name, "" for ephemeral and bridge
	Ephemeral bool
	Bridge    bool // true when Path is a bridge mount
	Cleanup   func()
}

// Resolve picks the workspace for an incoming request. Order:
//  1. X-CC-Workspace header → named workspace.
//  2. model alias bound to a workspace → that workspace.
//  3. principal has an active bridge → bridge mount.
//  4. ephemeral.
//
// principal comes from the authenticated token; empty principal skips (3).
// Existing-session reuse is applied by internal/session before this call.
func (r *Resolver) Resolve(req *http.Request, model config.Model, principal string) (Resolution, error) {
	if name := req.Header.Get(HeaderName); name != "" {
		path, ok := r.cfg.WorkspacePath(name)
		if !ok {
			return Resolution{}, fmt.Errorf("%w: %q", ErrUnknownWorkspace, name)
		}
		return Resolution{Path: path, Name: name, Cleanup: noop}, nil
	}
	if model.Workspace != "" {
		path, ok := r.cfg.WorkspacePath(model.Workspace)
		if !ok {
			return Resolution{}, fmt.Errorf("%w: model %q references %q", ErrUnknownWorkspace, model.ID, model.Workspace)
		}
		return Resolution{Path: path, Name: model.Workspace, Cleanup: noop}, nil
	}
	requireBridge := req.Header.Get(HeaderRequireBridge) == "true"
	if r.bridgeLookup != nil && principal != "" {
		if path, ok := r.bridgeLookup(principal); ok {
			return Resolution{Path: path, Bridge: true, Cleanup: noop}, nil
		}
		if requireBridge {
			return Resolution{}, ErrBridgeUnavailable
		}
	} else if requireBridge {
		return Resolution{}, ErrBridgeUnavailable
	}
	return r.makeEphemeral()
}

// ResolveByName is used by the session manager when re-binding a request
// to a previously stored workspace. It accepts an empty name as "create
// a fresh ephemeral dir."
func (r *Resolver) ResolveByName(name string) (Resolution, error) {
	if name == "" {
		return r.makeEphemeral()
	}
	path, ok := r.cfg.WorkspacePath(name)
	if !ok {
		return Resolution{}, fmt.Errorf("%w: %q", ErrUnknownWorkspace, name)
	}
	return Resolution{Path: path, Name: name, Cleanup: noop}, nil
}

func (r *Resolver) makeEphemeral() (Resolution, error) {
	id := uuid.NewString()
	dir := filepath.Join(r.tmpRoot, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Resolution{}, fmt.Errorf("create ephemeral %s: %w", dir, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	return Resolution{Path: dir, Ephemeral: true, Cleanup: cleanup}, nil
}

func noop() {}
