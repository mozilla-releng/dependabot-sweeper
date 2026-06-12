package models

import "testing"

func TestPRStageConstants(t *testing.T) {
	cases := []struct {
		stage PRStage
		want  string
	}{
		{StagePending, "pending"},
		{StageAnalysing, "analysing"},
		{StageApproved, "approved"},
		{StageImplStarting, "impl_starting"},
		{StageImplRunning, "impl_running"},
		{StageWaitingCI, "waiting_ci"},
		{StageResuming, "impl_resuming"},
		{StageReviewing, "reviewing"},
		{StageFinalized, "finalized"},
		{StageFlagged, "flagged_human"},
		{StageGaveUp, "gave_up"},
		{StageSkipped, "skipped"},
		{StageSettling, "ci_settling"},
		{StageError, "error"},
	}
	for _, c := range cases {
		if string(c.stage) != c.want {
			t.Errorf("stage = %q, want %q", string(c.stage), c.want)
		}
	}
}

func TestPRProgressZeroValue(t *testing.T) {
	var p PRProgress
	if p.History != nil {
		t.Errorf("zero-value History should be nil, got %v", p.History)
	}
	if p.ReplacementPR != nil {
		t.Errorf("zero-value ReplacementPR should be nil")
	}
}
