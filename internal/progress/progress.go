// Package progress defines the storage abstraction for live PR progress.
// It is depended on by the orchestrator, implementation pipeline, web server,
// service, and sqlitestore packages — it imports only models so none of those
// form a cycle. The production implementation is internal/sqlitestore (SQLite,
// for the "worker"/"web" daemon split).
package progress

import "github.com/mozilla-releng/dependabot-sweeper/internal/models"

// Writer records PR progress. Implemented by sqlitestore.Store.
// The orchestrator and implementation pipeline hold a Writer.
type Writer interface {
	Report(prNumber int, pkg, bump string, stage models.PRStage, detail string)
	SetImplMeta(prNumber int, sessionID, worktreePath, branch string)
	SetReplacementPR(prNumber, n int, replacementURL string)

	// SetCheckpoint records (empty string clears) the resumable implementation-
	// pipeline checkpoint blob for a known PR. No-op for an unknown PR. The blob
	// is opaque to the store; the implementation package owns its shape. Written
	// each time the pipeline yields with CI pending; cleared on terminal outcome.
	SetCheckpoint(prNumber int, checkpoint string)

	// Reap removes rows for any PR whose number is not in openPRs. It is called
	// once per scan, keyed off the full set returned by GetDependabotPRs, so
	// closed PRs disappear from the dashboard on the next scan cycle.
	Reap(openPRs []int)

	// SetVersions records the version metadata (old/new version string and
	// ecosystem) for a known PR. No-op for an unknown PR. Called once at
	// StagePending so every card on the board shows the version diff.
	SetVersions(prNumber int, oldVer, newVer, ecosystem, url string)

	// SetCI records the latest CI snapshot for a known PR. No-op for an unknown
	// PR. Called after the initial fetch (pr.CI from GetDependabotPRs) and after
	// every verifyCI poll in the implementation loop so the drawer tracks the
	// live impl-branch checks.
	SetCI(prNumber int, ci models.CIStatus)

	// SetAnalysis records the analyser's verdict for a known PR. No-op for an
	// unknown PR. Called once after a successful Analyse call.
	SetAnalysis(prNumber int, a models.AgentAnalysis)

	// SetOutcome records the terminal head SHA and stage outcome for a known PR
	// (Bug #23). Called alongside every terminal reportStage so the next scan
	// at the same headSHA can skip re-processing via a DB lookup. No-op when
	// headSHA is empty (used for retriable/transient outcomes).
	SetOutcome(prNumber int, headSHA, outcome string)

	// RecordCreatedPR records that the tool itself opened replacement PR
	// createdPR (the sweeper "fix" PR) for origin dependabot PR originPR. These
	// records are PERMANENT and reap-exempt: they must survive Store.Reap, which
	// prunes pr_progress rows each cycle — otherwise our own PR could be
	// re-ingested and re-processed as a fresh dependabot PR, a runaway-cost
	// incident (Q14 / review C1). The exclusion is the cost-safety backstop to
	// the author filter, not a branch-name heuristic (branch names are spoofable).
	RecordCreatedPR(createdPR, originPR int)
}

// ReadWriter combines Writer and Reader. Used by the orchestrator so it can
// both write progress updates and read back stored outcomes for idempotency
// (Bug #23). sqlitestore.Store satisfies this interface.
type ReadWriter interface {
	Writer
	Reader
}

// Reader exposes PR progress for the dashboard.
type Reader interface {
	All() []models.PRProgress
	Get(prNumber int) (models.PRProgress, bool)

	// CreatedPRs returns the set of PRs the tool created, mapping each sweeper
	// "fix" PR number to its origin dependabot PR number. The orchestrator uses
	// the keys to exclude its own PRs from scans (Q14); the dashboard can use the
	// origin values to render and navigate the dependabot↔sweeper pairing.
	CreatedPRs() map[int]int
}

// Notifier delivers change notifications to SSE subscribers. sqlitestore.Notifier
// is driven by a data_version poller that detects commits made by the writer process.
type Notifier interface {
	Subscribe() chan struct{}
	Unsubscribe(ch chan struct{})
}
