package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"nhooyr.io/websocket"
)

// wsConn is the server-side Connection backed by a websocket. It owns a
// single read goroutine that fans incoming frames out to either the
// pending-call map (for FrameResp) or an event handler channel (for
// FrameEvent).
type wsConn struct {
	principal string
	root      atomic.Value // string, set after hello

	connectedAt time.Time
	conn        *websocket.Conn
	ctx         context.Context // tied to connection lifetime
	cancel      context.CancelFunc

	writeMu sync.Mutex
	nextID  atomic.Uint64

	pendingMu sync.Mutex
	pending   map[uint64]chan *Frame

	closeOnce   sync.Once
	closeReason atomic.Value // string
	done        chan struct{}

	events chan WatchEvent

	rpcObserver func(method, result string, d time.Duration)
}

// SetRPCObserver wires a metrics callback called for every Call.
func (c *wsConn) SetRPCObserver(fn func(method, result string, d time.Duration)) {
	c.rpcObserver = fn
}

// NewServerConnection wraps an already-upgraded websocket. Caller is
// responsible for calling Run (which blocks on the read loop) and for
// invoking the hello handshake before treating the connection as live.
//
// Returns the concrete *wsConn so the route handler can drive the
// post-upgrade lifecycle (SetRoot/Run/Events). It satisfies Connection,
// which is what the registry stores.
func NewServerConnection(parent context.Context, principal string, ws *websocket.Conn) *wsConn {
	ctx, cancel := context.WithCancel(parent)
	c := &wsConn{
		principal:   principal,
		conn:        ws,
		ctx:         ctx,
		cancel:      cancel,
		pending:     map[uint64]chan *Frame{},
		connectedAt: time.Now(),
		events:      make(chan WatchEvent, 64),
		done:        make(chan struct{}),
	}
	c.root.Store("")
	c.closeReason.Store("")
	return c
}

func (c *wsConn) Principal() string      { return c.principal }
func (c *wsConn) ConnectedAt() time.Time { return c.connectedAt }

func (c *wsConn) Root() string {
	v, _ := c.root.Load().(string)
	return v
}

func (c *wsConn) Closed() bool {
	select {
	case <-c.ctx.Done():
		return true
	default:
		return false
	}
}

// Close tears down the websocket. Idempotent.
func (c *wsConn) Close(reason string) {
	c.closeOnce.Do(func() {
		c.closeReason.Store(reason)
		// websocket.StatusNormalClosure for clean exits; the daemon side
		// also gets to see `reason` in the close frame.
		_ = c.conn.Close(websocket.StatusNormalClosure, reason)
		c.cancel()
		// Drain pending callers with an error so they don't hang.
		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()
	})
}

// Done returns a channel that closes when the read loop has exited and the
// connection is fully torn down. Useful for handlers that want to block
// until the daemon disconnects.
func (c *wsConn) Done() <-chan struct{} { return c.done }

// Run drives the read loop. It returns when the connection terminates
// (clean close, peer disconnect, or context cancellation). Callers should
// invoke this in a goroutine after Register.
func (c *wsConn) Run() {
	defer close(c.done)
	defer c.Close("read-loop exited")
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			return
		}
		var f Frame
		if err := json.Unmarshal(data, &f); err != nil {
			// Malformed frame: drop the connection. Daemon must speak our
			// protocol; silent acceptance hides bugs.
			c.Close("malformed frame: " + err.Error())
			return
		}
		switch f.Type {
		case FrameResp:
			c.deliverResponse(&f)
		case FrameEvent:
			c.dispatchEvent(&f)
		case FrameReq:
			// v1: server is the only caller. A daemon-initiated req is a
			// protocol error.
			c.Close("daemon issued a req frame; not supported in v1")
			return
		default:
			c.Close("unknown frame type: " + f.Type)
			return
		}
	}
}

// Call issues an RPC and waits for the response. The returned Frame is
// always Type=FrameResp on success; Frame.Error may be non-nil when the
// daemon reported an application-level error.
func (c *wsConn) Call(ctx context.Context, method string, params any) (resp *Frame, err error) {
	if c.Closed() {
		return nil, errors.New("bridge connection closed")
	}
	start := time.Now()
	defer func() {
		if c.rpcObserver == nil {
			return
		}
		result := "ok"
		if err != nil {
			result = "transport_error"
		} else if resp != nil && resp.Error != nil {
			result = resp.Error.Code
			if result == "" {
				result = "error"
			}
		}
		c.rpcObserver(method, result, time.Since(start))
	}()
	id := c.nextID.Add(1)
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal params: %w", err)
	}
	frame := Frame{ID: id, Type: FrameReq, Method: method, Params: raw}
	ch := make(chan *Frame, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()

	if err := c.writeFrame(&frame); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	select {
	case got, ok := <-ch:
		if !ok {
			return nil, errors.New("connection closed before response")
		}
		resp = got
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, errors.New("connection closed before response")
	}
}

func (c *wsConn) writeFrame(f *Frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	wctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(wctx, websocket.MessageText, data)
}

func (c *wsConn) deliverResponse(f *Frame) {
	c.pendingMu.Lock()
	ch := c.pending[f.ID]
	delete(c.pending, f.ID)
	c.pendingMu.Unlock()
	if ch == nil {
		// Stale response (caller timed out). Drop silently.
		return
	}
	ch <- f
	close(ch)
}

func (c *wsConn) dispatchEvent(f *Frame) {
	switch f.Method {
	case EventWatch:
		var ev WatchEvent
		if err := json.Unmarshal(f.Params, &ev); err != nil {
			return
		}
		select {
		case c.events <- ev:
		default:
			// Channel full: drop. Watch events are advisory; the FUSE
			// cache will re-stat on TTL expiry anyway.
		}
	case EventBye:
		var bye ByeEvent
		_ = json.Unmarshal(f.Params, &bye)
		c.Close("daemon said bye: " + bye.Reason)
	}
}

// SetRoot is called by the route handler after the hello handshake.
func (c *wsConn) SetRoot(root string) { c.root.Store(root) }

// Events returns the channel of watch notifications. Closed when the
// connection terminates.
func (c *wsConn) Events() <-chan WatchEvent { return c.events }
