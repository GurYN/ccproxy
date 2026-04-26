package bridgeclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/guryn/ccproxy/internal/bridge"
)

// FSHandlers implements Handlers against a real filesystem rooted at Root.
// All input paths are interpreted relative to Root and clamped — any path
// that, after Clean+EvalSymlinks, escapes Root returns ErrCodeOutOfRoot.
type FSHandlers struct {
	Root         string
	MaxFileBytes int64       // reject reads above this; 0 = unlimited
	Ignore       *IgnoreList // optional; nil disables denylist
	Confirm      ConfirmFunc // optional; nil auto-approves
}

// ConfirmFunc is called before any write/exec RPC. Return false to deny.
// The op string is short and human-readable: "write", "remove", "exec", etc.
type ConfirmFunc func(op, path string) bool

// NewFSHandlers builds an FSHandlers with a 32 MiB default file size limit.
func NewFSHandlers(root string) *FSHandlers {
	return &FSHandlers{Root: root, MaxFileBytes: 32 << 20}
}

// resolve clamps a relative path against h.Root. If the resolved path
// escapes the root (via .. or symlinks), it returns ErrCodeOutOfRoot.
// Also enforces the ignore list. Returns the absolute path on disk.
func (h *FSHandlers) resolve(rel string) (string, *bridge.RPCError) {
	rel = strings.TrimPrefix(rel, "/")
	if h.Ignore != nil && h.Ignore.Match(rel) {
		return "", &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "denied by .ccproxyignore: " + rel}
	}
	clean := filepath.Clean("/" + rel)
	abs := filepath.Join(h.Root, clean)
	// Reject anything that climbs above Root via .. — Clean above means
	// abs starts with h.Root, but we still re-check.
	if !isUnder(abs, h.Root) {
		return "", &bridge.RPCError{Code: bridge.ErrCodeOutOfRoot, Message: rel}
	}
	// Symlink resolution: only on existing paths. EvalSymlinks fails for
	// non-existent leaves, which is fine for write/create/mkdir flows —
	// we then evaluate the *parent* dir instead.
	if info, err := os.Lstat(abs); err == nil && info.Mode()&os.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil && !isUnder(real, h.Root) {
			return "", &bridge.RPCError{Code: bridge.ErrCodeOutOfRoot, Message: "symlink escapes root"}
		}
	}
	return abs, nil
}

func isUnder(child, parent string) bool {
	rp, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rp == "." || (!strings.HasPrefix(rp, "..") && !filepath.IsAbs(rp))
}

func errnoToCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, fs.ErrNotExist) {
		return bridge.ErrCodeNotFound
	}
	if errors.Is(err, fs.ErrPermission) {
		return bridge.ErrCodePermissionDenied
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		if errno, ok := pe.Err.(syscall.Errno); ok {
			switch errno {
			case syscall.ENOTDIR:
				return bridge.ErrCodeNotADirectory
			case syscall.EISDIR:
				return bridge.ErrCodeIsADirectory
			case syscall.ENOTEMPTY:
				return bridge.ErrCodeNotEmpty
			}
		}
	}
	return bridge.ErrCodeIO
}

func toRPCError(err error) *bridge.RPCError {
	if err == nil {
		return nil
	}
	return &bridge.RPCError{Code: errnoToCode(err), Message: err.Error()}
}

// --- methods -----------------------------------------------------------

func (h *FSHandlers) Stat(_ context.Context, p bridge.StatParams) (bridge.StatResult, *bridge.RPCError) {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return bridge.StatResult{}, rpcErr
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return bridge.StatResult{}, toRPCError(err)
	}
	return bridge.StatResult{
		Name:  info.Name(),
		Size:  info.Size(),
		Mode:  uint32(info.Mode()),
		MTime: info.ModTime().UnixNano(),
		IsDir: info.IsDir(),
	}, nil
}

func (h *FSHandlers) ReadDir(_ context.Context, p bridge.ReadDirParams) (bridge.ReadDirResult, *bridge.RPCError) {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return bridge.ReadDirResult{}, rpcErr
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return bridge.ReadDirResult{}, toRPCError(err)
	}
	out := make([]bridge.ReadDirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, bridge.ReadDirEntry{
			Name:  e.Name(),
			Mode:  uint32(info.Mode()),
			Size:  info.Size(),
			IsDir: e.IsDir(),
		})
	}
	return bridge.ReadDirResult{Entries: out}, nil
}

func (h *FSHandlers) Read(_ context.Context, p bridge.ReadParams) (bridge.ReadResult, *bridge.RPCError) {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return bridge.ReadResult{}, rpcErr
	}
	info, err := os.Stat(abs)
	if err != nil {
		return bridge.ReadResult{}, toRPCError(err)
	}
	if h.MaxFileBytes > 0 && info.Size() > h.MaxFileBytes && p.Offset == 0 && int64(p.Length) >= info.Size() {
		return bridge.ReadResult{}, &bridge.RPCError{Code: bridge.ErrCodeTooLarge, Message: fmt.Sprintf("file is %d bytes (limit %d)", info.Size(), h.MaxFileBytes)}
	}
	f, err := os.Open(abs)
	if err != nil {
		return bridge.ReadResult{}, toRPCError(err)
	}
	defer f.Close()
	if p.Offset > 0 {
		if _, err := f.Seek(p.Offset, io.SeekStart); err != nil {
			return bridge.ReadResult{}, toRPCError(err)
		}
	}
	length := p.Length
	if length <= 0 || length > 1<<20 {
		length = 1 << 20 // cap a single read at 1 MiB
	}
	buf := make([]byte, length)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return bridge.ReadResult{}, toRPCError(err)
	}
	eof := err == io.EOF || err == io.ErrUnexpectedEOF || int64(p.Offset+int64(n)) >= info.Size()
	return bridge.ReadResult{Data: buf[:n], EOF: eof}, nil
}

func (h *FSHandlers) Write(_ context.Context, p bridge.WriteParams) (bridge.WriteResult, *bridge.RPCError) {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return bridge.WriteResult{}, rpcErr
	}
	if h.Confirm != nil && !h.Confirm("write", p.Path) {
		return bridge.WriteResult{}, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "denied by user"}
	}
	// Pure truncate-to-size: Truncate=true with empty Data. Offset carries
	// the target file size. Writing-with-truncate (Truncate=true, non-empty
	// Data) is a separate path that opens with O_TRUNC and writes the body.
	if p.Truncate && len(p.Data) == 0 {
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			// Create the file then size it. Mirrors os.Truncate semantics.
			f, err := os.Create(abs)
			if err != nil {
				return bridge.WriteResult{}, toRPCError(err)
			}
			_ = f.Close()
		}
		if err := os.Truncate(abs, p.Offset); err != nil {
			return bridge.WriteResult{}, toRPCError(err)
		}
		return bridge.WriteResult{Written: 0}, nil
	}
	flag := os.O_WRONLY | os.O_CREATE
	if p.Truncate {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		return bridge.WriteResult{}, toRPCError(err)
	}
	defer f.Close()
	if p.Offset > 0 {
		if _, err := f.Seek(p.Offset, io.SeekStart); err != nil {
			return bridge.WriteResult{}, toRPCError(err)
		}
	}
	n, err := f.Write(p.Data)
	if err != nil {
		return bridge.WriteResult{}, toRPCError(err)
	}
	return bridge.WriteResult{Written: n}, nil
}

func (h *FSHandlers) Create(_ context.Context, p bridge.CreateParams) *bridge.RPCError {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return rpcErr
	}
	mode := os.FileMode(p.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return toRPCError(err)
	}
	_ = f.Close()
	return nil
}

func (h *FSHandlers) Mkdir(_ context.Context, p bridge.MkdirParams) *bridge.RPCError {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return rpcErr
	}
	mode := os.FileMode(p.Mode)
	if mode == 0 {
		mode = 0o755
	}
	if err := os.Mkdir(abs, mode); err != nil {
		return toRPCError(err)
	}
	return nil
}

func (h *FSHandlers) Remove(_ context.Context, p bridge.RemoveParams) *bridge.RPCError {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return rpcErr
	}
	// Defensive: never let remove(".") nuke the root.
	if abs == h.Root {
		return &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "refusing to remove bridge root"}
	}
	if h.Confirm != nil && !h.Confirm("remove", p.Path) {
		return &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "denied by user"}
	}
	var err error
	if p.Recursive {
		err = os.RemoveAll(abs)
	} else {
		err = os.Remove(abs)
	}
	return toRPCError(err)
}

func (h *FSHandlers) Rename(_ context.Context, p bridge.RenameParams) *bridge.RPCError {
	from, rpcErr := h.resolve(p.From)
	if rpcErr != nil {
		return rpcErr
	}
	to, rpcErr := h.resolve(p.To)
	if rpcErr != nil {
		return rpcErr
	}
	if err := os.Rename(from, to); err != nil {
		return toRPCError(err)
	}
	return nil
}

func (h *FSHandlers) Chmod(_ context.Context, p bridge.ChmodParams) *bridge.RPCError {
	abs, rpcErr := h.resolve(p.Path)
	if rpcErr != nil {
		return rpcErr
	}
	if err := os.Chmod(abs, os.FileMode(p.Mode)); err != nil {
		return toRPCError(err)
	}
	return nil
}

func (h *FSHandlers) Exec(ctx context.Context, p bridge.ExecParams) (bridge.ExecResult, *bridge.RPCError) {
	if strings.TrimSpace(p.Cmd) == "" {
		return bridge.ExecResult{}, &bridge.RPCError{Code: bridge.ErrCodeInvalidArgument, Message: "empty cmd"}
	}
	if h.Confirm != nil && !h.Confirm("exec", p.Cmd+" "+strings.Join(p.Args, " ")) {
		return bridge.ExecResult{}, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "denied by user"}
	}
	cmd := exec.CommandContext(ctx, p.Cmd, p.Args...)
	cmd.Dir = h.Root
	if len(p.Stdin) > 0 {
		cmd.Stdin = strings.NewReader(string(p.Stdin))
	}
	out, err := cmd.Output()
	res := bridge.ExecResult{Stdout: out}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.Stderr = ee.Stderr
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		return res, toRPCError(err)
	}
	return res, nil
}
