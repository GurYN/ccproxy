package translate

import "strings"

// Verbosity selects how Claude Code events are projected into OpenAI chunks.
// PRD §6.5.
type Verbosity string

const (
	VerbosityTextOnly Verbosity = "text-only"
	VerbosityVerbose  Verbosity = "verbose"
	VerbosityNarrated Verbosity = "narrated"
)

// ParseVerbosity normalizes the X-CC-Verbosity header value.
// Empty input returns the zero value; the caller decides the default.
func ParseVerbosity(s string) (Verbosity, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return "", false
	case "text-only", "text", "default":
		return VerbosityTextOnly, true
	case "verbose":
		return VerbosityVerbose, true
	case "narrated":
		return VerbosityNarrated, true
	default:
		return "", false
	}
}
