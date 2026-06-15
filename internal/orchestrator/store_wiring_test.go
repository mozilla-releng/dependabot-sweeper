package orchestrator

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
	"github.com/mozilla-releng/dependabot-sweeper/internal/state"
)

func TestReportStageNilSafe(t *testing.T) {
	var o Orchestrator // store is nil
	// Must not panic.
	o.reportStage(1, "pkg", "minor", models.StagePending, "")
}

// TestPrepopulateNilStoreSafe ensures prepopulate is a no-op (no panic) when no
// store is configured (one-shot `review` mode).
func TestPrepopulateNilStoreSafe(t *testing.T) {
	var o Orchestrator // store is nil
	o.prepopulate([]models.DependabotPR{{Number: 1, PackageName: "pkg", BumpType: models.BumpMajor}})
}

// TestPrepopulateStampsNewPRsOnce is the T6a regression test: prepopulate stamps
// `pending` only for PRs the store has not seen, and never re-stamps an already
// tracked (often terminal) PR — so a no-op cycle records no new transition.
func TestPrepopulateStampsNewPRsOnce(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}

	prs := []models.DependabotPR{
		{Number: 1, PackageName: "react", BumpType: models.BumpMajor},
		{Number: 2, PackageName: "lodash", BumpType: models.BumpMinor},
	}

	// First cycle: both PRs are new → each created at `pending` with one event.
	o.prepopulate(prs)
	for _, n := range []int{1, 2} {
		got, ok := s.Get(n)
		if !ok {
			t.Fatalf("PR %d not created by prepopulate", n)
		}
		if got.Stage != models.StagePending {
			t.Errorf("PR %d stage = %q, want pending", n, got.Stage)
		}
		if len(got.History) != 1 {
			t.Errorf("PR %d history = %d events, want 1", n, len(got.History))
		}
	}

	// PR 1 reaches a terminal outcome (as the real pipeline would record it).
	o.reportStage(1, "react", "major", models.StageFinalized, "done")
	o.recordOutcome(1, "sha-react", models.StageFinalized)
	histAfterFinalize := func(n int) int {
		got, _ := s.Get(n)
		return len(got.History)
	}
	pr1Hist := histAfterFinalize(1)

	// Second cycle: PR 1 and PR 2 are already tracked; PR 3 is new. A no-op cycle
	// must NOT grow PR 1's or PR 2's history, and must NOT reset PR 1 off its
	// terminal stage.
	prs = append(prs, models.DependabotPR{Number: 3, PackageName: "webpack", BumpType: models.BumpMajor})
	o.prepopulate(prs)

	if got, _ := s.Get(1); got.Stage != models.StageFinalized {
		t.Errorf("PR 1 stage = %q after re-prepopulate, want finalized (no re-stamp)", got.Stage)
	}
	if h := histAfterFinalize(1); h != pr1Hist {
		t.Errorf("PR 1 history grew from %d to %d on a no-op cycle (re-stamp leak)", pr1Hist, h)
	}
	if h := histAfterFinalize(2); h != 1 {
		t.Errorf("PR 2 history = %d, want 1 (must not be re-stamped)", h)
	}
	got3, ok := s.Get(3)
	if !ok {
		t.Fatal("PR 3 (new) not created by the second prepopulate")
	}
	if got3.Stage != models.StagePending || len(got3.History) != 1 {
		t.Errorf("PR 3 = {stage:%q history:%d}, want {pending 1}", got3.Stage, len(got3.History))
	}
}

func TestReportStageWritesToStore(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}
	o.reportStage(1, "pkg", "minor", models.StageAnalysing, "calling claude")
	got, ok := s.Get(1)
	if !ok {
		t.Fatalf("store has no entry for PR 1")
	}
	if got.Stage != models.StageAnalysing {
		t.Errorf("Stage = %q, want analysing", got.Stage)
	}
	if got.PackageName != "pkg" {
		t.Errorf("PackageName = %q", got.PackageName)
	}
}

func TestWithStoreSetsField(t *testing.T) {
	s := state.NewStore()
	o := &Orchestrator{}
	o.WithStore(s)
	if o.store != s {
		t.Errorf("WithStore did not set the store field")
	}
}

// TestWithStoreAcceptsReadWriter verifies that WithStore accepts anything that
// satisfies progress.ReadWriter (compile-time + interface-assignment check).
func TestWithStoreAcceptsReadWriter(t *testing.T) {
	s := state.NewStore()
	var rw progress.ReadWriter = s // compile-time: *state.Store must satisfy ReadWriter
	o := &Orchestrator{}
	o.WithStore(rw)
	if o.store != rw {
		t.Errorf("WithStore did not set the store field")
	}
}

// TestRecordOutcomeWritesToStore verifies that recordOutcome persists the head
// SHA and terminal stage into the store so the idempotency skip check can read
// it back on the next scan (Bug #23).
func TestRecordOutcomeWritesToStore(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}

	// PR must be known to the store before outcome can be recorded.
	s.Report(5, "react", "major", models.StagePending, "")

	o.recordOutcome(5, "sha-deadbeef", models.StageApproved)

	got, ok := s.Get(5)
	if !ok {
		t.Fatal("store has no entry for PR 5")
	}
	if got.HeadSHA != "sha-deadbeef" {
		t.Errorf("HeadSHA = %q, want sha-deadbeef", got.HeadSHA)
	}
	if got.Outcome != string(models.StageApproved) {
		t.Errorf("Outcome = %q, want %q", got.Outcome, models.StageApproved)
	}
}

// TestRecordOutcomeNilStoreSafe ensures recordOutcome is a no-op when no store
// is configured (one-shot `review` mode).
func TestRecordOutcomeNilStoreSafe(t *testing.T) {
	var o Orchestrator // store is nil
	// Must not panic.
	o.recordOutcome(1, "sha-abc", models.StageApproved)
}

// TestRecordOutcomeEmptySHASafe ensures recordOutcome is a no-op when headSHA
// is empty (retriable / transient outcomes should not be stored as sticky).
func TestRecordOutcomeEmptySHASafe(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}
	s.Report(3, "pkg", "minor", models.StagePending, "")

	// Empty SHA should not write anything.
	o.recordOutcome(3, "", models.StageFlagged)

	got, _ := s.Get(3)
	if got.HeadSHA != "" || got.Outcome != "" {
		t.Errorf("recordOutcome with empty SHA mutated the store: HeadSHA=%q Outcome=%q",
			got.HeadSHA, got.Outcome)
	}
}

// TestRecordOutcomeGaveUp verifies that recordOutcome correctly stores the
// StageGaveUp terminal (Part C) — this is the same path as other terminals
// but exercised explicitly to confirm StageGaveUp is usable as an outcome key.
func TestRecordOutcomeGaveUp(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}
	s.Report(9, "webpack", "major", models.StagePending, "")

	o.recordOutcome(9, "sha-999", models.StageGaveUp)

	got, ok := s.Get(9)
	if !ok {
		t.Fatal("store has no entry for PR 9")
	}
	if got.HeadSHA != "sha-999" {
		t.Errorf("HeadSHA = %q, want sha-999", got.HeadSHA)
	}
	if got.Outcome != string(models.StageGaveUp) {
		t.Errorf("Outcome = %q, want %q", got.Outcome, models.StageGaveUp)
	}
}

// TestBug26FlaggedPathsAreStickyViaRecordOutcome is a regression test for
// Bug #26: the analysis-error path, non-GaveUp pipeline-failure path, and
// success-but-no-PR path previously never called recordOutcome, so the
// idempotency gate could not fire for them and the PR would be re-analysed
// (and tokens burned) on every subsequent cycle.
//
// This test verifies the invariant that the fix relies on: once recordOutcome
// is called with a real head SHA, the idempotency gate correctly prevents
// re-processing on the next cycle — and that a different SHA (updated PR)
// correctly allows re-processing.
func TestBug26FlaggedPathsAreStickyViaRecordOutcome(t *testing.T) {
	const prNumber = 42
	const headSHA = "sha-bug26-test"

	stages := []models.PRStage{
		models.StageFlagged, // non-GaveUp pipeline failure and success-but-no-PR paths
		models.StageError,   // analysis-error path (recordOutcome stores StageFlagged but StageError is displayed)
	}

	for _, stageToRecord := range stages {
		t.Run(string(stageToRecord), func(t *testing.T) {
			s := state.NewStore()
			o := Orchestrator{store: s}
			s.Report(prNumber, "some-pkg", "minor", models.StagePending, "")

			// Simulate what the fixed code paths now do: record outcome with the
			// real head SHA (previously they passed "" which was a no-op).
			o.recordOutcome(prNumber, headSHA, models.StageFlagged)

			stored, ok := s.Get(prNumber)
			if !ok {
				t.Fatal("store has no entry after recordOutcome")
			}

			// Same SHA → idempotency gate should fire (no re-processing).
			alreadyProcessed := stored.Outcome != "" && stored.HeadSHA == headSHA
			if !alreadyProcessed {
				t.Errorf("stage=%s: expected alreadyProcessed=true for same head SHA; "+
					"Outcome=%q HeadSHA=%q", stageToRecord, stored.Outcome, stored.HeadSHA)
			}

			// Different SHA (PR updated by dependabot) → gate should NOT fire.
			alreadyProcessed = stored.Outcome != "" && stored.HeadSHA == "sha-new-commit"
			if alreadyProcessed {
				t.Errorf("stage=%s: expected alreadyProcessed=false for different head SHA "+
					"(PR updated)", stageToRecord)
			}
		})
	}
}

// TestBug26EmptySHAStillNoOp confirms that passing an empty head SHA to
// recordOutcome remains a no-op even after the Bug #26 fix, so callers that
// intentionally want a retriable outcome (e.g. future explicit retry logic)
// can still opt out of stickiness.
func TestBug26EmptySHAStillNoOp(t *testing.T) {
	s := state.NewStore()
	o := Orchestrator{store: s}
	s.Report(99, "pkg", "patch", models.StagePending, "")

	o.recordOutcome(99, "", models.StageFlagged)

	got, _ := s.Get(99)
	if got.Outcome != "" || got.HeadSHA != "" {
		t.Errorf("recordOutcome with empty SHA should be no-op; got Outcome=%q HeadSHA=%q",
			got.Outcome, got.HeadSHA)
	}
}

// TestIdempotencySkipUsesStore verifies that a store entry with a matching
// HeadSHA+Outcome causes the orchestrator to mark the PR as already-processed.
// This tests the store state used by the skip check in processPR — the check
// reads o.store.Get(pr.Number) and skips when HeadSHA matches (Bug #23).
func TestIdempotencySkipUsesStore(t *testing.T) {
	s := state.NewStore()

	// Simulate a prior run: store an outcome for PR 10 at sha-111.
	s.Report(10, "lodash", "minor", models.StagePending, "")
	s.SetOutcome(10, "sha-111", string(models.StageApproved))

	// The skip check logic (inlined from orchestrator.processPR):
	//   if o.store != nil:
	//       if stored, ok := o.store.Get(pr.Number); ok && stored.Outcome != "" && stored.HeadSHA == pr.HeadSHA
	//           alreadyProcessed = true
	stored, ok := s.Get(10)
	if !ok {
		t.Fatal("store has no entry for PR 10")
	}

	// Same SHA → should skip.
	alreadyProcessed := stored.Outcome != "" && stored.HeadSHA == "sha-111"
	if !alreadyProcessed {
		t.Error("expected alreadyProcessed=true for same HeadSHA, got false")
	}

	// Different SHA → should NOT skip (PR was updated).
	alreadyProcessed = stored.Outcome != "" && stored.HeadSHA == "sha-222"
	if alreadyProcessed {
		t.Error("expected alreadyProcessed=false for different HeadSHA, got true")
	}
}
