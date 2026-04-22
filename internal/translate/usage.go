package translate

import (
	"encoding/json"

	"github.com/guryn/ccproxy/internal/claude"
	"github.com/guryn/ccproxy/internal/openai"
)

// mapStopReason translates Claude's stop_reason into OpenAI's finish_reason.
// PRD §6.5: terminal chunk emits stop / length / tool_calls / error.
func mapStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence", "":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// mapUsage projects Claude's usage block into the OpenAI shape. PRD §9
// open question: subscription accounting differs from per-token billing —
// for M1 we pass through input/output as-is and document the caveat.
func mapUsage(r *claude.ResultEvent) *openai.Usage {
	if r == nil || len(r.Usage) == 0 {
		return nil
	}
	var u struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(r.Usage, &u); err != nil {
		return nil
	}
	return &openai.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// jsonString returns a json.RawMessage holding s encoded as a JSON string.
// Used to populate openai.Message.Content (which is a RawMessage).
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
