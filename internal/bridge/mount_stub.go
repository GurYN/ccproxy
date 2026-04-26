//go:build !bridge_fuse || !cgo || !(linux || darwin)

package bridge

import (
	"errors"
	"fmt"
	"os"
)

// Stub Manager for builds without cgo / FUSE. The server can still run
// (chat, /v1/models, etc.) — only the bridge mount path is unavailable.
// Calls to Mount return ErrFUSEUnavailable.

var ErrFUSEUnavailable = errors.New("bridge: FUSE support not built (need cgo + linux|darwin)")

type Mount struct {
	Principal string
	Path      string
}

type Manager struct {
	mountRoot string
}

func NewManager(mountRoot string) (*Manager, error) {
	if err := os.MkdirAll(mountRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create mount root %s: %w", mountRoot, err)
	}
	return &Manager{mountRoot: mountRoot}, nil
}

func (m *Manager) Mount(_ Connection) (*Mount, error) { return nil, ErrFUSEUnavailable }
func (m *Manager) Unmount(string)                     {}
func (m *Manager) Lookup(string) *Mount               { return nil }
func (m *Manager) MountPathFor(string) string         { return m.mountRoot }
