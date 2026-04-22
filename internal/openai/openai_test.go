package openai

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessage_ContentString_BareString(t *testing.T) {
	m := Message{Role: "user", Content: json.RawMessage(`"hello"`)}
	if got := m.ContentString(); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestMessage_ContentString_Parts(t *testing.T) {
	m := Message{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)}
	if got := m.ContentString(); got != "ab" {
		t.Errorf("got %q, want %q", got, "ab")
	}
}

func TestMessage_ContentString_EmptyAndUnknown(t *testing.T) {
	if (Message{}).ContentString() != "" {
		t.Error("empty content should return empty string")
	}
	m := Message{Content: json.RawMessage(`{"weird":"shape"}`)}
	if got := m.ContentString(); got == "" {
		t.Error("unknown shape should fall through to raw JSON, not empty")
	}
}

func TestSSEWriter_WriteChunkAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	sse, err := NewSSEWriter(rec)
	if err != nil {
		t.Fatalf("NewSSEWriter: %v", err)
	}
	chunk := ChatChunk{
		ID:     "chatcmpl-1",
		Object: "chat.completion.chunk",
		Model:  "claude-code",
		Choices: []ChunkChoice{{
			Index: 0,
			Delta: Delta{Role: "assistant", Content: "hi"},
		}},
	}
	if err := sse.WriteChunk(chunk); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if err := sse.WriteDone(); err != nil {
		t.Fatalf("WriteDone: %v", err)
	}

	body, _ := io.ReadAll(rec.Body)
	out := string(body)
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	if !strings.HasPrefix(out, "data: {") {
		t.Errorf("output should start with `data: {`, got %q", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("output should end with [DONE] frame, got %q", out)
	}
	// Each frame must be `data: <one-line>\n\n`.
	for _, frame := range strings.Split(strings.TrimSuffix(out, "\n\n"), "\n\n") {
		if !strings.HasPrefix(frame, "data: ") {
			t.Errorf("frame missing `data: ` prefix: %q", frame)
		}
	}
}

func TestSSEWriter_RejectsNonFlusher(t *testing.T) {
	if _, err := NewSSEWriter(noFlushWriter{httptest.NewRecorder()}); err == nil {
		t.Error("expected error when ResponseWriter is not a Flusher")
	}
}

type noFlushWriter struct{ http.ResponseWriter }

// Explicitly hide Flush by not embedding it (httptest.ResponseRecorder does
// implement Flusher via *ResponseRecorder, so we wrap to suppress it).

func TestWriteError_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnauthorized, "authentication_error", "missing bearer")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	var body ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Type != "authentication_error" || body.Error.Message != "missing bearer" {
		t.Errorf("body = %+v", body)
	}
}
