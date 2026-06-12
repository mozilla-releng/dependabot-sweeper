// Package codebase provides dependency usage analysis by grepping
// the target repository for import statements and references.
package codebase

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// importPatterns maps ecosystem names to grep-compatible regex patterns.
// The placeholder %s is replaced with the normalised package name.
var importPatterns = map[string][]string{
	"npm": {
		`require\(['"]%s`,
		`from ['"]%s`,
		`import ['"]%s`,
	},
	"gomod": {
		`import.*["']%s`,
		`["']%s`,
	},
	"pip": {
		`import %s`,
		`from %s import`,
	},
	"cargo": {
		`use %s`,
		`extern crate %s`,
	},
}

// grepIncludes lists the file-type include flags and vendored/generated dir
// exclusions passed to grep. Excluding vendored trees keeps third-party copies
// from flooding the (capped) snippet list and producing misleading usage hits.
var grepIncludes = []string{
	"--include=*.js",
	"--include=*.ts",
	"--include=*.go",
	"--include=*.py",
	"--include=*.rs",
	"--include=*.jsx",
	"--include=*.tsx",
	"--include=*.mjs",
	"--exclude-dir=node_modules",
	"--exclude-dir=vendor",
	"--exclude-dir=.git",
	"--exclude-dir=dist",
	"--exclude-dir=build",
}

// AnalyseCodebaseUsage scans a repository for references to packageName.
// If cloneDir is non-empty and exists, it is used directly; otherwise the
// repository is shallow-cloned from GitHub into a temporary directory that is
// removed before returning.
func AnalyseCodebaseUsage(ctx context.Context, repoName, packageName, ecosystem, cloneDir string) (models.CodebaseUsage, error) {
	repoDir := cloneDir
	if cloneDir == "" || !isDir(cloneDir) {
		clonedDir, cleanup, err := ShallowClone(ctx, repoName)
		if err != nil {
			return models.CodebaseUsage{}, fmt.Errorf("shallow clone %s: %w", repoName, err)
		}
		defer cleanup()
		repoDir = clonedDir
	}

	var importFiles []string
	var usageSnippets []models.UsageSnippet

	searchName := NormalisePackageName(packageName, ecosystem)

	// Search using ecosystem-specific import patterns. The package name is
	// regex-escaped before interpolation so metacharacters (e.g. the "." in
	// "lodash.merge") match literally rather than as wildcards.
	patterns := importPatterns[ecosystem]
	for _, tmpl := range patterns {
		pattern := fmt.Sprintf(tmpl, regexp.QuoteMeta(searchName))
		matches, err := runGrep(ctx, repoDir, []string{"-E", pattern})
		if err != nil {
			slog.Warn("grep failed for pattern", "pattern", pattern, "error", err)
			continue
		}
		importFiles, usageSnippets = collectMatches(matches, repoDir, importFiles, usageSnippets, false)
	}

	// Also do a plain fixed-string (-F) grep for the package name itself, so the
	// name is matched literally and never interpreted as a regex.
	matches, err := runGrep(ctx, repoDir, []string{"-F", searchName})
	if err != nil {
		slog.Warn("grep failed for package name", "name", searchName, "error", err)
	} else {
		importFiles, usageSnippets = collectMatches(matches, repoDir, importFiles, usageSnippets, true)
	}

	// Cap snippets at 50.
	if len(usageSnippets) > 50 {
		usageSnippets = usageSnippets[:50]
	}

	return models.CodebaseUsage{
		ImportFiles:   importFiles,
		UsageSnippets: usageSnippets,
	}, nil
}

// ShallowClone performs a depth-1 git clone of the given GitHub repository into
// a temporary directory. It returns the path to the cloned repo and a cleanup
// function that removes the temporary directory; the caller MUST call cleanup
// (e.g. via defer) to avoid leaking a full checkout per call. cleanup is always
// safe to call, including on the error path.
func ShallowClone(ctx context.Context, repoName string) (repoDir string, cleanup func(), err error) {
	tmpDir, err := os.MkdirTemp("", "dependabot-sweeper-")
	if err != nil {
		return "", func() {}, fmt.Errorf("create temp dir: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(tmpDir) }

	repoDir = filepath.Join(tmpDir, "repo")
	url := fmt.Sprintf("https://github.com/%s.git", repoName)

	cloneCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cloneCtx, "git", "clone", "--depth=1", url, repoDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("git clone %s: %w\n%s", url, err, output)
	}
	return repoDir, cleanup, nil
}

// NormalisePackageName adjusts a package name for search based on ecosystem conventions.
func NormalisePackageName(packageName, ecosystem string) string {
	switch ecosystem {
	case "pip":
		// Python: replace hyphens with underscores, strip extras like [dev].
		name := strings.ReplaceAll(packageName, "-", "_")
		if idx := strings.Index(name, "["); idx != -1 {
			name = name[:idx]
		}
		return name
	case "gomod":
		// Go: strip trailing version suffix like /v2, /v3, etc.
		re := regexp.MustCompile(`/v\d+$`)
		return re.ReplaceAllString(packageName, "")
	default:
		return packageName
	}
}

// runGrep executes grep -rn with the standard file-type includes and the
// given extra arguments against repoDir. It returns the raw stdout lines.
func runGrep(ctx context.Context, repoDir string, extraArgs []string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	args := []string{"-rn"}
	args = append(args, grepIncludes...)
	args = append(args, extraArgs...)
	args = append(args, repoDir)

	cmd := exec.CommandContext(ctx, "grep", args...)
	output, err := cmd.Output()
	if err != nil {
		// grep returns exit code 1 when no matches are found; that is not an error.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}

	raw := strings.TrimSpace(string(output))
	if raw == "" {
		return nil, nil
	}
	return strings.Split(raw, "\n"), nil
}

// collectMatches parses grep output lines and appends results to importFiles and usageSnippets.
// When dedup is true, a snippet is only added if no existing snippet shares the same file and line.
func collectMatches(
	lines []string,
	repoDir string,
	importFiles []string,
	usageSnippets []models.UsageSnippet,
	dedup bool,
) ([]string, []models.UsageSnippet) {
	prefix := repoDir + "/"

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Expected format: filepath:lineno:content
		parts := strings.SplitN(line, ":", 3)
		if len(parts) < 3 {
			continue
		}

		fp := strings.TrimPrefix(parts[0], prefix)
		lineno := parts[1]
		content := strings.TrimSpace(parts[2])
		if len(content) > 200 {
			content = content[:200]
		}

		if !contains(importFiles, fp) {
			importFiles = append(importFiles, fp)
		}

		if dedup && snippetExists(usageSnippets, fp, lineno) {
			continue
		}

		usageSnippets = append(usageSnippets, models.UsageSnippet{
			File:    fp,
			Line:    lineno,
			Content: content,
		})
	}

	return importFiles, usageSnippets
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func snippetExists(snippets []models.UsageSnippet, file, line string) bool {
	for _, s := range snippets {
		if s.File == file && s.Line == line {
			return true
		}
	}
	return false
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
