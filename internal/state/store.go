// Package state holds the live, in-memory progress of every PR currently being
// processed. It is the single source of truth for the web dashboard when the
// in-process daemon is used. The store is safe for concurrent use: the
// orchestrator and implementation pipeline call the mutators from many goroutines
// while the web server reads via Get/All and streams changes via Subscribe.
//
// It depends only on the models and progress packages and the standard library —
// it must never import orchestrator, implementation, service, or web (that would
// create an import cycle).
package state

import (
	"sort"
	"sync"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
)

// Compile-time assertions: *Store must satisfy all progress interfaces.
var (
	_ progress.Writer     = (*Store)(nil)
	_ progress.Reader     = (*Store)(nil)
	_ progress.Notifier   = (*Store)(nil)
	_ progress.ReadWriter = (*Store)(nil)
)

// Store is a mutex-guarded map of PR number -> live progress, plus a set of
// subscriber channels for change broadcasts (used by the SSE endpoint).
type Store struct {
	mu          sync.RWMutex
	prs         map[int]*models.PRProgress
	subscribers map[chan struct{}]struct{}
}

// NewStore returns an empty, ready-to-use Store.
func NewStore() *Store {
	return &Store{
		prs:         make(map[int]*models.PRProgress),
		subscribers: make(map[chan struct{}]struct{}),
	}
}

// Report records that PR `prNumber` has entered `stage`. The first call for a PR
// creates its entry (and records pkg/bump); later calls update the stage and
// append a StageEvent to the timeline. pkg/bump are only set on creation, so a
// later Report with empty pkg/bump won't wipe the metadata. A change broadcast
// is sent to all subscribers (non-blocking).
func (s *Store) Report(prNumber int, pkg, bump string, stage models.PRStage, detail string) {
	now := time.Now()
	s.mu.Lock()
	p, ok := s.prs[prNumber]
	if !ok {
		p = &models.PRProgress{
			PRNumber:    prNumber,
			PackageName: pkg,
			BumpType:    bump,
		}
		s.prs[prNumber] = p
	}
	p.Stage = stage
	p.LastUpdated = now
	p.History = append(p.History, models.StageEvent{Stage: stage, At: now, Detail: detail})
	s.mu.Unlock()
	s.broadcast()
}

// SetImplMeta records the implementation worktree metadata for a PR that is
// already known to the store. It is a no-op (and does NOT create an entry) for
// an unknown PR — the pipeline always reports a stage before setting meta.
func (s *Store) SetImplMeta(prNumber int, sessionID, worktreePath, branch string) {
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		p.SessionID = sessionID
		p.WorktreePath = worktreePath
		p.ImplBranch = branch
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// SetReplacementPR records the replacement PR number for a known PR. No-op for
// an unknown PR.
func (s *Store) SetReplacementPR(prNumber, n int) {
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		v := n
		p.ReplacementPR = &v
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// SetVersions records the version metadata for a known PR. No-op for an unknown PR.
func (s *Store) SetVersions(prNumber int, oldVer, newVer, ecosystem string) {
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		p.OldVersion = oldVer
		p.NewVersion = newVer
		p.Ecosystem = ecosystem
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// SetCI records the latest CI snapshot for a known PR. No-op for an unknown PR.
func (s *Store) SetCI(prNumber int, ci models.CIStatus) {
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		cp := copyCI(ci)
		p.CI = &cp
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// SetOutcome records the terminal head SHA and outcome for a known PR (Bug #23).
// No-op for an unknown PR or when headSHA is empty.
func (s *Store) SetOutcome(prNumber int, headSHA, outcome string) {
	if headSHA == "" {
		return
	}
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		p.HeadSHA = headSHA
		p.Outcome = outcome
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// SetAnalysis records the analyser verdict for a known PR. No-op for an unknown PR.
func (s *Store) SetAnalysis(prNumber int, a models.AgentAnalysis) {
	s.mu.Lock()
	if p, ok := s.prs[prNumber]; ok {
		cp := copyAnalysis(a)
		p.Analysis = &cp
		p.LastUpdated = time.Now()
	}
	s.mu.Unlock()
	s.broadcast()
}

// Reap removes entries for every PR whose number is not in openPRs. Called once
// per scan with the authoritative open-PR set from GitHub. Broadcasts if any
// rows were removed so the dashboard updates immediately.
func (s *Store) Reap(openPRs []int) {
	open := make(map[int]bool, len(openPRs))
	for _, n := range openPRs {
		open[n] = true
	}
	s.mu.Lock()
	changed := false
	for n := range s.prs {
		if !open[n] {
			delete(s.prs, n)
			changed = true
		}
	}
	s.mu.Unlock()
	if changed {
		s.broadcast()
	}
}

// Get returns a deep copy of a PR's progress and whether it exists. The History
// slice and ReplacementPR pointer are copied so a caller can never mutate
// internal state.
func (s *Store) Get(prNumber int) (models.PRProgress, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.prs[prNumber]
	if !ok {
		return models.PRProgress{}, false
	}
	return clone(p), true
}

// All returns deep copies of every PR's progress, sorted ascending by PR number
// for stable dashboard ordering.
func (s *Store) All() []models.PRProgress {
	s.mu.RLock()
	out := make([]models.PRProgress, 0, len(s.prs))
	for _, p := range s.prs {
		out = append(out, clone(p))
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].PRNumber < out[j].PRNumber })
	return out
}

// Subscribe returns a new buffered channel that receives an empty struct on
// every store change. Each subscriber gets its own channel. Callers MUST call
// Unsubscribe when done (e.g. when the SSE connection closes) to free it.
func (s *Store) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

// Unsubscribe removes and closes a subscriber channel. Safe to call once per
// channel; calling it twice for the same channel is a no-op.
func (s *Store) Unsubscribe(ch chan struct{}) {
	s.mu.Lock()
	if _, ok := s.subscribers[ch]; ok {
		delete(s.subscribers, ch)
		close(ch)
	}
	s.mu.Unlock()
}

// broadcast does a non-blocking send to every subscriber. A subscriber whose
// buffer is already full simply misses this tick (it will catch up on the next
// tick or its periodic poll); a slow reader can never block a writer.
func (s *Store) broadcast() {
	s.mu.RLock()
	for ch := range s.subscribers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.mu.RUnlock()
}

// clone returns a value copy of a PRProgress with its slice/pointer fields
// independently copied, so callers cannot mutate the store's internals.
func clone(p *models.PRProgress) models.PRProgress {
	cp := *p
	if p.History != nil {
		cp.History = make([]models.StageEvent, len(p.History))
		copy(cp.History, p.History)
	}
	if p.ReplacementPR != nil {
		v := *p.ReplacementPR
		cp.ReplacementPR = &v
	}
	if p.CI != nil {
		ci := copyCI(*p.CI)
		cp.CI = &ci
	}
	if p.Analysis != nil {
		a := copyAnalysis(*p.Analysis)
		cp.Analysis = &a
	}
	return cp
}

// copyCheckDetails returns a deep copy of a []CheckDetail slice, including the
// *string Conclusion pointer in each element.
func copyCheckDetails(src []models.CheckDetail) []models.CheckDetail {
	if src == nil {
		return nil
	}
	dst := make([]models.CheckDetail, len(src))
	for i, c := range src {
		dst[i] = c
		if c.Conclusion != nil {
			v := *c.Conclusion
			dst[i].Conclusion = &v
		}
	}
	return dst
}

// copyCI returns a value copy of a CIStatus with its slice fields deep-copied.
func copyCI(ci models.CIStatus) models.CIStatus {
	cp := ci
	cp.Failures = copyCheckDetails(ci.Failures)
	cp.Checks = copyCheckDetails(ci.Checks)
	return cp
}

// copyAnalysis returns a value copy of an AgentAnalysis with its slice fields
// deep-copied.
func copyAnalysis(a models.AgentAnalysis) models.AgentAnalysis {
	cp := a
	if a.BreakingChanges != nil {
		cp.BreakingChanges = make([]string, len(a.BreakingChanges))
		copy(cp.BreakingChanges, a.BreakingChanges)
	}
	if a.Deprecations != nil {
		cp.Deprecations = make([]string, len(a.Deprecations))
		copy(cp.Deprecations, a.Deprecations)
	}
	if a.CodebaseImpact != nil {
		cp.CodebaseImpact = make([]models.CodeImpact, len(a.CodebaseImpact))
		copy(cp.CodebaseImpact, a.CodebaseImpact)
	}
	if a.CodeChanges != nil {
		cp.CodeChanges = make([]models.CodeChangeEntry, len(a.CodeChanges))
		copy(cp.CodeChanges, a.CodeChanges)
	}
	return cp
}
