package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/claude"
	"github.com/guryn/ccproxy/internal/config"
	"github.com/guryn/ccproxy/internal/openai"
	"github.com/guryn/ccproxy/internal/ratelimit"
	"github.com/guryn/ccproxy/internal/session"
	"github.com/guryn/ccproxy/internal/translate"
	"github.com/guryn/ccproxy/internal/workspace"
)

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	logger := s.logger.With("trace_id", traceID(r.Context()))
	startedAt := time.Now()

	var req openai.ChatRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	if err := dec.Decode(&req); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("malformed JSON: %v", err))
		return
	}

	cleanModel := session.StripSessionFromModel(req.Model)
	model, ok := s.resolveModel(cleanModel)
	if !ok {
		openai.WriteError(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unknown model %q (see GET /v1/models)", req.Model))
		return
	}

	clientSessionID := pickSessionID(r, &req)
	tok := tokenFromCtx(r.Context())

	if err := s.rt.RateLimit.AllowRequest(tok.ID, tok.RateLimitRPM); err != nil {
		openai.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
			"per-token requests/minute exceeded")
		return
	}
	if err := s.rt.RateLimit.CheckDailyTokens(r.Context(), tok.ID, tok.RateLimitTPD); err != nil {
		if errors.Is(err, ratelimit.ErrRateLimited) {
			openai.WriteError(w, http.StatusTooManyRequests, "rate_limit_exceeded",
				"per-token tokens/day exceeded")
			return
		}
		logger.Warn("daily token check failed", "err", err)
	}

	if clientSessionID != "" && !tok.HasScope(auth.ScopeSessionPersistent) {
		openai.WriteError(w, http.StatusForbidden, "permission_error",
			"token missing required scope: session:persistent")
		return
	}
	if wsName := workspaceNameForRequest(r, model); wsName != "" {
		if !tok.HasScope(auth.WorkspaceScope(wsName)) {
			openai.WriteError(w, http.StatusForbidden, "permission_error",
				"token missing required scope: workspace:"+wsName)
			return
		}
	}

	inv, err := translate.RequestToInvocation(&req)
	if err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if eff, err := s.resolveEffort(r, &req, model, inv.Effort); err != nil {
		openai.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	} else {
		inv.Effort = eff
	}
	inv.ClaudeModel = s.resolveClaudeModel(r, model)

	if len(inv.IgnoredParams) > 0 {
		w.Header().Set("X-CC-Ignored-Params", strings.Join(inv.IgnoredParams, ","))
	}

	verbosity := s.defaultVerbosity()
	if v, ok := translate.ParseVerbosity(r.Header.Get("X-CC-Verbosity")); ok {
		verbosity = v
	}
	if tok.DefaultVerb != "" && r.Header.Get("X-CC-Verbosity") == "" {
		if v, ok := translate.ParseVerbosity(tok.DefaultVerb); ok {
			verbosity = v
		}
	}

	ctx, cancel := timeoutContext(r.Context(), s.rt.Cfg.RequestTimeout.Duration())
	defer cancel()

	tr := &translate.Translator{
		ChunkID:   "chatcmpl-" + uuid.NewString(),
		Model:     model.ID,
		Verbosity: verbosity,
	}

	if clientSessionID == "" {
		s.runStateless(ctx, w, r, model, inv, tr, req.Stream, startedAt, logger)
		return
	}
	s.runPersistent(ctx, w, r, model, inv, tr, clientSessionID, req.Stream, startedAt, logger)
}

// pickSessionID applies the PRD §6.3 resolution chain: header → body
// session_id field → model-string suffix.
func pickSessionID(r *http.Request, req *openai.ChatRequest) string {
	if v := strings.TrimSpace(r.Header.Get(session.HeaderName)); v != "" {
		return v
	}
	if req.SessionID != "" {
		return strings.TrimSpace(req.SessionID)
	}
	return session.IDFromRequest("", req.Model)
}

func (s *Server) runStateless(ctx context.Context, w http.ResponseWriter, r *http.Request, model config.Model, inv translate.ClaudeInvocation, tr *translate.Translator, streaming bool, startedAt time.Time, logger *slog.Logger) {
	wsRes, err := s.workspace.Resolve(r, model)
	if err != nil {
		s.respondWorkspaceErr(w, logger, err)
		return
	}
	defer wsRes.Cleanup()

	logger = logger.With("model", model.ID, "workspace", wsRes.Path,
		"ephemeral", wsRes.Ephemeral, "mode", "stateless")

	sess, err := s.spawn(ctx, model, inv, wsRes.Path, "")
	if err != nil {
		logger.Error("spawn failed", "err", err)
		openai.WriteError(w, http.StatusBadGateway, "server_error",
			fmt.Sprintf("spawn claude: %v", err))
		return
	}
	defer func() { _ = sess.Close() }()

	if streaming {
		s.streamChat(w, r, sess, tr, nil, tokenFromCtx(r.Context()).ID, startedAt, logger)
		return
	}
	s.bufferedChat(w, r, sess, tr, nil, tokenFromCtx(r.Context()).ID, logger)
}

func (s *Server) runPersistent(ctx context.Context, w http.ResponseWriter, r *http.Request, model config.Model, inv translate.ClaudeInvocation, tr *translate.Translator, clientID string, streaming bool, startedAt time.Time, logger *slog.Logger) {

	wsName := workspaceNameForRequest(r, model)

	cs, release, err := s.sessions.AcquireOrCreate(clientID, wsName)
	if err != nil {
		s.respondWorkspaceErr(w, logger, err)
		return
	}
	defer release()

	logger = logger.With(
		"model", model.ID,
		"workspace", cs.Workspace.Path,
		"ephemeral", cs.Workspace.Ephemeral,
		"mode", "persistent",
		"client_session_id", cs.ClientID,
		"cc_session_id", cs.CCSessionID,
	)

	sp, err := s.spawn(ctx, model, inv, cs.Workspace.Path, cs.CCSessionID)
	if err != nil {
		logger.Error("spawn failed", "err", err)
		openai.WriteError(w, http.StatusBadGateway, "server_error",
			fmt.Sprintf("spawn claude: %v", err))
		return
	}
	defer func() { _ = sp.Close() }()

	if streaming {
		s.streamChat(w, r, sp, tr, cs, tokenFromCtx(r.Context()).ID, startedAt, logger)
		return
	}
	s.bufferedChat(w, r, sp, tr, cs, tokenFromCtx(r.Context()).ID, logger)
}

// workspaceNameForRequest returns the workspace name to bind on first call
// of a persistent session: header wins, then model binding, then "" (which
// the manager interprets as "create an ephemeral dir").
func workspaceNameForRequest(r *http.Request, model config.Model) string {
	if v := r.Header.Get(workspace.HeaderName); v != "" {
		return v
	}
	return model.Workspace
}

func (s *Server) spawn(ctx context.Context, model config.Model, inv translate.ClaudeInvocation, wsPath, resumeID string) (*claude.Session, error) {
	sp, err := claude.Spawn(ctx, claude.Options{
		BinaryPath:         s.rt.Cfg.ClaudeBinary,
		Prompt:             inv.Prompt,
		Workspace:          wsPath,
		AppendSystemPrompt: combineSystemPrompts(model.SystemPrompt, inv.AppendSystemPrompt),
		ResumeID:           resumeID,
		Effort:             inv.Effort,
		ClaudeModel:        inv.ClaudeModel,
		PermissionMode:     s.resolvePermissionMode(model),
	})
	if err != nil {
		return nil, err
	}
	s.metrics.SubprocessSpawned()
	s.metrics.ActiveSubprocessesInc()
	go func() {
		_ = sp.Wait()
		s.metrics.ActiveSubprocessesDec()
	}()
	return sp, nil
}

// resolveModel maps the request's model id to a config.Model.
//
// Resolution order:
//  1. Empty id → the first configured alias (default for dumb clients).
//  2. Configured alias by exact id.
//  3. Passthrough (when allow_passthrough_models): the bare family names
//     (opus|sonnet|haiku) are pinned via config.ModelVersions if present;
//     full ids of the form claude-{family}-* pass through verbatim.
//
// In passthrough mode, ccproxy synthesizes a Model with ClaudeModel set so
// the spawn step appends `--model <id>`. The synthesized model has no
// workspace binding, no system prompt, and no per-alias effort — those
// require a real configured alias.
func (s *Server) resolveModel(id string) (config.Model, bool) {
	if id == "" {
		if len(s.rt.Cfg.Models) > 0 {
			return s.rt.Cfg.Models[0], true
		}
		// Aliasless config + passthrough on: synthesize a default model from
		// default_claude_model (which itself may be empty — claude picks).
		if s.rt.Cfg.PassthroughEnabled() {
			return config.Model{ID: "default", ClaudeModel: s.rt.Cfg.DefaultClaudeModel}, true
		}
		return config.Model{}, false
	}
	if m, ok := s.rt.Cfg.FindModel(id); ok {
		return m, true
	}
	if s.rt.Cfg.PassthroughEnabled() {
		if resolved, ok := s.rt.Cfg.ResolvePassthroughModel(id); ok {
			return config.Model{ID: id, ClaudeModel: resolved}, true
		}
	}
	return config.Model{}, false
}

// resolveEffort applies the chain: X-CC-Effort header → request body
// reasoning_effort (already normalized into bodyEffort) → model alias →
// config default. An invalid header value is a 400.
func (s *Server) resolveEffort(r *http.Request, _ *openai.ChatRequest, model config.Model, bodyEffort string) (string, error) {
	if v := r.Header.Get(translate.HeaderEffort); v != "" {
		eff, ok := translate.NormalizeEffort(v)
		if !ok {
			return "", translate.EffortError(v)
		}
		return eff, nil
	}
	if bodyEffort != "" {
		return bodyEffort, nil
	}
	if model.Effort != "" {
		return model.Effort, nil
	}
	return s.rt.Cfg.DefaultEffort, nil
}

// resolveClaudeModel applies the chain: X-CC-Claude-Model header →
// model alias claude_model → config default_claude_model. Empty is fine
// (claude picks its own default).
func (s *Server) resolveClaudeModel(r *http.Request, model config.Model) string {
	if v := strings.TrimSpace(r.Header.Get(translate.HeaderClaudeModel)); v != "" {
		return v
	}
	if model.ClaudeModel != "" {
		return model.ClaudeModel
	}
	return s.rt.Cfg.DefaultClaudeModel
}

// resolvePermissionMode picks the claude --permission-mode value. Per-alias
// setting wins; otherwise the top-level default. Empty means "don't pass
// the flag" (claude uses its own default, which prompts — broken in -p mode).
func (s *Server) resolvePermissionMode(model config.Model) string {
	if model.PermissionMode != "" {
		return model.PermissionMode
	}
	return s.rt.Cfg.DefaultPermissionMode
}

func (s *Server) defaultVerbosity() translate.Verbosity {
	if v, ok := translate.ParseVerbosity(s.rt.Cfg.DefaultVerbosity); ok {
		return v
	}
	return translate.VerbosityTextOnly
}

func combineSystemPrompts(modelPrompt, requestPrompt string) string {
	switch {
	case modelPrompt == "":
		return requestPrompt
	case requestPrompt == "":
		return modelPrompt
	default:
		return modelPrompt + "\n\n" + requestPrompt
	}
}

func (s *Server) respondWorkspaceErr(w http.ResponseWriter, logger *slog.Logger, err error) {
	if errors.Is(err, workspace.ErrUnknownWorkspace) {
		openai.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	logger.Error("workspace resolve failed", "err", err)
	openai.WriteError(w, http.StatusInternalServerError, "server_error", err.Error())
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, sp *claude.Session, tr *translate.Translator, cs *session.Session, tokenID string, startedAt time.Time, logger *slog.Logger) {
	sse, err := openai.NewSSEWriter(w)
	if err != nil {
		openai.WriteError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	cap := captureFromCtx(r.Context())
	firstChunkSeen := false
	for {
		select {
		case <-r.Context().Done():
			logger.Info("client disconnected; terminating subprocess")
			return
		case ev, ok := <-sp.Events():
			if !ok {
				cap.Write("done", nil)
				_ = sse.WriteDone()
				return
			}
			cap.Write("event", ev)
			s.captureSessionID(cs, ev)
			s.recordUsageFromResult(r.Context(), tokenID, ev, logger)
			for _, c := range tr.EventToChunks(ev) {
				cap.Write("chunk", c)
				if err := sse.WriteChunk(c); err != nil {
					if !errors.Is(err, io.ErrClosedPipe) {
						logger.Warn("write chunk failed", "err", err)
					}
					return
				}
				if !firstChunkSeen {
					firstChunkSeen = true
					s.metrics.ObserveFirstChunk("/v1/chat/completions", time.Since(startedAt))
				}
			}
		}
	}
}

func (s *Server) bufferedChat(w http.ResponseWriter, r *http.Request, sp *claude.Session, tr *translate.Translator, cs *session.Session, tokenID string, logger *slog.Logger) {
	cap := captureFromCtx(r.Context())
	var events []claude.Event
	for {
		select {
		case <-r.Context().Done():
			logger.Info("client disconnected mid-buffer")
			return
		case ev, ok := <-sp.Events():
			if !ok {
				resp := tr.NonStreamingResponse(events)
				cap.Write("response", resp)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			cap.Write("event", ev)
			s.captureSessionID(cs, ev)
			s.recordUsageFromResult(r.Context(), tokenID, ev, logger)
			events = append(events, ev)
		}
	}
}

// recordUsageFromResult adds the request's input+output tokens to the
// daily counter when we see the terminal result event. Errors are logged
// but never fail the response.
func (s *Server) recordUsageFromResult(ctx context.Context, tokenID string, ev claude.Event, logger *slog.Logger) {
	if ev.Type != claude.EventResult || ev.Result == nil || tokenID == "" {
		return
	}
	var u struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if len(ev.Result.Usage) > 0 {
		_ = json.Unmarshal(ev.Result.Usage, &u)
	}
	total := u.InputTokens + u.OutputTokens
	if total <= 0 {
		return
	}
	if err := s.rt.RateLimit.RecordUsage(ctx, tokenID, total); err != nil {
		logger.Warn("ratelimit record failed", "err", err)
	}
}

// captureSessionID records the cc session id from claude's first
// system/init event so future requests on the same client session can
// resume via --resume.
func (s *Server) captureSessionID(cs *session.Session, ev claude.Event) {
	if cs == nil || ev.SessionID == "" {
		return
	}
	s.sessions.SetCCSessionID(cs, ev.SessionID)
}

func timeoutContext(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, d)
}
