package agent_test

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/agent"
	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func TestParseAgentVerdict_Recommend(t *testing.T) {
	raw := `{"outcome":"recommend","recommend_body":"No breaking changes affect this codebase. The upstream update only changed internal implementation details; the public API we use (Client.Get) is unchanged.","flag_reason":"","justification":""}`
	v, err := agent.ParseAgentVerdict(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Outcome != models.AgentOutcomeRecommend {
		t.Errorf("outcome = %q, want recommend", v.Outcome)
	}
	if v.RecommendBody == "" {
		t.Error("recommend_body should be non-empty")
	}
}

func TestParseAgentVerdict_NeedsChanges(t *testing.T) {
	raw := `{"outcome":"needs_changes","recommend_body":"","flag_reason":"","justification":"The upstream API changed the signature of Connect() to require a context.Context argument. This codebase calls Connect() in server.go:42 without a context. Updated to pass context.Background() as a stopgap; this should be threaded through from the caller in a follow-up."}`
	v, err := agent.ParseAgentVerdict(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Outcome != models.AgentOutcomeNeedsChanges {
		t.Errorf("outcome = %q, want needs_changes", v.Outcome)
	}
	if v.Justification == "" {
		t.Error("justification should be non-empty for needs_changes")
	}
}

func TestParseAgentVerdict_FlagHuman(t *testing.T) {
	raw := `{"outcome":"flag_human","recommend_body":"","flag_reason":"The new version introduces a dependency on an LGPL-licensed component; legal review is needed before this codebase can adopt it.","justification":""}`
	v, err := agent.ParseAgentVerdict(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Outcome != models.AgentOutcomeFlagHuman {
		t.Errorf("outcome = %q, want flag_human", v.Outcome)
	}
	if v.FlagReason == "" {
		t.Error("flag_reason should be non-empty for flag_human")
	}
}

func TestParseAgentVerdict_GaveUp(t *testing.T) {
	raw := `{"outcome":"gave_up","recommend_body":"","flag_reason":"","justification":""}`
	v, err := agent.ParseAgentVerdict(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Outcome != models.AgentOutcomeGaveUp {
		t.Errorf("outcome = %q, want gave_up", v.Outcome)
	}
}

func TestParseAgentVerdict_InvalidOutcome(t *testing.T) {
	raw := `{"outcome":"magic","recommend_body":"","flag_reason":"","justification":""}`
	_, err := agent.ParseAgentVerdict(raw)
	if err == nil {
		t.Error("expected error for invalid outcome, got nil")
	}
}

func TestParseAgentVerdict_RecommendWithEmptyBody(t *testing.T) {
	raw := `{"outcome":"recommend","recommend_body":"","flag_reason":"","justification":""}`
	_, err := agent.ParseAgentVerdict(raw)
	if err == nil {
		t.Error("expected error: recommend requires a non-empty recommend_body")
	}
}

func TestParseAgentVerdict_FlagHumanWithEmptyReason(t *testing.T) {
	raw := `{"outcome":"flag_human","recommend_body":"","flag_reason":"","justification":""}`
	_, err := agent.ParseAgentVerdict(raw)
	if err == nil {
		t.Error("expected error: flag_human requires a non-empty flag_reason")
	}
}

func TestParseAgentVerdict_JSONInProse(t *testing.T) {
	// The agent may wrap the JSON in prose — llmutil.ExtractJSON should handle it.
	raw := `After analysing the upstream changes, here is my verdict:

{"outcome":"recommend","recommend_body":"The upstream changes are limited to internal refactoring of the build system; the public API is unchanged. grep for the package name returns no usages that would be affected.","flag_reason":"","justification":""}

I have verified the above by checking the upstream changelog.`
	v, err := agent.ParseAgentVerdict(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.Outcome != models.AgentOutcomeRecommend {
		t.Errorf("outcome = %q, want recommend", v.Outcome)
	}
}

func TestParseAgentVerdict_MalformedJSON(t *testing.T) {
	raw := `not json at all`
	_, err := agent.ParseAgentVerdict(raw)
	if err == nil {
		t.Error("expected error for malformed JSON, got nil")
	}
}
