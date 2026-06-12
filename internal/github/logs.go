package ghclient

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

const (
	// maxLogBytes per check fed into the analyser prompt. 10 KB is generous
	// enough to capture the failure tail (stack trace, "FAILED" summary)
	// but small enough that 12 failing checks × 10 KB = 120 KB stays well
	// within Claude's input budget.
	maxLogBytes = 10_000
)

// FetchFailureLogs returns a map from check name to its failure detail, for each
// failing check on the PR's head SHA. The detail is the generic check `output`
// (summary + text) captured during aggregation — no provider API, no URL
// parsing. Best-effort: a check with no output yields an empty string entry,
// which the caller can display as "log unavailable".
func (c *Client) FetchFailureLogs(_ context.Context, pr models.DependabotPR) map[string]string {
	return c.fetchFailureLogs(pr.CI, pr.Number)
}

// FetchBranchFailureLogs returns a map from check name to its failure detail,
// for each failing check in the given branch CI status. It is the branch-context
// counterpart of FetchFailureLogs (which keys off a PR): the orchestrator's
// CI-fix loop verifies CI for a branch (not a PR object) and needs the failing
// checks' detail to feed a resume turn. `prNumber` is used only for log context.
func (c *Client) FetchBranchFailureLogs(_ context.Context, ci models.CIStatus, prNumber int) map[string]string {
	return c.fetchFailureLogs(ci, prNumber)
}

// fetchFailureLogs is the shared implementation behind FetchFailureLogs and
// FetchBranchFailureLogs. It is a pure common-denominator reader: each failing
// check's detail is its generic check `output`, captured by the aggregator,
// truncated to the maxLogBytes tail budget. No provider dispatch, no network.
// Best-effort — a check with no captured output yields an empty string entry.
func (c *Client) fetchFailureLogs(ci models.CIStatus, _ int) map[string]string {
	out := make(map[string]string, len(ci.Failures))
	for _, check := range ci.Failures {
		if check.Output == "" {
			out[check.Name] = ""
			continue
		}
		tail, err := readTail(strings.NewReader(check.Output), maxLogBytes)
		if err != nil {
			out[check.Name] = ""
			continue
		}
		out[check.Name] = tail
	}
	return out
}

// readTail reads the entire reader and returns the last n bytes (or fewer,
// if the content is smaller). For failure detail we keep the tail — it
// typically contains the failure summary / stack trace.
func readTail(r io.Reader, n int) (string, error) {
	// Read at most n+1MB to bound memory if the content is enormous.
	limited := io.LimitReader(r, int64(n)+1<<20)
	all, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	if len(all) <= n {
		return string(all), nil
	}
	return "[... log truncated to last " + strconv.Itoa(n) + " bytes ...]\n" + string(all[len(all)-n:]), nil
}
