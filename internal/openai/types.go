package openai

import "encoding/json"

// ChatRequest is the subset of OpenAI's POST /v1/chat/completions body
// that ccproxy understands. Unknown fields are accepted (json.Decoder is
// lenient by default) and silently ignored.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature *float64  `json:"temperature,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`

	// SessionID is a ccproxy extension (PRD §6.3 path #2). Clients that can
	// set arbitrary body fields use this; everyone else falls back to the
	// X-CC-Session header or a model-string suffix.
	SessionID string `json:"session_id,omitempty"`

	// ReasoningEffort follows OpenAI's o-series convention. Accepted values:
	// "low", "medium", "high", plus the ccproxy/Claude extensions "xhigh"
	// and "max". Maps to `claude --effort <level>`. Empty leaves it unset.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`

	// Tools is accepted but ignored in v1 (PRD §6.2). Kept here so the
	// translator can list it in X-CC-Ignored-Params.
	Tools json.RawMessage `json:"tools,omitempty"`
}

// Message is one item in messages[]. Content is a string in the simple
// case; OpenAI also allows an array of content parts, which we accept as
// raw JSON and let the translator flatten.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

// ContentString returns the message content as a plain string. It handles
// both the bare-string and content-parts forms; unknown shapes return the
// raw JSON as a string for the translator to log and ignore.
func (m Message) ContentString() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Content, &parts); err == nil {
		var out string
		for _, p := range parts {
			if p.Type == "text" {
				out += p.Text
			}
		}
		return out
	}
	return string(m.Content)
}

// ChatResponse is the non-streaming response shape.
type ChatResponse struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"` // always "chat.completion"
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message,omitempty"`
	FinishReason string  `json:"finish_reason,omitempty"`
}

// ChatChunk is one SSE frame in a streaming response.
type ChatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"` // always "chat.completion.chunk"
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// Delta is the incremental piece of an assistant message in a chunk.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// ToolCalls used by verbose mode (M2.4); kept here so the type lines up
	// across milestones.
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function *FunctionCallDelta `json:"function,omitempty"`
}

type FunctionCallDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// Usage mirrors OpenAI's usage object. PRD §9 open question: subscription
// plans don't map cleanly; M1 just passes Claude Code's reported counts.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Model is one entry in GET /v1/models.
type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"` // always "model"
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type ModelList struct {
	Object string  `json:"object"` // always "list"
	Data   []Model `json:"data"`
}

// ErrorBody is the OpenAI-style error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}
