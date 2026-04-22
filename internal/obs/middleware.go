package obs

import (
	"net/http"
	"strconv"
	"time"
)

// Instrument wraps next and records request count + duration metrics.
// endpoint is a coarse label like "/v1/chat/completions" — high-cardinality
// path values are NOT acceptable as labels.
//
// The token name is read off the response header X-CC-Token-Name, which the
// server sets after authentication so this middleware doesn't need to know
// about the auth store.
func (m *Metrics) Instrument(endpoint string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusCapture{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		dur := time.Since(start).Seconds()
		tokenName := w.Header().Get("X-CC-Token-Name")
		if tokenName == "" {
			tokenName = "anonymous"
		}
		m.RequestsTotal.WithLabelValues(endpoint, r.Method, strconv.Itoa(sw.status), tokenName).Inc()
		m.RequestDuration.WithLabelValues(endpoint).Observe(dur)
	})
}

type statusCapture struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (c *statusCapture) WriteHeader(status int) {
	if c.wroteHeader {
		return
	}
	c.status = status
	c.wroteHeader = true
	c.ResponseWriter.WriteHeader(status)
}

// Flush proxies through to the inner Flusher so SSE streams keep working.
func (c *statusCapture) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
