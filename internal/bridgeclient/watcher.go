package bridgeclient

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/guryn/ccproxy/internal/bridge"
)

// EventEmitter is what the watcher uses to publish notifications back to
// the server. The daemon's connection (a *daemonSession) implements it.
type EventEmitter interface {
	emitEvent(method string, payload any) error
}

// runWatcher watches the daemon's --root for create/write/remove/rename
// events and emits bridge.EventWatch frames. Returns when ctx is
// cancelled or the watcher fails.
func runWatcher(ctx context.Context, root string, emit EventEmitter) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	if err := addRecursive(w, root); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			rel, err := filepath.Rel(root, ev.Name)
			if err != nil || strings.HasPrefix(rel, "..") {
				continue
			}
			kind := classify(ev.Op)
			if kind == "" {
				continue
			}
			_ = emit.emitEvent(bridge.EventWatch, bridge.WatchEvent{Path: rel, Kind: kind})
			// New directory? Watch it too so descendant events surface.
			if ev.Op&fsnotify.Create != 0 {
				_ = addIfDir(w, ev.Name)
			}
		case <-w.Errors:
			// fsnotify errors are non-fatal; keep going.
		}
	}
}

func classify(op fsnotify.Op) string {
	switch {
	case op&fsnotify.Create != 0:
		return "created"
	case op&fsnotify.Write != 0:
		return "modified"
	case op&fsnotify.Remove != 0, op&fsnotify.Rename != 0:
		return "removed"
	}
	return ""
}

func addRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info pathInfo, err error) error {
		if err != nil {
			return nil // skip silently — best-effort
		}
		if info != nil && info.IsDir() {
			_ = w.Add(path)
		}
		return nil
	})
}

func addIfDir(w *fsnotify.Watcher, path string) error {
	info, err := pathStat(path)
	if err != nil || info == nil || !info.IsDir() {
		return err
	}
	return w.Add(path)
}
