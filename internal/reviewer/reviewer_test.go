package reviewer

import (
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// newTestReviewer returns a Reviewer suitable for testing BuildBrief and
// ParseResponse which do not invoke the subprocess.
func newTestReviewer() *Reviewer {
	return &Reviewer{}
}

func TestBuildBrief_IncludesAssessment(t *testing.T) {
	r := newTestReviewer()
	brief := r.BuildBrief(
		"abc123",
		"auto/fix/sentry-node-10.0.0",
		"Breaking: removed initSentry()",
		[]models.CodeChangeEntry{
			{File: "src/sentry.js", Description: "update import"},
		},
		2,
		[]string{"commit one", "commit two"},
		1,
	)

	if !strings.Contains(brief, "initSentry") {
		t.Error("brief should contain 'initSentry' from the assessment body")
	}
	if !strings.Contains(brief, "src/sentry.js") {
		t.Error("brief should contain 'src/sentry.js' from the code changes")
	}
}

func TestBuildBrief_IncludesBumpTipSHA(t *testing.T) {
	r := newTestReviewer()
	brief := r.BuildBrief(
		"deadbeef",
		"auto/fix/lodash-5.0.0",
		"assessment text",
		nil,
		1,
		[]string{"fix bar"},
		1,
	)

	if !strings.Contains(brief, "deadbeef") {
		t.Error("brief should contain the bumpTipSHA 'deadbeef'")
	}
	if !strings.Contains(brief, "git diff deadbeef..HEAD") {
		t.Error("brief should contain the git diff command referencing bumpTipSHA")
	}
}

func TestBuildBrief_IncludesCommitMessages(t *testing.T) {
	r := newTestReviewer()
	brief := r.BuildBrief(
		"abc123",
		"auto/fix/lodash-5.0.0",
		"assessment text",
		nil,
		3,
		[]string{"msg1", "msg2", "msg3"},
		1,
	)

	for _, msg := range []string{"msg1", "msg2", "msg3"} {
		if !strings.Contains(brief, msg) {
			t.Errorf("brief should contain commit message %q", msg)
		}
	}
}

func TestParseResponse_Approve(t *testing.T) {
	r := newTestReviewer()
	verdict, err := r.ParseResponse(`{"verdict": "approve", "concerns": []}`)
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
	verdict, err := r.ParseResponse(`{"verdict": "request_changes", "concerns": ["test_foo was deleted", "workaround in bar.js"]}`)
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
	_, err := r.ParseResponse(`{"concerns": []}`)
	if err == nil {
		t.Fatal("expected error for missing verdict, got nil")
	}
}

func TestParseResponse_InvalidVerdict(t *testing.T) {
	r := newTestReviewer()
	_, err := r.ParseResponse(`{"verdict": "maybe", "concerns": []}`)
	if err == nil {
		t.Fatal("expected error for invalid verdict 'maybe', got nil")
	}
}

func TestParseResponse_StripsCodeFences(t *testing.T) {
	r := newTestReviewer()
	input := "```json\n{\"verdict\": \"approve\", \"concerns\": []}\n```"
	verdict, err := r.ParseResponse(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if verdict.Verdict != "approve" {
		t.Errorf("verdict = %q, want %q", verdict.Verdict, "approve")
	}
}
