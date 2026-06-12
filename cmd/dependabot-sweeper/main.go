// Command dependabot-sweeper reviews and manages dependabot PRs using Claude.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
	"github.com/mozilla-releng/dependabot-sweeper/internal/orchestrator"
)

// stderr returns the process's standard error stream.
func stderr() *os.File { return os.Stderr }

// stringList is a flag.Value that accumulates repeated string flags.
// Use as: --reviewers alice --reviewers bob
type stringList []string

func (s *stringList) String() string     { return fmt.Sprintf("%v", *s) }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	os.Exit(run())
}

func run() int {
	// Subcommand: review
	reviewCmd := flag.NewFlagSet("review", flag.ExitOnError)
	repo := reviewCmd.String("repos", "", "Target repository (owner/repo format) [required]")
	dryRun := reviewCmd.Bool("dry-run", false, "Analyse PRs but don't submit reviews or modify anything")
	verbose := reviewCmd.Bool("verbose", false, "Log full prompts and responses for debugging")
	pr := reviewCmd.Int("pr", 0, "Process only this PR number (useful for testing)")
	var reviewers stringList
	reviewCmd.Var(&reviewers, "reviewers", "GitHub username to request review from on replacement PRs (repeat flag for multiple)")
	var acceptAuthors stringList
	reviewCmd.Var(&acceptAuthors, "accept-author", "Additional PR author login to process, alongside dependabot[bot]/renovate[bot] (repeat flag for multiple)")
	var ignoreChecks stringList
	reviewCmd.Var(&ignoreChecks, "ignore-check", "CI check name to treat as non-blocking — known pre-existing/structural failures unrelated to the bump (repeat flag for multiple)")
	maxImplTime := reviewCmd.Int("max-impl-time", 21600, "Maximum seconds for implementation phase (default 6h — generous so slow CI cycles don't cut off a working agent)")
	maxImplBudget := reviewCmd.Float64("max-impl-budget", 50.00, "Maximum USD spend per PR for the implementation agent (the primary cost guard)")
	maxImplIterations := reviewCmd.Int("max-impl-iterations", 30, "Max CI-fix resume turns per review cycle")
	maxReviewRetries := reviewCmd.Int("max-review-retries", 1, "Times to retry implementation after review rejection")
	implModel := reviewCmd.String("impl-model", "", "Model for implementation agent (default: Claude Code default)")
	analyserModel := reviewCmd.String("analyser-model", "", "Anthropic model for the analysis agent (default: claude-sonnet-4-6)")
	analyserThinkingBudget := reviewCmd.Int("analyser-thinking-budget", 0, "Extended-thinking token budget for the analyser (0 = disabled; requires a thinking-capable model)")
	reviewerModel := reviewCmd.String("reviewer-model", "", "Anthropic model for the reviewer agent (default: claude-sonnet-4-6)")
	reviewerThinkingBudget := reviewCmd.Int("reviewer-thinking-budget", 0, "Extended-thinking token budget for the reviewer (0 = disabled; requires a thinking-capable model)")
	ciVerifyMaxWait := reviewCmd.Int("ci-verify-max-wait", 5400, "Seconds to wait for CI to settle after an implementation push")
	concurrency := reviewCmd.Int("concurrency", 20, "Max PRs to process in parallel")
	ciStaleness := reviewCmd.Duration("ci-staleness", 12*time.Hour, "a CI check pending longer than this (from creation) is treated as stale")
	botName := reviewCmd.String("bot-name", "dependabot-helper", "git committer name for implementation commits")
	botEmail := reviewCmd.String("bot-email", "dependabot-helper@users.noreply.github.com", "git committer email for implementation commits")

	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: dependabot-sweeper <command> [flags]\n\nCommands:\n  review    Review open dependabot PRs (one-shot)\n  worker    Run as a daemon: scan loop + implementation agents, writes to --db\n  web       Run the web dashboard, reads from --db written by worker\n")
		return 1
	}

	switch os.Args[1] {
	case "review":
		reviewCmd.Parse(os.Args[2:])
	case "worker":
		// Root context cancelled on SIGINT/SIGTERM so in-flight API calls abort cleanly.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		opts, err := parseWorkerFlags(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		return runWorker(ctx, opts)
	case "web":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		opts, err := parseWebFlags(os.Args[2:])
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			return 1
		}
		return runWeb(ctx, opts)
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n", os.Args[1])
		return 1
	}

	if *repo == "" {
		fmt.Fprintln(os.Stderr, "Error: --repo is required")
		reviewCmd.Usage()
		return 1
	}

	// Setup logging
	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	// Root context cancelled on SIGINT/SIGTERM so in-flight API calls abort cleanly.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Build config
	opts := []config.Option{
		config.WithMaxImplTime(*maxImplTime),
		config.WithMaxImplBudget(*maxImplBudget),
		config.WithMaxImplIterations(*maxImplIterations),
		config.WithMaxReviewRetries(*maxReviewRetries),
		config.WithCIVerifyMaxWait(*ciVerifyMaxWait),
		config.WithConcurrency(*concurrency),
		config.WithCIStaleness(*ciStaleness),
		config.WithBotName(*botName),
		config.WithBotEmail(*botEmail),
	}
	if *implModel != "" {
		opts = append(opts, config.WithImplModel(*implModel))
	}
	if *analyserModel != "" {
		opts = append(opts, config.WithAnalyserModel(*analyserModel))
	}
	if *analyserThinkingBudget > 0 {
		opts = append(opts, config.WithAnalyserThinkingBudget(*analyserThinkingBudget))
	}
	if *reviewerModel != "" {
		opts = append(opts, config.WithReviewerModel(*reviewerModel))
	}
	if *reviewerThinkingBudget > 0 {
		opts = append(opts, config.WithReviewerThinkingBudget(*reviewerThinkingBudget))
	}
	if len(ignoreChecks) > 0 {
		opts = append(opts, config.WithIgnoreChecks(ignoreChecks))
	}

	cfg, err := config.FromEnv(opts...)
	if err != nil {
		slog.Error("Configuration error", "error", err)
		return 1
	}

	if !*dryRun {
		if err := orchestrator.ValidateModels(ctx, cfg); err != nil {
			slog.Error("Model validation failed", "error", err)
			return 1
		}
	}

	orch, err := orchestrator.New(ctx, cfg, *repo, *dryRun, *verbose, reviewers, acceptAuthors, *pr)
	if err != nil {
		slog.Error("Failed to create orchestrator", "error", err)
		return 1
	}

	results := orch.Run(ctx)
	orchestrator.PrintSummary(results)

	for _, r := range results {
		if !r.Success {
			return 1
		}
	}
	return 0
}
