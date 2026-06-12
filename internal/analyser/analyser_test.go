package analyser

import (
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func concl(s string) *string { return &s }

func makeFailure(name, output string) models.CheckDetail {
	return models.CheckDetail{
		Name:       name,
		Status:     "completed",
		Conclusion: concl("failure"),
		Output:     output,
	}
}

func TestFormatFailureLogs_NoneFailiing(t *testing.T) {
	ci := models.CIStatus{}
	got := formatFailureLogs(ci, nil)
	if got != "(no failing checks)" {
		t.Errorf("got %q, want no-failures notice", got)
	}
}

func TestFormatFailureLogs_MissingLog(t *testing.T) {
	ci := models.CIStatus{Failures: []models.CheckDetail{makeFailure("build", "")}}
	got := formatFailureLogs(ci, map[string]string{"build": ""})
	if !strings.Contains(got, "log unavailable") {
		t.Errorf("expected 'log unavailable' notice, got: %s", got)
	}
}

func TestFormatFailureLogs_TotalBudget(t *testing.T) {
	// Build 6 checks each with a 10 KB log — 60 KB total, which exceeds the
	// 50 KB budget. The output must be capped at ~50 KB, with omission notices
	// for the checks that don't fit.
	chunkSize := 10_000
	chunk := strings.Repeat("x", chunkSize)

	var failures []models.CheckDetail
	logMap := map[string]string{}
	for i := range 6 {
		name := strings.Repeat(string(rune('a'+i)), 5)
		failures = append(failures, makeFailure(name, chunk))
		logMap[name] = chunk
	}
	ci := models.CIStatus{Failures: failures}

	got := formatFailureLogs(ci, logMap)

	// Should contain at least one "omitted" or "truncated" notice.
	if !strings.Contains(got, "omitted") && !strings.Contains(got, "truncated") {
		t.Errorf("expected omission/truncation notice when over budget, got none; len=%d", len(got))
	}
	// Output size must not wildly exceed the budget (allow some headroom for
	// the section headers and code fences around each block).
	const headroom = 2_000
	if len(got) > maxTotalLogBytes+headroom {
		t.Errorf("output %d bytes, want ≤ %d (budget + headroom)", len(got), maxTotalLogBytes+headroom)
	}
}

func TestFormatFailureLogs_WithinBudget(t *testing.T) {
	// Two small logs — both should appear in full.
	ci := models.CIStatus{
		Failures: []models.CheckDetail{
			makeFailure("lint", "lint error here"),
			makeFailure("test", "test failure here"),
		},
	}
	logs := map[string]string{"lint": "lint error here", "test": "test failure here"}
	got := formatFailureLogs(ci, logs)
	if !strings.Contains(got, "lint error here") {
		t.Errorf("expected lint log in output")
	}
	if !strings.Contains(got, "test failure here") {
		t.Errorf("expected test log in output")
	}
}
