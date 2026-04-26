package bridgeclient

import (
	"context"

	"github.com/guryn/ccproxy/internal/bridge"
)

// StubHandlers is a placeholder Handlers implementation that fails every
// call with ErrCodeUnsupported. Used in B0 to prove the protocol works
// before B1 lands the real filesystem handlers.
type StubHandlers struct{}

func NewStubHandlers() Handlers { return StubHandlers{} }

func unsupported() *bridge.RPCError {
	return &bridge.RPCError{Code: bridge.ErrCodeUnsupported, Message: "filesystem handlers land in B1"}
}

func (StubHandlers) Stat(context.Context, bridge.StatParams) (bridge.StatResult, *bridge.RPCError) {
	return bridge.StatResult{}, unsupported()
}
func (StubHandlers) ReadDir(context.Context, bridge.ReadDirParams) (bridge.ReadDirResult, *bridge.RPCError) {
	return bridge.ReadDirResult{}, unsupported()
}
func (StubHandlers) Read(context.Context, bridge.ReadParams) (bridge.ReadResult, *bridge.RPCError) {
	return bridge.ReadResult{}, unsupported()
}
func (StubHandlers) Write(context.Context, bridge.WriteParams) (bridge.WriteResult, *bridge.RPCError) {
	return bridge.WriteResult{}, unsupported()
}
func (StubHandlers) Create(context.Context, bridge.CreateParams) *bridge.RPCError { return unsupported() }
func (StubHandlers) Mkdir(context.Context, bridge.MkdirParams) *bridge.RPCError   { return unsupported() }
func (StubHandlers) Remove(context.Context, bridge.RemoveParams) *bridge.RPCError { return unsupported() }
func (StubHandlers) Rename(context.Context, bridge.RenameParams) *bridge.RPCError { return unsupported() }
func (StubHandlers) Chmod(context.Context, bridge.ChmodParams) *bridge.RPCError   { return unsupported() }
func (StubHandlers) Exec(context.Context, bridge.ExecParams) (bridge.ExecResult, *bridge.RPCError) {
	return bridge.ExecResult{}, unsupported()
}
