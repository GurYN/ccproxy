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

// ErrUnknownWorkspace is returned when a request asks for a workspace name
// that isn't configured. The caller turns this into HTTP 400.
var ErrUnknownWorkspace = errors.New("unknown workspace")

// Resolver implements the PRD §6.4 resolution chain for the M2.2 surface
// (header → model binding → ephemeral). Existing-session reuse (#3) is
// applied by internal/session before calling Resolve.
type Resolver struct {
	cfg     *config.Config
	tmpRoot string
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
	Name      string // configured name, "" for ephemeral
	Ephemeral bool
	Cleanup   func()
}

// Resolve picks the workspace for an incoming request, applying
// PRD §6.4 #1 (header), #2 (model binding), and #4 (ephemeral default).
// #3 (existing session) is the session manager's responsibility.
func (r *Resolver) Resolve(req *http.Request, model config.Model) (Resolution, error) {
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
