package config

import (
	"strings"
	"testing"
)

func TestPassthroughEnabled_DefaultsTrue(t *testing.T) {
	c := &Config{}
	if !c.PassthroughEnabled() {
		t.Error("default should enable passthrough")
	}
	f := false
	c.AllowPassthroughModels = &f
	if c.PassthroughEnabled() {
		t.Error("explicit false should disable passthrough")
	}
}

func TestResolvePassthroughModel_FamilyAliasUnpinned(t *testing.T) {
	c := &Config{}
	got, ok := c.ResolvePassthroughModel("haiku")
	if !ok || got != "haiku" {
		t.Errorf("unpinned haiku → (%q,%v), want (haiku,true)", got, ok)
	}
}

func TestResolvePassthroughModel_FamilyAliasPinned(t *testing.T) {
	c := &Config{ModelVersions: map[string]string{
		"opus":   "claude-opus-4-7",
		"sonnet": "claude-sonnet-4-6",
		"haiku":  "claude-haiku-4-5",
	}}
	cases := map[string]string{
		"opus":   "claude-opus-4-7",
		"sonnet": "claude-sonnet-4-6",
		"haiku":  "claude-haiku-4-5",
	}
	for in, want := range cases {
		got, ok := c.ResolvePassthroughModel(in)
		if !ok || got != want {
			t.Errorf("%q → (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
}

func TestResolvePassthroughModel_FullID(t *testing.T) {
	c := &Config{}
	cases := []string{"claude-opus-4-7", "claude-sonnet-4-6", "claude-haiku-4-5"}
	for _, id := range cases {
		got, ok := c.ResolvePassthroughModel(id)
		if !ok || got != id {
			t.Errorf("full id %q → (%q,%v), want (%q,true)", id, got, ok, id)
		}
	}
}

func TestResolvePassthroughModel_RejectsNonClaude(t *testing.T) {
	c := &Config{}
	for _, id := range []string{"gpt-4o", "claude-code", "claude-", "claude-foo-1", "haik", ""} {
		if _, ok := c.ResolvePassthroughModel(id); ok {
			t.Errorf("%q should NOT pass through", id)
		}
	}
}

func TestValidate_RejectsBadModelVersionKey(t *testing.T) {
	c := Defaults()
	c.Models = []Model{{ID: "m1"}}
	c.ModelVersions = map[string]string{"gpt": "claude-sonnet-4-6"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "model_versions key") {
		t.Errorf("err = %v, want bad-key error", err)
	}
}

func TestValidate_RejectsBadModelVersionValue(t *testing.T) {
	c := Defaults()
	c.Models = []Model{{ID: "m1"}}
	c.ModelVersions = map[string]string{"opus": "gpt-4o"}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "must be a full claude id") {
		t.Errorf("err = %v, want bad-value error", err)
	}
}
