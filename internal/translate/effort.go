package translate

import (
	"fmt"
	"strings"
)

// HeaderEffort lets clients override req.reasoning_effort via a request
// header, useful for chat UIs that don't expose the body field.
const HeaderEffort = "X-CC-Effort"

// HeaderClaudeModel lets clients pick the underlying Claude model
// (`claude --model <name>`) without changing the OpenAI `model` field.
// Accepts aliases (opus|sonnet|haiku) or full ids.
const HeaderClaudeModel = "X-CC-Claude-Model"

// validEfforts mirrors `claude --effort` choices.
var validEfforts = map[string]bool{
	"low":    true,
	"medium": true,
	"high":   true,
	"xhigh":  true,
	"max":    true,
}

// NormalizeEffort returns a lowercased, validated effort level. Empty input
// returns "" with ok=true (no effort set, default behavior). An unrecognized
// value returns ok=false so the server can reject it as invalid_request_error.
func NormalizeEffort(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	if v == "" {
		return "", true
	}
	if !validEfforts[v] {
		return "", false
	}
	return v, true
}

// EffortError formats a uniform invalid_request_error message.
func EffortError(s string) error {
	return fmt.Errorf("reasoning_effort %q must be one of: low, medium, high, xhigh, max", s)
}
