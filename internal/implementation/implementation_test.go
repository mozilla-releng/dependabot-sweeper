package implementation

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func TestEnsureBranchUpToDate(t *testing.T) {
	errBoom := errors.New("boom")

	tests := []struct {
		name      string
		manualErr error
		wantErr   bool
	}{
		{name: "ManualRebaseSucceeds", manualErr: nil, wantErr: false},
		{name: "ManualRebaseFailurePropagates", manualErr: errBoom, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manualCalled := false

			err := ensureBranchUpToDate(func() error {
				manualCalled = true
				return tt.manualErr
			})

			// Bug #6: the manual rebase must ALWAYS be the path taken — we never
			// post `@dependabot rebase`, which could close the PR.
			if !manualCalled {
				t.Errorf("manual rebase was not invoked; ensureBranchUpToDate must always rebase directly")
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildBranchName(t *testing.T) {
	tests := []struct {
		name        string
		packageName string
		newVersion  string
		want        string
	}{
		{
			name:        "Simple",
			packageName: "nock",
			newVersion:  "14.0.11",
			want:        "auto/fix/nock-14.0.11",
		},
		{
			name:        "ScopedNPM",
			packageName: "@sentry/node",
			newVersion:  "10.27.0",
			want:        "auto/fix/sentry-node-10.27.0",
		},
		{
			name:        "GoModule",
			packageName: "github.com/foo/bar",
			newVersion:  "2.0.0",
			want:        "auto/fix/foo-bar-2.0.0",
		},
		{
			name:        "VersionWithV",
			packageName: "lodash",
			newVersion:  "v5.0.0",
			want:        "auto/fix/lodash-5.0.0",
		},
		{
			name:        "GroupedNoVersion",
			packageName: "node-deps",
			newVersion:  "",
			want:        "auto/fix/node-deps-group",
		},
		{
			name:        "GroupedScoped",
			packageName: "frontend-deps",
			newVersion:  "",
			want:        "auto/fix/frontend-deps-group",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildBranchName(tt.packageName, tt.newVersion)
			if got != tt.want {
				t.Errorf("BuildBranchName(%q, %q) = %q, want %q", tt.packageName, tt.newVersion, got, tt.want)
			}
		})
	}
}

func TestSweeperPRTitle(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"build(deps): bump query-string from 7.1.1 to 9.3.1", "fix(deps): bump query-string from 7.1.1 to 9.3.1"},
		{"build(deps-dev): bump eslint from 8 to 9", "fix(deps-dev): bump eslint from 8 to 9"},
		{"build(deps): bump the node-deps group with 14 updates", "fix(deps): bump the node-deps group with 14 updates"},
		{"Bump lodash from 4.17.21 to 5.0.0", "fix(deps): Bump lodash from 4.17.21 to 5.0.0"}, // no prefix → prepend
		{"chore: update X", "fix: update X"},                     // any type → fix
		{"fix(deps): already a fix", "fix(deps): already a fix"}, // idempotent-ish
		{"no colon here", "fix(deps): no colon here"},            // not conventional
	}
	for _, tt := range tests {
		if got := SweeperPRTitle(tt.in); got != tt.want {
			t.Errorf("SweeperPRTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseDiffStat(t *testing.T) {
	tests := []struct {
		name     string
		statLine string
		want     string
	}{
		{
			name:     "Simple",
			statLine: " 3 files changed, 40 insertions(+), 12 deletions(-)",
			want:     "+40 -12 across 3 files",
		},
		{
			name:     "InsertionsOnly",
			statLine: " 1 file changed, 5 insertions(+)",
			want:     "+5 -0 across 1 file",
		},
		{
			name:     "Empty",
			statLine: "",
			want:     "(no changes)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseDiffStat(tt.statLine)
			if got != tt.want {
				t.Errorf("ParseDiffStat(%q) = %q, want %q", tt.statLine, got, tt.want)
			}
		})
	}
}

func TestDecideCIFixLoop(t *testing.T) {
	cases := []struct {
		name          string
		ciAcceptable  bool
		iteration     int
		maxIterations int
		elapsed       float64
		maxTime       float64
		want          ciFixVerdict
	}{
		{"acceptable -> done", true, 1, 3, 100, 3600, ciFixDone},
		{"not acceptable, iters left -> continue", false, 1, 3, 100, 3600, ciFixContinue},
		{"at max iters still continues", false, 3, 3, 100, 3600, ciFixContinue},
		{"iters exhausted -> giveup", false, 4, 3, 100, 3600, ciFixGiveUp},
		{"time exceeded -> giveup", false, 1, 3, 3600, 3600, ciFixGiveUp},
		{"acceptable wins even at limits", true, 3, 3, 3600, 3600, ciFixDone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := decideCIFixLoop(c.ciAcceptable, c.iteration, c.maxIterations, c.elapsed, c.maxTime)
			if got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestDecideNoProgress(t *testing.T) {
	const noFloor = math.MaxInt
	cases := []struct {
		name                        string
		blockingCount, floor, stall int
		maxStall                    int
		wantGiveUp                  bool
		wantFloor, wantStall        int
	}{
		// First call establishes the floor; never gives up.
		{"first call sets floor", 2, noFloor, 0, 8, false, 2, 0},
		// Equal count → no improvement → stall increments.
		{"equal count stalls", 2, 2, 0, 8, false, 2, 1},
		// Worse count → still no improvement → stall increments, floor unchanged.
		{"worse count stalls", 3, 2, 4, 8, false, 2, 5},
		// A strictly lower count is progress → new floor, stall resets.
		{"new low resets stall", 1, 2, 5, 8, false, 1, 0},
		// Stall reaches maxStall → give up.
		{"reaches maxStall", 2, 2, 7, 8, true, 2, 8},
		// maxStall=1 → one non-improving attempt gives up immediately.
		{"maxStall1 single stall", 2, 2, 0, 1, true, 2, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotGiveUp, gotFloor, gotStall := decideNoProgress(c.blockingCount, c.floor, c.stall, c.maxStall)
			if gotGiveUp != c.wantGiveUp || gotFloor != c.wantFloor || gotStall != c.wantStall {
				t.Errorf("decideNoProgress(%d,%d,%d,%d) = (%v,%d,%d), want (%v,%d,%d)",
					c.blockingCount, c.floor, c.stall, c.maxStall,
					gotGiveUp, gotFloor, gotStall, c.wantGiveUp, c.wantFloor, c.wantStall)
			}
		})
	}
}

// driveNoProgress replays a sequence of per-attempt blocking-check counts
// through decideNoProgress exactly as the CI-fix loop does, returning the
// 1-based attempt index at which it gives up (or 0 if it never does).
func driveNoProgress(counts []int, maxStall int) int {
	floor, stall := math.MaxInt, 0
	for i, c := range counts {
		var giveUp bool
		giveUp, floor, stall = decideNoProgress(c, floor, stall, maxStall)
		if giveUp {
			return i + 1
		}
	}
	return 0
}

// TestDecideNoProgressSequences drives the metric over whole sequences — the
// behaviour that matters operationally. The oscillation case is the Q12 fix:
// the old exact-set guard never fired on it, so a thrashing worker ran to the
// global iteration/time cap.
func TestDecideNoProgressSequences(t *testing.T) {
	rep := func(v, n int) []int {
		s := make([]int, n)
		for i := range s {
			s[i] = v
		}
		return s
	}
	osc := func(a, b, n int) []int {
		s := make([]int, n)
		for i := range s {
			if i%2 == 0 {
				s[i] = a
			} else {
				s[i] = b
			}
		}
		return s
	}

	// Stationary at 2: attempt 1 sets the floor, then 8 non-improving attempts
	// → give up on attempt 9.
	if got := driveNoProgress(rep(2, 20), 8); got != 9 {
		t.Errorf("stationary: gave up at attempt %d, want 9", got)
	}
	// Oscillation 5,4,5,4,…: floor reaches 4 on attempt 2 and never improves; 8
	// stalls later → give up on attempt 10. The OLD guard never fired here.
	if got := driveNoProgress(osc(5, 4, 30), 8); got != 10 {
		t.Errorf("oscillation: gave up at attempt %d, want 10", got)
	}
	// Strictly decreasing: genuine progress every attempt → never gives up.
	if got := driveNoProgress([]int{8, 7, 6, 5, 4, 3, 2, 1}, 8); got != 0 {
		t.Errorf("monotonic progress: gave up at attempt %d, want 0 (never)", got)
	}
	// Progress to a floor of 1 by attempt 4, then stuck → 8 stalls → attempt 12.
	if got := driveNoProgress(append([]int{4, 3, 2, 1}, rep(1, 20)...), 8); got != 12 {
		t.Errorf("progress-then-stall: gave up at attempt %d, want 12", got)
	}
}

func TestWorkerCommand(t *testing.T) {
	id := "abc-123"
	launch := workerCommand(id, false, 50, "")
	joined := strings.Join(launch, " ")
	if !strings.Contains(joined, "--print") ||
		!strings.Contains(joined, "--session-id "+id) || !strings.Contains(joined, "--max-budget-usd") {
		t.Fatalf("launch cmd missing required flags: %q", joined)
	}
	// --bare is deliberately NOT used: it blocks hooks/skills/plugins that may
	// be installed on the managed GCP instance (principle violation). Verify it
	// is absent.
	if strings.Contains(joined, "--bare") {
		t.Fatalf("launch cmd must NOT contain --bare (6.B): %q", joined)
	}
	if strings.Contains(joined, "--resume") {
		t.Fatalf("launch cmd must not contain --resume: %q", joined)
	}
	// The claude CLI has no thinking-budget flag — the worker must never emit one.
	if strings.Contains(joined, "--thinking-budget-tokens") {
		t.Fatalf("launch cmd must not contain --thinking-budget-tokens: %q", joined)
	}
	resume := workerCommand(id, true, 50, "claude-sonnet-4-6")
	rj := strings.Join(resume, " ")
	if !strings.Contains(rj, "--resume "+id) || strings.Contains(rj, "--session-id") {
		t.Fatalf("resume cmd wrong: %q", rj)
	}
	if !strings.Contains(rj, "--model claude-sonnet-4-6") {
		t.Fatalf("resume cmd missing model: %q", rj)
	}
}

func TestBoundedTurnBrief(t *testing.T) {
	b := BuildImplementationBrief("query-string", "7.1.1", "9.3.1", "npm", "assessment", nil, 55)
	low := strings.ToLower(b)
	for _, want := range []string{"draft", "push", "exit"} {
		if !strings.Contains(low, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	for _, bad := range []string{"gh pr checks", "--watch", "wait for ci", "until", "squash"} {
		if strings.Contains(low, bad) {
			t.Errorf("brief still contains CI-wait language %q", bad)
		}
	}
}

func TestBuildCIFeedback(t *testing.T) {
	fb := buildCIFeedback([]string{"client-web"}, map[string]string{"client-web": "ERROR: lockfile mismatch"})
	low := strings.ToLower(fb)
	if !strings.Contains(fb, "client-web") || !strings.Contains(fb, "lockfile mismatch") {
		t.Fatalf("feedback missing check/log: %q", fb)
	}
	if !strings.Contains(low, "push") || !strings.Contains(low, "exit") {
		t.Fatalf("feedback must tell the worker to fix, push, and exit: %q", fb)
	}
	if strings.Contains(low, "gh pr checks") {
		t.Fatalf("feedback must not tell the worker to check CI: %q", fb)
	}
}

func TestBuildReviewFeedback(t *testing.T) {
	fb := buildReviewFeedback([]string{"deleted a test in foo_test.go", "unjustified API change"})
	low := strings.ToLower(fb)
	// Must surface each concern verbatim.
	for _, want := range []string{"deleted a test in foo_test.go", "unjustified API change"} {
		if !strings.Contains(fb, want) {
			t.Errorf("review feedback missing concern %q: %q", want, fb)
		}
	}
	// Must tell the worker to fix, push, and exit — and NOT to watch CI itself.
	for _, want := range []string{"push", "exit"} {
		if !strings.Contains(low, want) {
			t.Errorf("review feedback must tell the worker to %q: %q", want, fb)
		}
	}
	if strings.Contains(low, "gh pr checks") {
		t.Errorf("review feedback must not tell the worker to check CI: %q", fb)
	}
}

func TestNewSessionID(t *testing.T) {
	// RFC-4122 v4 shape: 8-4-4-4-12 hex, version nibble '4', variant in [89ab].
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newSessionID()
		if !re.MatchString(id) {
			t.Fatalf("newSessionID() = %q, not a v4 UUID", id)
		}
		if seen[id] {
			t.Fatalf("newSessionID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestBuildGroupedImplementationBrief(t *testing.T) {
	pr := models.DependabotPR{
		Number:      42,
		PackageName: "node-deps",
		Ecosystem:   "npm",
		Grouped:     true,
		GroupedUpdates: []models.PackageBump{
			{Name: "react", From: "17.0.0", To: "18.0.0"},
			{Name: "react-dom", From: "17.0.0", To: "18.0.0"},
		},
	}
	analysis := &models.AgentAnalysis{
		ReviewBody:  "React 18 breaks concurrent mode usage.",
		CodeChanges: []models.CodeChangeEntry{{File: "src/index.js", Description: "update to createRoot"}},
	}
	got := BuildGroupedImplementationBrief(pr, analysis.ReviewBody, analysis.CodeChanges)

	for _, want := range []string{
		"node-deps",
		"react",
		"react-dom",
		"#42",
		"gh pr create --draft",
		"implementation stage",
		"independently reviewed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief missing %q", want)
		}
	}
	low := strings.ToLower(got)
	for _, bad := range []string{"gh pr checks", "--watch", "wait for ci", "until", "squash"} {
		if strings.Contains(low, bad) {
			t.Errorf("brief must not contain CI-wait language %q", bad)
		}
	}
}

func TestBuildImplementationBrief_IncludesContext(t *testing.T) {
	brief := BuildImplementationBrief(
		"nock",
		"13.0.0",
		"14.0.0",
		"npm",
		"interceptors API changed",
		[]models.CodeChangeEntry{
			{File: "test/mock.js", Description: "update interceptor calls"},
		},
		8333,
	)

	required := []string{
		"implementation stage",
		"independently reviewed",
		"nock",
		"13.0.0",
		"14.0.0",
		"interceptors API changed",
		"test/mock.js",
		"#8333",
		"gh pr create --draft",
	}
	for _, s := range required {
		if !strings.Contains(brief, s) {
			t.Errorf("BuildImplementationBrief output missing %q", s)
		}
	}
}
