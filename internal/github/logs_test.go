package ghclient

import (
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func TestFetchFailureLogs_FromOutput(t *testing.T) {
	failing := func(name, output string) models.CheckDetail {
		c := "failure"
		return models.CheckDetail{Name: name, Status: "completed", Conclusion: &c, Output: output}
	}

	ci := models.CIStatus{
		Failures: []models.CheckDetail{
			failing("build", "compile error: undefined symbol"),
			failing("lint", ""), // no output → empty entry, best-effort
		},
	}

	c := &Client{}
	got := c.fetchFailureLogs(ci, 42)

	if got["build"] != "compile error: undefined symbol" {
		t.Errorf("build log = %q, want the captured output", got["build"])
	}
	if got["lint"] != "" {
		t.Errorf("lint log = %q, want empty (no output)", got["lint"])
	}
}

func TestFetchFailureLogs_Truncates(t *testing.T) {
	big := strings.Repeat("x", maxLogBytes+5000)
	c := "failure"
	ci := models.CIStatus{
		Failures: []models.CheckDetail{
			{Name: "huge", Status: "completed", Conclusion: &c, Output: big},
		},
	}

	got := (&Client{}).fetchFailureLogs(ci, 1)["huge"]
	if len(got) <= maxLogBytes {
		// readTail prepends a truncation marker, so the result is the marker
		// plus the last maxLogBytes bytes — it should be just over maxLogBytes
		// in length but it must NOT contain the whole oversized input.
	}
	if !strings.Contains(got, "log truncated") {
		t.Errorf("expected a truncation marker for oversized output, got %d bytes without one", len(got))
	}
	if strings.Count(got, "x") > maxLogBytes {
		t.Errorf("output not truncated: %d x's > budget %d", strings.Count(got, "x"), maxLogBytes)
	}
}
