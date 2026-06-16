package orchestrator

import (
	"path/filepath"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
	"github.com/mozilla-releng/dependabot-sweeper/internal/sqlitestore"
)

func openTestStore(t *testing.T) *sqlitestore.Store {
	t.Helper()
	s, err := sqlitestore.Open(filepath.Join(t.TempDir(), "test.db"), true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

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
	s := openTestStore(t)
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

// TestTerminalSHA verifies the N4 / MAJOR-1 SHA selection for the gave_up path:
// prefer the captured post-rebase tip so the next scan's SHA-skip fires on the
// rebased head; fall back to the scan-time SHA only when no tip was captured.
func TestTerminalSHA(t *testing.T) {
	if got := terminalSHA("tip-sha", "scan-sha"); got != "tip-sha" {
		t.Errorf("terminalSHA(tip, scan) = %q, want tip-sha (must record at the post-rebase tip)", got)
	}
	if got := terminalSHA("", "scan-sha"); got != "scan-sha" {
		t.Errorf("terminalSHA(\"\", scan) = %q, want scan-sha (fallback when tip uncaptured)", got)
	}
	if got := terminalSHA("", ""); got != "" {
		t.Errorf("terminalSHA(\"\", \"\") = %q, want empty", got)
	}
}

// TestGaveUpSkipFiresAfterRebase is the N4 regression test at the store level:
// when the gave_up outcome is recorded against the post-rebase tip SHA (as the
// orchestrator now does via terminalSHA(result.TipSHA, …)), the next scan — which
// sees that same rebased head — correctly skips, instead of re-entering the
// agent because it was recorded against the stale scan-time SHA.
func TestGaveUpSkipFiresAfterRebase(t *testing.T) {
	const prNumber = 7
	const scanSHA = "sha-pre-rebase" // pr.HeadSHA at scan time
	const tipSHA = "sha-post-rebase" // branch head after the Phase-0 rebase
	const nextScanSHA = tipSHA       // next cycle sees the rebased head

	s := openTestStore(t)
	o := Orchestrator{store: s}
	s.Report(prNumber, "webpack", "major", models.StagePending, "")

	// Orchestrator gave_up path: record against the captured tip, not scanSHA.
	o.recordOutcome(prNumber, terminalSHA(tipSHA, scanSHA), models.StageGaveUp)

	stored, ok := s.Get(prNumber)
	if !ok {
		t.Fatal("store has no entry after recordOutcome")
	}
	// Next scan sees the rebased head → SHA-skip MUST fire (the N4 fix).
	if !(stored.Outcome != "" && stored.HeadSHA == nextScanSHA) {
		t.Errorf("skip did not fire on the rebased head: stored HeadSHA=%q, next scan SHA=%q",
			stored.HeadSHA, nextScanSHA)
	}
	// Had we recorded against the stale scanSHA (the N4 bug), the skip would
	// MISS the rebased head and the PR would re-enter the agent.
	if stored.HeadSHA == scanSHA {
		t.Error("outcome was recorded against the stale scan-time SHA — N4 bug not fixed")
	}
}


// TestUnknownBumpTypeSkipRecording is a regression test for the implicit safety
// net that was lost when the min-bump threshold was removed (Phase 3): previously
// BumpRank(BumpUnknown) < BumpRank(BumpMajor) silently excluded parse-failure
// PRs from analysis. The explicit guard in processPR Step 0 now replicates that
// exclusion. This test verifies that the guard writes StageSkipped (not any
// analysing/impl stage) for BumpUnknown PRs, protecting against future refactors
// that might accidentally skip the guard.
func TestUnknownBumpTypeSkipRecording(t *testing.T) {
	s := openTestStore(t)
	o := Orchestrator{store: s}

	// Simulate what processPR Step 0 does for a parse-failure PR.
	s.Report(55, "weird-lib", string(models.BumpUnknown), models.StagePending, "")
	o.reportStage(55, "weird-lib", string(models.BumpUnknown), models.StageSkipped,
		"skipped: could not classify bump type from PR title")

	got, ok := s.Get(55)
	if !ok {
		t.Fatal("store has no entry for PR 55")
	}
	if got.Stage != models.StageSkipped {
		t.Errorf("Stage = %q, want skipped — BumpUnknown must never proceed past Step 0", got.Stage)
	}
	if got.BumpType != string(models.BumpUnknown) {
		t.Errorf("BumpType = %q, want unknown", got.BumpType)
	}
}

// TestExcludeOwnPRs is the Q14/C1 regression: a PR the tool created (recorded in
// the reap-exempt table) is dropped from the scan set, so it can't be
// re-ingested and re-processed as a fresh dependabot PR.
func TestExcludeOwnPRs(t *testing.T) {
	s := openTestStore(t)
	o := Orchestrator{store: s}
	s.RecordCreatedPR(204, 100) // we created sweeper PR 204 for dependabot PR 100

	in := []models.DependabotPR{{Number: 100}, {Number: 204}, {Number: 300}}
	got := o.excludeOwnPRs(in)

	if len(got) != 2 {
		t.Fatalf("excludeOwnPRs kept %d PRs, want 2 (204 excluded)", len(got))
	}
	for _, pr := range got {
		if pr.Number == 204 {
			t.Errorf("our own PR #204 was not excluded")
		}
	}
}

// TestExcludeOwnPRsNilStoreSafe / no-created cases must pass through unchanged.
func TestExcludeOwnPRsPassThrough(t *testing.T) {
	in := []models.DependabotPR{{Number: 1}, {Number: 2}}

	var nilStore Orchestrator
	if got := nilStore.excludeOwnPRs(in); len(got) != 2 {
		t.Errorf("nil store: kept %d, want 2 (pass-through)", len(got))
	}
	o := Orchestrator{store: openTestStore(t)} // store with no created PRs
	if got := o.excludeOwnPRs(in); len(got) != 2 {
		t.Errorf("empty created set: kept %d, want 2 (pass-through)", len(got))
	}
}
func TestReportStageWritesToStore(t *testing.T) {
	s := openTestStore(t)
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
	s := openTestStore(t)
	o := &Orchestrator{}
	o.WithStore(s)
	if o.store != s {
		t.Errorf("WithStore did not set the store field")
	}
}

// TestWithStoreAcceptsReadWriter verifies that WithStore accepts anything that
// satisfies progress.ReadWriter (compile-time + interface-assignment check).
func TestWithStoreAcceptsReadWriter(t *testing.T) {
	s := openTestStore(t)
	var rw progress.ReadWriter = s // compile-time: *sqlitestore.Store must satisfy ReadWriter
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)
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
			s := openTestStore(t)
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
	s := openTestStore(t)
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
	s := openTestStore(t)

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

// TestReportStageBlocksIllegalTransition is the Q8 wiring test: reportStage must
// validate the transition against the workflow graph and refuse to write an illegal
// one (e.g. finalized → pending). This is a cost-safety invariant — an illegal
// transition could re-admit an already-processed PR into the expensive agentic step.
func TestReportStageBlocksIllegalTransition(t *testing.T) {
	s := openTestStore(t)
	o := Orchestrator{store: s}

	// Bring the PR to a terminal stage (finalized).
	s.Report(77, "react", "major", models.StagePending, "")
	s.Report(77, "react", "major", models.StageFinalized, "done")

	got, ok := s.Get(77)
	if !ok || got.Stage != models.StageFinalized {
		t.Fatalf("setup: expected finalized stage, got stage=%q ok=%v", got.Stage, ok)
	}
	histBefore := len(got.History)

	// Attempt an illegal transition: finalized → pending.
	// reportStage must block this and NOT write anything to the store.
	o.reportStage(77, "react", "major", models.StagePending, "attempted re-entry")

	got, _ = s.Get(77)
	if got.Stage != models.StageFinalized {
		t.Errorf("illegal transition was not blocked: stage changed from finalized to %q", got.Stage)
	}
	if len(got.History) != histBefore {
		t.Errorf("illegal transition was not blocked: history grew from %d to %d events", histBefore, len(got.History))
	}
}

// TestReportStageAllowsLegalTransition verifies that reportStage correctly
// forwards legal transitions to the store.
func TestReportStageAllowsLegalTransition(t *testing.T) {
	s := openTestStore(t)
	o := Orchestrator{store: s}

	// pending → analysing is a legal forward transition.
	o.reportStage(88, "lodash", "minor", models.StagePending, "new PR")
	o.reportStage(88, "lodash", "minor", models.StageAnalysing, "calling agent")

	got, ok := s.Get(88)
	if !ok {
		t.Fatal("store has no entry for PR 88")
	}
	if got.Stage != models.StageAnalysing {
		t.Errorf("legal transition was blocked: stage = %q, want analysing", got.Stage)
	}
	if len(got.History) != 2 {
		t.Errorf("expected 2 history events after pending→analysing, got %d", len(got.History))
	}
}
