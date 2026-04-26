package translate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/guryn/ccproxy/internal/claude"
	"github.com/guryn/ccproxy/internal/openai"
)

func msg(role, content string) openai.Message {
	b, _ := json.Marshal(content)
	return openai.Message{Role: role, Content: b}
}

func TestRequestToInvocation_SingleUser(t *testing.T) {
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages: []openai.Message{msg("user", "ping")},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if inv.Prompt != "ping" {
		t.Errorf("prompt = %q, want %q", inv.Prompt, "ping")
	}
	if inv.AppendSystemPrompt != "" {
		t.Errorf("system prompt should be empty, got %q", inv.AppendSystemPrompt)
	}
	if len(inv.IgnoredParams) != 0 {
		t.Errorf("no params should be ignored, got %v", inv.IgnoredParams)
	}
}

func TestRequestToInvocation_SystemMessagesJoined(t *testing.T) {
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages: []openai.Message{
			msg("system", "be terse"),
			msg("system", "no jokes"),
			msg("user", "hi"),
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if inv.AppendSystemPrompt != "be terse\n\nno jokes" {
		t.Errorf("system prompt = %q", inv.AppendSystemPrompt)
	}
}

func TestRequestToInvocation_MultiTurnPreamble(t *testing.T) {
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages: []openai.Message{
			msg("user", "first"),
			msg("assistant", "answer1"),
			msg("user", "second"),
		},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(inv.Prompt, "Conversation so far") {
		t.Errorf("expected preamble, got %q", inv.Prompt)
	}
	if !strings.HasSuffix(inv.Prompt, "second") {
		t.Errorf("prompt should end with latest user msg, got %q", inv.Prompt)
	}
}

func TestRequestToInvocation_AcceptsTrailingNonUser(t *testing.T) {
	// Clients replaying a tool round-trip may end with role=tool or
	// role=assistant. We accept these and serialize the full convo.
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages: []openai.Message{msg("user", "hi"), msg("assistant", "hi back")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(inv.Prompt, "hi back") {
		t.Errorf("prompt should include trailing assistant content, got %q", inv.Prompt)
	}
	if !strings.Contains(inv.Prompt, "Continue") {
		t.Errorf("prompt should signal continuation, got %q", inv.Prompt)
	}
}

func TestRequestToInvocation_IgnoredParams(t *testing.T) {
	temp := 0.7
	maxT := 100
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages:    []openai.Message{msg("user", "hi")},
		Temperature: &temp,
		MaxTokens:   &maxT,
		Tools:       json.RawMessage(`[{"type":"function"}]`),
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := map[string]bool{"temperature": true, "max_tokens": true, "tools": true}
	if len(inv.IgnoredParams) != len(want) {
		t.Errorf("ignored = %v, want %v", inv.IgnoredParams, want)
	}
	for _, p := range inv.IgnoredParams {
		if !want[p] {
			t.Errorf("unexpected ignored param %q", p)
		}
	}
}

func newTranslator() *Translator {
	return &Translator{
		ChunkID:   "chatcmpl-test",
		Model:     "claude-code",
		Verbosity: VerbosityTextOnly,
		Now:       func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func TestEventToChunks_AssistantTextOnce(t *testing.T) {
	tr := newTranslator()
	ev := claude.Event{
		Type: claude.EventAssistant,
		Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{Type: "text", Text: "hello"}},
		}},
	}
	chunks := tr.EventToChunks(ev)
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.Choices[0].Delta.Role != "assistant" {
		t.Errorf("first chunk should set role=assistant, got %+v", c.Choices[0].Delta)
	}
	if c.Choices[0].Delta.Content != "hello" {
		t.Errorf("content = %q", c.Choices[0].Delta.Content)
	}
	if c.Choices[0].FinishReason != nil {
		t.Errorf("non-terminal chunk should have nil finish_reason")
	}

	// Second assistant event should not re-emit role.
	chunks = tr.EventToChunks(ev)
	if chunks[0].Choices[0].Delta.Role != "" {
		t.Errorf("role should only appear on first chunk")
	}
}

func TestEventToChunks_ToolUseInContent_HiddenInTextOnly(t *testing.T) {
	tr := newTranslator()
	ev := claude.Event{
		Type: claude.EventAssistant,
		Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{
				{Type: "tool_use"},
				{Type: "text", Text: "result is 4"},
			},
		}},
	}
	chunks := tr.EventToChunks(ev)
	if len(chunks) != 1 || chunks[0].Choices[0].Delta.Content != "result is 4" {
		t.Errorf("text-only mode should keep only text content, got %+v", chunks)
	}
}

func TestEventToChunks_TerminalResult(t *testing.T) {
	tr := newTranslator()
	ev := claude.Event{
		Type: claude.EventResult,
		Result: &claude.ResultEvent{
			StopReason: "end_turn",
			Result:     "done",
			Usage:      json.RawMessage(`{"input_tokens":3,"output_tokens":5}`),
		},
	}
	chunks := tr.EventToChunks(ev)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	c := chunks[0]
	if c.Choices[0].FinishReason == nil || *c.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %v, want stop", c.Choices[0].FinishReason)
	}
	if c.Usage == nil || c.Usage.PromptTokens != 3 || c.Usage.CompletionTokens != 5 || c.Usage.TotalTokens != 8 {
		t.Errorf("usage = %+v", c.Usage)
	}
}

func TestEventToChunks_StreamErrorBecomesErrorTerminal(t *testing.T) {
	tr := newTranslator()
	ev := claude.Event{
		Type: claude.EventStreamError,
		Err:  &claude.StreamError{Message: "claude died"},
	}
	chunks := tr.EventToChunks(ev)
	if len(chunks) != 1 || chunks[0].Choices[0].FinishReason == nil ||
		*chunks[0].Choices[0].FinishReason != "error" {
		t.Errorf("expected error finish_reason, got %+v", chunks)
	}
}

func TestNonStreamingResponse_AggregatesText(t *testing.T) {
	tr := newTranslator()
	events := []claude.Event{
		{Type: claude.EventAssistant, Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{Type: "text", Text: "Hi "}},
		}}},
		{Type: claude.EventAssistant, Assistant: &claude.AssistantEvent{Message: claude.AssistantMessage{
			Content: []claude.AssistantContent{{Type: "text", Text: "there"}},
		}}},
		{Type: claude.EventResult, Result: &claude.ResultEvent{StopReason: "end_turn"}},
	}
	resp := tr.NonStreamingResponse(events)
	if resp.Object != "chat.completion" {
		t.Errorf("object = %q", resp.Object)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %q", resp.Choices[0].FinishReason)
	}
	if got := resp.Choices[0].Message.ContentString(); got != "Hi there" {
		t.Errorf("content = %q", got)
	}
}

func TestParseVerbosity(t *testing.T) {
	cases := map[string]Verbosity{
		"text-only": VerbosityTextOnly,
		"verbose":   VerbosityVerbose,
		"narrated":  VerbosityNarrated,
		"VERBOSE":   VerbosityVerbose,
	}
	for in, want := range cases {
		got, ok := ParseVerbosity(in)
		if !ok || got != want {
			t.Errorf("ParseVerbosity(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
	if _, ok := ParseVerbosity("nonsense"); ok {
		t.Error("garbage input should return ok=false")
	}
}

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"max_tokens":    "length",
		"tool_use":      "tool_calls",
		"":              "stop",
		"unknown":       "stop",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Errorf("mapStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}
