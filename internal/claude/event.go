package claude

import (
	"encoding/json"
	"fmt"
)

// EventType is the top-level "type" field of a stream-json line.
type EventType string

const (
	EventSystem    EventType = "system"
	EventAssistant EventType = "assistant"
	EventUser      EventType = "user"
	EventRateLimit EventType = "rate_limit_event"
	EventResult    EventType = "result"
	// EventStreamEvent is emitted by `claude --include-partial-messages` and
	// carries the Anthropic Messages API event stream (message_start,
	// content_block_delta, etc.) in its `event` field.
	EventStreamEvent EventType = "stream_event"
	// EventStreamError is synthesized by ccproxy when a line fails to parse
	// or the subprocess exits abnormally — it never appears on the wire.
	EventStreamError EventType = "stream_error"
)

// Event is a tagged union over the Claude Code stream-json event types.
// Raw holds the original JSON so verbose-mode translation can re-serialize
// without re-parsing, and so unknown fields survive a round-trip.
type Event struct {
	Type      EventType
	SessionID string
	Raw       json.RawMessage

	System    *SystemEvent    `json:",omitempty"`
	Assistant *AssistantEvent `json:",omitempty"`
	User      *UserEvent      `json:",omitempty"`
	Result    *ResultEvent    `json:",omitempty"`
	Stream    *StreamEvent    `json:",omitempty"`
	Err       *StreamError    `json:",omitempty"`
}

// StreamEvent is the inner Anthropic Messages stream event carried by
// `{"type":"stream_event","event":{...}}` lines. We only decode the fields
// needed for incremental delta forwarding — other shapes survive via Raw.
type StreamEvent struct {
	Type  string `json:"type"` // message_start, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
	Index int    `json:"index"`
	Delta struct {
		Type string `json:"type"` // text_delta, input_json_delta, thinking_delta, ...
		Text string `json:"text"`
	} `json:"delta"`
}

// SystemEvent is `{"type":"system","subtype":"init"|"hook_started"|...}`.
type SystemEvent struct {
	Subtype string          `json:"subtype"`
	Model   string          `json:"model,omitempty"`
	CWD     string          `json:"cwd,omitempty"`
	Tools   []string        `json:"tools,omitempty"`
	Extra   json.RawMessage `json:"-"`
}

// AssistantEvent wraps an Anthropic-shaped message. Content items are kept
// as raw JSON because content can be text, tool_use, thinking, etc., and
// only the verbose translator needs to walk every variant.
type AssistantEvent struct {
	Message AssistantMessage `json:"message"`
}

type AssistantMessage struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	Role       string             `json:"role"`
	Content    []AssistantContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      json.RawMessage    `json:"usage,omitempty"`
}

// AssistantContent is one item in message.content[]. We decode the common
// fields used by translation; the full original payload is preserved in
// Raw so verbose mode can echo richer shapes verbatim.
type AssistantContent struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`    // tool_use.id
	Name  string          `json:"name,omitempty"`  // tool_use.name
	Input json.RawMessage `json:"input,omitempty"` // tool_use.input (JSON object)
	Raw   json.RawMessage `json:"-"`
}

// UserEvent wraps `{"type":"user","message":{...},...}`. Tool results from
// the model's tool calls arrive on this event as content items with
// type=tool_result.
type UserEvent struct {
	Message UserMessage `json:"message"`
}

type UserMessage struct {
	Role    string        `json:"role"`
	Content []UserContent `json:"content"`
}

// UserContent supports tool_result items today; other shapes preserve in Raw.
type UserContent struct {
	Type       string          `json:"type"`
	ToolUseID  string          `json:"tool_use_id,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Content    json.RawMessage `json:"content,omitempty"` // string or array of parts
	Raw        json.RawMessage `json:"-"`
}

// TextResult returns the tool_result content as a plain string. Claude's
// stream emits it as either a bare string or an array of {type,text} parts.
func (uc UserContent) TextResult() string {
	if len(uc.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(uc.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(uc.Content, &parts); err == nil {
		var out string
		for _, p := range parts {
			if p.Type == "text" {
				out += p.Text
			}
		}
		return out
	}
	return string(uc.Content)
}

// ResultEvent is the terminal `{"type":"result",...}` line.
type ResultEvent struct {
	Subtype      string          `json:"subtype"`
	IsError      bool            `json:"is_error"`
	DurationMS   int             `json:"duration_ms"`
	NumTurns     int             `json:"num_turns"`
	Result       string          `json:"result"`
	StopReason   string          `json:"stop_reason"`
	TotalCostUSD float64         `json:"total_cost_usd"`
	Usage        json.RawMessage `json:"usage,omitempty"`
}

// StreamError is a ccproxy-synthesized event signalling that the
// subprocess failed in a way the caller needs to know about.
type StreamError struct {
	Message string
	Stderr  string
	// ExitCode is -1 if the process did not exit normally (e.g. killed).
	ExitCode int
}

// parseLine turns one NDJSON line into an Event. Unknown event types are
// preserved as Type with only Raw populated.
func parseLine(line []byte) (Event, error) {
	var head struct {
		Type      string `json:"type"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Event{}, fmt.Errorf("parse line head: %w", err)
	}
	ev := Event{
		Type:      EventType(head.Type),
		SessionID: head.SessionID,
		Raw:       append(json.RawMessage(nil), line...),
	}
	switch ev.Type {
	case EventSystem:
		var s SystemEvent
		if err := json.Unmarshal(line, &s); err != nil {
			return Event{}, fmt.Errorf("parse system: %w", err)
		}
		ev.System = &s
	case EventAssistant:
		var a AssistantEvent
		if err := json.Unmarshal(line, &a); err != nil {
			return Event{}, fmt.Errorf("parse assistant: %w", err)
		}
		// Preserve raw content items so verbose mode can re-serialize them.
		var rawWrap struct {
			Message struct {
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rawWrap); err == nil {
			for i := range a.Message.Content {
				if i < len(rawWrap.Message.Content) {
					a.Message.Content[i].Raw = rawWrap.Message.Content[i]
				}
			}
		}
		ev.Assistant = &a
	case EventUser:
		var u UserEvent
		if err := json.Unmarshal(line, &u); err != nil {
			return Event{}, fmt.Errorf("parse user: %w", err)
		}
		var rawWrap struct {
			Message struct {
				Content []json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &rawWrap); err == nil {
			for i := range u.Message.Content {
				if i < len(rawWrap.Message.Content) {
					u.Message.Content[i].Raw = rawWrap.Message.Content[i]
				}
			}
		}
		ev.User = &u
	case EventResult:
		var r ResultEvent
		if err := json.Unmarshal(line, &r); err != nil {
			return Event{}, fmt.Errorf("parse result: %w", err)
		}
		ev.Result = &r
	case EventStreamEvent:
		var wrap struct {
			Event StreamEvent `json:"event"`
		}
		if err := json.Unmarshal(line, &wrap); err != nil {
			return Event{}, fmt.Errorf("parse stream_event: %w", err)
		}
		ev.Stream = &wrap.Event
	}
	return ev, nil
}
