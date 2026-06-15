package reviewer

import (
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// newTestReviewer returns a Reviewer with a zero-value client, suitable for
// testing BuildPrompt and ParseResponse which do not call the API.
func newTestReviewer() *Reviewer {
	return &Reviewer{}
}

func TestBuildPrompt_IncludesAssessment(t *testing.T) {
	r := newTestReviewer()
	prompt := r.BuildPrompt(
		"Breaking: removed initSentry()",
		[]models.CodeChangeEntry{
			{File: "src/sentry.js", Description: "update import"},
		},
		"some diff",
		2,
		[]string{"commit one", "commit two"},
	)

	if !strings.Contains(prompt, "initSentry") {
		t.Error("prompt should contain 'initSentry' from the assessment body")
	}
	if !strings.Contains(prompt, "src/sentry.js") {
		t.Error("prompt should contain 'src/sentry.js' from the code changes")
	}
}

func TestBuildPrompt_IncludesDiff(t *testing.T) {
	r := newTestReviewer()
	prompt := r.BuildPrompt(
		"assessment text",
		nil,
		"+bar()",
		1,
		[]string{"fix bar"},
	)

	if !strings.Contains(prompt, "+bar()") {
		t.Error("prompt should contain the diff content '+bar()'")
	}
}

func TestBuildPrompt_IncludesCommitMessages(t *testing.T) {
	r := newTestReviewer()
	prompt := r.BuildPrompt(
		"assessment text",
		nil,
		"diff",
		3,
		[]string{"msg1", "msg2", "msg3"},
	)

	for _, msg := range []string{"msg1", "msg2", "msg3"} {
		if !strings.Contains(prompt, msg) {
			t.Errorf("prompt should contain commit message %q", msg)
		}
	}
}

func TestParseResponse_Approve(t *testing.T) {
	r := newTestReviewer()
	verdict, err := r.ParseResponse(`{"verdict": "approve", "concerns": [], "summary": "Changes look correct"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if verdict.Verdict != "approve" {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, "approve")
	}
	if len(verdict.Concerns) != 0 {
		t.Errorf("concerns = %v, want empty", verdict.Concerns)
	}
}

func TestParseResponse_RequestChanges(t *testing.T) {
	r := newTestReviewer()
	verdict, err := r.ParseResponse(`{"verdict": "request_changes", "concerns": ["test_foo was deleted", "workaround in bar.js"], "summary": "Issues found"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if verdict.Verdict != "request_changes" {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, "request_changes")
	}
	if len(verdict.Concerns) != 2 {
		t.Errorf("len(concerns) = %d, want 2", len(verdict.Concerns))
	}
}

func TestParseResponse_InvalidJSON(t *testing.T) {
	r := newTestReviewer()
	_, err := r.ParseResponse("not json")
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseResponse_MissingVerdict(t *testing.T) {
	r := newTestReviewer()
	_, err := r.ParseResponse(`{"concerns": [], "summary": "no verdict"}`)
	if err == nil {
		t.Fatal("expected error for missing verdict, got nil")
	}
}

func TestParseResponse_InvalidVerdict(t *testing.T) {
	r := newTestReviewer()
	_, err := r.ParseResponse(`{"verdict": "maybe", "concerns": [], "summary": "unsure"}`)
	if err == nil {
		t.Fatal("expected error for invalid verdict 'maybe', got nil")
	}
}

func TestParseResponse_StripsCodeFences(t *testing.T) {
	r := newTestReviewer()
	input := "```json\n{\"verdict\": \"approve\", \"concerns\": [], \"summary\": \"All good\"}\n```"
	verdict, err := r.ParseResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if verdict.Verdict != "approve" {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, "approve")
	}
}
