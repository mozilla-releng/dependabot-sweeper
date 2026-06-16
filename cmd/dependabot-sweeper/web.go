package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/sqlitestore"
	"github.com/mozilla-releng/dependabot-sweeper/internal/web"
)

type webOptions struct {
	listenAddr   string
	db           string
	logDir       string
	dataDir      string
	repo         string
	pollInterval time.Duration
	verbose      bool
}

func parseWebFlags(args []string) (*webOptions, error) {
	fs := flag.NewFlagSet("web", flag.ContinueOnError)
	o := &webOptions{}
	fs.StringVar(&o.listenAddr, "listen-addr", "localhost:8080", "Address for the web dashboard")
	fs.StringVar(&o.db, "db", resolveFlag("", "SWEEPER_DB_PATH", "dependabot-sweeper.db"),
		"Path to the shared SQLite database (must match --db on the worker process) [env: SWEEPER_DB_PATH]")
	fs.StringVar(&o.logDir, "log-dir", resolveFlag("", "SWEEPER_LOG_DIR", ""),
		"Legacy: flat directory for per-PR agent JSONL logs; superseded by --data-dir [env: SWEEPER_LOG_DIR]")
	fs.StringVar(&o.dataDir, "data-dir", resolveFlag("", "SWEEPER_DATA_DIR", ""),
		"Sweeper data directory for canonical per-PR log paths; must match --data-dir on the worker process [env: SWEEPER_DATA_DIR]")
	fs.StringVar(&o.repo, "repo", resolveFlag("", "SWEEPER_REPO", ""),
		"Target repository (owner/repo format) — used with --data-dir to construct per-PR log paths [env: SWEEPER_REPO]")
	fs.DurationVar(&o.pollInterval, "poll-interval", time.Second,
		"How often to poll the database for changes and push SSE updates to the browser")
	fs.BoolVar(&o.verbose, "verbose", false, "Log HTTP requests and DB polling details")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.db == "" {
		return nil, fmt.Errorf("--db is required (or set SWEEPER_DB_PATH)")
	}
	return o, nil
}

func runWeb(ctx context.Context, o *webOptions) int {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr(), &slog.HandlerOptions{Level: level})))

	store, err := sqlitestore.Open(o.db, false /*reader*/)
	if err != nil {
		slog.Error("Failed to open database", "path", o.db, "error", err)
		return 1
	}
	defer store.Close()
	slog.Info("Web server database opened", "path", o.db)

	// data_version notifier: fires the SSE broadcast whenever the worker process
	// commits a write to the shared SQLite file.
	notifier := sqlitestore.NewNotifier(store.DB(), o.pollInterval)
	go notifier.Run(ctx) //nolint:errcheck

	statusReader := sqlitestore.NewStatusReader(store.DB())
	srv := web.NewServer(store, notifier, statusReader, o.logDir, o.listenAddr)
	if o.dataDir != "" && o.repo != "" {
		// Canonical per-PR log path: derive the repo slug (owner/repo → owner-repo)
		// and configure the server to find logs under the per-PR workdir.
		repoSlug := strings.ReplaceAll(o.repo, "/", "-")
		srv.WithDataDir(o.dataDir, repoSlug)
		slog.Info("Using canonical per-PR log paths", "dataDir", o.dataDir, "repoSlug", repoSlug)
	}
	slog.Info("Dashboard listening", "addr", o.listenAddr)

	if err := srv.ListenAndServe(ctx); err != nil {
		slog.Error("Web server stopped with error", "error", err)
		return 1
	}
	return 0
}
