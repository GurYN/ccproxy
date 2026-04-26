// Package bridgeclient is the daemon side of the bridge protocol: it
// connects an outbound websocket to ccproxy, accepts RPCs from the server,
// and executes them against the local filesystem rooted at --root.
package bridgeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"

	"github.com/guryn/ccproxy/internal/bridge"
	"github.com/guryn/ccproxy/internal/buildver"
)

// Options configures a daemon Client.
type Options struct {
	ServerURL   string // wss://host/bridge
	BearerToken string
	Root        string
	AllowWrite  bool
	AllowExec   bool
	Version     string
	// PingInterval is how often the daemon sends application-level keepalives
	// (server expects to see traffic regularly).
	PingInterval time.Duration
	// AuditPath, if non-empty, is the file the daemon appends one JSON
	// line per RPC to (audit log).
	AuditPath string
	// Watch enables fsnotify-backed change detection that streams
	// bridge.EventWatch frames back to the server.
	Watch bool
}

// Client is a single bridge connection. Use Run to dial and serve until
// the context is cancelled or the server disconnects.
type Client struct {
	opts     Options
	logger   loggerFunc
	handlers Handlers
}

type loggerFunc func(level, msg string, kv ...any)

// Handlers is the set of FS operations the daemon exposes. A stub is
// available via NewStubHandlers for protocol-only testing.
type Handlers interface {
	Stat(ctx context.Context, p bridge.StatParams) (bridge.StatResult, *bridge.RPCError)
	ReadDir(ctx context.Context, p bridge.ReadDirParams) (bridge.ReadDirResult, *bridge.RPCError)
	Read(ctx context.Context, p bridge.ReadParams) (bridge.ReadResult, *bridge.RPCError)
	Write(ctx context.Context, p bridge.WriteParams) (bridge.WriteResult, *bridge.RPCError)
	Create(ctx context.Context, p bridge.CreateParams) (*bridge.RPCError)
	Mkdir(ctx context.Context, p bridge.MkdirParams) (*bridge.RPCError)
	Remove(ctx context.Context, p bridge.RemoveParams) (*bridge.RPCError)
	Rename(ctx context.Context, p bridge.RenameParams) (*bridge.RPCError)
	Chmod(ctx context.Context, p bridge.ChmodParams) (*bridge.RPCError)
	Exec(ctx context.Context, p bridge.ExecParams) (bridge.ExecResult, *bridge.RPCError)
}

// New returns an initialized client. The handlers parameter must not be
// nil; pass NewStubHandlers() for protocol-only testing.
func New(opts Options, handlers Handlers, log loggerFunc) *Client {
	if log == nil {
		log = func(string, string, ...any) {}
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = 30 * time.Second
	}
	if opts.Version == "" {
		opts.Version = buildver.String("ccproxy-bridge", "")
	}
	return &Client{opts: opts, logger: log, handlers: handlers}
}

// Run dials the server and serves frames until ctx is cancelled or the
// connection terminates. Returns nil on clean shutdown, error otherwise.
func (c *Client) Run(ctx context.Context) error {
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+c.opts.BearerToken)
	ws, _, err := websocket.Dial(ctx, c.opts.ServerURL, &websocket.DialOptions{
		HTTPHeader:      hdr,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", c.opts.ServerURL, err)
	}
	c.logger("info", "bridge connected", "url", c.opts.ServerURL)
	defer func() {
		_ = ws.Close(websocket.StatusNormalClosure, "client exiting")
	}()

	auditor, err := NewAuditor(c.opts.AuditPath)
	if err != nil {
		c.logger("warn", "audit log disabled", "err", err)
		auditor = &Auditor{}
	}
	defer auditor.Close()

	sess := &daemonSession{
		ws:       ws,
		ctx:      ctx,
		opts:     c.opts,
		handlers: c.handlers,
		logger:   c.logger,
		auditor:  auditor,
	}
	if c.opts.Watch {
		go func() {
			if err := runWatcher(ctx, c.opts.Root, sess); err != nil {
				c.logger("warn", "watcher exited", "err", err)
			}
		}()
	}
	return sess.serve()
}

// daemonSession holds per-connection state.
type daemonSession struct {
	ws       *websocket.Conn
	ctx      context.Context
	opts     Options
	handlers Handlers
	logger   loggerFunc
	auditor  *Auditor

	writeMu sync.Mutex
}

// emitEvent satisfies EventEmitter so the watcher can publish frames.
func (s *daemonSession) emitEvent(method string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return s.writeFrame(&bridge.Frame{Type: bridge.FrameEvent, Method: method, Params: raw})
}

func (s *daemonSession) serve() error {
	for {
		_, data, err := s.ws.Read(s.ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		var f bridge.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("malformed frame: %w", err)
		}
		switch f.Type {
		case bridge.FrameReq:
			// Handle in a goroutine — long-running ops (large reads) must
			// not stall ping/keepalive frames behind them.
			go s.dispatch(&f)
		case bridge.FrameEvent, bridge.FrameResp:
			// Server doesn't initiate responses or events to the daemon
			// in v1. Ignore quietly so we don't crash on protocol drift.
		}
	}
}

func (s *daemonSession) dispatch(f *bridge.Frame) {
	resp := bridge.Frame{ID: f.ID, Type: bridge.FrameResp}
	switch f.Method {
	case bridge.MethodHello:
		result := bridge.HelloResult{
			DaemonVersion: s.opts.Version,
			Root:          s.opts.Root,
			AllowWrite:    s.opts.AllowWrite,
			AllowExec:     s.opts.AllowExec,
		}
		s.setResult(&resp, result, nil)
	case bridge.MethodStat:
		var p bridge.StatParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		out, rpcErr := s.handlers.Stat(s.ctx, p)
		s.setResult(&resp, out, rpcErr)
	case bridge.MethodReadDir:
		var p bridge.ReadDirParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		out, rpcErr := s.handlers.ReadDir(s.ctx, p)
		s.setResult(&resp, out, rpcErr)
	case bridge.MethodRead:
		var p bridge.ReadParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		out, rpcErr := s.handlers.Read(s.ctx, p)
		s.setResult(&resp, out, rpcErr)
	case bridge.MethodWrite:
		var p bridge.WriteParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled on this bridge"})
			break
		}
		out, rpcErr := s.handlers.Write(s.ctx, p)
		s.setResult(&resp, out, rpcErr)
	case bridge.MethodCreate:
		var p bridge.CreateParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled"})
			break
		}
		s.setResult(&resp, bridge.EmptyResult{}, s.handlers.Create(s.ctx, p))
	case bridge.MethodMkdir:
		var p bridge.MkdirParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled"})
			break
		}
		s.setResult(&resp, bridge.EmptyResult{}, s.handlers.Mkdir(s.ctx, p))
	case bridge.MethodRemove:
		var p bridge.RemoveParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled"})
			break
		}
		s.setResult(&resp, bridge.EmptyResult{}, s.handlers.Remove(s.ctx, p))
	case bridge.MethodRename:
		var p bridge.RenameParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled"})
			break
		}
		s.setResult(&resp, bridge.EmptyResult{}, s.handlers.Rename(s.ctx, p))
	case bridge.MethodChmod:
		var p bridge.ChmodParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowWrite {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "writes disabled"})
			break
		}
		s.setResult(&resp, bridge.EmptyResult{}, s.handlers.Chmod(s.ctx, p))
	case bridge.MethodExec:
		var p bridge.ExecParams
		if rpcErr := decodeParams(f.Params, &p); rpcErr != nil {
			s.setResult(&resp, nil, rpcErr)
			break
		}
		if !s.opts.AllowExec {
			s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodePermissionDenied, Message: "exec disabled (start daemon with --allow-exec)"})
			break
		}
		out, rpcErr := s.handlers.Exec(s.ctx, p)
		s.setResult(&resp, out, rpcErr)
	default:
		s.setResult(&resp, nil, &bridge.RPCError{Code: bridge.ErrCodeUnsupported, Message: "unknown method: " + f.Method})
	}
	if err := s.writeFrame(&resp); err != nil {
		s.logger("warn", "write response failed", "err", err)
	}
	s.audit(f, &resp)
}

// audit extracts a path-ish field from common method params for the
// audit log; falls back to "" for methods without one (hello, exec).
func (s *daemonSession) audit(req *bridge.Frame, resp *bridge.Frame) {
	if s.auditor == nil {
		return
	}
	result := "ok"
	if resp.Error != nil {
		result = resp.Error.Code
	}
	pathField := ""
	switch req.Method {
	case bridge.MethodStat, bridge.MethodReadDir, bridge.MethodRead, bridge.MethodWrite,
		bridge.MethodCreate, bridge.MethodMkdir, bridge.MethodRemove, bridge.MethodChmod:
		var p struct {
			Path string `json:"path"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pathField = p.Path
	case bridge.MethodRename:
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		_ = json.Unmarshal(req.Params, &p)
		pathField = p.From + "→" + p.To
	}
	s.auditor.Log(req.Method, pathField, result)
}

func decodeParams(raw json.RawMessage, dst any) *bridge.RPCError {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &bridge.RPCError{Code: bridge.ErrCodeInvalidArgument, Message: err.Error()}
	}
	return nil
}

func (s *daemonSession) setResult(resp *bridge.Frame, result any, rpcErr *bridge.RPCError) {
	if rpcErr != nil {
		resp.Error = rpcErr
		return
	}
	if result == nil {
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		resp.Error = &bridge.RPCError{Code: bridge.ErrCodeInternal, Message: "marshal: " + err.Error()}
		return
	}
	resp.Result = raw
}

func (s *daemonSession) writeFrame(f *bridge.Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	return s.ws.Write(wctx, websocket.MessageText, data)
}
