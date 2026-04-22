package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/guryn/ccproxy/internal/claude"
	"github.com/guryn/ccproxy/internal/openai"
)

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
	if a == nil {
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

// --- verbose -----------------------------------------------------------

func (t *Translator) verbose(ev claude.Event) []openai.ChatChunk {
	switch ev.Type {
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
			if c.Text == "" {
				continue
			}
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{Content: c.Text}), nil))
		case "tool_use":
			if t.toolCallByID == nil {
				t.toolCallByID = map[string]int{}
			}
			idx := t.toolCallIndex
			t.toolCallByID[c.ID] = idx
			t.toolCallIndex++
			args := string(c.Input)
			if args == "" {
				args = "{}"
			}
			out = append(out, t.chunk(t.deltaWithRole(openai.Delta{
				ToolCalls: []openai.ToolCallDelta{{
					Index: idx,
					ID:    c.ID,
					Type:  "function",
					Function: &openai.FunctionCallDelta{
						Name:      c.Name,
						Arguments: args,
					},
				}},
			}), nil))
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
		// Synthetic role:"tool" delta chunk so OpenAI clients with tool
		// awareness can attribute the result to the right tool_call.
		body := truncate(c.TextResult(), 8<<10)
		if c.IsError {
			body = "[error] " + body
		}
		_ = c.ToolUseID // index could be looked up via t.toolCallByID; not needed by spec
		out = append(out, t.chunk(openai.Delta{
			Role:    "tool",
			Content: body,
		}, nil))
	}
	return out
}

// --- narrated ----------------------------------------------------------

func (t *Translator) narrated(ev claude.Event) []openai.ChatChunk {
	switch ev.Type {
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
			if c.Text == "" {
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
		head := truncate(strings.TrimSpace(c.TextResult()), 200)
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
