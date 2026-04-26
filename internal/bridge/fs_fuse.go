//go:build bridge_fuse && cgo && (linux || darwin)

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

const (
	defaultMountTimeout = 10 * time.Second
	defaultCacheTTL     = 1500 * time.Millisecond
)

// fuseFS is a cgofuse Filesystem that delegates every operation to a
// bridge Connection via RPC. It is created per-bridge by the Manager and
// torn down when the underlying connection closes.
type fuseFS struct {
	fuse.FileSystemBase
	conn      Connection
	timeout   time.Duration
	nextHandle atomic.Uint64

	// statCache memoizes Stat results for a short TTL. Watch events
	// invalidate entries; otherwise FUSE re-stats every operation which
	// would push every getattr through the websocket.
	cacheMu sync.Mutex
	cache   map[string]*statCacheEntry
	cacheTTL time.Duration
}

type statCacheEntry struct {
	result    StatResult
	exists    bool // false means "we know it does not exist"
	err       string
	expiresAt time.Time
}

// NewFUSEFilesystem builds a Filesystem ready to be passed to fuse.NewFileSystemHost.
func NewFUSEFilesystem(conn Connection) fuse.FileSystemInterface {
	return &fuseFS{
		conn:     conn,
		timeout:  10 * time.Second,
		cache:    map[string]*statCacheEntry{},
		cacheTTL: 1500 * time.Millisecond,
	}
}

func (f *fuseFS) call(method string, params any, dst any) error {
	ctx, cancel := context.WithTimeout(context.Background(), f.timeout)
	defer cancel()
	resp, err := f.conn.Call(ctx, method, params)
	if err != nil {
		return err
	}
	if resp.Error != nil {
		return resp.Error
	}
	if dst == nil || len(resp.Result) == 0 {
		return nil
	}
	return json.Unmarshal(resp.Result, dst)
}

func errToErrno(err error) int {
	if err == nil {
		return 0
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case ErrCodeNotFound:
			return -fuse.ENOENT
		case ErrCodeNotADirectory:
			return -fuse.ENOTDIR
		case ErrCodeIsADirectory:
			return -fuse.EISDIR
		case ErrCodePermissionDenied:
			return -fuse.EACCES
		case ErrCodeNotEmpty:
			return -fuse.ENOTEMPTY
		case ErrCodeInvalidArgument:
			return -fuse.EINVAL
		case ErrCodeOutOfRoot:
			return -fuse.EACCES
		case ErrCodeTooLarge:
			return -fuse.EFBIG
		case ErrCodeUnsupported:
			return -fuse.ENOSYS
		}
	}
	return -fuse.EIO
}

func (f *fuseFS) cacheGet(p string) *statCacheEntry {
	f.cacheMu.Lock()
	defer f.cacheMu.Unlock()
	e := f.cache[p]
	if e == nil {
		return nil
	}
	if time.Now().After(e.expiresAt) {
		delete(f.cache, p)
		return nil
	}
	return e
}

func (f *fuseFS) cachePut(p string, e *statCacheEntry) {
	e.expiresAt = time.Now().Add(f.cacheTTL)
	f.cacheMu.Lock()
	f.cache[p] = e
	f.cacheMu.Unlock()
}

// InvalidateCache drops any cached stat for the given path (and its
// parent for safety, since dir mtime may have changed).
func (f *fuseFS) InvalidateCache(p string) {
	f.cacheMu.Lock()
	delete(f.cache, p)
	delete(f.cache, filepath.Dir(p))
	f.cacheMu.Unlock()
}

// --- FileSystemInterface ----------------------------------------------

func (f *fuseFS) Getattr(p string, stat *fuse.Stat_t, fh uint64) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	if rel == "." || rel == "" {
		rel = ""
	}
	if e := f.cacheGet(rel); e != nil {
		if !e.exists {
			return -fuse.ENOENT
		}
		applyStat(stat, e.result)
		return 0
	}
	var res StatResult
	err := f.call(MethodStat, StatParams{Path: rel}, &res)
	if err != nil {
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == ErrCodeNotFound {
			f.cachePut(rel, &statCacheEntry{exists: false})
		}
		return errToErrno(err)
	}
	f.cachePut(rel, &statCacheEntry{result: res, exists: true})
	applyStat(stat, res)
	return 0
}

func applyStat(stat *fuse.Stat_t, r StatResult) {
	stat.Mode = r.Mode
	if r.IsDir && (stat.Mode&fuse.S_IFMT) == 0 {
		stat.Mode |= fuse.S_IFDIR
	}
	if !r.IsDir && (stat.Mode&fuse.S_IFMT) == 0 {
		stat.Mode |= fuse.S_IFREG
	}
	stat.Size = r.Size
	stat.Mtim = fuse.NewTimespec(time.Unix(0, r.MTime))
	stat.Atim = stat.Mtim
	stat.Ctim = stat.Mtim
	stat.Nlink = 1
}

func (f *fuseFS) Readdir(p string, fill func(name string, stat *fuse.Stat_t, ofst int64) bool, ofst int64, fh uint64) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	if rel == "." {
		rel = ""
	}
	var res ReadDirResult
	if err := f.call(MethodReadDir, ReadDirParams{Path: rel}, &res); err != nil {
		return errToErrno(err)
	}
	fill(".", nil, 0)
	fill("..", nil, 0)
	for _, e := range res.Entries {
		st := &fuse.Stat_t{}
		mode := e.Mode
		if e.IsDir {
			mode |= fuse.S_IFDIR
		} else if mode&fuse.S_IFMT == 0 {
			mode |= fuse.S_IFREG
		}
		st.Mode = mode
		st.Size = e.Size
		st.Nlink = 1
		if !fill(e.Name, st, 0) {
			break
		}
	}
	return 0
}

func (f *fuseFS) Open(p string, flags int) (errc int, fh uint64) {
	// We don't track real handles; assign sequential ids so writes can
	// reuse them later if needed.
	return 0, f.nextHandle.Add(1)
}

func (f *fuseFS) Opendir(p string) (errc int, fh uint64) {
	return 0, f.nextHandle.Add(1)
}

func (f *fuseFS) Read(p string, buff []byte, ofst int64, fh uint64) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	var res ReadResult
	err := f.call(MethodRead, ReadParams{Path: rel, Offset: ofst, Length: len(buff)}, &res)
	if err != nil {
		return errToErrno(err)
	}
	n := copy(buff, res.Data)
	return n
}

func (f *fuseFS) Write(p string, buff []byte, ofst int64, fh uint64) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	var res WriteResult
	err := f.call(MethodWrite, WriteParams{Path: rel, Offset: ofst, Data: append([]byte(nil), buff...)}, &res)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return res.Written
}

func (f *fuseFS) Truncate(p string, size int64, fh uint64) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	// Truncate via a zero-length write at the truncation point with the
	// truncate flag — keeps the protocol smaller than a dedicated method.
	err := f.call(MethodWrite, WriteParams{Path: rel, Offset: size, Data: nil, Truncate: true}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return 0
}

func (f *fuseFS) Create(p string, flags int, mode uint32) (errc int, fh uint64) {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	err := f.call(MethodCreate, CreateParams{Path: rel, Mode: mode}, nil)
	if err != nil {
		return errToErrno(err), 0
	}
	f.InvalidateCache(rel)
	return 0, f.nextHandle.Add(1)
}

func (f *fuseFS) Mkdir(p string, mode uint32) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	err := f.call(MethodMkdir, MkdirParams{Path: rel, Mode: mode}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return 0
}

func (f *fuseFS) Unlink(p string) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	err := f.call(MethodRemove, RemoveParams{Path: rel}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return 0
}

func (f *fuseFS) Rmdir(p string) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	err := f.call(MethodRemove, RemoveParams{Path: rel, Recursive: false}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return 0
}

func (f *fuseFS) Rename(oldp, newp string) int {
	from := strings.TrimPrefix(path.Clean(oldp), "/")
	to := strings.TrimPrefix(path.Clean(newp), "/")
	err := f.call(MethodRename, RenameParams{From: from, To: to}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(from)
	f.InvalidateCache(to)
	return 0
}

func (f *fuseFS) Chmod(p string, mode uint32) int {
	rel := strings.TrimPrefix(path.Clean(p), "/")
	err := f.call(MethodChmod, ChmodParams{Path: rel, Mode: mode}, nil)
	if err != nil {
		return errToErrno(err)
	}
	f.InvalidateCache(rel)
	return 0
}
