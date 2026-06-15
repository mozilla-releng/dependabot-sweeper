// Package web serves the live dashboard for the persistent service. It reads
// from a progress.Reader / progress.Notifier and a StatusProvider (scan timing)
// and exposes JSON, an SSE event stream, an agent-log tail, and a Svelte SPA
// dashboard. It uses only the standard library — no web framework.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/progress"
	"github.com/mozilla-releng/dependabot-sweeper/internal/workflow"
)

//go:embed all:ui/dist
var distFS embed.FS

// distSub is the ui/dist sub-tree of the embedded filesystem, rooted at the
// dist directory so paths like "/index.html" resolve correctly.
var distSub = func() fs.FS {
	s, err := fs.Sub(distFS, "ui/dist")
	if err != nil {
		panic("web: could not sub ui/dist from embed: " + err.Error())
	}
	return s
}()

// Status is the scan-timing snapshot shown on the dashboard.
type Status struct {
	LastScan time.Time `json:"last_scan"`
	NextScan time.Time `json:"next_scan"`
	InFlight int       `json:"in_flight"`
}

// StatusProvider supplies the current scan-timing snapshot.
type StatusProvider interface {
	Status() Status
}

const logTailLines = 200

// Server exposes the store over HTTP.
type Server struct {
	store  progress.Reader
	notify progress.Notifier
	status StatusProvider
	logDir string
	addr   string
}

// NewServer builds a Server bound (logically) to addr. logDir is the directory
// where the implementation pipeline writes per-PR agent logs; it must be the
// same path both worker and web processes use (see --log-dir / SWEEPER_LOG_DIR).
func NewServer(store progress.Reader, notify progress.Notifier, status StatusProvider, logDir, addr string) *Server {
	return &Server{store: store, notify: notify, status: status, logDir: logDir, addr: addr}
}

// agentLogPath returns the on-disk path of a PR's live agent JSON log.
func (s *Server) agentLogPath(prNumber int) string {
	return filepath.Join(s.logDir, fmt.Sprintf("pr-%d-agent.jsonl", prNumber))
}

// Handler returns the fully-wired http.Handler (the route mux).
//
// Public by design: this dashboard is intentionally served without
// authentication. It is a read-only, open-source prototype — every route is a
// GET, there is no admin interface, and it exposes only PR/CI triage state.
// There is deliberately no auth middleware here.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleSPA)
	mux.HandleFunc("GET /api/v1/prs", s.handlePRs)
	mux.HandleFunc("GET /api/v1/prs/{n}", s.handlePR)
	mux.HandleFunc("GET /api/v1/prs/{n}/log", s.handleLog)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/events", s.handleEvents)
	mux.HandleFunc("GET /api/v1/workflow", s.handleWorkflow)
	return mux
}

// ListenAndServe runs the HTTP server until ctx is cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	httpSrv := &http.Server{
		Addr:    s.addr,
		Handler: s.Handler(),
		// ReadHeaderTimeout/ReadTimeout guard against slow-client header/body
		// attacks. No WriteTimeout: the /api/v1/events SSE stream is long-lived
		// and a write deadline would sever it. IdleTimeout bounds keep-alive.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handlePRs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.All())
}

func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}
	p, ok := s.store.Get(n)
	if !ok {
		http.Error(w, "PR not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.status.Status())
}

func (s *Server) handleLog(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.Error(w, "invalid PR number", http.StatusBadRequest)
		return
	}
	data, err := os.ReadFile(s.agentLogPath(n))
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "could not read log", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(tailLines(data, logTailLines))
}

// tailLines returns the last n newline-delimited lines of data.
func tailLines(data []byte, n int) []byte {
	if len(data) == 0 || n <= 0 {
		return nil
	}
	count := 0
	end := len(data)
	i := end - 1
	if data[i] == '\n' {
		i--
	}
	for ; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n {
				return data[i+1:]
			}
		}
	}
	return data
}

// handleSPA serves the Svelte SPA from the embedded ui/dist tree. Any path
// not found in the tree falls back to index.html so client-side routing works.
// The /api/v1/... routes registered above take precedence over this catch-all.
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/" {
		path = "/index.html"
	}
	// Strip leading slash for FS lookup.
	name := path[1:]
	f, err := distSub.Open(name)
	if err != nil {
		// File not found — serve the SPA shell for client-side routing.
		s.serveIndex(w)
		return
	}
	f.Close()
	http.FileServerFS(distSub).ServeHTTP(w, r)
}

func (s *Server) serveIndex(w http.ResponseWriter) {
	data, err := fs.ReadFile(distSub, "index.html")
	if err != nil {
		http.Error(w, "dashboard not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// handleWorkflow returns the canonical workflow graph as JSON. The graph is
// static (pure data from workflow.Spec()) — no store access required.
func (s *Server) handleWorkflow(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, workflow.Spec())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := s.notify.Subscribe()
	defer s.notify.Unsubscribe(ch)

	// Initial frame so a freshly-connected client renders immediately.
	fmt.Fprint(w, "event: update\ndata: {}\n\n")
	flusher.Flush()

	ctx := r.Context()
	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ch:
			if !open {
				return
			}
			fmt.Fprint(w, "event: update\ndata: {}\n\n")
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}
