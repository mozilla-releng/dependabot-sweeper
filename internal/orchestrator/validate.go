package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/mozilla-releng/dependabot-sweeper/internal/config"
)

// thinkingCapablePrefixes lists the model ID prefixes known to support extended
// thinking. Kept as a simple prefix list rather than an exhaustive allowlist so
// new point releases (e.g. claude-sonnet-4-20260101) are accepted automatically.
var thinkingCapablePrefixes = []string{
	"claude-sonnet-4", // claude-sonnet-4-*, claude-sonnet-4-6
	"claude-opus-4",   // claude-opus-4-*, claude-opus-4-8
	"claude-fable",    // claude-fable-5
}

func modelSupportsThinking(modelID string) bool {
	for _, prefix := range thinkingCapablePrefixes {
		if strings.HasPrefix(modelID, prefix) {
			return true
		}
	}
	return false
}

// ValidateModels confirms each configured Anthropic model exists (zero
// token cost — just GET /v1/models/{id}) and that any model with a thinking
// budget > 0 is known to support extended thinking. Call this once at
// startup (before Run), not on every scan.
func ValidateModels(ctx context.Context, cfg *config.Config) error {
	client := anthropic.NewClient(option.WithAPIKey(cfg.AnthropicAPIKey))

	type entry struct {
		model          string
		thinkingBudget int
		modelFlag      string
		budgetFlag     string
	}
	checks := []entry{
		{cfg.AnalyserModel, cfg.AnalyserThinkingBudget, "--analyser-model", "--analyser-thinking-budget"},
		{cfg.ReviewerModel, cfg.ReviewerThinkingBudget, "--reviewer-model", "--reviewer-thinking-budget"},
	}
	if cfg.ImplModel != "" {
		// The impl model runs via the claude CLI (which has no thinking-budget
		// flag), so validate only that the model exists — no thinking check.
		checks = append(checks, entry{cfg.ImplModel, 0, "--impl-model", ""})
	}

	for _, e := range checks {
		if _, err := client.Models.Get(ctx, e.model, anthropic.ModelGetParams{}); err != nil {
			var apiErr *anthropic.Error
			if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
				return fmt.Errorf("%s %q: model not found — check spelling", e.modelFlag, e.model)
			}
			return fmt.Errorf("%s %q: could not verify model (API error): %w", e.modelFlag, e.model, err)
		}
		if e.thinkingBudget > 0 && !modelSupportsThinking(e.model) {
			return fmt.Errorf(
				"%s %q does not appear to support extended thinking; "+
					"remove %s or choose a thinking-capable model (claude-sonnet-4-*, claude-opus-4-*, claude-fable-*)",
				e.modelFlag, e.model, e.budgetFlag,
			)
		}
	}
	return nil
}
