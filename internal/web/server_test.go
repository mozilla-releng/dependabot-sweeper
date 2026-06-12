package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/state"
)

// stubStatus is a fixed StatusProvider for tests.
type stubStatus struct{}

func (stubStatus) Status() Status {
	return Status{
		LastScan: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
		NextScan: time.Date(2026, 6, 10, 12, 30, 0, 0, time.UTC),
		InFlight: 2,
	}
}

func newTestServer(t *testing.T) (*Server, *state.Store) {
	t.Helper()
	s := state.NewStore()
	// *state.Store satisfies progress.Reader and progress.Notifier; pass it as both.
	srv := NewServer(s, s, stubStatus{}, filepath.Join(os.TempDir(), "sweeper-agent-logs"), "localhost:0")
	return srv, s
}

func TestPRsEndpointReturnsJSON(t *testing.T) {
	srv, store := newTestServer(t)
	store.Report(1, "lodash", "minor", models.StageAnalysing, "")
	store.Report(2, "react", "major", models.StageFlagged, "needs human")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var got []models.PRProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON array: %v\nbody=%s", err, rec.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("got %d PRs, want 2", len(got))
	}
	if got[0].PRNumber != 1 || got[1].PRNumber != 2 {
		t.Errorf("PRs not sorted: %d, %d", got[0].PRNumber, got[1].PRNumber)
	}
}

func TestSinglePREndpoint(t *testing.T) {
	srv, store := newTestServer(t)
	store.Report(42, "pkg", "minor", models.StageWaitingCI, "iter 1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/42", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got models.PRProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.PRNumber != 42 || got.Stage != models.StageWaitingCI {
		t.Errorf("unexpected PR: %+v", got)
	}
}

func TestSinglePRNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/999", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestSinglePRBadNumber(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/notanumber", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.InFlight != 2 {
		t.Errorf("InFlight = %d, want 2", got.InFlight)
	}
}

func TestLogTailReturnsLastLines(t *testing.T) {
	srv, _ := newTestServer(t)
	dir := filepath.Join(os.TempDir(), "sweeper-agent-logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "pr-77-agent.jsonl")
	var b []byte
	for i := 0; i < 300; i++ {
		b = append(b, []byte(`{"line":`+itoa(i)+"}\n")...)
	}
	if err := os.WriteFile(logPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(logPath) })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/77/log", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	lines := splitNonEmptyLines(body)
	if len(lines) != 200 {
		t.Fatalf("got %d log lines, want 200", len(lines))
	}
	if lines[0] != `{"line":100}` {
		t.Errorf("first tailed line = %q, want line 100", lines[0])
	}
	if lines[len(lines)-1] != `{"line":299}` {
		t.Errorf("last tailed line = %q, want line 299", lines[len(lines)-1])
	}
}

func TestLogTailMissingFile(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/424242/log", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a not-yet-created log", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("expected empty body for missing log, got %q", rec.Body.String())
	}
}

func TestTailLinesNoTrailingNewline(t *testing.T) {
	data := []byte("a\nb\nc")
	got := string(tailLines(data, 2))
	if got != "b\nc" {
		t.Errorf("tailLines = %q, want %q", got, "b\nc")
	}
}

func TestTailLinesFewerThanN(t *testing.T) {
	data := []byte("only\none\n")
	got := string(tailLines(data, 200))
	if got != "only\none\n" {
		t.Errorf("tailLines = %q, want whole input", got)
	}
}

func TestTailLinesEmpty(t *testing.T) {
	if got := tailLines(nil, 200); got != nil {
		t.Errorf("tailLines(nil) = %q, want nil", got)
	}
}

// --- tiny test helpers ---

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

func splitNonEmptyLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			if i > start {
				out = append(out, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func TestIndexServesHTML(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	// The SPA shell must be a valid HTML document with a script entry point.
	// The API strings (EventSource, /api/v1/events, etc.) now live in the
	// hashed JS bundle, not index.html itself.
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "<script type", `id="app"`} {
		if !containsStr(body, want) {
			t.Errorf("dashboard HTML missing %q", want)
		}
	}
}

// TestPRsEndpointSerializesVersionsAndCI verifies that the new PRProgress fields
// (old_version, new_version, ecosystem, ci, analysis) flow through the /api/v1/prs
// JSON response unchanged.  This guards against the widened struct being silently
// omitted — since the handler just json.Encodes store.All(), any marshal failure or
// missing omitempty would silently produce the wrong output.
func TestPRsEndpointSerializesVersionsAndCI(t *testing.T) {
	srv, store := newTestServer(t)
	store.Report(7, "lodash", "minor", models.StageAnalysing, "")
	store.SetVersions(7, "4.17.20", "4.17.21", "npm")
	conc := "success"
	store.SetCI(7, models.CIStatus{
		State:  "success",
		Total:  3,
		Passed: 3,
		Checks: []models.CheckDetail{
			{Name: "lint", Status: "completed", Conclusion: &conc},
		},
	})
	store.SetAnalysis(7, models.AgentAnalysis{
		Recommendation:  models.RecommendApprove,
		Confidence:      models.ConfidenceHigh,
		BreakingChanges: []string{"none"},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got []models.PRProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody=%s", err, rec.Body.String())
	}
	if len(got) != 1 {
		t.Fatalf("got %d PRs, want 1", len(got))
	}
	p := got[0]
	if p.OldVersion != "4.17.20" || p.NewVersion != "4.17.21" || p.Ecosystem != "npm" {
		t.Errorf("version metadata wrong: old=%q new=%q eco=%q", p.OldVersion, p.NewVersion, p.Ecosystem)
	}
	if p.CI == nil {
		t.Fatalf("CI nil in JSON response")
	}
	if p.CI.State != "success" || p.CI.Total != 3 || p.CI.Passed != 3 {
		t.Errorf("CI aggregate wrong: %+v", p.CI)
	}
	if len(p.CI.Checks) != 1 || p.CI.Checks[0].Name != "lint" {
		t.Errorf("CI checks wrong: %+v", p.CI.Checks)
	}
	if p.CI.Checks[0].Conclusion == nil || *p.CI.Checks[0].Conclusion != "success" {
		t.Errorf("CI check conclusion wrong: %v", p.CI.Checks[0].Conclusion)
	}
	if p.Analysis == nil {
		t.Fatalf("Analysis nil in JSON response")
	}
	if p.Analysis.Recommendation != models.RecommendApprove {
		t.Errorf("Analysis recommendation = %q, want approve", p.Analysis.Recommendation)
	}
	if len(p.Analysis.BreakingChanges) != 1 || p.Analysis.BreakingChanges[0] != "none" {
		t.Errorf("Analysis breaking changes wrong: %+v", p.Analysis.BreakingChanges)
	}
}

// TestSinglePREndpointSerializesCI mirrors the above for /api/v1/prs/{n}.
func TestSinglePREndpointSerializesCI(t *testing.T) {
	srv, store := newTestServer(t)
	store.Report(99, "axios", "major", models.StageWaitingCI, "")
	store.SetVersions(99, "1.6.0", "2.0.0", "npm")
	conc := "failure"
	store.SetCI(99, models.CIStatus{
		State:  "failure",
		Total:  5,
		Passed: 3,
		Failed: 2,
		Failures: []models.CheckDetail{
			{Name: "test", Status: "completed", Conclusion: &conc},
		},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/prs/99", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got models.PRProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got.OldVersion != "1.6.0" || got.NewVersion != "2.0.0" {
		t.Errorf("version metadata wrong: old=%q new=%q", got.OldVersion, got.NewVersion)
	}
	if got.CI == nil {
		t.Fatalf("CI nil in JSON response")
	}
	if got.CI.State != "failure" || got.CI.Failed != 2 {
		t.Errorf("CI aggregate wrong: %+v", got.CI)
	}
	if len(got.CI.Failures) != 1 || got.CI.Failures[0].Name != "test" {
		t.Errorf("CI failures wrong: %+v", got.CI.Failures)
	}
}

func TestEventsStreamEmitsInitialAndOnReport(t *testing.T) {
	srv, store := newTestServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	store.Report(1, "pkg", "minor", models.StageAnalysing, "")
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !containsStr(body, "event: update") {
		t.Errorf("SSE body missing an 'event: update' frame; got:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

func TestWorkflowEndpoint(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/workflow", nil)
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Deserialise and check basic shape.
	var got struct {
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
		Edges []struct {
			From string `json:"from"`
			To   string `json:"to"`
		} `json:"edges"`
		EntryID string `json:"entryId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %v\nbody=%s", err, rec.Body.String())
	}
	if len(got.Nodes) == 0 {
		t.Error("workflow has no nodes")
	}
	if len(got.Edges) == 0 {
		t.Error("workflow has no edges")
	}
	if got.EntryID == "" {
		t.Error("workflow entryId is empty")
	}
	// Entry node must be in the node list.
	found := false
	for _, n := range got.Nodes {
		if n.ID == got.EntryID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entryId %q not found in nodes", got.EntryID)
	}
}
