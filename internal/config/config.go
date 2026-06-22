// Package config loads configuration from environment variables and .env files.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config holds all configuration for the pipeline.
type Config struct {
	GitHubToken             string
	AnthropicAPIKey         string
	MaxImplTime             int     // seconds
	MaxImplBudget           float64 // USD
	MaxReviewRetries        int
	ImplModel               string   // empty = Claude Code default
	AnalyserModel           string   // empty = use the claude-sonnet-4-6 default set in FromEnv
	AnalyserThinkingBudget  int      // tokens for extended thinking in the analyser (0 = disabled)
	ReviewerModel           string   // empty = use the claude-sonnet-4-6 default set in FromEnv
	ReviewerThinkingBudget  int      // tokens for extended thinking in the reviewer (0 = disabled)
	CombinedAgentModel      string   // empty = Claude Code default (combined analysis+decision agent, Q10)
	CombinedAgentBudget     float64  // max USD spend per PR for the combined agent (default 20.0)
	IgnoreChecks            []string // CI check names treated as non-blocking (known pre-existing/structural failures)
	Concurrency             int      // max PRs processed in parallel
	MaxImplIterations       int      // max CI-fix resume turns per review cycle (initial turn not counted)
	CIVerifyMaxWait         int      // seconds to wait for CI to settle after implementation push
	MaxNoProgressIterations int      // consecutive settled CI-fix attempts with no improvement in the failing-check floor before giving up (Q12; default 8)

	CIStaleness time.Duration // a check pending longer than this (from creation) is "stale"
	BotName     string        // git committer identity for impl commits
	BotEmail    string

	// DataDir is the root of the sweeper's persistent data: per-PR working
	// directories and the base bare clone. Defaults to
	// $SWEEPER_DATA_DIR or ~/.local/share/dependabot-sweeper.
	// When empty, per-PR workdirs fall back to os.MkdirTemp.
	DataDir string
}

// FromEnv creates a Config from environment variables and optional .env file.
// Override fields can be passed via the opts parameter.
func FromEnv(opts ...Option) (*Config, error) {
	loadDotenv()

	token := os.Getenv("DEPENDABOT_REVIEWER_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("DEPENDABOT_REVIEWER_TOKEN environment variable is required — set it to a GitHub API token for the bot account that will submit reviews")
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY environment variable is required — set it to your Anthropic API key for Claude access")
	}

	cfg := &Config{
		GitHubToken:      token,
		AnthropicAPIKey:  apiKey,
		MaxImplTime:      21600, // 6h — generous so a slow CI cycle never guillotines a working agent; cost is bounded by MaxImplBudget instead
		MaxImplBudget:    50.00, // primary cost guard per PR (a looping agent hits this long before the time cap)
		MaxReviewRetries: 1,
		Concurrency:      20,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	if cfg.MaxImplIterations == 0 {
		cfg.MaxImplIterations = 30
	}
	if cfg.CIVerifyMaxWait == 0 {
		cfg.CIVerifyMaxWait = 5400
	}
	if cfg.MaxNoProgressIterations == 0 {
		cfg.MaxNoProgressIterations = 8
	}
	if cfg.AnalyserModel == "" {
		cfg.AnalyserModel = "claude-sonnet-4-6"
	}
	if cfg.ReviewerModel == "" {
		cfg.ReviewerModel = "claude-sonnet-4-6"
	}
	if cfg.CombinedAgentBudget == 0 {
		cfg.CombinedAgentBudget = 20.0
	}
	if cfg.CIStaleness == 0 {
		cfg.CIStaleness = 12 * time.Hour
	}
	if cfg.BotName == "" {
		cfg.BotName = "dependabot-helper"
	}
	if cfg.BotEmail == "" {
		cfg.BotEmail = "dependabot-helper@users.noreply.github.com"
	}
	if cfg.DataDir == "" {
		if v := os.Getenv("SWEEPER_DATA_DIR"); v != "" {
			cfg.DataDir = v
		} else {
			home, err := os.UserHomeDir()
			if err == nil {
				cfg.DataDir = filepath.Join(home, ".local", "share", "dependabot-sweeper")
			} else {
				cfg.DataDir = filepath.Join(os.TempDir(), "sweeper-data")
			}
		}
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate checks that numeric fields are in valid ranges.
func (c *Config) validate() error {
	if c.Concurrency <= 0 {
		return fmt.Errorf("--concurrency must be > 0, got %d", c.Concurrency)
	}
	if c.MaxImplIterations <= 0 {
		return fmt.Errorf("--max-impl-iterations must be > 0, got %d", c.MaxImplIterations)
	}
	if c.MaxNoProgressIterations <= 0 {
		return fmt.Errorf("--max-no-progress-iterations must be > 0, got %d", c.MaxNoProgressIterations)
	}
	if c.MaxImplTime <= 0 {
		return fmt.Errorf("--max-impl-time must be > 0, got %d", c.MaxImplTime)
	}
	if c.MaxImplBudget <= 0 {
		return fmt.Errorf("--max-impl-budget must be > 0, got %.2f", c.MaxImplBudget)
	}
	if c.CIVerifyMaxWait <= 0 {
		return fmt.Errorf("--ci-verify-max-wait must be > 0, got %d", c.CIVerifyMaxWait)
	}
	if c.MaxReviewRetries < 0 {
		return fmt.Errorf("--max-review-retries must be >= 0, got %d", c.MaxReviewRetries)
	}
	if err := validateThinkingBudget("--analyser-thinking-budget", c.AnalyserThinkingBudget); err != nil {
		return err
	}
	if err := validateThinkingBudget("--reviewer-thinking-budget", c.ReviewerThinkingBudget); err != nil {
		return err
	}
	return nil
}

// validateThinkingBudget rejects values that are either negative or in the
// 1–1023 range that the Anthropic API rejects (minimum is 1024).
func validateThinkingBudget(flag string, v int) error {
	if v < 0 {
		return fmt.Errorf("%s must be >= 0, got %d", flag, v)
	}
	if v > 0 && v < 1024 {
		return fmt.Errorf("%s must be 0 (disabled) or >= 1024, got %d (Anthropic API minimum is 1024)", flag, v)
	}
	return nil
}

// Option is a functional option for overriding Config defaults.
type Option func(*Config)

// WithMaxImplTime sets the maximum wall-clock seconds the implementation
// pipeline may run per PR (default 21600 = 6 h). Cost is bounded by
// WithMaxImplBudget first; this is a hard outer cap.
func WithMaxImplTime(v int) Option { return func(c *Config) { c.MaxImplTime = v } }

// WithMaxImplBudget sets the maximum USD spend per PR across all
// implementation agent turns (default 50.00). This is the primary cost guard.
func WithMaxImplBudget(v float64) Option { return func(c *Config) { c.MaxImplBudget = v } }

// WithMaxReviewRetries sets how many times the implementation worker may be
// resumed after a reviewer request_changes verdict (default 1).
func WithMaxReviewRetries(v int) Option { return func(c *Config) { c.MaxReviewRetries = v } }

// WithImplModel sets the Claude model passed to the implementation worker
// subprocess (empty = Claude Code default).
func WithImplModel(v string) Option { return func(c *Config) { c.ImplModel = v } }

// WithCombinedAgentModel sets the Claude model for the combined
// analyse-and-decide agent subprocess (empty = Claude Code default).
func WithCombinedAgentModel(v string) Option { return func(c *Config) { c.CombinedAgentModel = v } }

// WithCombinedAgentBudget sets the max USD spend per PR for the combined
// analyse-and-decide agent (default 20.00).
func WithCombinedAgentBudget(v float64) Option {
	return func(c *Config) { c.CombinedAgentBudget = v }
}

// WithAnalyserModel sets the Claude model for the legacy SDK-based analyser,
// used only with --legacy-analyser (default claude-sonnet-4-6).
func WithAnalyserModel(v string) Option { return func(c *Config) { c.AnalyserModel = v } }

// WithAnalyserThinkingBudget sets the extended-thinking token budget for the
// legacy SDK-based analyser (0 = disabled; minimum non-zero value is 1024).
func WithAnalyserThinkingBudget(v int) Option {
	return func(c *Config) { c.AnalyserThinkingBudget = v }
}

// WithReviewerModel sets the Claude model for the reviewer subprocess
// (default claude-sonnet-4-6).
func WithReviewerModel(v string) Option { return func(c *Config) { c.ReviewerModel = v } }

// WithReviewerThinkingBudget sets the extended-thinking token budget for the
// reviewer (0 = disabled; minimum non-zero value is 1024).
func WithReviewerThinkingBudget(v int) Option {
	return func(c *Config) { c.ReviewerThinkingBudget = v }
}

// WithIgnoreChecks sets CI check names that are never blocking in
// AcceptableGiven, regardless of their result — for known structural or
// pre-existing failures that the tool should not attempt to fix. Repeatable;
// deployed with e.g. "CodeQL", "Dependabot auto-merge".
func WithIgnoreChecks(v []string) Option { return func(c *Config) { c.IgnoreChecks = v } }

// WithConcurrency sets the maximum number of PRs processed concurrently
// within a single scan (default 20).
func WithConcurrency(v int) Option { return func(c *Config) { c.Concurrency = v } }

// WithMaxImplIterations sets the maximum number of CI-fix resume turns per
// review cycle, not counting the initial implementation turn (default 30).
func WithMaxImplIterations(n int) Option { return func(c *Config) { c.MaxImplIterations = n } }

// WithMaxNoProgressIterations sets how many consecutive settled CI-fix turns
// with no reduction in the failing-check count are allowed before the
// pipeline gives up (Q12; default 8).
func WithMaxNoProgressIterations(n int) Option {
	return func(c *Config) { c.MaxNoProgressIterations = n }
}

// WithCIVerifyMaxWait sets the maximum seconds to wait for CI to settle after
// an implementation push before timing out (default 5400 = 90 min). Under the
// level-triggered pipeline this is a per-scan cap, not a blocking wait.
func WithCIVerifyMaxWait(v int) Option { return func(c *Config) { c.CIVerifyMaxWait = v } }

// WithCIStaleness sets the duration after which a still-pending check is
// considered stale and no longer blocks settledness (default 12h). Stale
// checks in the --ignore-check list are silently bypassed; stale blocking
// checks surface as "stuck" in diagnostic output.
func WithCIStaleness(v time.Duration) Option { return func(c *Config) { c.CIStaleness = v } }

// WithBotName sets the git committer name used for implementation commits
// (default "dependabot-helper").
func WithBotName(v string) Option { return func(c *Config) { c.BotName = v } }

// WithBotEmail sets the git committer email used for implementation commits.
func WithBotEmail(v string) Option { return func(c *Config) { c.BotEmail = v } }

// WithDataDir sets the root directory for persistent data: per-PR working
// directories and the base bare clone. Defaults to $SWEEPER_DATA_DIR or
// ~/.local/share/dependabot-sweeper.
func WithDataDir(v string) Option { return func(c *Config) { c.DataDir = v } }

// loadDotenv loads a .env file from the current directory, if present.
// Existing environment variables are not overridden.
func loadDotenv() {
	candidates := []string{
		filepath.Join(".", ".env"),
	}
	// Also try relative to the executable
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), ".env"))
	}

	for _, path := range candidates {
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
				continue
			}
			key, value, _ := strings.Cut(line, "=")
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			// Don't override existing env vars
			if _, exists := os.LookupEnv(key); !exists {
				os.Setenv(key, value)
			}
		}
		break // only load the first .env found
	}
}
