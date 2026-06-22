package workflow_test

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/workflow"
)

// TestTransitionGuardLegalForwardPaths verifies the guard allows known-good
// forward transitions in the post-Q10 state machine.
func TestTransitionGuardLegalForwardPaths(t *testing.T) {
	legal := [][2]models.PRStage{
		// Normal forward path
		{models.StagePending, models.StageSettling},
		{models.StagePending, models.StageAnalysing},
		{models.StagePending, models.StageSkipped},

		// Agent running to outcomes
		{models.StageAnalysing, models.StageApproved},
		{models.StageAnalysing, models.StageFlagged},
		{models.StageAnalysing, models.StageGaveUp},
		{models.StageAnalysing, models.StageImplStarting},
		{models.StageAnalysing, models.StageError},
		// finalized via replacement-already-exists idempotency path
		{models.StageAnalysing, models.StageFinalized},

		// Impl pipeline
		{models.StageImplStarting, models.StageImplRunning},
		{models.StageImplRunning, models.StageWaitingCI},
		{models.StageWaitingCI, models.StageWaitingCI}, // self-loop (poll)
		{models.StageWaitingCI, models.StageResuming},
		{models.StageWaitingCI, models.StageGaveUp},
		{models.StageWaitingCI, models.StageReviewing},

		// Review gate
		{models.StageReviewing, models.StageFinalized},
		{models.StageReviewing, models.StageResuming},
		{models.StageReviewing, models.StageFlagged},

		// Resume loop back edges (N2: both decision AND back edges must be collapsed)
		{models.StageResuming, models.StageWaitingCI},
		{models.StageResuming, models.StageGaveUp},
		{models.StageResuming, models.StageReviewing},
	}

	for _, pair := range legal {
		from, to := pair[0], pair[1]
		if err := workflow.ValidateTransition(from, to); err != nil {
			t.Errorf("expected transition %q -> %q to be legal, got error: %v", from, to, err)
		}
	}
}

// TestTransitionGuardIllegalTransitions verifies the guard rejects known-bad
// transitions. The most critical is any terminal -> active, which would
// re-enter the agentic pipeline.
func TestTransitionGuardIllegalTransitions(t *testing.T) {
	illegal := [][2]models.PRStage{
		// Terminal stages must not re-enter the pipeline (cost-safety)
		{models.StageFinalized, models.StagePending},
		{models.StageFinalized, models.StageAnalysing},
		{models.StageApproved, models.StageAnalysing},
		{models.StageApproved, models.StagePending},
		{models.StageGaveUp, models.StageAnalysing},
		{models.StageGaveUp, models.StagePending},
		{models.StageFlagged, models.StageAnalysing},
		{models.StageFlagged, models.StagePending},
		{models.StageSkipped, models.StageAnalysing},
		{models.StageError, models.StageAnalysing},

		// No backwards impl-pipeline jumps
		{models.StageReviewing, models.StageImplStarting},
		{models.StageWaitingCI, models.StageImplStarting},
	}

	for _, pair := range illegal {
		from, to := pair[0], pair[1]
		if err := workflow.ValidateTransition(from, to); err == nil {
			t.Errorf("expected transition %q -> %q to be illegal, but ValidateTransition returned nil", from, to)
		}
	}
}

// TestTransitionGuardInitialPopulation verifies that a zero from-stage
// (initial row creation) is always allowed.
func TestTransitionGuardInitialPopulation(t *testing.T) {
	if err := workflow.ValidateTransition("", models.StagePending); err != nil {
		t.Errorf("initial population should be allowed: %v", err)
	}
}

// TestTransitionGuardSelfLoop verifies that self-loops are permitted
// (e.g. waiting_ci -> waiting_ci during polling).
func TestTransitionGuardSelfLoop(t *testing.T) {
	stages := []models.PRStage{
		models.StageWaitingCI,
		models.StageResuming,
		models.StageImplRunning,
	}
	for _, s := range stages {
		if err := workflow.ValidateTransition(s, s); err != nil {
			t.Errorf("self-loop on %q should be allowed: %v", s, err)
		}
	}
}

// TestTransitionGuardResumptionLoopRoundTrip tests the full impl resume loop
// round-trip: impl_running -> waiting_ci -> impl_resuming -> waiting_ci.
// This is the critical path for N2 (review C2): back edges must be followed
// when building the allowed-transitions set.
func TestTransitionGuardResumptionLoopRoundTrip(t *testing.T) {
	path := []models.PRStage{
		models.StageImplRunning,
		models.StageWaitingCI,
		models.StageResuming,
		models.StageWaitingCI, // back into CI gate
		models.StageResuming,  // again (second fix iteration)
		models.StageWaitingCI,
		models.StageReviewing,
		models.StageResuming, // review-fix back to impl
		models.StageWaitingCI,
		models.StageReviewing,
		models.StageFinalized,
	}

	for i := 0; i < len(path)-1; i++ {
		from, to := path[i], path[i+1]
		if err := workflow.ValidateTransition(from, to); err != nil {
			t.Errorf("round-trip step %d: transition %q -> %q should be legal: %v", i, from, to, err)
		}
	}
}
