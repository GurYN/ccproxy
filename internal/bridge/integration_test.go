package bridge_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"

	"github.com/guryn/ccproxy/internal/bridge"
)

// TestBridgeEndToEnd_HelloAndStat spins up a tiny http test server that
// upgrades to websocket using bridge.NewServerConnection, registers it,
// completes the hello handshake, and issues a Stat RPC. A goroutine on
// the daemon side speaks the protocol manually.
func TestBridgeEndToEnd_HelloAndStat(t *testing.T) {
	registry := bridge.NewRegistry()
	const principal = "alice"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns:  []string{"*"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		conn := bridge.NewServerConnection(ctx, principal, ws)
		go conn.Run()

		helloCtx, hcancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := conn.Call(helloCtx, bridge.MethodHello, bridge.HelloParams{ServerVersion: "test"})
		hcancel()
		if err != nil {
			t.Errorf("hello: %v", err)
			return
		}
		var hello bridge.HelloResult
		if err := json.Unmarshal(resp.Result, &hello); err != nil {
			t.Errorf("hello unmarshal: %v", err)
			return
		}
		conn.SetRoot(hello.Root)
		if err := registry.Register(principal, conn); err != nil {
			t.Errorf("register: %v", err)
			return
		}
		defer registry.Unregister(principal, conn)

		// Issue a Stat — daemon stub will reject it with ErrCodeUnsupported.
		statCtx, scancel := context.WithTimeout(ctx, 2*time.Second)
		statResp, err := conn.Call(statCtx, bridge.MethodStat, bridge.StatParams{Path: "."})
		scancel()
		if err != nil {
			t.Errorf("stat: %v", err)
			return
		}
		if statResp.Error == nil || statResp.Error.Code != bridge.ErrCodeUnsupported {
			t.Errorf("expected ErrCodeUnsupported, got %+v", statResp.Error)
		}
	}))
	defer srv.Close()

	// Build the daemon side: a Client that dials the test server. We use
	// a custom Authorization-free dial since the test server doesn't
	// authenticate.
	wsURL := strings.Replace(srv.URL, "http", "ws", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "test done")

	// Manual daemon: read frames, answer.
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var f bridge.Frame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("read frame: %v", err)
		}
		resp := bridge.Frame{ID: f.ID, Type: bridge.FrameResp}
		switch f.Method {
		case bridge.MethodHello:
			r, _ := json.Marshal(bridge.HelloResult{
				DaemonVersion: "test-daemon",
				Root:          "/tmp/test-root",
				AllowWrite:    true,
			})
			resp.Result = r
		case bridge.MethodStat:
			resp.Error = &bridge.RPCError{Code: bridge.ErrCodeUnsupported, Message: "stub"}
		default:
			resp.Error = &bridge.RPCError{Code: bridge.ErrCodeUnsupported, Message: "unknown method"}
		}
		out, _ := json.Marshal(resp)
		if err := ws.Write(ctx, websocket.MessageText, out); err != nil {
			t.Fatalf("write: %v", err)
		}
		if f.Method == bridge.MethodStat {
			// Last expected RPC — let the test server unwind.
			break
		}
	}

	// Give the server side a moment to register.
	time.Sleep(50 * time.Millisecond)
}
