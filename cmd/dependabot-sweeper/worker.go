package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
	"github.com/mozilla-releng/dependabot-sweeper/internal/orchestrator"
	"github.com/mozilla-releng/dependabot-sweeper/internal/service"
	"github.com/mozilla-releng/dependabot-sweeper/internal/sqlitestore"
)

type workerOptions struct {
	repo                   string
	verbose                bool
	interval               time.Duration
	db                     string
	logDir                 string
	reviewers              []string
	acceptAuthors          []string
	ignoreChecks           []string
	maxImplTime            int
	maxImplBudget          float64
	maxImplIterations      int
	maxNoProgressIters     int
	maxReviewRetries       int
	implModel              string
	analyserModel          string
	analyserThinkingBudget int
	reviewerModel          string
	reviewerThinkingBudget int
	ciVerifyMaxWait        int
	concurrency            int
	ciStaleness            time.Duration
	botName                string
	botEmail               string
	minBumpToEngage        string
}

func parseWorkerFlags(args []string) (*workerOptions, error) {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	o := &workerOptions{}
	fs.StringVar(&o.repo, "repos", "", "Target repository (owner/repo format) [required]")
	fs.BoolVar(&o.verbose, "verbose", false, "Log full prompts and responses for debugging")
	fs.DurationVar(&o.interval, "interval", 30*time.Minute, "Time between scans")
	fs.StringVar(&o.db, "db", resolveFlag("", "SWEEPER_DB_PATH", "dependabot-sweeper.db"),
		"Path to the shared SQLite database (worker and web MUST use the same file) [env: SWEEPER_DB_PATH]")
	fs.StringVar(&o.logDir, "log-dir", resolveFlag("", "SWEEPER_LOG_DIR", filepath.Join(os.TempDir(), "sweeper-agent-logs")),
		"Directory for per-PR agent JSONL logs; must match --log-dir on the web process [env: SWEEPER_LOG_DIR]")
	var reviewers stringList
	fs.Var(&reviewers, "reviewers", "GitHub username to request review from on replacement PRs (repeat flag for multiple)")
	var acceptAuthors stringList
	fs.Var(&acceptAuthors, "accept-author", "Additional PR author login to process (repeat flag for multiple)")
	var ignoreChecks stringList
	fs.Var(&ignoreChecks, "ignore-check", "CI check name to treat as non-blocking (repeat flag for multiple)")
	fs.IntVar(&o.maxImplTime, "max-impl-time", 21600, "Maximum seconds for implementation phase")
	fs.Float64Var(&o.maxImplBudget, "max-impl-budget", 50.00, "Maximum USD spend per PR for the implementation agent")
	fs.IntVar(&o.maxImplIterations, "max-impl-iterations", 30, "Max CI-fix resume turns per review cycle")
	fs.IntVar(&o.maxNoProgressIters, "max-no-progress-iterations", 8, "Give up after this many consecutive settled CI-fix attempts with no improvement in the failing-check count (Q12)")
	fs.IntVar(&o.maxReviewRetries, "max-review-retries", 1, "Times to retry implementation after review rejection")
	fs.StringVar(&o.implModel, "impl-model", "", "Model for implementation agent")
	fs.StringVar(&o.analyserModel, "analyser-model", "", "Anthropic model for the analysis agent")
	fs.IntVar(&o.analyserThinkingBudget, "analyser-thinking-budget", 0, "Extended-thinking token budget for the analyser")
	fs.StringVar(&o.reviewerModel, "reviewer-model", "", "Anthropic model for the reviewer agent")
	fs.IntVar(&o.reviewerThinkingBudget, "reviewer-thinking-budget", 0, "Extended-thinking token budget for the reviewer")
	fs.IntVar(&o.ciVerifyMaxWait, "ci-verify-max-wait", 5400, "Seconds to wait for CI to settle after an implementation push")
	fs.IntVar(&o.concurrency, "concurrency", 20, "Max PRs to process in parallel")
	fs.DurationVar(&o.ciStaleness, "ci-staleness", 12*time.Hour, "a CI check pending longer than this (from creation) is treated as stale")
	fs.StringVar(&o.botName, "bot-name", "dependabot-helper", "git committer name for implementation commits")
	fs.StringVar(&o.botEmail, "bot-email", "dependabot-helper@users.noreply.github.com", "git committer email for implementation commits")
	fs.StringVar(&o.minBumpToEngage, "min-bump-to-engage", "major", "Lowest bump severity to engage (major|minor|patch); bumps below it are skipped out of policy")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if o.repo == "" {
		return nil, fmt.Errorf("--repo is required")
	}
	o.reviewers = reviewers
	o.acceptAuthors = acceptAuthors
	o.ignoreChecks = ignoreChecks
	return o, nil
}

func buildWorkerConfig(o *workerOptions) (*config.Config, error) {
	opts := []config.Option{
		config.WithMaxImplTime(o.maxImplTime),
		config.WithMaxImplBudget(o.maxImplBudget),
		config.WithMaxImplIterations(o.maxImplIterations),
		config.WithMaxNoProgressIterations(o.maxNoProgressIters),
		config.WithMaxReviewRetries(o.maxReviewRetries),
		config.WithCIVerifyMaxWait(o.ciVerifyMaxWait),
		config.WithConcurrency(o.concurrency),
		config.WithCIStaleness(o.ciStaleness),
		config.WithBotName(o.botName),
		config.WithBotEmail(o.botEmail),
		config.WithMinBumpToEngage(models.BumpType(o.minBumpToEngage)),
	}
	if o.implModel != "" {
		opts = append(opts, config.WithImplModel(o.implModel))
	}
	if o.analyserModel != "" {
		opts = append(opts, config.WithAnalyserModel(o.analyserModel))
	}
	if o.analyserThinkingBudget > 0 {
		opts = append(opts, config.WithAnalyserThinkingBudget(o.analyserThinkingBudget))
	}
	if o.reviewerModel != "" {
		opts = append(opts, config.WithReviewerModel(o.reviewerModel))
	}
	if o.reviewerThinkingBudget > 0 {
		opts = append(opts, config.WithReviewerThinkingBudget(o.reviewerThinkingBudget))
	}
	if len(o.ignoreChecks) > 0 {
		opts = append(opts, config.WithIgnoreChecks(o.ignoreChecks))
	}
	return config.FromEnv(opts...)
}

func runWorker(ctx context.Context, o *workerOptions) int {
	level := slog.LevelInfo
	if o.verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(stderr(), &slog.HandlerOptions{Level: level})))

	cfg, err := buildWorkerConfig(o)
	if err != nil {
		slog.Error("Configuration error", "error", err)
		return 1
	}

	if err := orchestrator.ValidateModels(ctx, cfg); err != nil {
		slog.Error("Model validation failed", "error", err)
		return 1
	}

	store, err := sqlitestore.Open(o.db, true /*writer*/)
	if err != nil {
		slog.Error("Failed to open database", "path", o.db, "error", err)
		return 1
	}
	defer store.Close()
	slog.Info("Worker database opened", "path", o.db)

	scan := func(scanCtx context.Context) []models.ReviewResult {
		orch, err := orchestrator.New(scanCtx, cfg, o.repo, false, o.verbose, o.reviewers, o.acceptAuthors, 0)
		if err != nil {
			slog.Error("Failed to build orchestrator for this scan — will retry next tick", "error", err)
			return nil
		}
		orch.WithStore(store)
		orch.WithLogDir(o.logDir)
		return orch.Run(scanCtx)
	}

	svc := service.New(scan, o.interval, store)
	if err := svc.Run(ctx); err != nil {
		slog.Error("Service stopped with error", "error", err)
		return 1
	}
	return 0
}

// resolveFlag returns flagVal if non-empty, else the env var envName, else def.
func resolveFlag(flagVal, envName, def string) string {
	if flagVal != "" {
		return flagVal
	}
	if v := os.Getenv(envName); v != "" {
		return v
	}
	return def
}
