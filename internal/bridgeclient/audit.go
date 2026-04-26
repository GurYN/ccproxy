package bridgeclient

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Auditor appends one JSON line per RPC to a log file. Best-effort: write
// errors are dropped so a misbehaving filesystem can't crash the daemon.
type Auditor struct {
	mu sync.Mutex
	w  io.WriteCloser
}

// NewAuditor opens the log at path (creating the directory if needed) in
// append mode. Pass an empty path to disable auditing — the returned
// Auditor will be a no-op.
func NewAuditor(path string) (*Auditor, error) {
	if path == "" {
		return &Auditor{}, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &Auditor{w: f}, nil
}

// Log writes one entry. The entry is a flat object so jq-friendly tooling
// can filter on method/path easily.
func (a *Auditor) Log(method, path, result string) {
	if a == nil || a.w == nil {
		return
	}
	rec := map[string]any{
		"ts":     time.Now().UTC().Format(time.RFC3339Nano),
		"method": method,
		"path":   path,
		"result": result,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(append(data, '\n'))
}

// Close flushes and releases the file. Idempotent.
func (a *Auditor) Close() error {
	if a == nil || a.w == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	err := a.w.Close()
	a.w = nil
	return err
}
