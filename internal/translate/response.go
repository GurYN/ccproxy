package translate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/guryn/ccproxy/internal/claude"
	"github.com/guryn/ccproxy/internal/openai"
)

// systemReminderRE matches Claude Code's internal harness reminders. These
// are injected into tool_result text by the host (e.g. "Whenever you read a
// file, consider whether it would be malware...") and are not part of the
// actual file/command output. Strip them before exposing results to clients.
var systemReminderRE = regexp.MustCompile(`(?s)\s*<system-reminder>.*?</system-reminder>\s*`)

// Translator carries the per-request state needed to project Claude Code
// events into OpenAI chunks: the chunk ID, the model name to echo back,
// the verbosity mode, and a clock for the `created` timestamp.
type Translator struct {
	ChunkID   string
	Model     string
	Verbosity Verbosity
	Now       func() time.Time

	roleEmitted    bool
	toolCallIndex  int
	toolCallByID   map[string]int // tool_use.id → emitted tool_calls index
	// partialTextSeen is set when we've already streamed text via
	// content_block_delta events, so the terminal `assistant` event's
	// text content items must be suppressed to avoid duplication.
	partialTextSeen bool
}

// EventToChunks projects one Claude Code event into zero or more OpenAI
// chunks. The terminal `result` event is the only one that produces a chunk
// with a non-nil finish_reason and a populated usage block.
func (t *Translator) EventToChunks(ev claude.Event) []openai.ChatChunk {
	switch t.Verbosity {
	case VerbosityVerbose:
		return t.verbose(ev)
	case VerbosityNarrated:
		return t.narrated(ev)
	default:
		return t.textOnly(ev)
	}
}

// --- text-only ---------------------------------------------------------

func (t *Translator) textOnly(ev claude.Event) []openai.ChatChunk {
	switch ev.Type {
	case claude.EventStreamEvent:
		return t.streamDelta(ev.Stream)
	case claude.EventAssistant:
		return t.assistantText(ev.Assistant)
	case claude.EventResult:
		return []openai.ChatChunk{t.terminalChunk(ev.Result, "")}
	case claude.EventStreamError:
		msg := "claude subprocess error"
		if ev.Err != nil && ev.Err.Message != "" {
			msg = ev.Err.Message
		}
		return []openai.ChatChunk{t.terminalChunk(nil, msg)}
	}
	return nil
}

func (t *Translator) assistantText(a *claude.AssistantEvent) []openai.ChatChunk {
	if a == nil || t.partialTextSeen {
		// Text already streamed via content_block_delta; don't re-emit.
		return nil
	}
	var text string
	for _, c := range a.Message.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	if text == "" {
		return nil
	}
	return []openai.ChatChunk{t.chunk(t.deltaWithRole(openai.Delta{Content: text}), nil)}
}

// streamDelta forwards Anthropic content_block_delta events as incremental
// OpenAI chunks. Only text_delta is projected today; input_json_delta and
// thinking_delta are ignored (the terminal `assistant` event still carries
// the assembled tool_use/thinking blocks for verbose mode).
func (t *Translator) streamDelta(se *claude.StreamEvent) []openai.ChatChunk {
	if se == nil || se.Type != "content_block_delta" {
		return nil
	}
	if se.Delta.Type != "text_delta" || se.Delta.Text == "" {
		return nil
	}
	t.partialTextSeen = true
	return []openai.ChatChunk{t.chunk(t.deltaWithRole(openai.Delta{Content: se.Delta.Text}), nil)}
}

// --- verbose -----------------------------------------------------------

func (t *Translator) verbose(ev claude.Event) []openai.ChatChunk {
	switch ev.Type {
	case claude.EventStreamEvent:
		return t.streamDelta(ev.Stream)
	case claude.EventAssistant:
		return t.assistantVerbose(ev.Assistant)
	case claude.EventUser:
		return t.userVerbose(ev.User)
	case claude.EventResult:
		finish := mapStopReason(ev.Result.StopReason)
		// If the run ended on a tool call (rare in stateless mode but possible),
		// surface the OpenAI-spec "tool_calls" finish reason.
		if t.toolCallIndex > 0 && finish == "stop" {
			finish = "tool_calls"
		}
		c := t.chunk(openai.Delta{}, &finish)
		c.Usage = mapUsage(ev.Result)
		return []openai.ChatChunk{c}
	case claude.EventStreamError:
		msg := "claude subprocess error"
		if ev.Err != nil && ev.Err.Message != "" {
			msg = ev.Err.Message
		}
		return []openai.ChatChunk{t.terminalChunk(nil, msg)}
	}
	return nil
}

func (t *Translator) assistantVerbose(a *claude.AssistantEvent) []openai.ChatChunk {
	if a == nil {
		return nil
	}
	out := make([]openai.ChatChunk, 0, len(a.Message.Content))
	for _, c := range a.Message.Content {
		switch c.Type {
		case "text":
			if c.Text == "" || t.partialTextSeen {
				continue
			}
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: c.Text}), nil))
		case "tool_use":
			// Claude Code's tools (Bash, Read, etc.) execute server-side.
			// Emitting them as OpenAI `tool_calls` makes clients believe
			// THEY must execute the call and error with "unavailable tool".
			// Render as informational content instead, with full arguments
			// for inspection. Track the index for tool_result correlation.
			if t.toolCallByID == nil {
				t.toolCallByID = map[string]int{}
			}
			t.toolCallByID[c.ID] = t.toolCallIndex
			t.toolCallIndex++
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			line := fmt.Sprintf("\n\n**🔧 %s**\n```json\n%s\n```\n", c.Name, args)
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: line}), nil))
		case "thinking":
			// Thinking blocks surface only in verbose mode, prefixed so a
			// downstream UI can choose to fold them.
			if c.Text == "" {
				continue
			}
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{
				Content: "\n[thinking] " + c.Text + "\n",
			}), nil))
		}
	}
	return out
}

func (t *Translator) userVerbose(u *claude.UserEvent) []openai.ChatChunk {
	if u == nil {
		return nil
	}
	var out []openai.ChatChunk
	for _, c := range u.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		// Server-side tool: render the result as a fenced code block so the
		// content (which may contain <tags>, backticks, system-reminders,
		// etc.) renders literally instead of being parsed as inline HTML or
		// markdown. Use a 4-backtick fence so embedded triple-backticks in
		// the body don't break out.
		body := truncate(stripSystemReminders(c.TextResult()), 8<<10)
		header := "↪ result"
		if c.IsError {
			header = "⚠ error"
		}
		_ = c.ToolUseID
		line := fmt.Sprintf("\n**%s**\n````\n%s\n````\n", header, body)
		out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: line}), nil))
	}
	return out
}

// --- narrated ----------------------------------------------------------

func (t *Translator) narrated(ev claude.Event) []openai.ChatChunk {
	switch ev.Type {
	case claude.EventStreamEvent:
		return t.streamDelta(ev.Stream)
	case claude.EventAssistant:
		return t.assistantNarrated(ev.Assistant)
	case claude.EventUser:
		return t.userNarrated(ev.User)
	case claude.EventResult:
		return []openai.ChatChunk{t.terminalChunk(ev.Result, "")}
	case claude.EventStreamError:
		msg := "claude subprocess error"
		if ev.Err != nil && ev.Err.Message != "" {
			msg = ev.Err.Message
		}
		return []openai.ChatChunk{t.terminalChunk(nil, msg)}
	}
	return nil
}

func (t *Translator) assistantNarrated(a *claude.AssistantEvent) []openai.ChatChunk {
	if a == nil {
		return nil
	}
	var out []openai.ChatChunk
	for _, c := range a.Message.Content {
		switch c.Type {
		case "text":
			if c.Text == "" || t.partialTextSeen {
				continue
			}
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: c.Text}), nil))
		case "tool_use":
			line := fmt.Sprintf("\n🔧 %s(%s)\n", c.Name, summarizeInput(c.Input))
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: line}), nil))
		}
	}
	return out
}

func (t *Translator) userNarrated(u *claude.UserEvent) []openai.ChatChunk {
	if u == nil {
		return nil
	}
	var out []openai.ChatChunk
	for _, c := range u.Message.Content {
		if c.Type != "tool_result" {
			continue
		}
		head := truncate(strings.TrimSpace(stripSystemReminders(c.TextResult())), 200)
		marker := "↳"
		if c.IsError {
			marker = "✗"
		}
		out = append(out, t.chunk(t.deltaWithRole(openai.Delta{
			Content: fmt.Sprintf("   %s %s\n", marker, head),
		}), nil))
	}
	return out
}

// --- shared helpers ----------------------------------------------------

func (t *Translator) deltaWithRole(d openai.Delta) openai.Delta {
	if !t.roleEmitted {
		d.Role = "assistant"
		t.roleEmitted = true
	}
	return d
}

func (t *Translator) chunk(delta openai.Delta, finish *string) openai.ChatChunk {
	return openai.ChatChunk{
		ID:      t.ChunkID,
		Object:  "chat.completion.chunk",
		Created: t.now().Unix(),
		Model:   t.Model,
		Choices: []openai.ChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: finish,
		}},
	}
}

func (t *Translator) terminalChunk(result *claude.ResultEvent, errMsg string) openai.ChatChunk {
	finish := "stop"
	if errMsg != "" {
		finish = "error"
	} else if result != nil {
		finish = mapStopReason(result.StopReason)
	}
	c := t.chunk(openai.Delta{}, &finish)
	if result != nil {
		c.Usage = mapUsage(result)
	}
	return c
}

// NonStreamingResponse aggregates an entire event stream into one OpenAI
// chat.completion response. Tool calls and results are flattened into
// the assistant message content as a best-effort representation.
func (t *Translator) NonStreamingResponse(events []claude.Event) openai.ChatResponse {
	var (
		text   string
		result *claude.ResultEvent
		errMsg string
	)
	for _, ev := range events {
		switch ev.Type {
		case claude.EventAssistant:
			if ev.Assistant != nil {
				for _, c := range ev.Assistant.Message.Content {
					if c.Type == "text" {
						text += c.Text
					}
				}
			}
		case claude.EventResult:
			result = ev.Result
		case claude.EventStreamError:
			if ev.Err != nil && errMsg == "" {
				errMsg = ev.Err.Message
			}
		}
	}
	finish := "stop"
	if errMsg != "" {
		finish = "error"
	} else if result != nil {
		finish = mapStopReason(result.StopReason)
		if text == "" {
			text = result.Result
		}
	}
	resp := openai.ChatResponse{
		ID:      t.ChunkID,
		Object:  "chat.completion",
		Created: t.now().Unix(),
		Model:   t.Model,
		Choices: []openai.Choice{{
			Index:        0,
			FinishReason: finish,
			Message: openai.Message{
				Role:    "assistant",
				Content: jsonString(text),
			},
		}},
	}
	if result != nil {
		resp.Usage = mapUsage(result)
	}
	return resp
}

func (t *Translator) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// summarizeInput produces a one-line preview of a tool's input JSON for the
// narrated UI. Keeps full keys for short inputs; truncates the value.
func summarizeInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncate(string(raw), 80)
	}
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncate(fmt.Sprint(v), 40)))
	}
	return truncate(strings.Join(parts, ", "), 120)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func stripSystemReminders(s string) string {
	return strings.TrimSpace(systemReminderRE.ReplaceAllString(s, "\n"))
}
