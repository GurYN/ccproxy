package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"nhooyr.io/websocket"

	"github.com/guryn/ccproxy/internal/bridge"
	"github.com/guryn/ccproxy/internal/buildver"
	"github.com/guryn/ccproxy/internal/openai"
)

// handleBridge upgrades the request to a websocket and registers the
// resulting connection under the token's Principal. The daemon must speak
// the protocol from internal/bridge.
//
// Lifecycle:
//  1. authed middleware has already verified ScopeBridgeConnect.
//  2. Token must have a non-empty Principal.
//  3. Upgrade → wsConn.
//  4. Server-initiated hello handshake (3s timeout).
//  5. Register; on collision return 409 (single-bridge-per-principal v1).
//  6. Run blocks on the read loop until the daemon disconnects.
//  7. Unregister on exit.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	tok := tokenFromCtx(r.Context())
	if tok == nil || tok.Principal == "" {
		// authed sets the token; an empty Principal is a config issue on
		// the operator's side — fail clearly.
		openai.WriteError(w, http.StatusForbidden, "permission_error",
			"bridge tokens must have a principal (recreate with `ccproxy token create --principal <id> --bridge`)")
		return
	}

	// CompressionDisabled: per-message-deflate adds CPU for little benefit
	// on small JSON frames. OriginPatterns is "*" because the bridge is on
	// a trusted LAN/Tailscale network and uses bearer auth, not cookies.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
		OriginPatterns:  []string{"*"},
	})
	if err != nil {
		s.logger.Warn("bridge upgrade failed", "err", err, "principal", tok.Principal)
		return
	}

	// Use a connection-scoped context that survives the request handler
	// returning. The websocket Read loop pumps frames until the daemon
	// disconnects, which can be much longer than the inbound HTTP request.
	connCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	conn := bridge.NewServerConnection(connCtx, tok.Principal, ws)
	conn.SetRPCObserver(s.metrics.ObserveBridgeRPC)

	// Drive the hello handshake before publishing the connection, so chat
	// requests don't see a half-initialized bridge.
	helloCtx, helloCancel := context.WithTimeout(connCtx, 3*time.Second)
	go conn.Run() // start read loop so the response can be delivered
	resp, err := conn.Call(helloCtx, bridge.MethodHello, bridge.HelloParams{
		ServerVersion: buildver.String("ccproxy", ""),
	})
	helloCancel()
	if err != nil || resp == nil || resp.Error != nil {
		reason := "hello failed"
		if err != nil {
			reason = "hello: " + err.Error()
		} else if resp != nil && resp.Error != nil {
			reason = "hello rejected: " + resp.Error.Error()
		}
		conn.Close(reason)
		s.logger.Warn("bridge hello failed", "principal", tok.Principal, "err", reason)
		return
	}
	var hello bridge.HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		conn.Close("hello result malformed: " + err.Error())
		return
	}
	conn.SetRoot(hello.Root)

	s.metrics.BridgeConnected()
	defer s.metrics.BridgeDisconnected()

	if err := s.bridges.Register(tok.Principal, conn); err != nil {
		// Either ErrPrincipalAlreadyConnected or ErrEmptyPrincipal. The
		// empty-principal case is already filtered above; the other is a
		// genuine collision.
		conn.Close("register: " + err.Error())
		s.logger.Info("bridge register rejected", "principal", tok.Principal, "err", err)
		return
	}
	s.logger.Info("bridge connected",
		"principal", tok.Principal,
		"root", hello.Root,
		"daemon_version", hello.DaemonVersion,
		"allow_write", hello.AllowWrite,
		"allow_exec", hello.AllowExec,
	)
	defer func() {
		s.bridges.Unregister(tok.Principal, conn)
		s.logger.Info("bridge disconnected", "principal", tok.Principal)
	}()

	// Block until the read loop terminates (peer disconnect or close).
	<-conn.Done()
}
