package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/guryn/ccproxy/internal/openai"
)

func TestNormalizeEffort(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"low":    "low",
		"MEDIUM": "medium",
		" High ": "high",
		"xhigh":  "xhigh",
		"max":    "max",
	}
	for in, want := range cases {
		got, ok := NormalizeEffort(in)
		if !ok || got != want {
			t.Errorf("NormalizeEffort(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
	if _, ok := NormalizeEffort("turbo"); ok {
		t.Error("expected ok=false for unknown effort")
	}
}

func TestRequestToInvocation_PlumbsEffort(t *testing.T) {
	body, _ := json.Marshal("hi")
	inv, err := RequestToInvocation(&openai.ChatRequest{
		Messages:        []openai.Message{{Role: "user", Content: body}},
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Effort != "high" {
		t.Errorf("inv.Effort = %q, want high", inv.Effort)
	}
}

func TestRequestToInvocation_RejectsBadEffort(t *testing.T) {
	body, _ := json.Marshal("hi")
	_, err := RequestToInvocation(&openai.ChatRequest{
		Messages:        []openai.Message{{Role: "user", Content: body}},
		ReasoningEffort: "extreme",
	})
	if err == nil || !strings.Contains(err.Error(), "extreme") {
		t.Errorf("err = %v, want validation error mentioning extreme", err)
	}
}
