package implementation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
)

// git runs a git command in dir and fails the test on error, returning trimmed
// stdout. Used by the squashBranch integration test below.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Deterministic identity + no inherited config noise.
	cmd.Env = append([]string{
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	}, "HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestSquashBranchUsesCapturedTipNotStaleBase is the T9 integration test. It
// builds a branch with a bump commit (B1) plus two agent commits on top, then
// squashes relative to the captured post-rebase bump tip (B1). The resulting
// single fix commit must:
//   - have B1 as its parent, and
//   - contain ONLY the agent's files — never the bump commit's file.
//
// It then shows the bug the fix prevents: diffing against an EARLIER base (B0,
// the stale scan-time SHA the old code used) WOULD bundle the bump file into
// the "fix". This is exactly the T9 pollution seen on petemoore/taskcluster #193.
func TestSquashBranchUsesCapturedTipNotStaleBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()

	root := t.TempDir()
	repo := filepath.Join(root, "work")
	remote := filepath.Join(root, "remote.git")

	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.name", "t")
	git(t, repo, "config", "user.email", "t@e")

	// B0 — the branch's original base (the stale scan-time SHA in the bug).
	writeFile(t, filepath.Join(repo, "app.txt"), "v1\n")
	git(t, repo, "add", "app.txt")
	git(t, repo, "commit", "-q", "-m", "B0 base")
	b0 := git(t, repo, "rev-parse", "HEAD")

	// B1 — the bump commit (post-rebase tip). Adds dep.txt; this file must NOT
	// appear in the squashed fix commit.
	const branch = "auto/fix/dep-2.0.0"
	git(t, repo, "checkout", "-q", "-b", branch)
	writeFile(t, filepath.Join(repo, "dep.txt"), "dep@2.0.0\n")
	git(t, repo, "add", "dep.txt")
	git(t, repo, "commit", "-q", "-m", "build(deps): bump dep to 2.0.0")
	b1 := git(t, repo, "rev-parse", "HEAD")

	// Two agent turns on top of the bump: modify app.txt, then add fix.txt.
	writeFile(t, filepath.Join(repo, "app.txt"), "v1 — adapted for dep 2.0.0\n")
	git(t, repo, "add", "app.txt")
	git(t, repo, "commit", "-q", "-m", "agent turn 1: adapt call sites")
	writeFile(t, filepath.Join(repo, "fix.txt"), "compat shim\n")
	git(t, repo, "add", "fix.txt")
	git(t, repo, "commit", "-q", "-m", "agent turn 2: add shim")

	// Establish the branch on a bare origin so squashBranch's force-with-lease
	// push has a remote ref to lease against.
	if err := os.MkdirAll(remote, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, remote, "init", "-q", "--bare")
	git(t, repo, "remote", "add", "origin", remote)
	git(t, repo, "push", "-q", "origin", branch)

	p := &Pipeline{config: &config.Config{}}
	if err := p.squashBranch(ctx, repo, b1, branch, "fix: adapt code for dep 2.0.0"); err != nil {
		t.Fatalf("squashBranch: %v", err)
	}

	// The fix commit's parent must be the captured bump tip (B1), not B0.
	parent := git(t, repo, "rev-parse", "HEAD~1")
	if parent != b1 {
		t.Errorf("fix commit parent = %s, want bump tip %s (T9: must base on the captured tip)", parent, b1)
	}

	// The fix commit (B1..HEAD) must contain ONLY the agent's files.
	files := git(t, repo, "diff", "--name-only", b1+"..HEAD")
	if !strings.Contains(files, "app.txt") || !strings.Contains(files, "fix.txt") {
		t.Errorf("fix commit missing agent files; got:\n%s", files)
	}
	if strings.Contains(files, "dep.txt") {
		t.Errorf("fix commit bundled the bump file dep.txt — T9 pollution:\n%s", files)
	}

	// Demonstrate the bug the fix avoids: based off the stale B0, the same tree
	// WOULD include dep.txt (the bump) in the "fix".
	staleFiles := git(t, repo, "diff", "--name-only", b0+"..HEAD")
	if !strings.Contains(staleFiles, "dep.txt") {
		t.Errorf("expected the stale-base diff to include the bump file dep.txt; got:\n%s", staleFiles)
	}
}
