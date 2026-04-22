package openai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SSEWriter writes Server-Sent Events in the OpenAI streaming format.
// Every chunk is a single `data: <json>\n\n` frame; the stream is
// terminated by `data: [DONE]\n\n`.
type SSEWriter struct {
	w  io.Writer
	f  http.Flusher
	bw io.Writer // optional: not used yet, reserved for buffering experiments
}

// NewSSEWriter wraps an http.ResponseWriter, sets the SSE headers, and
// returns a writer that flushes after each chunk. The caller must have
// already validated that the request asked for streaming.
func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer does not support flushing")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx buffering if fronted
	w.WriteHeader(http.StatusOK)
	f.Flush()
	return &SSEWriter{w: w, f: f}, nil
}

// WriteChunk serializes one ChatChunk and flushes.
func (s *SSEWriter) WriteChunk(c ChatChunk) error {
	b, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal chunk: %w", err)
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// WriteDone writes the terminal `data: [DONE]` frame.
func (s *SSEWriter) WriteDone() error {
	if _, err := io.WriteString(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.f.Flush()
	return nil
}

// WriteError replies with an OpenAI-style error envelope and the given
// HTTP status. errType corresponds to OpenAI's error.type field
// (e.g. "invalid_request_error", "authentication_error", "server_error",
// "rate_limit_exceeded").
func WriteError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorBody{Error: ErrorDetail{
		Type:    errType,
		Message: msg,
	}})
}
