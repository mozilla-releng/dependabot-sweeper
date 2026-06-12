package ghclient

import (
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

func TestAggregateChecks(t *testing.T) {
	headTime := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	checkTime := time.Date(2026, 6, 9, 11, 0, 0, 0, time.UTC)
	concl := func(s string) *string { return &s }

	got := aggregateChecks([]models.CheckDetail{
		{Name: "build", Status: "completed", Conclusion: concl("success"), CreatedAt: checkTime},
		{Name: "lint", Status: "completed", Conclusion: concl("failure"), CreatedAt: checkTime, Output: "boom"},
		{Name: "flaky", Status: "completed", Conclusion: concl("timed_out"), CreatedAt: checkTime}, // terminal, non-pass, non-fail
		{Name: "release", Status: "completed", Conclusion: concl("cancelled"), CreatedAt: checkTime},
		{Name: "deploy", Status: "in_progress"}, // no timestamp → head-commit fallback
	}, headTime)

	if got.Total != 5 {
		t.Errorf("Total = %d, want 5", got.Total)
	}
	if got.Passed != 1 {
		t.Errorf("Passed = %d, want 1 (only build)", got.Passed)
	}
	if got.Failed != 1 {
		t.Errorf("Failed = %d, want 1 (only lint)", got.Failed)
	}
	if got.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (only deploy; timed_out/cancelled are terminal)", got.Pending)
	}
	if got.State != "failure" {
		t.Errorf("State = %q, want failure", got.State)
	}
	if len(got.Failures) != 1 || got.Failures[0].Name != "lint" {
		t.Errorf("Failures = %+v, want [lint]", got.Failures)
	}
	if len(got.Checks) != 5 {
		t.Errorf("Checks length = %d, want 5", len(got.Checks))
	}
	// Head-commit fallback for the timestamp-less pending check.
	for _, c := range got.Checks {
		if c.Name == "deploy" {
			if !c.CreatedAt.Equal(headTime) {
				t.Errorf("deploy CreatedAt = %v, want head-commit fallback %v", c.CreatedAt, headTime)
			}
		}
		if c.Name == "build" && !c.CreatedAt.Equal(checkTime) {
			t.Errorf("build CreatedAt = %v, want its own %v", c.CreatedAt, checkTime)
		}
	}
}

func TestAggregateChecks_AllGreen(t *testing.T) {
	got := aggregateChecks([]models.CheckDetail{
		{Name: "a", Status: "completed", Conclusion: ptrStr("success")},
		{Name: "b", Status: "completed", Conclusion: ptrStr("skipped")},
	}, time.Time{})
	if got.State != "success" {
		t.Errorf("State = %q, want success", got.State)
	}
	if got.Failed != 0 || got.Pending != 0 || got.Passed != 2 {
		t.Errorf("counts: passed=%d failed=%d pending=%d, want 2/0/0", got.Passed, got.Failed, got.Pending)
	}
}

func ptrStr(s string) *string { return &s }

func TestParseGroupedPRTitle(t *testing.T) {
	tests := []struct {
		title     string
		wantGroup string
		wantOK    bool
	}{
		{"build(deps): bump the node-deps group with 14 updates", "node-deps", true},
		{"build(deps-dev): bump the dev-deps group with 1 update", "dev-deps", true},
		{"bump the production-dependencies group across 1 directory with 3 updates", "production-dependencies", true},
		{"build(deps): bump lodash from 4.17.21 to 4.17.23", "", false}, // single-package, not grouped
		{"chore: something unrelated", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.title, func(t *testing.T) {
			group, ok := ParseGroupedPRTitle(tt.title)
			if ok != tt.wantOK || group != tt.wantGroup {
				t.Errorf("ParseGroupedPRTitle(%q) = (%q, %v), want (%q, %v)", tt.title, group, ok, tt.wantGroup, tt.wantOK)
			}
		})
	}
}

func TestParseGroupedUpdates(t *testing.T) {
	body := "Bumps the node-deps group with 3 updates:\n\n" +
		"| Package | From | To |\n" +
		"| --- | --- | --- |\n" +
		"| [@aws-sdk/client-ec2](https://github.com/aws/aws-sdk-js-v3) | `3.1049.0` | `3.1054.0` |\n" +
		"| [nodemailer](https://github.com/nodemailer/nodemailer) | `8.0.7` | `8.0.9` |\n" +
		"| semver | `7.8.0` | `7.8.1` |\n" +
		"\nUpdates `@aws-sdk/client-ec2` from 3.1049.0 to 3.1054.0\n<details>...</details>\n"

	got := parseGroupedUpdates(body)
	want := []models.PackageBump{
		{Name: "@aws-sdk/client-ec2", From: "3.1049.0", To: "3.1054.0"},
		{Name: "nodemailer", From: "8.0.7", To: "8.0.9"},
		{Name: "semver", From: "7.8.0", To: "7.8.1"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseGroupedUpdates returned %d bumps, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("bump[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestParseGroupedUpdates_NoTable(t *testing.T) {
	if got := parseGroupedUpdates("no table here\njust prose"); len(got) != 0 {
		t.Errorf("expected no bumps, got %+v", got)
	}
}

func TestNormalizeGitHubRepoURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"PlainHTTPS", "https://github.com/sindresorhus/query-string", "https://github.com/sindresorhus/query-string"},
		{"GitPlusHTTPSDotGit", "git+https://github.com/sindresorhus/query-string.git", "https://github.com/sindresorhus/query-string"},
		{"GitProtocol", "git://github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"GitPlusSSH", "git+ssh://git@github.com/foo/bar.git", "https://github.com/foo/bar"},
		{"SSHShorthand", "git@github.com:foo/bar.git", "https://github.com/foo/bar"},
		{"NPMShorthand", "github:foo/bar", "https://github.com/foo/bar"},
		{"ScopedRepoName", "git+https://github.com/testing-library/react-testing-library.git", "https://github.com/testing-library/react-testing-library"},
		{"TrailingSlash", "https://github.com/foo/bar/", "https://github.com/foo/bar"},
		{"NonGitHub", "https://gitlab.com/foo/bar", ""},
		{"Empty", "", ""},
		{"Garbage", "not a url", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeGitHubRepoURL(tt.raw); got != tt.want {
				t.Errorf("normalizeGitHubRepoURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func pr(number int, pkg, oldVer, newVer string) models.DependabotPR {
	return models.DependabotPR{
		Number:      number,
		PackageName: pkg,
		OldVersion:  oldVer,
		NewVersion:  newVer,
	}
}

func TestFindNewerPRForPackage(t *testing.T) {
	tests := []struct {
		name       string
		current    models.DependabotPR
		all        []models.DependabotPR
		wantNumber int // 0 means no superseder
	}{
		{
			// Regression test for the baseline-run-#1 bug: PR #4 (5.2.4) was
			// wrongly closed as superseded by PR #7 (5.2.3) because the old
			// code picked the PR with the higher number regardless of version.
			name:    "lower-version-higher-number must NOT supersede higher-version-lower-number",
			current: pr(4, "webpack-dev-server", "3.11.3", "5.2.4"),
			all: []models.DependabotPR{
				pr(4, "webpack-dev-server", "3.11.3", "5.2.4"),
				pr(7, "webpack-dev-server", "3.11.3", "5.2.3"),
			},
			wantNumber: 0,
		},
		{
			// Inverse direction is fine: a higher-version PR DOES supersede
			// a lower-version one, regardless of which has the higher number.
			name:    "the higher-version PR is correctly identified as superseder",
			current: pr(7, "webpack-dev-server", "3.11.3", "5.2.3"),
			all: []models.DependabotPR{
				pr(4, "webpack-dev-server", "3.11.3", "5.2.4"),
				pr(7, "webpack-dev-server", "3.11.3", "5.2.3"),
			},
			wantNumber: 4,
		},
		{
			// Original happy case that motivated the function: PR #6 (ws 8.20.0)
			// is correctly superseded by PR #15 (ws 8.20.1). Here PR #15 also
			// has the higher number, so both the old buggy version and the new
			// fixed version agree.
			name:    "higher-version-higher-number is identified as superseder (no conflict)",
			current: pr(6, "ws", "7.5.10", "8.20.0"),
			all: []models.DependabotPR{
				pr(6, "ws", "7.5.10", "8.20.0"),
				pr(15, "ws", "7.5.10", "8.20.1"),
			},
			wantNumber: 15,
		},
		{
			// Of multiple candidates, return the one with the highest version,
			// not the first matched or the highest-numbered.
			name:    "picks the highest-version superseder when multiple exist",
			current: pr(1, "foo", "1.0.0", "1.0.1"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.0.1"),
				pr(2, "foo", "1.0.0", "1.0.2"),
				pr(3, "foo", "1.0.0", "1.0.4"),
				pr(4, "foo", "1.0.0", "1.0.3"),
			},
			wantNumber: 3,
		},
		{
			name:    "different packages are not considered",
			current: pr(1, "foo", "1.0.0", "1.0.1"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.0.1"),
				pr(2, "bar", "1.0.0", "2.0.0"),
			},
			wantNumber: 0,
		},
		{
			name:    "equal version does NOT supersede (must be strictly higher)",
			current: pr(1, "foo", "1.0.0", "1.0.1"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.0.1"),
				pr(2, "foo", "1.0.0", "1.0.1"),
			},
			wantNumber: 0,
		},
		{
			// Non-semver current version: conservatively return nil. We can't
			// confidently say anything supersedes a version we can't parse.
			name:    "non-semver current version returns nil",
			current: pr(1, "foo", "abc", "xyz"),
			all: []models.DependabotPR{
				pr(1, "foo", "abc", "xyz"),
				pr(2, "foo", "abc", "9.9.9"),
			},
			wantNumber: 0,
		},
		{
			// Non-semver candidate: skip it, keep looking. If a parseable
			// candidate also exists, it can still supersede.
			name:    "non-semver candidate is skipped; parseable higher version still wins",
			current: pr(1, "foo", "1.0.0", "1.0.1"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.0.1"),
				pr(2, "foo", "1.0.0", "weird"),
				pr(3, "foo", "1.0.0", "1.0.5"),
			},
			wantNumber: 3,
		},
		{
			// Major bump supersedes minor bump.
			name:    "major bump supersedes minor bump on same package",
			current: pr(1, "foo", "1.0.0", "1.5.0"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.5.0"),
				pr(2, "foo", "1.0.0", "2.0.0"),
			},
			wantNumber: 2,
		},
		{
			// Versions with v-prefix and pre-release suffix on patch.
			name:    "v-prefix and pre-release suffix on patch parse correctly",
			current: pr(1, "foo", "v1.0.0", "v1.0.5-beta"),
			all: []models.DependabotPR{
				pr(1, "foo", "v1.0.0", "v1.0.5-beta"),
				pr(2, "foo", "v1.0.0", "v1.0.6"),
			},
			wantNumber: 2,
		},
		{
			name:       "empty allPRs returns nil",
			current:    pr(1, "foo", "1.0.0", "1.0.1"),
			all:        []models.DependabotPR{},
			wantNumber: 0,
		},
		{
			name:    "current PR alone in list returns nil",
			current: pr(1, "foo", "1.0.0", "1.0.1"),
			all: []models.DependabotPR{
				pr(1, "foo", "1.0.0", "1.0.1"),
			},
			wantNumber: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FindNewerPRForPackage(tc.current, tc.all)
			if tc.wantNumber == 0 {
				if got != nil {
					t.Errorf("expected nil, got PR #%d (version %s)", got.Number, got.NewVersion)
				}
				return
			}
			if got == nil {
				t.Errorf("expected PR #%d, got nil", tc.wantNumber)
				return
			}
			if got.Number != tc.wantNumber {
				t.Errorf("expected PR #%d, got PR #%d", tc.wantNumber, got.Number)
			}
		})
	}
}

func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b [3]int
		want int
	}{
		{[3]int{1, 0, 0}, [3]int{1, 0, 0}, 0},
		{[3]int{1, 0, 0}, [3]int{2, 0, 0}, -1},
		{[3]int{2, 0, 0}, [3]int{1, 0, 0}, 1},
		{[3]int{1, 2, 3}, [3]int{1, 2, 4}, -1},
		{[3]int{1, 3, 0}, [3]int{1, 2, 99}, 1},
		{[3]int{5, 2, 4}, [3]int{5, 2, 3}, 1}, // The exact case from the baseline run bug
		{[3]int{5, 2, 3}, [3]int{5, 2, 4}, -1},
	}
	for _, tc := range tests {
		got := compareSemver(tc.a, tc.b)
		if got != tc.want {
			t.Errorf("compareSemver(%v, %v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
