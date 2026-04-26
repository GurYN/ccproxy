// Package bridge defines the wire protocol between the ccproxy server and
// the user-side ccproxy-bridge daemon, plus the in-process plumbing the
// server uses to track connected bridges.
//
// The protocol is a simple JSON-RPC variant carried over a single websocket
// per bridge: the server (which owns the FUSE mount) issues `req` frames to
// the daemon for filesystem operations, the daemon answers with `resp`
// frames, and the daemon emits `event` frames for asynchronous things like
// inotify/FSEvents notifications.
package bridge

import "encoding/json"

// FrameType is the discriminator for Frame.Type.
const (
	FrameReq   = "req"
	FrameResp  = "resp"
	FrameEvent = "event"
)

// Method names. Centralized here so server and daemon agree on spelling.
const (
	MethodHello   = "hello"
	MethodStat    = "stat"
	MethodReadDir = "readdir"
	MethodRead    = "read"
	MethodWrite   = "write"
	MethodCreate  = "create"
	MethodMkdir   = "mkdir"
	MethodRemove  = "remove"
	MethodRename  = "rename"
	MethodChmod   = "chmod"
	MethodExec    = "exec"
)

// Event names.
const (
	EventWatch = "watch"
	EventBye   = "bye"
)

// RPC error codes — small set, mirrors os/syscall categories the server
// needs to translate back into FUSE errno values.
const (
	ErrCodeNotFound         = "not_found"
	ErrCodeNotADirectory    = "not_a_directory"
	ErrCodeIsADirectory     = "is_a_directory"
	ErrCodePermissionDenied = "permission_denied"
	ErrCodeNotEmpty         = "not_empty"
	ErrCodeInvalidArgument  = "invalid_argument"
	ErrCodeIO               = "io"
	ErrCodeUnsupported      = "unsupported"
	ErrCodeOutOfRoot        = "out_of_root"
	ErrCodeTooLarge         = "too_large"
	ErrCodeInternal         = "internal"
)

// Frame is the single envelope used in both directions. Exactly one of
// Method (req), Result/Error (resp), or Method+Params (event) is populated.
type Frame struct {
	ID     uint64          `json:"id,omitempty"`     // correlation id; events may omit
	Type   string          `json:"type"`             // FrameReq | FrameResp | FrameEvent
	Method string          `json:"method,omitempty"` // for req and event
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError travels back inside a `resp` frame when a method fails.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// --- method param/result types -----------------------------------------

// HelloParams is the first frame the server sends after the websocket
// upgrade. The daemon replies with HelloResult; an error response means the
// daemon refused (e.g. version mismatch).
type HelloParams struct {
	ServerVersion string `json:"server_version"`
	Capabilities  []string `json:"capabilities,omitempty"`
}

type HelloResult struct {
	DaemonVersion string   `json:"daemon_version"`
	Root          string   `json:"root"`           // absolute path the daemon is exposing
	Capabilities  []string `json:"capabilities,omitempty"`
	AllowWrite    bool     `json:"allow_write"`
	AllowExec     bool     `json:"allow_exec"`
}

type StatParams struct {
	Path string `json:"path"` // path relative to the daemon's --root
}

type StatResult struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`  // os.FileMode bits
	MTime int64  `json:"mtime"` // unix nanos
	IsDir bool   `json:"is_dir"`
}

type ReadDirParams struct {
	Path string `json:"path"`
}

type ReadDirEntry struct {
	Name  string `json:"name"`
	Mode  uint32 `json:"mode"`
	Size  int64  `json:"size"`
	IsDir bool   `json:"is_dir"`
}

type ReadDirResult struct {
	Entries []ReadDirEntry `json:"entries"`
}

type ReadParams struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

type ReadResult struct {
	Data []byte `json:"data"` // base64 via json.Marshal default
	EOF  bool   `json:"eof"`
}

type WriteParams struct {
	Path     string `json:"path"`
	Offset   int64  `json:"offset"`
	Data     []byte `json:"data"`
	Truncate bool   `json:"truncate"`
}

type WriteResult struct {
	Written int `json:"written"`
}

type CreateParams struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type MkdirParams struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type RemoveParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive"`
}

type RenameParams struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ChmodParams struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type ExecParams struct {
	Cmd   string   `json:"cmd"`
	Args  []string `json:"args,omitempty"`
	Stdin []byte   `json:"stdin,omitempty"`
}

type ExecResult struct {
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// WatchEvent is a daemon→server notification that something changed under
// --root. The server uses these to invalidate FUSE caches.
type WatchEvent struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "created" | "modified" | "removed"
}

// ByeEvent is the daemon's clean-shutdown signal.
type ByeEvent struct {
	Reason string `json:"reason,omitempty"`
}

// EmptyResult is the canonical zero-value response for methods that
// succeed without returning data.
type EmptyResult struct{}
