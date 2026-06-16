// Package ghclient wraps the GitHub API for dependabot PR operations.
package ghclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v72/github"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// Patterns for parsing dependabot PR titles.
var (
	// bumpPattern matches titles like "bump lodash from 4.17.21 to 4.17.23".
	bumpPattern = regexp.MustCompile(`(?i)bump\s+(\S+)\s+from\s+(\S+)\s+to\s+(\S+)`)

	// updatePattern matches titles like "update module github.com/foo/bar from v1.2.3 to v2.0.0".
	updatePattern = regexp.MustCompile(`(?i)update\s+(?:module\s+|dependency\s+)?(\S+)\s+(?:from\s+)?(?:v?(\S+)\s+)?to\s+v?(\S+)`)

	// groupedTitlePattern matches dependabot grouped-update titles like
	// "bump the node-deps group with 14 updates" or
	// "bump the X group across 1 directory with 3 updates".
	groupedTitlePattern = regexp.MustCompile(`(?i)bump\s+the\s+(\S+)\s+group\b.*?\bwith\s+\d+\s+updates?`)

	// groupedRowPattern matches a row of the "| Package | From | To |" table in
	// a grouped PR body. The package cell may be a markdown link or bare name;
	// the version cells are backtick-wrapped.
	groupedRowPattern = regexp.MustCompile("^\\|\\s*(?:\\[)?([^\\]|]+?)(?:\\]\\([^)]*\\))?\\s*\\|\\s*`([^`]+)`\\s*\\|\\s*`([^`]+)`\\s*\\|")
)

const (
	maxDiffLen       = 50_000
	maxChangelogLen  = 10_000
	maxReleaseBody   = 5_000
	maxReleases      = 50
	rebasePollPeriod = 15 * time.Second
	defaultTimeout   = 300 * time.Second
)

// Client provides GitHub API operations for dependabot PR management.
type Client struct {
	gh              *github.Client
	repo            *github.Repository
	token           string
	owner           string
	repoName        string
	acceptedAuthors map[string]bool
	botLogin        string // login of the authenticated user; empty if unresolved

	// requiredChecksCache memoises the required-status-check set per base branch
	// for the lifetime of this client. The client is created fresh each scan
	// cycle (orchestrator.New → NewClient), so this is effectively a per-cycle
	// cache shared by every PR goroutine — one branch-protection read per base
	// branch per cycle instead of one per PR (Q7). Guarded by requiredChecksMu.
	requiredChecksMu    sync.Mutex
	requiredChecksCache map[string]map[string]bool
}

// defaultAcceptedAuthors are the PR authors GetDependabotPRs always accepts.
var defaultAcceptedAuthors = []string{"dependabot[bot]", "renovate[bot]"}

// NewClient creates a Client for the given repository. repoFullName must be
// in "owner/repo" format. additionalAuthors are extra login names whose PRs
// should be processed alongside dependabot[bot] and renovate[bot].
func NewClient(ctx context.Context, token, repoFullName string, additionalAuthors []string) (*Client, error) {
	parts := strings.SplitN(repoFullName, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("invalid repository name %q: expected owner/repo", repoFullName)
	}

	gh := github.NewClient(nil).WithAuthToken(token)

	repo, _, err := gh.Repositories.Get(ctx, parts[0], parts[1])
	if err != nil {
		return nil, fmt.Errorf("fetching repository %s: %w", repoFullName, err)
	}

	accepted := make(map[string]bool, len(defaultAcceptedAuthors)+len(additionalAuthors))
	for _, a := range defaultAcceptedAuthors {
		accepted[a] = true
	}
	for _, a := range additionalAuthors {
		if a != "" {
			accepted[a] = true
		}
	}

	// Resolve the authenticated user's login. This is REQUIRED, not best-effort:
	// it identifies the bot's own comments for the idempotency guards
	// (findStatusComment, IsAlreadyProcessedAtSHA). If it were left empty those
	// guards become no-ops, and the tool would re-comment every cycle — a
	// notification storm that violates the hard idempotency contract. Better to
	// fail fast: the token is unusable for our purposes anyway.
	me, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("resolving authenticated user (needed for idempotency): %w", err)
	}
	botLogin := me.GetLogin()
	if botLogin == "" {
		return nil, fmt.Errorf("authenticated user has an empty login; cannot guarantee idempotency")
	}

	return &Client{
		gh:              gh,
		repo:            repo,
		token:           token,
		owner:           parts[0],
		repoName:        parts[1],
		acceptedAuthors: accepted,
		botLogin:        botLogin,
	}, nil
}

// GetDependabotPRs lists all open PRs authored by dependabot[bot] or
// renovate[bot], parses their titles, and enriches them with CI status and diff
// information.
func (c *Client) GetDependabotPRs(ctx context.Context) ([]models.DependabotPR, error) {
	var result []models.DependabotPR

	opts := &github.PullRequestListOptions{
		State:       "open",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		pulls, resp, err := c.gh.PullRequests.List(ctx, c.owner, c.repoName, opts)
		if err != nil {
			return nil, fmt.Errorf("listing pull requests: %w", err)
		}

		for _, pr := range pulls {
			login := pr.GetUser().GetLogin()
			if !c.acceptedAuthors[login] {
				continue
			}

			pkg, oldVer, newVer, ok := ParsePRTitle(pr.GetTitle())
			var grouped bool
			var groupedUpdates []models.PackageBump
			var bump models.BumpType
			switch {
			case ok:
				bump = ClassifyBump(oldVer, newVer)
			default:
				// Not a single-package bump — is it a grouped update?
				if group, isGroup := ParseGroupedPRTitle(pr.GetTitle()); isGroup {
					grouped = true
					pkg = group // group name stands in for the package name
					groupedUpdates = parseGroupedUpdates(pr.GetBody())
					bump = maxGroupedBump(groupedUpdates)
				} else {
					// Unparseable title from an accepted bot author. Do NOT drop it
					// silently — in an unattended cron loop that loses the PR with no
					// signal. Process it with an unknown bump and the title as the
					// display name; the analyser still works off the diff/body, and it
					// stays visible on the dashboard.
					slog.Warn("could not parse PR title — processing via diff",
						"number", pr.GetNumber(), "title", pr.GetTitle())
					pkg = pr.GetTitle()
					bump = models.BumpUnknown
				}
			}

			ecosystem := c.detectEcosystem(pr)
			ci := c.getCIStatus(ctx, pr)
			diff := c.getDiff(ctx, pr)

			result = append(result, models.DependabotPR{
				Number:         pr.GetNumber(),
				Title:          pr.GetTitle(),
				Body:           pr.GetBody(),
				Author:         login,
				PackageName:    pkg,
				Ecosystem:      ecosystem,
				OldVersion:     oldVer,
				NewVersion:     newVer,
				BumpType:       bump,
				CI:             ci,
				Diff:           diff,
				URL:            pr.GetHTMLURL(),
				HeadSHA:        pr.GetHead().GetSHA(),
				HeadRef:        pr.GetHead().GetRef(),
				BaseRef:        pr.GetBase().GetRef(),
				Grouped:        grouped,
				GroupedUpdates: groupedUpdates,
			})
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return result, nil
}

// ParsePRTitle extracts the package name, old version, and new version from a
// dependabot/renovate PR title. Returns ok=false if the title cannot be parsed.
func ParsePRTitle(title string) (packageName, oldVersion, newVersion string, ok bool) {
	for _, pat := range []*regexp.Regexp{bumpPattern, updatePattern} {
		m := pat.FindStringSubmatch(title)
		if m == nil {
			continue
		}
		packageName = m[1]
		oldVersion = m[2]
		newVersion = m[3]
		return packageName, oldVersion, newVersion, true
	}
	return "", "", "", false
}

// ParseGroupedPRTitle recognises a dependabot grouped-update PR title and
// returns the group name. Grouped PRs bump several packages at once and have no
// single from/to version, so they need separate handling from ParsePRTitle.
func ParseGroupedPRTitle(title string) (group string, ok bool) {
	if m := groupedTitlePattern.FindStringSubmatch(title); m != nil {
		return m[1], true
	}
	return "", false
}

// parseGroupedUpdates extracts the individual package bumps from a grouped PR
// body's "| Package | From | To |" markdown table.
func parseGroupedUpdates(body string) []models.PackageBump {
	var bumps []models.PackageBump
	for _, line := range strings.Split(body, "\n") {
		m := groupedRowPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := strings.TrimSpace(m[1])
		// Skip the header row ("Package") if it somehow matched.
		if strings.EqualFold(name, "package") {
			continue
		}
		bumps = append(bumps, models.PackageBump{
			Name: name,
			From: strings.TrimSpace(m[2]),
			To:   strings.TrimSpace(m[3]),
		})
	}
	return bumps
}

// ClassifyBump determines the semver bump type (major, minor, or patch) by
// comparing two version strings.
func ClassifyBump(oldVersion, newVersion string) models.BumpType {
	oldParts, oldOK := parseSemver(oldVersion)
	newParts, newOK := parseSemver(newVersion)
	if !oldOK || !newOK {
		return models.BumpUnknown
	}

	if newParts[0] != oldParts[0] {
		return models.BumpMajor
	}
	if newParts[1] != oldParts[1] {
		return models.BumpMinor
	}
	return models.BumpPatch
}

// maxGroupedBump returns the highest-severity bump among a grouped update's
// members (major > minor > patch > unknown), so a group containing any major
// bump is classified major. Returns BumpUnknown for an empty/unparseable group.
func maxGroupedBump(updates []models.PackageBump) models.BumpType {
	rank := map[models.BumpType]int{
		models.BumpUnknown: 0, models.BumpPatch: 1, models.BumpMinor: 2, models.BumpMajor: 3,
	}
	best := models.BumpUnknown
	for _, u := range updates {
		if b := ClassifyBump(u.From, u.To); rank[b] > rank[best] {
			best = b
		}
	}
	return best
}

// parseSemver strips a leading "v" and splits into [major, minor, patch].
// Returns ok=false if fewer than three numeric parts are found.
func parseSemver(v string) ([3]int, bool) {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	// Tolerate 1- and 2-component versions (e.g. GitHub Actions tags like "3" or
	// "3.4") by treating the missing trailing components as 0, so "3" -> 3.0.0.
	// Without this every github-actions bump classifies as unknown and loses the
	// patch/minor fast-path.
	var out [3]int
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			out[i] = 0
			continue
		}
		// Trim any pre-release/build suffix from the last present component.
		seg := parts[i]
		if i == len(parts)-1 {
			if idx := strings.IndexAny(seg, "-+"); idx != -1 {
				seg = seg[:idx]
			}
		}
		n, err := strconv.Atoi(seg)
		if err != nil {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// FindNewerPRForPackage returns the PR in allPRs (other than currentPR) that
// targets the same package and bumps to a strictly higher semver version than
// currentPR. If multiple candidates exist, the one with the highest version
// is returned. Returns nil if no superseder exists, or if currentPR's
// NewVersion cannot be parsed as semver (in which case we can't make a
// confident comparison, so we conservatively say nothing supersedes it).
//
// Comparison is by version, not PR number: PR numbers correlate with version
// recency only on average, and we observed real-world cases where an older
// PR number had a newer version (e.g. a 5.2.4 bump opened before a 5.2.3 bump
// in the same monorepo). Closing the higher-version PR as "superseded" by
// the lower-version one is wrong.
func FindNewerPRForPackage(currentPR models.DependabotPR, allPRs []models.DependabotPR) *models.DependabotPR {
	currentVer, currentOK := parseSemver(currentPR.NewVersion)
	if !currentOK {
		// Can't compare; be conservative.
		return nil
	}

	var best *models.DependabotPR
	var bestVer [3]int
	for i := range allPRs {
		pr := &allPRs[i]
		if pr.Number == currentPR.Number || pr.PackageName != currentPR.PackageName {
			continue
		}
		ver, ok := parseSemver(pr.NewVersion)
		if !ok || compareSemver(ver, currentVer) <= 0 {
			continue
		}
		if best == nil || compareSemver(ver, bestVer) > 0 {
			best = pr
			bestVer = ver
		}
	}
	return best
}

// FindSupersedingGroup returns the grouped PR in allPRs that already covers
// currentPR's package at a version >= currentPR.NewVersion, making the
// individual PR redundant (Q6). If several groups qualify, the one covering the
// highest version is returned. Returns nil when:
//   - currentPR is itself a grouped PR (an individual never closes a group, and
//     a group is not superseded this way),
//   - currentPR.NewVersion doesn't parse as semver (no confident comparison), or
//   - no open group covers the package at >= that version.
//
// The direction is asymmetric by design (Q6): a group can supersede an
// individual, but an individual never closes a whole group — the group still
// bumps its other members. Package match is exact string equality, mirroring
// FindNewerPRForPackage. Members are read from the group's already-parsed
// GroupedUpdates table.
func FindSupersedingGroup(currentPR models.DependabotPR, allPRs []models.DependabotPR) *models.DependabotPR {
	if currentPR.Grouped {
		return nil
	}
	currentVer, ok := parseSemver(currentPR.NewVersion)
	if !ok {
		return nil // can't compare; be conservative and leave it open.
	}

	var best *models.DependabotPR
	var bestVer [3]int
	for i := range allPRs {
		g := &allPRs[i]
		if !g.Grouped || g.Number == currentPR.Number {
			continue
		}
		for _, m := range g.GroupedUpdates {
			if m.Name != currentPR.PackageName {
				continue
			}
			ver, ok := parseSemver(m.To)
			if !ok || compareSemver(ver, currentVer) < 0 {
				continue // group covers it at a LOWER version → keep the individual.
			}
			// Group covers the package at >= the individual's version → supersedes.
			if best == nil || compareSemver(ver, bestVer) > 0 {
				best = g
				bestVer = ver
			}
		}
	}
	return best
}

// compareSemver returns -1 if a < b, 0 if a == b, 1 if a > b. Operates on
// the [major, minor, patch] tuples returned by parseSemver.
func compareSemver(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// GetUpstreamInfo fetches release notes and changelog content from the
// upstream repository of a dependency.
func (c *Client) GetUpstreamInfo(ctx context.Context, packageName, ecosystem, oldVersion, newVersion string) models.UpstreamInfo {
	repoURL := c.findUpstreamRepo(ctx, packageName, ecosystem)
	info := models.UpstreamInfo{
		RepoURL: repoURL,
	}

	if repoURL == "" {
		return info
	}

	// Extract owner/repo from the URL.
	repoPath := strings.TrimPrefix(repoURL, "https://github.com/")
	parts := strings.SplitN(repoPath, "/", 2)
	if len(parts) != 2 {
		return info
	}

	info.Releases = c.getReleasesBetween(ctx, parts[0], parts[1], oldVersion, newVersion)

	snippet := c.getChangelog(ctx, parts[0], parts[1])
	if snippet != "" {
		info.ChangelogSnippet = snippet
	}

	return info
}

// NOTE: there is intentionally no method to submit a pull request review. The
// tool proposes (via comments and replacement PRs) and a human decides; it must
// never submit a native APPROVE/REQUEST_CHANGES review on a user's PR. Removing
// the capability keeps that guarantee structural, not just convention.

// statusCommentMarker is embedded in the bot's sticky status comment so it
// can be found and updated on subsequent cron cycles rather than re-posted.
const statusCommentMarker = "<!-- sweeper:status -->"

// shaMarkerPrefix / Suffix wrap the head SHA embedded in terminal status
// comments. A subsequent cron run finds the SHA and skips re-analysis when
// nothing has changed on the PR (idempotency — no unnecessary edits).
const shaMarkerPrefix = "<!-- sweeper:sha:"
const shaMarkerSuffix = " -->"

// UpsertStatusComment creates or updates the bot's sticky status comment on
// the PR. When headSHA is non-empty the SHA is embedded in the comment so
// IsAlreadyProcessedAtSHA can detect repeat runs with no new work.
// Pass headSHA="" for transient outcomes that should be retried next cycle
// (analysis errors, implementation failures); pass pr.HeadSHA for terminal
// outcomes where the same analysis would be repeated with identical results
// (low confidence, human review, CI-failing flags).
func (c *Client) UpsertStatusComment(ctx context.Context, prNumber int, headSHA, body string) error {
	var sb strings.Builder
	sb.WriteString(statusCommentMarker)
	if headSHA != "" {
		sb.WriteString("\n")
		sb.WriteString(shaMarkerPrefix)
		sb.WriteString(headSHA)
		sb.WriteString(shaMarkerSuffix)
	}
	sb.WriteString("\n")
	sb.WriteString(body)
	markedBody := sb.String()

	existing, _, err := c.findStatusComment(ctx, prNumber)
	if err != nil {
		return err
	}

	if existing != 0 {
		_, _, err = c.gh.Issues.EditComment(ctx, c.owner, c.repoName, existing,
			&github.IssueComment{Body: github.Ptr(markedBody)})
		if err != nil {
			return fmt.Errorf("editing status comment on #%d: %w", prNumber, err)
		}
		slog.Info("updated status comment", "pr", prNumber, "comment_id", existing)
		return nil
	}

	_, _, err = c.gh.Issues.CreateComment(ctx, c.owner, c.repoName, prNumber,
		&github.IssueComment{Body: github.Ptr(markedBody)})
	if err != nil {
		return fmt.Errorf("creating status comment on #%d: %w", prNumber, err)
	}
	slog.Info("created status comment", "pr", prNumber)
	return nil
}

// IsAlreadyProcessedAtSHA reports whether the bot has already posted a
// terminal status comment on this PR at the given head SHA. Returns false
// when botLogin is unknown or headSHA is empty.
func (c *Client) IsAlreadyProcessedAtSHA(ctx context.Context, prNumber int, headSHA string) (bool, error) {
	if c.botLogin == "" || headSHA == "" {
		return false, nil
	}
	_, body, err := c.findStatusComment(ctx, prNumber)
	if err != nil || body == "" {
		return false, err
	}
	needle := shaMarkerPrefix + headSHA + shaMarkerSuffix
	return strings.Contains(body, needle), nil
}

// findStatusComment returns the ID and body of the bot's existing status
// comment on the PR, or (0, "", nil) if none exists.
func (c *Client) findStatusComment(ctx context.Context, prNumber int) (int64, string, error) {
	if c.botLogin == "" {
		return 0, "", nil
	}
	// Paginate: a long-lived PR can accumulate well over one page of comments,
	// and the bot's status comment may be on any page. Missing it would post a
	// duplicate every cycle instead of editing in place.
	opts := &github.IssueListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, c.owner, c.repoName, prNumber, opts)
		if err != nil {
			return 0, "", fmt.Errorf("listing comments on #%d: %w", prNumber, err)
		}
		for _, comment := range comments {
			if strings.EqualFold(comment.GetUser().GetLogin(), c.botLogin) &&
				strings.Contains(comment.GetBody(), statusCommentMarker) {
				return comment.GetID(), comment.GetBody(), nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return 0, "", nil
}

// RebaseDependabotPR posts a "@dependabot rebase" comment and polls until
// the head SHA changes or the timeout expires. A zero timeout uses the default
// of 300 seconds. Returns true if the rebase completed.
func (c *Client) RebaseDependabotPR(ctx context.Context, prNumber int, timeout time.Duration) (bool, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repoName, prNumber)
	if err != nil {
		return false, fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}

	oldSHA := pr.GetHead().GetSHA()
	slog.Info("requesting dependabot rebase",
		"pr", prNumber,
		"sha", oldSHA[:7])

	_, _, err = c.gh.Issues.CreateComment(ctx, c.owner, c.repoName, prNumber,
		&github.IssueComment{Body: github.Ptr("@dependabot rebase")})
	if err != nil {
		return false, fmt.Errorf("posting rebase comment on #%d: %w", prNumber, err)
	}

	deadline := time.After(timeout)
	ticker := time.NewTicker(rebasePollPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-deadline:
			slog.Warn("dependabot rebase timed out",
				"pr", prNumber,
				"timeout", timeout)
			return false, nil
		case <-ticker.C:
			pr, _, err = c.gh.PullRequests.Get(ctx, c.owner, c.repoName, prNumber)
			if err != nil {
				slog.Warn("error polling PR during rebase",
					"pr", prNumber,
					"error", err)
				continue
			}

			if pr.GetHead().GetSHA() != oldSHA {
				slog.Info("dependabot rebase complete",
					"pr", prNumber,
					"new_sha", pr.GetHead().GetSHA()[:7])
				return true, nil
			}

			// Check recent comments for conflict reports.
			if c.hasRebaseConflict(ctx, prNumber) {
				slog.Warn("dependabot reported rebase conflict",
					"pr", prNumber)
				return false, nil
			}
		}
	}
}

// hasRebaseConflict checks the last few issue comments for a dependabot
// conflict message.
func (c *Client) hasRebaseConflict(ctx context.Context, prNumber int) bool {
	comments, _, err := c.gh.Issues.ListComments(ctx, c.owner, c.repoName, prNumber,
		&github.IssueListCommentsOptions{
			Sort:        github.Ptr("created"),
			Direction:   github.Ptr("desc"),
			ListOptions: github.ListOptions{PerPage: 3},
		})
	if err != nil {
		return false
	}

	for _, comment := range comments {
		login := strings.ToLower(comment.GetUser().GetLogin())
		body := strings.ToLower(comment.GetBody())
		if strings.Contains(login, "dependabot") && strings.Contains(body, "conflict") {
			return true
		}
	}
	return false
}

// IsBranchBehindBase checks whether a PR's head branch is behind its base
// branch.
func (c *Client) IsBranchBehindBase(ctx context.Context, prNumber int) (bool, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repoName, prNumber)
	if err != nil {
		return false, fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}

	comparison, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repoName,
		pr.GetBase().GetRef(), pr.GetHead().GetSHA(), nil)
	if err != nil {
		return false, fmt.Errorf("comparing commits for #%d: %w", prNumber, err)
	}

	return comparison.GetBehindBy() > 0, nil
}

// RequiredChecks returns the set of status-check names that branch protection
// requires on `branch` ("CI passing" ≡ "required checks passing", Q7). The
// result is memoised per branch for the client's lifetime (one branch-protection
// read per base branch per scan cycle, shared across PR goroutines).
//
// An empty set means "fall back to all-checks gating" (review M2 — a vacuously
// satisfied required set must never let an all-red PR read as acceptable) and is
// returned, with no error, whenever the required set can't be determined: branch
// protection isn't configured (404), the token lacks the admin scope to read it
// (403), or any other error. Reading branch protection needs an admin-scoped
// token; without it the tool degrades safely to gating on every check.
func (c *Client) RequiredChecks(ctx context.Context, branch string) map[string]bool {
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	if c.requiredChecksCache == nil {
		c.requiredChecksCache = make(map[string]map[string]bool)
	}
	if cached, ok := c.requiredChecksCache[branch]; ok {
		return cached
	}

	set := make(map[string]bool)
	prot, _, err := c.gh.Repositories.GetBranchProtection(ctx, c.owner, c.repoName, branch)
	if err != nil {
		slog.Warn("could not read required status checks — falling back to all-checks CI gating",
			"branch", branch, "error", err)
		c.requiredChecksCache[branch] = set
		return set
	}
	if rsc := prot.GetRequiredStatusChecks(); rsc != nil {
		for _, name := range rsc.GetContexts() { // legacy field
			set[name] = true
		}
		for _, chk := range rsc.GetChecks() { // modern field
			if chk != nil && chk.Context != "" {
				set[chk.Context] = true
			}
		}
	}
	if len(set) > 0 {
		slog.Info("Gating CI on required checks", "branch", branch, "count", len(set))
	} else {
		slog.Info("No required checks configured — gating on all checks", "branch", branch)
	}
	c.requiredChecksCache[branch] = set
	return set
}

// MarkPRReadyForReview converts a draft PR to ready-for-review using the
// GraphQL API (the REST API does not support this operation).
func (c *Client) MarkPRReadyForReview(ctx context.Context, prNumber int) error {
	// First, fetch the PR's node ID via REST.
	pr, _, err := c.gh.PullRequests.Get(ctx, c.owner, c.repoName, prNumber)
	if err != nil {
		return fmt.Errorf("fetching PR #%d: %w", prNumber, err)
	}

	nodeID := pr.GetNodeID()
	if nodeID == "" {
		return fmt.Errorf("PR #%d has no node ID", prNumber)
	}

	// Use the GraphQL mutation.
	query := fmt.Sprintf(`mutation {
		markPullRequestReadyForReview(input: {pullRequestId: %q}) {
			pullRequest { isDraft }
		}
	}`, nodeID)

	req, err := c.gh.NewRequest("POST", "graphql", map[string]string{"query": query})
	if err != nil {
		return fmt.Errorf("building GraphQL request: %w", err)
	}

	_, err = c.gh.Do(ctx, req, nil)
	if err != nil {
		return fmt.Errorf("marking PR #%d ready for review: %w", prNumber, err)
	}

	slog.Info("marked PR ready for review", "pr", prNumber)
	return nil
}

// UpdatePRTitle changes a PR's title.
func (c *Client) UpdatePRTitle(ctx context.Context, prNumber int, title string) error {
	_, _, err := c.gh.PullRequests.Edit(ctx, c.owner, c.repoName, prNumber,
		&github.PullRequest{Title: github.Ptr(title)})
	if err != nil {
		return fmt.Errorf("updating title of #%d: %w", prNumber, err)
	}
	slog.Info("updated PR title", "pr", prNumber, "title", title)
	return nil
}

// UpdatePRBody replaces a PR's body text.
func (c *Client) UpdatePRBody(ctx context.Context, prNumber int, body string) error {
	_, _, err := c.gh.PullRequests.Edit(ctx, c.owner, c.repoName, prNumber,
		&github.PullRequest{Body: github.Ptr(body)})
	if err != nil {
		return fmt.Errorf("updating body of #%d: %w", prNumber, err)
	}
	slog.Info("updated PR body", "pr", prNumber)
	return nil
}

// FindPRByBranch finds an open PR whose head branch matches branch. Returns
// the PR number and whether one was found.
func (c *Client) FindPRByBranch(ctx context.Context, branch string) (int, bool, error) {
	pulls, _, err := c.gh.PullRequests.List(ctx, c.owner, c.repoName,
		&github.PullRequestListOptions{
			State: "open",
			Head:  fmt.Sprintf("%s:%s", c.owner, branch),
		})
	if err != nil {
		return 0, false, fmt.Errorf("listing PRs for branch %s: %w", branch, err)
	}
	if len(pulls) == 0 {
		return 0, false, nil
	}
	return pulls[0].GetNumber(), true, nil
}

// ClosePRWithComment adds a comment to a PR and then closes it.
func (c *Client) ClosePRWithComment(ctx context.Context, prNumber int, comment string) error {
	_, _, err := c.gh.Issues.CreateComment(ctx, c.owner, c.repoName, prNumber,
		&github.IssueComment{Body: github.Ptr(comment)})
	if err != nil {
		return fmt.Errorf("commenting on #%d: %w", prNumber, err)
	}

	_, _, err = c.gh.PullRequests.Edit(ctx, c.owner, c.repoName, prNumber,
		&github.PullRequest{State: github.Ptr("closed")})
	if err != nil {
		return fmt.Errorf("closing #%d: %w", prNumber, err)
	}

	slog.Info("closed PR with comment", "pr", prNumber)
	return nil
}

// CreateReplacementPR creates a new draft PR and requests reviewers. Returns
// the new PR number.
func (c *Client) CreateReplacementPR(ctx context.Context, title, body, headBranch, baseBranch string, reviewers []string) (int, error) {
	newPR := &github.NewPullRequest{
		Title: github.Ptr(title),
		Body:  github.Ptr(body),
		Head:  github.Ptr(headBranch),
		Base:  github.Ptr(baseBranch),
		Draft: github.Ptr(true),
	}

	pr, _, err := c.gh.PullRequests.Create(ctx, c.owner, c.repoName, newPR)
	if err != nil {
		return 0, fmt.Errorf("creating replacement PR: %w", err)
	}

	if len(reviewers) > 0 {
		_, _, err = c.gh.PullRequests.RequestReviewers(ctx, c.owner, c.repoName, pr.GetNumber(),
			github.ReviewersRequest{Reviewers: reviewers})
		if err != nil {
			slog.Warn("could not request reviewers",
				"pr", pr.GetNumber(),
				"error", err)
		}
	}

	slog.Info("created replacement PR", "pr", pr.GetNumber())
	return pr.GetNumber(), nil
}

// --- Private helpers ---

// detectEcosystem determines the package ecosystem from PR labels or branch
// name.
func (c *Client) detectEcosystem(pr *github.PullRequest) string {
	for _, label := range pr.Labels {
		name := strings.ToLower(label.GetName())
		switch {
		case strings.Contains(name, "npm") || strings.Contains(name, "javascript"):
			return "npm"
		case strings.Contains(name, "go") || strings.Contains(name, "gomod"):
			return "gomod"
		case strings.Contains(name, "pip") || strings.Contains(name, "python"):
			return "pip"
		case strings.Contains(name, "cargo") || strings.Contains(name, "rust"):
			return "cargo"
		case strings.Contains(name, "github-actions") || strings.Contains(name, "github_actions"):
			return "github-actions"
		}
	}

	branch := pr.GetHead().GetRef()
	switch {
	case strings.Contains(branch, "/npm_and_yarn/"):
		return "npm"
	case strings.Contains(branch, "/go_modules/"):
		return "gomod"
	case strings.Contains(branch, "/pip/"):
		return "pip"
	case strings.Contains(branch, "/cargo/"):
		return "cargo"
	case strings.Contains(branch, "/github_actions/"):
		return "github-actions"
	}

	return "unknown"
}

// checkRunToDetail maps a generic GitHub check run to a CheckDetail. The
// staleness clock is the run's StartedAt; the generic failure detail is the
// check output (summary + text), captured here so no second round-trip is
// needed. Provider-blind: no URL parsing, no provider API.
func checkRunToDetail(run *github.CheckRun) models.CheckDetail {
	var conclusion *string
	if run.Conclusion != nil {
		conclusion = run.Conclusion
	}
	var output string
	if o := run.GetOutput(); o != nil {
		summary := o.GetSummary()
		text := o.GetText()
		switch {
		case summary != "" && text != "":
			output = summary + "\n\n" + text
		case summary != "":
			output = summary
		default:
			output = text
		}
	}
	return models.CheckDetail{
		Name:       run.GetName(),
		Status:     run.GetStatus(),
		Conclusion: conclusion,
		DetailsURL: run.GetDetailsURL(),
		CreatedAt:  run.GetStartedAt().Time,
		Output:     output,
	}
}

// statusToDetail maps a legacy commit status to a CheckDetail. Legacy statuses
// have no check output, so the generic detail is the status description.
func statusToDetail(s *github.RepoStatus) models.CheckDetail {
	state := s.GetState()
	status := "completed"
	var conclusion *string
	if state == "pending" {
		status = "pending"
	} else {
		conclusion = github.Ptr(state)
	}
	return models.CheckDetail{
		Name:       s.GetContext(),
		Status:     status,
		Conclusion: conclusion,
		DetailsURL: s.GetTargetURL(),
		CreatedAt:  s.GetCreatedAt().Time,
		Output:     s.GetDescription(),
	}
}

// aggregateChecks is the single generic check aggregator (de-dupes the former
// PR-context and branch-context switches). It computes Passed/Failed/Pending/
// State plus the per-check lists (Failures = terminal failures only; Checks =
// all checks). It fills each check's CreatedAt fallback to headCommitTime when
// the check carries no usable timestamp (a check cannot predate its commit, so
// the head-commit time is a sound lower bound on the check's age, generically).
// GitHub's own terminal conclusions (stale/timed_out/cancelled) count as
// terminal-non-pass, never pending. Pure.
func aggregateChecks(checks []models.CheckDetail, headCommitTime time.Time) models.CIStatus {
	var passed, failed, pending int
	var failures []models.CheckDetail
	out := make([]models.CheckDetail, len(checks))
	for i, ch := range checks {
		if ch.CreatedAt.IsZero() {
			ch.CreatedAt = headCommitTime
		}
		out[i] = ch
		switch {
		case ch.Conclusion != nil && *ch.Conclusion == "failure":
			failed++
			failures = append(failures, ch)
		case ch.Conclusion != nil && (*ch.Conclusion == "success" || *ch.Conclusion == "neutral" || *ch.Conclusion == "skipped"):
			passed++
		case ch.Status == "in_progress" || ch.Status == "queued" || ch.Status == "pending":
			pending++
		default:
			// Terminal but neither pass nor fail (e.g. GitHub's own
			// stale/timed_out/cancelled, or a legacy "error"). Counts as
			// terminal-non-pass — it does not keep CI pending.
		}
	}

	state := "success"
	if failed > 0 {
		state = "failure"
	} else if pending > 0 {
		state = "pending"
	}

	return models.CIStatus{
		State:    state,
		Total:    len(out),
		Passed:   passed,
		Failed:   failed,
		Pending:  pending,
		Failures: failures,
		Checks:   out,
	}
}

// getCIStatus aggregates check runs and legacy commit statuses for the PR's
// head commit, via the generic GitHub Checks API only.
func (c *Client) getCIStatus(ctx context.Context, pr *github.PullRequest) models.CIStatus {
	headSHA := pr.GetHead().GetSHA()

	// Paginate check runs: a repo with a large CI matrix can exceed one page,
	// and missing runs would make AcceptableGiven decide on incomplete data.
	var checks []models.CheckDetail
	checkOpts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.owner, c.repoName, headSHA, checkOpts)
		if err != nil {
			// Any fetch error (first OR later page) makes the check set incomplete.
			// Return the "unknown" sentinel and discard partial data: an empty or
			// partial set would otherwise read as vacuously settled/green (Settled()
			// over zero checks is true), which on the PR-triage path can lead to an
			// erroneous approve. The orchestrator treats "unknown" as skip-and-revisit.
			slog.Warn("could not fetch check runs — treating CI as indeterminate",
				"pr", pr.GetNumber(),
				"error", err)
			return models.CIStatus{State: "unknown"}
		}
		for _, run := range runs.CheckRuns {
			checks = append(checks, checkRunToDetail(run))
		}
		if resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}

	// Legacy commit statuses (Netlify, pre-commit.ci, etc.), also paginated.
	statusOpts := &github.ListOptions{PerPage: 100}
	for {
		combined, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, c.owner, c.repoName, headSHA, statusOpts)
		if err != nil {
			slog.Warn("could not fetch combined status",
				"pr", pr.GetNumber(),
				"error", err)
			break
		}
		for _, s := range combined.Statuses {
			checks = append(checks, statusToDetail(s))
		}
		if resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	return aggregateChecks(checks, c.headCommitTime(ctx, headSHA))
}

// headCommitTime returns the committer timestamp of a commit, best-effort. Used
// as the staleness-clock fallback for checks with no usable timestamp of their
// own (a check cannot predate its commit). Zero time on any error — callers
// treat a zero fallback as "no staleness clock" (a never-timestamped check then
// blocks rather than being silently aged out).
func (c *Client) headCommitTime(ctx context.Context, sha string) time.Time {
	commit, _, err := c.gh.Repositories.GetCommit(ctx, c.owner, c.repoName, sha, nil)
	if err != nil {
		slog.Warn("could not fetch head commit time for staleness fallback", "sha", sha, "error", err)
		return time.Time{}
	}
	return commit.GetCommit().GetCommitter().GetDate().Time
}

// getDiff builds a unified diff string from the PR's changed files. The
// result is truncated to maxDiffLen characters.
func (c *Client) getDiff(ctx context.Context, pr *github.PullRequest) string {
	// Paginate the file list: a grouped/lockfile bump can change well over one
	// page of files, and missing files beyond page 1 would hide changes from the
	// analyser.
	var parts []string
	opts := &github.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.gh.PullRequests.ListFiles(ctx, c.owner, c.repoName, pr.GetNumber(), opts)
		if err != nil {
			slog.Warn("could not fetch diff",
				"pr", pr.GetNumber(),
				"error", err)
			break // use whatever was gathered so far
		}
		for _, f := range files {
			if patch := f.GetPatch(); patch != "" {
				parts = append(parts, fmt.Sprintf("--- %s\n%s", f.GetFilename(), patch))
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	diff := strings.Join(parts, "\n\n")
	if len(diff) > maxDiffLen {
		diff = diff[:maxDiffLen] + "\n\n... (diff truncated)"
	}
	return diff
}

// findUpstreamRepo attempts to locate the upstream GitHub repository for a
// package.
func (c *Client) findUpstreamRepo(ctx context.Context, packageName, ecosystem string) string {
	// For Go modules, the package path often IS the repo path.
	if ecosystem == "gomod" && strings.Contains(packageName, "github.com/") {
		trimmed := strings.TrimPrefix(packageName, "github.com/")
		parts := strings.SplitN(trimmed, "/", 3)
		if len(parts) >= 2 {
			return fmt.Sprintf("https://github.com/%s/%s", parts[0], parts[1])
		}
	}

	// For npm, the npm registry records the upstream repository directly —
	// far more reliable than guessing via code search, and it handles scoped
	// packages (@scope/name) which are invalid as a repo-search query (Bug #8).
	if ecosystem == "npm" {
		if repo := c.findUpstreamRepoFromNPM(ctx, packageName); repo != "" {
			return repo
		}
	}

	// Fall back to GitHub code search. Scoped names like "@scope/name" are not
	// a valid search query, so normalise to the bare terms.
	searchTerms := strings.ReplaceAll(strings.TrimPrefix(packageName, "@"), "/", " ")
	query := fmt.Sprintf("%s in:name", searchTerms)
	result, _, err := c.gh.Search.Repositories(ctx, query,
		&github.SearchOptions{
			Sort:        "stars",
			ListOptions: github.ListOptions{PerPage: 3},
		})
	if err != nil {
		slog.Warn("could not search for upstream repo",
			"package", packageName,
			"error", err)
		return ""
	}

	normalised := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(packageName, "@", ""), "/", "-"))
	for _, repo := range result.Repositories {
		if strings.Contains(strings.ToLower(repo.GetFullName()), normalised) {
			return repo.GetHTMLURL()
		}
	}

	return ""
}

// npmRepoRe extracts owner/repo from a GitHub URL in any of the forms npm
// package metadata uses (git+https, git://, ssh, .git suffix, etc.).
var npmRepoRe = regexp.MustCompile(`github\.com[:/]([^/]+)/([^/#?]+?)(?:\.git)?/?$`)

// npmHTTPClient bounds calls to the npm registry so a hung response can't block
// a scan goroutine for the whole cycle (the request also honours ctx).
var npmHTTPClient = &http.Client{Timeout: 10 * time.Second}

// normalizeGitHubRepoURL canonicalises a repository reference (as found in npm
// package `repository` fields) to "https://github.com/owner/repo", or "" if it
// isn't a GitHub repository.
func normalizeGitHubRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// npm shorthand: "github:owner/repo".
	if rest, ok := strings.CutPrefix(raw, "github:"); ok {
		rest = strings.TrimSuffix(strings.Trim(rest, "/"), ".git")
		if parts := strings.SplitN(rest, "/", 2); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			return fmt.Sprintf("https://github.com/%s/%s", parts[0], parts[1])
		}
		return ""
	}
	if m := npmRepoRe.FindStringSubmatch(raw); m != nil {
		return fmt.Sprintf("https://github.com/%s/%s", m[1], m[2])
	}
	return ""
}

// findUpstreamRepoFromNPM resolves a package's GitHub repository from the npm
// registry's `repository` field. Returns "" if the package can't be resolved
// or isn't hosted on GitHub.
func (c *Client) findUpstreamRepoFromNPM(ctx context.Context, packageName string) string {
	url := "https://registry.npmjs.org/" + packageName + "/latest"
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return ""
	}
	resp, err := npmHTTPClient.Do(req)
	if err != nil {
		slog.Warn("could not query npm registry for upstream repo", "package", packageName, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	// `repository` may be a string ("github:owner/repo") or an object
	// ({"type":"git","url":"git+https://github.com/owner/repo.git"}).
	var doc struct {
		Repository json.RawMessage `json:"repository"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || len(doc.Repository) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(doc.Repository, &asString) == nil {
		return normalizeGitHubRepoURL(asString)
	}
	var asObject struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(doc.Repository, &asObject) == nil {
		return normalizeGitHubRepoURL(asObject.URL)
	}
	return ""
}

// getReleasesBetween fetches GitHub releases between two version tags
// (inclusive of newVersion, exclusive of oldVersion).
func (c *Client) getReleasesBetween(ctx context.Context, owner, repo, oldVersion, newVersion string) []models.Release {
	oldV := strings.TrimPrefix(oldVersion, "v")
	newV := strings.TrimPrefix(newVersion, "v")

	var releases []models.Release
	collecting := false

	opts := &github.ListOptions{PerPage: 50}
	for {
		rels, resp, err := c.gh.Repositories.ListReleases(ctx, owner, repo, opts)
		if err != nil {
			slog.Warn("could not fetch releases",
				"repo", fmt.Sprintf("%s/%s", owner, repo),
				"error", err)
			return releases
		}

		for _, rel := range rels {
			tag := strings.TrimPrefix(rel.GetTagName(), "v")

			if tag == newV {
				collecting = true
			}
			if collecting {
				body := rel.GetBody()
				if len(body) > maxReleaseBody {
					body = body[:maxReleaseBody]
				}
				releases = append(releases, models.Release{
					Tag:  rel.GetTagName(),
					Name: rel.GetName(),
					Body: body,
				})
			}
			if tag == oldV {
				return releases
			}
			if len(releases) > maxReleases {
				return releases
			}
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return releases
}

// getChangelog attempts to fetch a changelog file from the root of the
// upstream repository. It tries several common filenames.
func (c *Client) getChangelog(ctx context.Context, owner, repo string) string {
	candidates := []string{"CHANGELOG.md", "CHANGES.md", "HISTORY.md", "changelog.md"}

	for _, name := range candidates {
		content, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, name,
			&github.RepositoryContentGetOptions{})
		if err != nil {
			continue
		}

		// GetContent decodes from base64 automatically.
		text, err := content.GetContent()
		if err != nil || text == "" {
			continue
		}

		if len(text) > maxChangelogLen {
			text = text[:maxChangelogLen]
		}
		return text
	}

	return ""
}

// RepoFullName returns the "owner/repo" string.
func (c *Client) RepoFullName() string {
	return c.owner + "/" + c.repoName
}

// CompareCommits returns the list of commits on branch since baseRef, via the
// GitHub comparison API.
func (c *Client) CompareCommits(ctx context.Context, baseRef, branch string) ([]models.CommitInfo, error) {
	comp, _, err := c.gh.Repositories.CompareCommits(ctx, c.owner, c.repoName, baseRef, branch, nil)
	if err != nil {
		return nil, err
	}
	var commits []models.CommitInfo
	for _, rc := range comp.Commits {
		msg := ""
		if rc.Commit != nil && rc.Commit.Message != nil {
			msg = strings.SplitN(*rc.Commit.Message, "\n", 2)[0]
		}
		stat := "(no stats)"
		if rc.Stats != nil {
			files := 0
			if rc.Files != nil {
				files = len(rc.Files)
			}
			stat = fmt.Sprintf("%d+ %d- across %d files",
				rc.Stats.GetAdditions(), rc.Stats.GetDeletions(), files)
		}
		commits = append(commits, models.CommitInfo{
			SHA:      rc.GetSHA()[:7],
			Message:  msg,
			DiffStat: stat,
		})
	}
	return commits, nil
}

// GetBranchCI returns the CI status for the latest commit on a branch.
func (c *Client) GetBranchCI(ctx context.Context, branch string) (*models.CIStatus, error) {
	ref, _, err := c.gh.Repositories.GetBranch(ctx, c.owner, c.repoName, branch, 0)
	if err != nil {
		return nil, err
	}
	sha := ref.GetCommit().GetSHA()
	// Head-commit time is already in the branch payload — use it directly as the
	// staleness-clock fallback (no extra round-trip needed in this path).
	headTime := ref.GetCommit().GetCommit().GetCommitter().GetDate().Time

	// Paginate check runs (mirrors getCIStatus): a large CI matrix exceeds one
	// page, and this status gates un-drafting the replacement PR — deciding on a
	// partial page could wrongly treat a branch as acceptable.
	var checks []models.CheckDetail
	checkOpts := &github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.owner, c.repoName, sha, checkOpts)
		if err != nil {
			return nil, err
		}
		for _, run := range runs.CheckRuns {
			checks = append(checks, checkRunToDetail(run))
		}
		if resp.NextPage == 0 {
			break
		}
		checkOpts.Page = resp.NextPage
	}

	// Legacy commit statuses (Netlify, pre-commit.ci, etc.) — same generic path, paginated.
	statusOpts := &github.ListOptions{PerPage: 100}
	for {
		combined, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, c.owner, c.repoName, sha, statusOpts)
		if err != nil {
			slog.Warn("could not fetch combined status for branch", "branch", branch, "error", err)
			break
		}
		for _, s := range combined.Statuses {
			checks = append(checks, statusToDetail(s))
		}
		if resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	ci := aggregateChecks(checks, headTime)
	return &ci, nil
}
