package sqlitestore

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// openWriter returns a writer-mode Store backed by a temp file.
func openWriter(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, true)
	if err != nil {
		t.Fatalf("Open writer: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// openAt opens a fresh Store handle on an existing DB file.
func openAt(t *testing.T, path string, writer bool) *Store {
	t.Helper()
	s, err := Open(path, writer)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReportCreatesAndAppendsHistory(t *testing.T) {
	s := openWriter(t)
	s.Report(42, "lodash", "minor", models.StagePending, "")
	s.Report(42, "lodash", "minor", models.StageAnalysing, "calling claude")

	got, ok := s.Get(42)
	if !ok {
		t.Fatalf("Get(42) not found")
	}
	if got.Stage != models.StageAnalysing {
		t.Errorf("Stage = %q, want %q", got.Stage, models.StageAnalysing)
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

func TestReportPreservesPackageMetadataOnUpdate(t *testing.T) {
	s := openWriter(t)
	s.Report(1, "react", "major", models.StagePending, "")
	// Later call with empty pkg/bump must NOT wipe the first-written metadata.
	s.Report(1, "", "", models.StageAnalysing, "")
	got, _ := s.Get(1)
	if got.PackageName != "react" {
		t.Errorf("PackageName = %q, want react (first-write-wins)", got.PackageName)
	}
	if got.BumpType != "major" {
		t.Errorf("BumpType = %q, want major (first-write-wins)", got.BumpType)
	}
}

func TestGetReturnsCopyNotAlias(t *testing.T) {
	s := openWriter(t)
	s.Report(1, "pkg", "major", models.StagePending, "")
	got, _ := s.Get(1)
	got.History[0].Detail = "mutated by caller"
	got.Stage = models.StageError
	_ = got.Stage // mutation under test; verify isolation via again below

	again, _ := s.Get(1)
	if again.History[0].Detail == "mutated by caller" {
		t.Errorf("Get returned aliased History; caller mutation leaked into the store")
	}
	if again.Stage == models.StageError {
		t.Errorf("Get returned a value aliasing internal state")
	}
}

func TestGetNotFound(t *testing.T) {
	s := openWriter(t)
	_, ok := s.Get(999)
	if ok {
		t.Errorf("Get on missing PR should return false")
	}
}

func TestSetImplMetaAndReplacement(t *testing.T) {
	s := openWriter(t)
	s.Report(7, "pkg", "minor", models.StageImplStarting, "")
	s.SetImplMeta(7, "sess-uuid", "/tmp/sweeper-impl-xyz/repo", "auto/fix/pkg-2.0.0")
	s.SetReplacementPR(7, 99)

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
	s := openWriter(t)
	s.SetImplMeta(123, "s", "w", "b") // must not create a row
	_, ok := s.Get(123)
	if ok {
		t.Errorf("SetImplMeta on unknown PR must not create an entry")
	}
}

func TestSetReplacementPROnUnknownPRIsNoop(t *testing.T) {
	s := openWriter(t)
	s.SetReplacementPR(999, 1) // must not create a row
	_, ok := s.Get(999)
	if ok {
		t.Errorf("SetReplacementPR on unknown PR must not create an entry")
	}
}

func TestAllReturnsSortedByPRNumber(t *testing.T) {
	s := openWriter(t)
	s.Report(3, "c", "minor", models.StagePending, "")
	s.Report(1, "a", "minor", models.StagePending, "")
	s.Report(2, "b", "minor", models.StagePending, "")
	all := s.All()
	if len(all) != 3 {
		t.Fatalf("All len = %d, want 3", len(all))
	}
	if all[0].PRNumber != 1 || all[1].PRNumber != 2 || all[2].PRNumber != 3 {
		t.Errorf("All not sorted: %d, %d, %d", all[0].PRNumber, all[1].PRNumber, all[2].PRNumber)
	}
}

func TestAllIncludesHistory(t *testing.T) {
	s := openWriter(t)
	s.Report(5, "pkg", "patch", models.StagePending, "start")
	s.Report(5, "pkg", "patch", models.StageAnalysing, "analysing")
	all := s.All()
	if len(all) == 0 {
		t.Fatal("All is empty")
	}
	if len(all[0].History) != 2 {
		t.Errorf("History len = %d, want 2", len(all[0].History))
	}
}

// — durability: data survives a close + reopen —

func TestDurabilityAfterReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "durable.db")

	s1 := openAt(t, path, true)
	s1.Report(10, "vue", "major", models.StagePending, "first")
	s1.Report(10, "vue", "major", models.StageAnalysing, "second")
	s1.SetImplMeta(10, "sid", "/worktree", "branch")
	s1.SetReplacementPR(10, 42)
	s1.Close()

	s2 := openAt(t, path, false)
	got, ok := s2.Get(10)
	if !ok {
		t.Fatalf("Get(10) not found after reopen")
	}
	if got.PackageName != "vue" {
		t.Errorf("PackageName = %q, want vue", got.PackageName)
	}
	if len(got.History) != 2 {
		t.Errorf("History len = %d, want 2", len(got.History))
	}
	if got.SessionID != "sid" {
		t.Errorf("SessionID = %q, want sid", got.SessionID)
	}
	if got.ReplacementPR == nil || *got.ReplacementPR != 42 {
		t.Errorf("ReplacementPR = %v, want 42", got.ReplacementPR)
	}
}

// — concurrency: SetMaxOpenConns(1) serialises ~20 concurrent goroutines —

func TestConcurrentWritesSerialized(t *testing.T) {
	s := openWriter(t)
	const goroutines = 20
	const reportsEach = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			prNum := g + 1
			for i := range reportsEach {
				stage := models.StagePending
				if i%2 == 1 {
					stage = models.StageAnalysing
				}
				s.Report(prNum, fmt.Sprintf("pkg-%d", g), "minor", stage, fmt.Sprintf("step %d", i))
			}
		}()
	}
	wg.Wait()

	all := s.All()
	if len(all) != goroutines {
		t.Errorf("All len = %d, want %d", len(all), goroutines)
	}
	for _, p := range all {
		if len(p.History) != reportsEach {
			t.Errorf("PR #%d history len = %d, want %d", p.PRNumber, len(p.History), reportsEach)
		}
	}
}

// — Reap removes stale rows and cascades to stage_events —

func TestReapRemovesClosedPRs(t *testing.T) {
	s := openWriter(t)
	// Seed three PRs with at least one stage_event each.
	for _, n := range []int{1, 2, 3} {
		s.Report(n, fmt.Sprintf("pkg%d", n), "minor", models.StagePending, "")
	}

	// Reap keeping only 1 and 3.
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
			t.Errorf("unexpected PR #%d in All after Reap", p.PRNumber)
		}
	}
	// stage_events for PR 2 must have been cascade-deleted.
	var evCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM stage_events WHERE pr_number = 2`).Scan(&evCount); err != nil {
		t.Fatalf("query stage_events: %v", err)
	}
	if evCount != 0 {
		t.Errorf("stage_events for reaped PR 2: got %d rows, want 0", evCount)
	}
}

// TestCreatedPRsSurviveReap is the cost-safety regression (Q14 / review C1): the
// reap-exempt created_prs record must persist across a Reap that prunes
// pr_progress. Otherwise the tool would re-ingest its own replacement PR as a
// fresh dependabot PR and re-enter the expensive agentic step.
func TestCreatedPRsSurviveReap(t *testing.T) {
	s := openWriter(t)
	// PR 100 is the dependabot PR; PR 204 is the sweeper PR we created for it.
	s.Report(100, "lodash", "major", models.StagePending, "")
	s.RecordCreatedPR(204, 100)

	// Reap with an open set that excludes both — pr_progress for 100 is pruned,
	// but the created_prs record must remain.
	s.Reap([]int{999})

	if _, ok := s.Get(100); ok {
		t.Error("expected PR 100's pr_progress row to be reaped (sanity: Reap ran)")
	}
	created := s.CreatedPRs()
	if origin, ok := created[204]; !ok || origin != 100 {
		t.Errorf("created_prs lost after Reap: got %v, want {204:100}", created)
	}
}

func TestRecordCreatedPRRoundtrip(t *testing.T) {
	s := openWriter(t)
	s.RecordCreatedPR(204, 100)
	s.RecordCreatedPR(205, 101)
	s.RecordCreatedPR(204, 100) // idempotent re-record
	created := s.CreatedPRs()
	if len(created) != 2 || created[204] != 100 || created[205] != 101 {
		t.Errorf("CreatedPRs = %v, want {204:100, 205:101}", created)
	}
}

// Reap with an empty/nil open set must NOT wipe the table: an empty set can be
// a spurious API result, and destroying the idempotency history would force
// re-processing of every PR. Rows are retained and self-heal on the next reap
// that sees at least one open PR.
func TestReapEmptyListRetainsRows(t *testing.T) {
	s := openWriter(t)
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StagePending, "")

	s.Reap(nil)

	if len(s.All()) != 2 {
		t.Errorf("Reap(nil) wiped rows: got %d, want 2 retained", len(s.All()))
	}
}

func TestReapNoOpWhenAllOpen(t *testing.T) {
	s := openWriter(t)
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StageAnalysing, "")

	s.Reap([]int{1, 2})

	all := s.All()
	if len(all) != 2 {
		t.Errorf("All len = %d after no-op Reap, want 2", len(all))
	}
}

// — v2 setters: versions, CI, analysis —

func TestSetVersionsRoundTrip(t *testing.T) {
	s := openWriter(t)
	s.Report(10, "lodash", "minor", models.StagePending, "")
	s.SetVersions(10, "4.17.20", "4.17.21", "npm")

	got, _ := s.Get(10)
	if got.OldVersion != "4.17.20" || got.NewVersion != "4.17.21" || got.Ecosystem != "npm" {
		t.Errorf("SetVersions not persisted: old=%q new=%q eco=%q", got.OldVersion, got.NewVersion, got.Ecosystem)
	}
}

func TestSetVersionsOnUnknownPRIsNoop(t *testing.T) {
	s := openWriter(t)
	s.SetVersions(999, "1.0", "2.0", "npm")
	if _, ok := s.Get(999); ok {
		t.Error("SetVersions on unknown PR must not create a row")
	}
}

func TestSetCIRoundTrip(t *testing.T) {
	s := openWriter(t)
	s.Report(20, "react", "major", models.StagePending, "")
	conc := "success"
	s.SetCI(20, models.CIStatus{
		State: "success", Total: 3, Passed: 2, Failed: 1, Pending: 0,
		Checks: []models.CheckDetail{
			{Name: "lint", Status: "completed", Conclusion: &conc, DetailsURL: "https://ci/1"},
			{Name: "test", Status: "completed"},
		},
	})

	got, _ := s.Get(20)
	if got.CI == nil {
		t.Fatalf("CI nil after SetCI")
	}
	if got.CI.State != "success" || got.CI.Total != 3 || got.CI.Passed != 2 {
		t.Errorf("CI aggregate wrong: %+v", got.CI)
	}
	if len(got.CI.Checks) != 2 {
		t.Fatalf("CI.Checks len = %d, want 2", len(got.CI.Checks))
	}
	if got.CI.Checks[0].Name != "lint" || got.CI.Checks[0].DetailsURL != "https://ci/1" {
		t.Errorf("CI.Checks[0] wrong: %+v", got.CI.Checks[0])
	}
	if got.CI.Checks[0].Conclusion == nil || *got.CI.Checks[0].Conclusion != "success" {
		t.Errorf("CI.Checks[0].Conclusion wrong: %v", got.CI.Checks[0].Conclusion)
	}
}

func TestSetCIReplacesChecksWholesale(t *testing.T) {
	s := openWriter(t)
	s.Report(21, "vue", "minor", models.StagePending, "")

	// First SetCI: 2 checks.
	c1 := "success"
	s.SetCI(21, models.CIStatus{
		Total: 2, Checks: []models.CheckDetail{
			{Name: "a", Conclusion: &c1},
			{Name: "b", Conclusion: &c1},
		},
	})

	// Second SetCI: only 1 check — old rows must be gone.
	s.SetCI(21, models.CIStatus{
		Total: 1, Checks: []models.CheckDetail{{Name: "a", Conclusion: &c1}},
	})

	var cnt int
	if err := s.db.QueryRow(`SELECT count(*) FROM ci_checks WHERE pr_number=21`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("ci_checks after replacement: got %d rows, want 1", cnt)
	}
}

func TestSetAnalysisRoundTrip(t *testing.T) {
	s := openWriter(t)
	s.Report(30, "pkg", "major", models.StagePending, "")
	s.SetAnalysis(30, models.AgentAnalysis{
		Recommendation:  models.RecommendNeedsChanges,
		Confidence:      models.ConfidenceHigh,
		BreakingChanges: []string{"removed API X", "changed default Y"},
		CodebaseImpact:  []models.CodeImpact{{File: "src/foo.ts", Affected: true}},
	})

	got, _ := s.Get(30)
	if got.Analysis == nil {
		t.Fatalf("Analysis nil after SetAnalysis")
	}
	if got.Analysis.Recommendation != models.RecommendNeedsChanges {
		t.Errorf("Recommendation = %q, want needs_changes", got.Analysis.Recommendation)
	}
	if len(got.Analysis.BreakingChanges) != 2 || got.Analysis.BreakingChanges[0] != "removed API X" {
		t.Errorf("BreakingChanges wrong: %+v", got.Analysis.BreakingChanges)
	}
	if len(got.Analysis.CodebaseImpact) != 1 || got.Analysis.CodebaseImpact[0].File != "src/foo.ts" {
		t.Errorf("CodebaseImpact wrong: %+v", got.Analysis.CodebaseImpact)
	}
}

// — schema version stamped correctly on fresh open —

func TestSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "test.db"), true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("user_version = %d, want %d", version, currentSchemaVersion)
	}
}

// — SetOutcome round-trip —

func TestSetOutcomeRoundTrip(t *testing.T) {
	s := openWriter(t)
	s.Report(42, "react", "major", models.StagePending, "")
	s.SetOutcome(42, "sha-abc", string(models.StageApproved))

	got, ok := s.Get(42)
	if !ok {
		t.Fatal("Get(42) not found")
	}
	if got.HeadSHA != "sha-abc" {
		t.Errorf("HeadSHA = %q, want sha-abc", got.HeadSHA)
	}
	if got.Outcome != string(models.StageApproved) {
		t.Errorf("Outcome = %q, want %q", got.Outcome, models.StageApproved)
	}

	// A subsequent SetOutcome for a new SHA must overwrite (head moves forward).
	s.SetOutcome(42, "sha-def", string(models.StageFlagged))
	got2, _ := s.Get(42)
	if got2.HeadSHA != "sha-def" || got2.Outcome != string(models.StageFlagged) {
		t.Errorf("after overwrite: HeadSHA=%q Outcome=%q, want sha-def/%s",
			got2.HeadSHA, got2.Outcome, models.StageFlagged)
	}
}

// SetOutcome on an unknown PR must not create a row.
func TestSetOutcomeNoopOnUnknownPR(t *testing.T) {
	s := openWriter(t)
	s.SetOutcome(999, "sha-xyz", string(models.StageApproved)) // no Report first

	_, ok := s.Get(999)
	if ok {
		t.Error("SetOutcome created a row for an unknown PR; expected no-op")
	}
}

// SetOutcome with an empty headSHA must be a no-op (retriable / transient stages).
func TestSetOutcomeNoopOnEmptySHA(t *testing.T) {
	s := openWriter(t)
	s.Report(7, "pkg", "minor", models.StagePending, "")
	s.SetOutcome(7, "", string(models.StageApproved)) // empty SHA — should be skipped

	got, ok := s.Get(7)
	if !ok {
		t.Fatal("Get(7) not found")
	}
	if got.HeadSHA != "" || got.Outcome != "" {
		t.Errorf("SetOutcome with empty SHA mutated the row: HeadSHA=%q Outcome=%q", got.HeadSHA, got.Outcome)
	}
}

// — reap cascades to ci_checks —

func TestReapCascadesToCIChecks(t *testing.T) {
	s := openWriter(t)
	s.Report(1, "pkg", "minor", models.StagePending, "")
	s.Report(2, "pkg", "minor", models.StagePending, "")

	conc := "success"
	s.SetCI(1, models.CIStatus{Checks: []models.CheckDetail{{Name: "lint", Conclusion: &conc}}})
	s.SetCI(2, models.CIStatus{Checks: []models.CheckDetail{{Name: "test", Conclusion: &conc}}})

	s.Reap([]int{2}) // remove PR 1

	var cnt int
	if err := s.db.QueryRow(`SELECT count(*) FROM ci_checks WHERE pr_number=1`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Errorf("ci_checks for reaped PR 1: got %d rows, want 0", cnt)
	}
	// PR 2's checks must remain.
	if err := s.db.QueryRow(`SELECT count(*) FROM ci_checks WHERE pr_number=2`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("ci_checks for kept PR 2: got %d rows, want 1", cnt)
	}
}

// — timestamp round-trip —

func TestTimestampRoundTrip(t *testing.T) {
	s := openWriter(t)
	before := time.Now().Truncate(time.Nanosecond)
	s.Report(1, "pkg", "minor", models.StagePending, "")
	after := time.Now().Truncate(time.Nanosecond)

	got, _ := s.Get(1)
	if got.LastUpdated.IsZero() {
		t.Error("LastUpdated is zero")
	}
	if got.LastUpdated.Before(before) || got.LastUpdated.After(after.Add(time.Millisecond)) {
		t.Errorf("LastUpdated %v out of expected range [%v, %v]", got.LastUpdated, before, after)
	}
	if len(got.History) > 0 && got.History[0].At.IsZero() {
		t.Error("StageEvent.At is zero")
	}
}
