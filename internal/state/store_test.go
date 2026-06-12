package state

import (
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func timeAfter() <-chan time.Time { return time.After(2 * time.Second) }

func TestReportCreatesAndAppendsHistory(t *testing.T) {
	s := NewStore()
	s.Report(42, "lodash", "minor", models.StagePending, "")
	s.Report(42, "lodash", "minor", models.StageAnalysing, "calling claude")

	got, ok := s.Get(42)
	if !ok {
		t.Fatalf("Get(42) not found")
	}
	if got.Stage != models.StageAnalysing {
		t.Errorf("Stage = %q, want analysing", got.Stage)
	}
	if got.PackageName != "lodash" || got.BumpType != "minor" {
		t.Errorf("metadata not retained: %+v", got)
	}
	if len(got.History) != 2 {
		t.Fatalf("History len = %d, want 2", len(got.History))
	}
	if got.History[0].Stage != models.StagePending || got.History[1].Stage != models.StageAnalysing {
		t.Errorf("history order wrong: %+v", got.History)
	}
	if got.History[1].Detail != "calling claude" {
		t.Errorf("detail not recorded: %q", got.History[1].Detail)
	}
}

func TestGetReturnsCopyNotAlias(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "major", models.StagePending, "")
	got, _ := s.Get(1)
	got.History[0].Detail = "mutated by caller"
	got.Stage = models.StageError

	again, _ := s.Get(1)
	if again.History[0].Detail == "mutated by caller" {
		t.Errorf("Get returned an aliased History slice; caller mutation leaked into the store")
	}
	if again.Stage == models.StageError {
		t.Errorf("Get returned a value that aliased internal state")
	}
}

func TestGetReturnsCopyNotAliasCI(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "major", models.StagePending, "")
	conc := "success"
	s.SetCI(1, models.CIStatus{
		Checks: []models.CheckDetail{{Name: "lint", Conclusion: &conc}},
	})

	got, _ := s.Get(1)
	if got.CI == nil || len(got.CI.Checks) == 0 {
		t.Fatalf("CI not populated after SetCI")
	}
	// Mutate the returned copy.
	got.CI.Checks[0].Name = "mutated"
	newConc := "failure"
	got.CI.Checks[0].Conclusion = &newConc

	again, _ := s.Get(1)
	if again.CI.Checks[0].Name == "mutated" {
		t.Errorf("CI.Checks slice aliased: caller mutation leaked into the store")
	}
	if again.CI.Checks[0].Conclusion != nil && *again.CI.Checks[0].Conclusion == "failure" {
		t.Errorf("CI.Checks[0].Conclusion pointer aliased: caller mutation leaked into the store")
	}
}

func TestGetReturnsCopyNotAliasAnalysis(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "major", models.StagePending, "")
	s.SetAnalysis(1, models.AgentAnalysis{
		BreakingChanges: []string{"drop node 14"},
		Recommendation:  models.RecommendNeedsChanges,
	})

	got, _ := s.Get(1)
	if got.Analysis == nil {
		t.Fatalf("Analysis not populated after SetAnalysis")
	}
	got.Analysis.BreakingChanges[0] = "mutated"

	again, _ := s.Get(1)
	if again.Analysis.BreakingChanges[0] == "mutated" {
		t.Errorf("Analysis.BreakingChanges slice aliased: caller mutation leaked into the store")
	}
}

func TestSetVersions(t *testing.T) {
	s := NewStore()
	s.Report(10, "lodash", "minor", models.StagePending, "")
	s.SetVersions(10, "4.17.20", "4.17.21", "npm")

	got, _ := s.Get(10)
	if got.OldVersion != "4.17.20" || got.NewVersion != "4.17.21" || got.Ecosystem != "npm" {
		t.Errorf("SetVersions not stored: %+v", got)
	}
}

func TestSetVersionsOnUnknownPRIsNoop(t *testing.T) {
	s := NewStore()
	s.SetVersions(999, "1.0", "2.0", "npm")
	if _, ok := s.Get(999); ok {
		t.Error("SetVersions on unknown PR should not create an entry")
	}
}

func TestSetCI(t *testing.T) {
	s := NewStore()
	s.Report(20, "pkg", "minor", models.StagePending, "")
	conc := "success"
	s.SetCI(20, models.CIStatus{
		State: "success", Total: 2, Passed: 2,
		Checks: []models.CheckDetail{{Name: "lint", Status: "completed", Conclusion: &conc}},
	})

	got, _ := s.Get(20)
	if got.CI == nil {
		t.Fatalf("CI nil after SetCI")
	}
	if got.CI.Total != 2 || got.CI.Passed != 2 {
		t.Errorf("CI aggregate wrong: %+v", got.CI)
	}
	if len(got.CI.Checks) != 1 || got.CI.Checks[0].Name != "lint" {
		t.Errorf("CI checks wrong: %+v", got.CI.Checks)
	}
}

func TestSetAnalysis(t *testing.T) {
	s := NewStore()
	s.Report(30, "pkg", "major", models.StagePending, "")
	s.SetAnalysis(30, models.AgentAnalysis{
		Recommendation:  models.RecommendNeedsChanges,
		Confidence:      models.ConfidenceHigh,
		BreakingChanges: []string{"removed API X"},
	})

	got, _ := s.Get(30)
	if got.Analysis == nil {
		t.Fatalf("Analysis nil after SetAnalysis")
	}
	if got.Analysis.Recommendation != models.RecommendNeedsChanges {
		t.Errorf("Recommendation = %q, want needs_changes", got.Analysis.Recommendation)
	}
	if len(got.Analysis.BreakingChanges) != 1 || got.Analysis.BreakingChanges[0] != "removed API X" {
		t.Errorf("BreakingChanges wrong: %+v", got.Analysis.BreakingChanges)
	}
}

func TestSetImplMetaAndReplacement(t *testing.T) {
	s := NewStore()
	s.Report(7, "pkg", "minor", models.StageImplStarting, "")
	s.SetImplMeta(7, "sess-uuid", "/tmp/sweeper-impl-xyz/repo", "auto/fix/pkg-2.0.0")
	n := 99
	s.SetReplacementPR(7, n)

	got, _ := s.Get(7)
	if got.SessionID != "sess-uuid" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.WorktreePath != "/tmp/sweeper-impl-xyz/repo" {
		t.Errorf("WorktreePath = %q", got.WorktreePath)
	}
	if got.ImplBranch != "auto/fix/pkg-2.0.0" {
		t.Errorf("ImplBranch = %q", got.ImplBranch)
	}
	if got.ReplacementPR == nil || *got.ReplacementPR != 99 {
		t.Errorf("ReplacementPR = %v, want 99", got.ReplacementPR)
	}
}

func TestSetImplMetaOnUnknownPRIsNoop(t *testing.T) {
	s := NewStore()
	s.SetImplMeta(123, "s", "w", "b")
	if _, ok := s.Get(123); ok {
		t.Errorf("SetImplMeta on unknown PR should not create an entry")
	}
}

func TestAllReturnsSortedByPRNumber(t *testing.T) {
	s := NewStore()
	s.Report(3, "c", "minor", models.StagePending, "")
	s.Report(1, "a", "minor", models.StagePending, "")
	s.Report(2, "b", "minor", models.StagePending, "")
	all := s.All()
	if len(all) != 3 {
		t.Fatalf("All len = %d, want 3", len(all))
	}
	if all[0].PRNumber != 1 || all[1].PRNumber != 2 || all[2].PRNumber != 3 {
		t.Errorf("All not sorted ascending by PR number: %d,%d,%d", all[0].PRNumber, all[1].PRNumber, all[2].PRNumber)
	}
}

func TestSubscribeReceivesTickOnReport(t *testing.T) {
	s := NewStore()
	ch := s.Subscribe()
	s.Report(5, "pkg", "minor", models.StagePending, "")
	select {
	case <-ch:
		// got the broadcast
	default:
		t.Errorf("subscriber did not receive a tick after Report")
	}
}

func TestReportDoesNotBlockOnFullSubscriber(t *testing.T) {
	s := NewStore()
	_ = s.Subscribe() // never drained
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			s.Report(1, "pkg", "minor", models.StageAnalysing, "")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter():
		t.Fatalf("Report blocked on a full subscriber channel")
	}
}

func TestReapRemovesClosedPRs(t *testing.T) {
	s := NewStore()
	for _, n := range []int{1, 2, 3} {
		s.Report(n, "pkg", "minor", models.StagePending, "")
	}

	s.Reap([]int{1, 3})

	if _, ok := s.Get(2); ok {
		t.Error("Get(2) returned true after Reap; want false")
	}
	all := s.All()
	if len(all) != 2 {
		t.Fatalf("All len = %d, want 2", len(all))
	}
	for _, p := range all {
		if p.PRNumber != 1 && p.PRNumber != 3 {
			t.Errorf("unexpected PR #%d after Reap", p.PRNumber)
		}
	}
}

func TestReapEmptyListClearsAll(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StagePending, "")

	s.Reap(nil)

	if len(s.All()) != 0 {
		t.Error("All not empty after Reap(nil)")
	}
}

func TestReapNoOpWhenAllOpen(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StageAnalysing, "")

	s.Reap([]int{1, 2})

	if len(s.All()) != 2 {
		t.Errorf("All len != 2 after no-op Reap")
	}
}

func TestReapBroadcastsOnChange(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StagePending, "")

	ch := s.Subscribe()
	defer s.Unsubscribe(ch)
	// drain any buffered tick from Report
	select {
	case <-ch:
	default:
	}

	s.Reap([]int{1}) // removes PR 2 → should broadcast

	select {
	case <-ch:
		// received expected broadcast
	case <-timeAfter():
		t.Error("no broadcast received after Reap removed a PR")
	}
}

func TestReapNoBroadcastWhenNothingRemoved(t *testing.T) {
	s := NewStore()
	s.Report(1, "pkg", "minor", models.StagePending, "")

	ch := s.Subscribe()
	defer s.Unsubscribe(ch)
	select {
	case <-ch:
	default:
	}

	s.Reap([]int{1}) // nothing removed — no broadcast

	select {
	case <-ch:
		t.Error("unexpected broadcast when Reap removed nothing")
	case <-time.After(50 * time.Millisecond):
		// correct: no tick
	}
}

func TestUnsubscribeStopsDelivery(t *testing.T) {
	s := NewStore()
	ch := s.Subscribe()
	s.Unsubscribe(ch)
	s.Report(1, "pkg", "minor", models.StagePending, "")
	select {
	case _, open := <-ch:
		if open {
			t.Errorf("received a tick after Unsubscribe")
		}
	default:
		// no delivery — acceptable
	}
}
