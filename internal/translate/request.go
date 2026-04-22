package translate

import (
	"fmt"
	"strings"

	"github.com/guryn/ccproxy/internal/openai"
)

// ClaudeInvocation is the result of translating an OpenAI chat request into
// the inputs ccproxy needs to spawn or resume a Claude Code subprocess.
type ClaudeInvocation struct {
	// Prompt is the value passed to `claude -p`.
	Prompt string
	// AppendSystemPrompt is passed to `--append-system-prompt` when non-empty.
	AppendSystemPrompt string
	// Effort, when non-empty, is passed to `--effort`. Already normalized.
	Effort string
	// ClaudeModel, when non-empty, is passed to `--model`. Populated by the
	// server's resolveClaudeModel chain — translate itself does not parse it.
	ClaudeModel string
	// IgnoredParams lists request fields ccproxy accepted but did not act on.
	// Surfaced to the client via the X-CC-Ignored-Params response header
	// (PRD §6.2 #6).
	IgnoredParams []string
}

// RequestToInvocation flattens an OpenAI ChatRequest into ccproxy's
// per-call subprocess inputs.
//
// Mapping (PRD §6.2):
//   - All `system` messages are joined with blank-line separators and become
//     --append-system-prompt.
//   - Prior turns (everything except the trailing user message) are
//     serialized as a "Conversation so far:" preamble prepended to the
//     prompt. This is the stateless-mode behavior; persistent sessions
//     (M2.3) skip the preamble and only send the latest user message.
//   - The trailing user message is the actual -p prompt.
//   - temperature / top_p / max_tokens / tools are reported in
//     IgnoredParams so the server can surface X-CC-Ignored-Params.
func RequestToInvocation(req *openai.ChatRequest) (ClaudeInvocation, error) {
	if req == nil {
		return ClaudeInvocation{}, fmt.Errorf("nil request")
	}
	if len(req.Messages) == 0 {
		return ClaudeInvocation{}, fmt.Errorf("messages[] is empty")
	}

	var (
		systems   []string
		nonSystem []openai.Message
	)
	for _, m := range req.Messages {
		if strings.EqualFold(m.Role, "system") {
			if s := strings.TrimSpace(m.ContentString()); s != "" {
				systems = append(systems, s)
			}
			continue
		}
		nonSystem = append(nonSystem, m)
	}
	if len(nonSystem) == 0 {
		return ClaudeInvocation{}, fmt.Errorf("no non-system messages")
	}

	last := nonSystem[len(nonSystem)-1]
	if !strings.EqualFold(last.Role, "user") {
		return ClaudeInvocation{}, fmt.Errorf("last message must be role=user, got %q", last.Role)
	}

	var prompt strings.Builder
	if prior := nonSystem[:len(nonSystem)-1]; len(prior) > 0 {
		prompt.WriteString("Conversation so far:\n")
		for _, m := range prior {
			fmt.Fprintf(&prompt, "%s: %s\n", strings.ToLower(m.Role), m.ContentString())
		}
		prompt.WriteString("\nLatest user message:\n")
	}
	prompt.WriteString(last.ContentString())

	effort, ok := NormalizeEffort(req.ReasoningEffort)
	if !ok {
		return ClaudeInvocation{}, EffortError(req.ReasoningEffort)
	}

	inv := ClaudeInvocation{
		Prompt:             prompt.String(),
		AppendSystemPrompt: strings.Join(systems, "\n\n"),
		Effort:             effort,
		IgnoredParams:      collectIgnored(req),
	}
	return inv, nil
}

func collectIgnored(req *openai.ChatRequest) []string {
	var out []string
	if req.Temperature != nil {
		out = append(out, "temperature")
	}
	if req.TopP != nil {
		out = append(out, "top_p")
	}
	if req.MaxTokens != nil {
		out = append(out, "max_tokens")
	}
	if len(req.Tools) > 0 {
		out = append(out, "tools")
	}
	return out
}
