//go:build bridge_fuse && cgo && (linux || darwin)

package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/winfsp/cgofuse/fuse"
)

// Mount represents one active FUSE mount backed by a bridge connection.
type Mount struct {
	Principal string
	Path      string // mount point on disk
	host      *fuse.FileSystemHost
	fs        *fuseFS
	conn      Connection
}

// Manager creates and tears down FUSE mounts as bridges connect and
// disconnect. It is the glue between Registry and the workspace resolver.
type Manager struct {
	mountRoot string

	mu     sync.Mutex
	mounts map[string]*Mount // principal → mount
}

// NewManager prepares a Manager rooted at mountRoot. Each mount lives at
// $mountRoot/<principal-hash>/. mountRoot must exist and be writable.
func NewManager(mountRoot string) (*Manager, error) {
	if err := os.MkdirAll(mountRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create mount root %s: %w", mountRoot, err)
	}
	return &Manager{mountRoot: mountRoot, mounts: map[string]*Mount{}}, nil
}

// Mount creates and starts a FUSE mount for conn. The returned Mount is
// also stashed internally so Lookup can find it. Idempotent: if a mount
// already exists for the principal it is returned as-is.
func (m *Manager) Mount(conn Connection) (*Mount, error) {
	principal := conn.Principal()
	m.mu.Lock()
	if existing, ok := m.mounts[principal]; ok {
		m.mu.Unlock()
		return existing, nil
	}
	mountPath := m.mountPath(principal)
	if err := os.MkdirAll(mountPath, 0o700); err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("create mount point %s: %w", mountPath, err)
	}
	fs := &fuseFS{
		conn:     conn,
		timeout:  defaultMountTimeout,
		cache:    map[string]*statCacheEntry{},
		cacheTTL: defaultCacheTTL,
	}
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)
	mt := &Mount{Principal: principal, Path: mountPath, host: host, fs: fs, conn: conn}
	m.mounts[principal] = mt
	m.mu.Unlock()

	// Mount in a goroutine — host.Mount blocks until unmount. The empty
	// options slice keeps platform defaults; callers wanting allow_other
	// or noappledouble should configure in B2.
	go func() {
		ok := host.Mount(mountPath, []string{})
		if !ok {
			// Mount failed; clean state so a retry is possible.
			m.mu.Lock()
			delete(m.mounts, principal)
			m.mu.Unlock()
		}
	}()

	// Watch the connection's done channel and unmount on close.
	if done := connectionDone(conn); done != nil {
		go func() {
			<-done
			m.Unmount(principal)
		}()
	}
	// Invalidate FUSE stat cache when the daemon emits watch events.
	if events := connectionEvents(conn); events != nil {
		go func() {
			for ev := range events {
				fs.InvalidateCache(ev.Path)
			}
		}()
	}
	return mt, nil
}

func connectionEvents(c Connection) <-chan WatchEvent {
	type eventer interface{ Events() <-chan WatchEvent }
	if e, ok := c.(eventer); ok {
		return e.Events()
	}
	return nil
}

// Unmount tears down the mount for principal, if any.
func (m *Manager) Unmount(principal string) {
	m.mu.Lock()
	mt, ok := m.mounts[principal]
	if ok {
		delete(m.mounts, principal)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	mt.host.Unmount()
	_ = os.Remove(mt.Path) // best-effort; Unmount empties it
}

// Lookup returns the live mount for principal, if any.
func (m *Manager) Lookup(principal string) *Mount {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mounts[principal]
}

// MountPathFor returns where principal would be mounted (whether or not it
// currently is). Useful for error messages.
func (m *Manager) MountPathFor(principal string) string {
	return m.mountPath(principal)
}

func (m *Manager) mountPath(principal string) string {
	h := sha256.Sum256([]byte(principal))
	return filepath.Join(m.mountRoot, hex.EncodeToString(h[:8]))
}

// connectionDone tries to extract a Done channel from a Connection. The
// Registry stores Connection (the public interface), but our concrete
// *wsConn exposes Done(). Anything else returns nil and the mount stays
// active until Manager.Unmount is called explicitly.
func connectionDone(c Connection) <-chan struct{} {
	type doner interface{ Done() <-chan struct{} }
	if d, ok := c.(doner); ok {
		return d.Done()
	}
	return nil
}
