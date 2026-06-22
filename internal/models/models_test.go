package models

import (
	"sort"
	"testing"
	"time"
)

func TestSettled(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	stale := 12 * time.Hour
	done := func(concl string) CheckDetail {
		c := concl
		return CheckDetail{Status: "completed", Conclusion: &c, CreatedAt: now.Add(-time.Hour)}
	}
	pending := func(age time.Duration) CheckDetail {
		return CheckDetail{Status: "in_progress", CreatedAt: now.Add(-age)}
	}
	pendingNamed := func(name string, age time.Duration) CheckDetail {
		return CheckDetail{Name: name, Status: "in_progress", CreatedAt: now.Add(-age)}
	}
	cases := []struct {
		name    string
		checks  []CheckDetail
		ignored map[string]bool
		want    bool
	}{
		{"all terminal", []CheckDetail{done("success"), done("failure")}, nil, true},
		{"pending not stale", []CheckDetail{done("success"), pending(time.Hour)}, nil, false},
		{"pending stale", []CheckDetail{done("success"), pending(13 * time.Hour)}, nil, true},
		{"early red while pending", []CheckDetail{done("failure"), pending(time.Hour)}, nil, false}, // Bug #18 pin
		{"empty", nil, nil, true},
		// An ignored check we won't gate on shouldn't delay settledness either (Bug #21 follow-up).
		{"ignored pending does not block", []CheckDetail{done("success"), pendingNamed("slow-irrelevant", time.Hour)}, map[string]bool{"slow-irrelevant": true}, true},
		{"non-ignored pending still blocks", []CheckDetail{pendingNamed("relevant", time.Hour)}, map[string]bool{"other": true}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := Settled(c.checks, now, stale, c.ignored)
			if got != c.want {
				t.Fatalf("Settled=%v want %v", got, c.want)
			}
		})
	}
}

// failedCheck builds a terminal-failing check (modern check run).
func failedCheck(name string) CheckDetail {
	c := "failure"
	return CheckDetail{Name: name, Status: "completed", Conclusion: &c}
}

// ci builds a CIStatus whose Checks (and back-compat Failures) carry the named
// terminal failures. `state` is retained for back-compat/diagnostics only;
// AcceptableGiven reasons over Checks, not State.
func ci(state string, failing ...string) CIStatus {
	s := CIStatus{State: state}
	for _, name := range failing {
		ch := failedCheck(name)
		s.Failures = append(s.Failures, ch)
		s.Checks = append(s.Checks, ch)
	}
	s.Failed = len(s.Failures)
	return s
}

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func TestCIStatusAcceptableGiven(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	stale := 12 * time.Hour
	// pendingCheck builds a still-running check whose age (since CreatedAt) is `age`.
	pendingCheck := func(name string, age time.Duration) CheckDetail {
		return CheckDetail{Name: name, Status: "in_progress", CreatedAt: now.Add(-age)}
	}
	// withChecks wraps an explicit set of checks into a CIStatus.
	withChecks := func(checks ...CheckDetail) CIStatus { return CIStatus{Checks: checks} }

	tests := []struct {
		name         string
		ci           CIStatus
		ignored      map[string]bool
		baseFailures map[string]bool
		required     map[string]bool // empty/nil → all-checks gating (M2)
		wantOK       bool
		wantBlocking []string
	}{
		{
			name:   "AllGreenIsAcceptable",
			ci:     ci("success"),
			wantOK: true,
		},
		{
			name:         "PendingNotStaleBlocksDefensively",
			ci:           withChecks(pendingCheck("x", time.Hour)),
			wantOK:       false,
			wantBlocking: []string{"x"},
		},
		{
			name:         "StalePendingNotIgnoredBlocksWithStuckMarker",
			ci:           withChecks(pendingCheck("stuck-check", 13*time.Hour)),
			wantOK:       false,
			wantBlocking: []string{"stuck-check (stuck)"},
		},
		{
			name:    "StalePendingIgnoredIsAcceptable",
			ci:      withChecks(pendingCheck("stuck-check", 13*time.Hour)),
			ignored: set("stuck-check"),
			wantOK:  true,
		},
		{
			name: "MixStaleIgnoredPlusTerminalFailBlocks",
			ci: withChecks(
				pendingCheck("stuck-check", 13*time.Hour),
				failedCheck("client-web"),
			),
			ignored:      set("stuck-check"),
			wantOK:       false,
			wantBlocking: []string{"client-web"},
		},
		{
			name:         "UnignoredFailureBlocks",
			ci:           ci("failure", "client-web"),
			wantOK:       false,
			wantBlocking: []string{"client-web"},
		},
		{
			name:    "FailureInIgnoreListIsAcceptable",
			ci:      ci("failure", "meta-changelog-pr"),
			ignored: set("meta-changelog-pr"),
			wantOK:  true,
		},
		{
			name:         "FailureAlsoFailingOnBaseIsAcceptable",
			ci:           ci("failure", "go-modernize"),
			baseFailures: set("go-modernize"),
			wantOK:       true,
		},
		{
			name:         "MixOnlyNewFailureBlocks",
			ci:           ci("failure", "go-modernize", "meta-changelog-pr", "client-web"),
			ignored:      set("meta-changelog-pr"),
			baseFailures: set("go-modernize"),
			wantOK:       false,
			wantBlocking: []string{"client-web"},
		},
		{
			name:         "MultipleNewFailuresAllReported",
			ci:           ci("failure", "client-web", "ui-lint-test-build"),
			wantOK:       false,
			wantBlocking: []string{"client-web", "ui-lint-test-build"},
		},
		{
			name:         "AllFailuresSuppressedIsAcceptableEvenIfStateFailure",
			ci:           ci("failure", "go-modernize", "service-worker-manager"),
			baseFailures: set("go-modernize", "service-worker-manager"),
			wantOK:       true,
		},
		// --- Q7: required-checks gating ---
		{
			// With a required set, only required checks can block; a failing
			// non-required check is ignored entirely.
			name:         "RequiredGatingOnlyRequiredChecksBlock",
			ci:           ci("failure", "required-build", "optional-codeql"),
			required:     set("required-build"),
			wantOK:       false,
			wantBlocking: []string{"required-build"},
		},
		{
			name:     "RequiredGatingNonRequiredFailureIsAcceptable",
			ci:       ci("failure", "optional-codeql"),
			required: set("required-build"),
			wantOK:   true,
		},
		{
			// Empty required set must fall back to all-checks (M2) — never read an
			// all-red PR as vacuously acceptable.
			name:         "EmptyRequiredFallsBackToAllChecks",
			ci:           ci("failure", "some-check"),
			required:     nil,
			wantOK:       false,
			wantBlocking: []string{"some-check"},
		},
		{
			// ignored still wins within the required set.
			name:     "RequiredButIgnoredIsAcceptable",
			ci:       ci("failure", "required-build"),
			ignored:  set("required-build"),
			required: set("required-build"),
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, blocking := tt.ci.AcceptableGiven(tt.ignored, tt.baseFailures, tt.required, now, stale)
			if ok != tt.wantOK {
				t.Errorf("AcceptableGiven ok = %v, want %v (blocking=%v)", ok, tt.wantOK, blocking)
			}
			sort.Strings(blocking)
			want := append([]string(nil), tt.wantBlocking...)
			sort.Strings(want)
			if len(blocking) != len(want) {
				t.Fatalf("blocking = %v, want %v", blocking, want)
			}
			for i := range want {
				if blocking[i] != want[i] {
					t.Errorf("blocking = %v, want %v", blocking, want)
				}
			}
		})
	}
}
