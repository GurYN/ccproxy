package session

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/guryn/ccproxy/internal/workspace"
)

// HeaderName is the request header that pins a request to a persistent
// session. PRD §6.3 #1.
const HeaderName = "X-CC-Session"

// Session is the per-client state ccproxy keeps for persistent mode. The
// Mu mutex serializes concurrent requests against the same session because
// `claude --resume` is not safe to interleave (PRD M2.3).
type Session struct {
	ClientID     string
	CCSessionID  string // empty until populated from claude's `system/init` event
	Workspace    workspace.Resolution
	WorkspaceID  string // configured workspace name; "" for ephemeral
	LastActivity time.Time
	Mu           sync.Mutex
}

// Observer is the subset of obs.Metrics the session manager needs. Kept as
// an interface so this package doesn't import obs (avoids a cycle and keeps
// the manager unit-testable without Prometheus).
type Observer interface {
	ActiveSessionsInc()
	ActiveSessionsDec()
	SessionEvicted()
}

// Manager owns the in-memory registry of persistent sessions and the
// background goroutine that evicts idle ones.
type Manager struct {
	ttl       time.Duration
	resolver  *workspace.Resolver
	obs       Observer
	mu        sync.Mutex
	sessions  map[string]*Session
	stopCh    chan struct{}
	stoppedCh chan struct{}
}

// NewManager returns a Manager. Call Start to launch the eviction loop and
// Stop to shut it down (Stop also evicts everything).
func NewManager(ttl time.Duration, resolver *workspace.Resolver) *Manager {
	return &Manager{
		ttl:       ttl,
		resolver:  resolver,
		sessions:  map[string]*Session{},
		stopCh:    make(chan struct{}),
		stoppedCh: make(chan struct{}),
	}
}

// SetObserver attaches a metrics observer. Call once at boot; a nil observer
// disables instrumentation. Not safe to call concurrently with manager use.
func (m *Manager) SetObserver(o Observer) { m.obs = o }

// Start launches the eviction loop. Safe to call once.
func (m *Manager) Start(ctx context.Context) {
	go m.run(ctx)
}

// Stop closes the eviction loop and evicts all live sessions, calling each
// workspace.Resolution.Cleanup so ephemeral dirs are removed.
func (m *Manager) Stop() {
	select {
	case <-m.stopCh:
	default:
		close(m.stopCh)
	}
	<-m.stoppedCh
}

// AcquireOrCreate returns the Session for clientID with its mutex already
// held, creating one if needed. The caller MUST call s.Mu.Unlock() — the
// returned Release helper does that and also touches LastActivity.
//
// workspaceName is the name to bind on first creation (from header / model).
// Empty means "give me an ephemeral dir." On subsequent calls the original
// workspace is reused regardless of what's passed.
func (m *Manager) AcquireOrCreate(clientID, workspaceName string) (*Session, func(), error) {
	m.mu.Lock()
	s, ok := m.sessions[clientID]
	if !ok {
		ws, err := m.resolver.ResolveByName(workspaceName)
		if err != nil {
			m.mu.Unlock()
			return nil, nil, err
		}
		s = &Session{
			ClientID:     clientID,
			Workspace:    ws,
			WorkspaceID:  workspaceName,
			LastActivity: time.Now(),
		}
		m.sessions[clientID] = s
		if m.obs != nil {
			m.obs.ActiveSessionsInc()
		}
	}
	m.mu.Unlock()

	s.Mu.Lock()
	release := func() {
		s.LastActivity = time.Now()
		s.Mu.Unlock()
	}
	return s, release, nil
}

// SetCCSessionID stores the cc session id learned from a claude system/init
// event. Safe to call multiple times — the first non-empty value sticks.
func (m *Manager) SetCCSessionID(s *Session, id string) {
	if s == nil || id == "" || s.CCSessionID != "" {
		return
	}
	s.CCSessionID = id
}

// Snapshot returns a copy of all currently tracked sessions. Useful for
// /admin endpoints (M2.5+) and tests.
func (m *Manager) Snapshot() []SessionInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, SessionInfo{
			ClientID:     s.ClientID,
			CCSessionID:  s.CCSessionID,
			WorkspaceID:  s.WorkspaceID,
			Path:         s.Workspace.Path,
			Ephemeral:    s.Workspace.Ephemeral,
			LastActivity: s.LastActivity,
		})
	}
	return out
}

// SessionInfo is the read-only snapshot view of a Session.
type SessionInfo struct {
	ClientID     string
	CCSessionID  string
	WorkspaceID  string
	Path         string
	Ephemeral    bool
	LastActivity time.Time
}

func (m *Manager) run(ctx context.Context) {
	defer close(m.stoppedCh)
	t := time.NewTicker(m.ttl / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.evictAll()
			return
		case <-m.stopCh:
			m.evictAll()
			return
		case now := <-t.C:
			m.evictIdle(now)
		}
	}
}

func (m *Manager) evictIdle(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		// Don't evict a session whose mutex is currently held — a request
		// is using it. TryLock returns true if free.
		if !s.Mu.TryLock() {
			continue
		}
		if now.Sub(s.LastActivity) > m.ttl {
			s.Workspace.Cleanup()
			delete(m.sessions, id)
			if m.obs != nil {
				m.obs.ActiveSessionsDec()
				m.obs.SessionEvicted()
			}
		}
		s.Mu.Unlock()
	}
}

func (m *Manager) evictAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.Workspace.Cleanup()
		delete(m.sessions, id)
		if m.obs != nil {
			m.obs.ActiveSessionsDec()
		}
	}
}

// IDFromRequest implements the PRD §6.3 resolution chain: header → body
// session_id (handled by the caller, since it's parsed from the request
// body) → model-string suffix `…:session=<id>` → empty (stateless).
//
// This helper covers the header and model-string paths. The body field
// is checked by the chat handler before calling.
func IDFromRequest(headerVal, modelField string) string {
	if v := strings.TrimSpace(headerVal); v != "" {
		return v
	}
	if v := parseSessionFromModel(modelField); v != "" {
		return v
	}
	return ""
}

// parseSessionFromModel extracts the `session=<id>` suffix from a model
// string. Examples that match: "claude-code:session=abc",
// "claude-code-foo:session=abc". Anything else returns "".
func parseSessionFromModel(model string) string {
	const marker = ":session="
	i := strings.Index(model, marker)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(model[i+len(marker):])
}

// StripSessionFromModel returns the model id with any `:session=<id>`
// suffix removed. Used so the rest of the pipeline sees a clean model id.
func StripSessionFromModel(model string) string {
	const marker = ":session="
	if i := strings.Index(model, marker); i >= 0 {
		return model[:i]
	}
	return model
}
