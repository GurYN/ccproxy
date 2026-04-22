package server

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/guryn/ccproxy/internal/auth"
	"github.com/guryn/ccproxy/internal/openai"
)

type ctxKey int

const (
	ctxKeyTraceID ctxKey = iota + 1
	ctxKeyToken
	ctxKeyCapture
)

// withTraceID assigns or echoes a per-request trace ID and sets it on the
// response header. Logs and (M2.5+) the Claude subprocess env reuse it.
func (s *Server) withTraceID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		ctx := context.WithValue(r.Context(), ctxKeyTraceID, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func traceID(ctx context.Context) string {
	if v, _ := ctx.Value(ctxKeyTraceID).(string); v != "" {
		return v
	}
	return ""
}

func tokenFromCtx(ctx context.Context) *auth.Token {
	v, _ := ctx.Value(ctxKeyToken).(*auth.Token)
	return v
}

// authed validates the Authorization header against the token store. The
// resolved token is stashed on the request context so handlers can perform
// scope checks and logging.
func (s *Server) authed(required auth.Scope, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(h, prefix) {
			openai.WriteError(w, http.StatusUnauthorized, "authentication_error",
				"missing or malformed Authorization header")
			return
		}
		bearer := strings.TrimSpace(h[len(prefix):])
		tok, err := s.rt.Auth.Authenticate(r.Context(), bearer)
		if err != nil {
			openai.WriteError(w, http.StatusUnauthorized, "authentication_error",
				"invalid bearer token")
			return
		}
		if required != "" && !tok.HasScope(required) {
			openai.WriteError(w, http.StatusForbidden, "permission_error",
				"token missing required scope: "+string(required))
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyToken, tok)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
