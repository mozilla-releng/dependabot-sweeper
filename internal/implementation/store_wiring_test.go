package implementation

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/state"
)

func TestPipelineReportStageNilSafe(t *testing.T) {
	var p Pipeline                                                 // store nil
	p.reportStage(1, "pkg", "minor", models.StageImplStarting, "") // must not panic
}

func TestPipelineReportStageWrites(t *testing.T) {
	s := state.NewStore()
	p := Pipeline{store: s}
	p.reportStage(9, "pkg", "major", models.StageImplRunning, "launch turn")
	got, ok := s.Get(9)
	if !ok {
		t.Fatalf("no store entry for PR 9")
	}
	if got.Stage != models.StageImplRunning {
		t.Errorf("Stage = %q, want impl_running", got.Stage)
	}
}

func TestPipelineSetImplMetaNilSafe(t *testing.T) {
	var p Pipeline                                // store nil
	p.setImplMeta(1, "s", "/tmp/w", "auto/fix/x") // must not panic
}

func TestPipelineSetImplMetaWrites(t *testing.T) {
	s := state.NewStore()
	p := Pipeline{store: s}
	p.reportStage(9, "pkg", "major", models.StageImplStarting, "")
	p.setImplMeta(9, "sess", "/tmp/sweeper-impl-abc/repo", "auto/fix/pkg-2.0.0")
	got, _ := s.Get(9)
	if got.SessionID != "sess" || got.WorktreePath != "/tmp/sweeper-impl-abc/repo" || got.ImplBranch != "auto/fix/pkg-2.0.0" {
		t.Errorf("impl meta not recorded: %+v", got)
	}
}

func TestPipelineWithStoreSetsField(t *testing.T) {
	s := state.NewStore()
	p := &Pipeline{}
	p.WithStore(s)
	if p.store != s {
		t.Errorf("WithStore did not set the store field")
	}
}
