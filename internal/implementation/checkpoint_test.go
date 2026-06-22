package implementation

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// TestCheckpointJSONRoundTrip guards the resume contract: every field the
// pipeline needs to continue mid-flight must survive marshal→store→unmarshal. A
// silent loss here would make Resume() drive from wrong state (e.g. reset the
// no-progress floor, lose the reviewer's prior verdict, or re-review with no
// context). math.MaxInt (the initial Floor sentinel) and the pointer fields are
// the easy ones to get wrong.
func TestCheckpointJSONRoundTrip(t *testing.T) {
	cp := checkpoint{
		Phase:              phaseAwaitingImplCI,
		Branch:             "auto/fix/pkg-1.2.3",
		SessionID:          "sess-1",
		RepoDir:            "/data/pr/owner-repo/pr-1/repo",
		Workdir:            "/data/pr/owner-repo/pr-1",
		BumpTipSHA:         "abc123",
		BaseHeadSHA:        "def456",
		Iter:               4,
		Floor:              math.MaxInt,
		Stall:              2,
		ReviewRetriesLeft:  1,
		ReviewTurn:         3,
		LastVerdict:        &models.ReviewVerdict{Verdict: "request_changes", Concerns: []string{"narrow the type"}},
		AgentJustification: "upstream renamed the API; updated call sites",
		Analysis: &models.AgentAnalysis{
			ReviewBody:  "body",
			CodeChanges: []models.CodeChangeEntry{{File: "a.go", Description: "rename"}},
		},
	}

	blob, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got checkpoint
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Phase != cp.Phase || got.Branch != cp.Branch || got.SessionID != cp.SessionID ||
		got.RepoDir != cp.RepoDir || got.Workdir != cp.Workdir || got.BumpTipSHA != cp.BumpTipSHA ||
		got.BaseHeadSHA != cp.BaseHeadSHA || got.Iter != cp.Iter || got.Stall != cp.Stall ||
		got.ReviewRetriesLeft != cp.ReviewRetriesLeft || got.ReviewTurn != cp.ReviewTurn ||
		got.AgentJustification != cp.AgentJustification {
		t.Fatalf("scalar field mismatch:\n got %+v\nwant %+v", got, cp)
	}
	if got.Floor != math.MaxInt {
		t.Fatalf("Floor = %d did not survive round-trip (want math.MaxInt = %d)", got.Floor, math.MaxInt)
	}
	if got.LastVerdict == nil || got.LastVerdict.Verdict != "request_changes" || len(got.LastVerdict.Concerns) != 1 {
		t.Fatalf("LastVerdict lost: %+v", got.LastVerdict)
	}
	if got.Analysis == nil || got.Analysis.ReviewBody != "body" || len(got.Analysis.CodeChanges) != 1 ||
		got.Analysis.CodeChanges[0].File != "a.go" {
		t.Fatalf("Analysis lost: %+v", got.Analysis)
	}
}

// TestCheckpointJSONRoundTripEmptyPointers verifies the nil pointer fields stay
// nil (a fresh launch checkpoint has no verdict yet; the legacy path may carry
// no analysis), so Resume() doesn't dereference a spuriously-populated struct.
func TestCheckpointJSONRoundTripEmptyPointers(t *testing.T) {
	cp := checkpoint{Phase: phaseAwaitingPostSquashCI, Branch: "b", Floor: math.MaxInt}
	blob, err := json.Marshal(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got checkpoint
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.LastVerdict != nil {
		t.Errorf("LastVerdict = %+v, want nil", got.LastVerdict)
	}
	if got.Analysis != nil {
		t.Errorf("Analysis = %+v, want nil", got.Analysis)
	}
}

// TestCheckpointBaseHeadSHA covers the orchestrator's resume-invalidation hook.
func TestCheckpointBaseHeadSHA(t *testing.T) {
	blob, _ := json.Marshal(checkpoint{BaseHeadSHA: "headsha-xyz"})
	if got, ok := CheckpointBaseHeadSHA(string(blob)); !ok || got != "headsha-xyz" {
		t.Errorf("CheckpointBaseHeadSHA(valid) = (%q,%v), want (headsha-xyz,true)", got, ok)
	}
	if got, ok := CheckpointBaseHeadSHA(""); ok || got != "" {
		t.Errorf("CheckpointBaseHeadSHA(empty) = (%q,%v), want (\"\",false)", got, ok)
	}
	if got, ok := CheckpointBaseHeadSHA("{not valid json"); ok || got != "" {
		t.Errorf("CheckpointBaseHeadSHA(garbage) = (%q,%v), want (\"\",false)", got, ok)
	}
}
