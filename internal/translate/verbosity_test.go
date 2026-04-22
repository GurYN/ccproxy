package translate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/guryn/ccproxy/internal/claude"
)

func newVerbose() *Translator {
	return &Translator{
		ChunkID:   "chatcmpl-v",
		Model:     "claude-code",
		Verbosity: VerbosityVerbose,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func newNarrated() *Translator {
	return &Translator{
		ChunkID:   "chatcmpl-n",
		Model:     "claude-code",
		Verbosity: VerbosityNarrated,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func toolUseEvent(id, name, args string) claude.Event {
	return claude.Event{
		Type: claude.EventAssistant,
		Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{
				Type:  "tool_use",
				ID:    id,
				Name:  name,
				Input: json.RawMessage(args),
			}},
		}},
	}
}

func toolResultEvent(toolID, result string, isError bool) claude.Event {
	body, _ := json.Marshal(result)
	return claude.Event{
		Type: claude.EventUser,
		User: &claude.UserEvent{Message: claude.UserMessage{
			Content: []claude.UserContent{{
				Type:      "tool_result",
				ToolUseID: toolID,
				IsError:   isError,
				Content:   body,
			}},
		}},
	}
}

func TestVerbose_ToolUseBecomesToolCallDelta(t *testing.T) {
	tr := newVerbose()
	chunks := tr.EventToChunks(toolUseEvent("toolu_1", "Read", `{"file_path":"/x"}`))
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	d := chunks[0].Choices[0].Delta
	if len(d.ToolCalls) != 1 {
		t.Fatalf("expected one tool_calls entry, got %+v", d)
	}
	tc := d.ToolCalls[0]
	if tc.ID != "toolu_1" || tc.Type != "function" || tc.Function.Name != "Read" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.Function.Arguments != `{"file_path":"/x"}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
	if d.Role != "assistant" {
		t.Errorf("first chunk should set role=assistant, got %q", d.Role)
	}
}

func TestVerbose_ToolResultBecomesToolDelta(t *testing.T) {
	tr := newVerbose()
	tr.EventToChunks(toolUseEvent("toolu_1", "Read", "{}"))
	chunks := tr.EventToChunks(toolResultEvent("toolu_1", "hello world", false))
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	d := chunks[0].Choices[0].Delta
	if d.Role != "tool" || d.Content != "hello world" {
		t.Errorf("delta = %+v", d)
	}
}

func TestVerbose_ToolErrorPrefixed(t *testing.T) {
	tr := newVerbose()
	chunks := tr.EventToChunks(toolResultEvent("toolu_x", "boom", true))
	if got := chunks[0].Choices[0].Delta.Content; !strings.HasPrefix(got, "[error]") {
		t.Errorf("error result should be prefixed, got %q", got)
	}
}

func TestNarrated_ToolUseInlineEmoji(t *testing.T) {
	tr := newNarrated()
	chunks := tr.EventToChunks(toolUseEvent("t1", "Read", `{"file_path":"/main.go"}`))
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	c := chunks[0].Choices[0].Delta.Content
	if !strings.Contains(c, "🔧") || !strings.Contains(c, "Read") || !strings.Contains(c, "/main.go") {
		t.Errorf("narrated tool_use line = %q", c)
	}
}

func TestNarrated_ToolResultArrowLine(t *testing.T) {
	tr := newNarrated()
	chunks := tr.EventToChunks(toolResultEvent("t1", "the file contents", false))
	if len(chunks) != 1 {
		t.Fatal("want 1 chunk")
	}
	c := chunks[0].Choices[0].Delta.Content
	if !strings.Contains(c, "↳") || !strings.Contains(c, "the file contents") {
		t.Errorf("narrated tool_result = %q", c)
	}
}

func TestNarrated_TextStillPasses(t *testing.T) {
	tr := newNarrated()
	chunks := tr.EventToChunks(claude.Event{
		Type: claude.EventAssistant,
		Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{Type: "text", Text: "answer"}},
		}},
	})
	if len(chunks) != 1 || chunks[0].Choices[0].Delta.Content != "answer" {
		t.Errorf("narrated text = %+v", chunks)
	}
}

func TestVerbose_TextStillPasses(t *testing.T) {
	tr := newVerbose()
	chunks := tr.EventToChunks(claude.Event{
		Type: claude.EventAssistant,
		Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{Type: "text", Text: "answer"}},
		}},
	})
	if len(chunks) != 1 || chunks[0].Choices[0].Delta.Content != "answer" {
		t.Errorf("verbose text = %+v", chunks)
	}
}
