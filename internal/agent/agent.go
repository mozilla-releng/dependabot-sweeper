// Package agent implements the combined analyse-and-decide agentic step (Q10).
//
// This replaces the separate tool-less analyser (internal/analyser). Every
// engaged PR goes through a single agentic step with a live repo checkout
// and full tool access. The agent analyses upstream changes, searches the
// codebase, and ends in one of four outcomes: recommend, needs_changes,
// flag_human, or gave_up.
//
// Agent empowerment principle (docs/PRINCIPLES.md): the brief contains ONLY
// what the agent cannot derive itself — PR metadata and working environment.
// The agent fetches everything else autonomously (upstream data, changelogs,
// codebase snippets). No pre-fetching, no injection of upstream data.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mozilla-releng/dependabot-sweeper/internal/llmutil"
	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// AgentError indicates a failure in the combined agent.
type AgentError struct {
	Message string
}

func (e *AgentError) Error() string {
	return e.Message
}

// combinedAgentBrief is the prompt template for the combined analyse-and-decide
// agent. It receives only PR metadata and working environment — everything else
// is gathered autonomously by the agent.
//
// The WHY behind each instruction is included per the prompt-design principle
// (feedback_prompt_why): agents reason better about edge cases when they
// understand the purpose behind their instructions.
const combinedAgentBrief = `## Your role

You are the analysis and decision stage of an automated dependency upgrade
pipeline. You are the first and most important agent in this pipeline. Your
job is to:
1. Understand what changed upstream in this dependency bump.
2. Assess whether those changes affect this codebase.
3. Decide what to do: recommend the bump as-is, identify needed code changes,
   flag for human attention, or give up.

Why your role exists: the tool's goal is to reduce human attention spent on
dependency bumps. Every engaged PR gets your full analysis — even a green-CI
bump needs a reasoned WHY before it can be recommended, because CI only shows
the code compiles and tests pass, not whether an upstream change is semantically
concerning (supply-chain, license, semantic breakage that compiles).

Your analysis directly determines whether:
- A human can confidently merge the bump without re-deriving your analysis.
- Code changes are needed (which triggers the implementation pipeline).
- Human attention is warranted (which must be a rare, high-confidence signal).

You have FULL TOOL ACCESS and are fully autonomous. You are NOT restricted to
the information in this brief — that information only tells you where to start.
Use your tools to fetch upstream data (release notes, changelogs, compare URLs,
migration guides), search the codebase for usages, and verify your conclusions
directly. The quality of your analysis depends entirely on what you actually
look up and verify, not on what you were told.

## Working environment

Working directory: %s
Repo clone:        %s
Bare clone:        %s   [do not modify; clone from it if you need a fresh copy]

You work in the repo clone. Use git, gh, grep, find, and any other tools you
need. The working directory will remain on disk while this PR is open.

## PR to analyse

PR number:   #%d
PR title:    %s
Package:     %s
Ecosystem:   %s
Old version: %s
New version: %s
Bump type:   %s

The dependabot PR body (read with: gh pr view %d --repo %s) often contains
links to upstream release notes, compare views, and changelogs. Read those
links — they are your primary source of upstream change information.

## What you must analyse

1. What changed upstream between the old and new versions?
   Read the actual upstream changelog, release notes, migration guides.
   Follow compare URLs in the PR body if they are present.
   Fetch TypeScript definition files or API signatures if the ecosystem uses them.

2. Does any upstream change affect this codebase?
   Search the codebase for usage of APIs, symbols, or patterns that changed.
   Do NOT search by package name alone — search for the specific changed symbols.
   You must know what changed upstream before you can search for whether it is used.

3. What is the right action?
   recommend: required CI is already green AND you have verified no code change
     is needed. You must provide a concrete WHY — what you checked, what changed,
     why it does not affect this codebase. A vague "looks safe" is not acceptable.
   needs_changes: you identified specific code changes required. The
     implementation pipeline will take over; your justification seeds it.
   flag_human: you have a specific insight you cannot resolve autonomously.
     This is a LAST RESORT. One or two sentences, purpose-built.
     Good: "The new version has a critical known vulnerability — cannot recommend."
     Bad: "I am not sure about this change."
   gave_up: you genuinely could not reach a verdict (upstream data unavailable,
     codebase too complex to assess in one session). A silent draft is left; no
     comment is posted.

## Important constraints

Do NOT post anything to GitHub. The controlling process handles all GitHub
interactions based on your structured output.

Do NOT run CI or wait for CI. Your job is analysis, not CI monitoring.

The justification field (on the needs_changes path) is PRIVATE through the
implementation and reviewer loop. It will be posted to the PR body only on
final approval. Write it as if explaining to a competent human reviewer why
the changes you are requesting are correct and necessary.

## Response format

After completing your analysis, respond with ONLY a JSON object:

{
  "outcome": "recommend" or "needs_changes" or "flag_human" or "gave_up",
  "recommend_body": "markdown comment for recommend path: concrete WHY, grounded in what you verified. Empty for other outcomes.",
  "flag_reason": "one or two sentence concise reason for flag_human path. Empty for other outcomes.",
  "justification": "structured justification for needs_changes path: upstream scope, repo impact, how breaking changes were handled, what was considered but dismissed. Held private through review loop. Empty for other outcomes."
}

Respond ONLY with the JSON object, no other text.
`

// CombinedAgent is the analyse-and-decide agent (Q10). It replaces the old
// tool-less analyser; runs as a claude subprocess with full tool access.
type CombinedAgent struct {
	model  string
	budget float64
}

// NewCombinedAgent creates a CombinedAgent. The model and budget are passed
// through to the subprocess invocation.
func NewCombinedAgent(model string, budget float64) *CombinedAgent {
	return &CombinedAgent{
		model:  model,
		budget: budget,
	}
}

// Analyse runs the combined agent for a single PR. workdir is the per-PR
// working directory (canonicalWorkdir); repoDir is workdir/repo; barePath is
// the permanent bare clone path.
//
// The agent brief contains only PR metadata and working environment — the
// agent fetches everything else autonomously (agent empowerment principle).
func (a *CombinedAgent) Analyse(
	ctx context.Context,
	pr models.DependabotPR,
	workdir, repoDir, barePath string,
	repoFullName string,
	logPath string,
) (*models.AgentVerdict, error) {
	brief := a.buildBrief(pr, workdir, repoDir, barePath, repoFullName)

	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		verdict, err := a.runSubprocess(ctx, repoDir, brief, logPath, pr.Number)
		if err != nil {
			lastErr = err
			slog.Warn("combined agent attempt failed", "pr", pr.Number, "attempt", attempt, "error", err)
			continue
		}
		return verdict, nil
	}
	return nil, lastErr
}

// buildBrief constructs the stdin brief for the combined agent subprocess.
func (a *CombinedAgent) buildBrief(
	pr models.DependabotPR,
	workdir, repoDir, barePath string,
	repoFullName string,
) string {
	return fmt.Sprintf(combinedAgentBrief,
		workdir,
		repoDir,
		barePath,
		pr.Number,
		pr.Title,
		pr.PackageName,
		pr.Ecosystem,
		pr.OldVersion,
		pr.NewVersion,
		string(pr.BumpType),
		pr.Number,
		repoFullName,
	)
}

// runSubprocess invokes claude as a subprocess in repoDir with the brief as
// stdin. Logs output to logPath. The agent runs with --dangerously-skip-permissions
// and full tool access; --bare is NOT used (preserves hooks, skills, plugins).
func (a *CombinedAgent) runSubprocess(
	ctx context.Context,
	repoDir, brief, logPath string,
	prNumber int,
) (*models.AgentVerdict, error) {
	turnCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()

	args := []string{
		"claude", "--print", "--dangerously-skip-permissions",
		"--output-format", "text",
		"--max-budget-usd", fmt.Sprintf("%.2f", a.budget),
	}
	if a.model != "" {
		args = append(args, "--model", a.model)
	}

	proc := exec.CommandContext(turnCtx, args[0], args[1:]...)
	proc.Dir = repoDir

	var stdout bytes.Buffer
	proc.Stdout = &stdout
	proc.Stderr = &stdout // capture stderr for diagnostics

	stdin, err := proc.StdinPipe()
	if err != nil {
		return nil, &AgentError{Message: fmt.Sprintf("combined agent stdin pipe: %v", err)}
	}

	if err := proc.Start(); err != nil {
		return nil, &AgentError{Message: fmt.Sprintf("combined agent subprocess start: %v", err)}
	}

	if _, err := stdin.Write([]byte(brief)); err != nil {
		slog.Warn("failed writing brief to combined agent stdin", "pr", prNumber, "error", err)
	}
	stdin.Close()

	if err := proc.Wait(); err != nil {
		if turnCtx.Err() != nil {
			return nil, &AgentError{Message: "combined agent hit the time cap (60 minutes)"}
		}
		slog.Warn("combined agent ended non-zero", "pr", prNumber, "error", err, "output_len", stdout.Len())
		// Non-zero exit does not necessarily mean no output — try to parse.
	}

	rawOutput := stdout.String()

	// Append raw output to the per-PR log for diagnostics.
	if logPath != "" {
		appendToLog(logPath, rawOutput)
	}

	if rawOutput == "" {
		return nil, &AgentError{Message: "combined agent produced no output"}
	}

	slog.Debug("combined agent output", "pr", prNumber, "len", len(rawOutput))
	return ParseAgentVerdict(rawOutput)
}

// ParseAgentVerdict extracts an AgentVerdict from the agent's raw text output.
// Exported for testing.
func ParseAgentVerdict(rawOutput string) (*models.AgentVerdict, error) {
	text := llmutil.ExtractJSON(rawOutput)

	var data struct {
		Outcome       string `json:"outcome"`
		RecommendBody string `json:"recommend_body"`
		FlagReason    string `json:"flag_reason"`
		Justification string `json:"justification"`
	}
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		return nil, &AgentError{
			Message: fmt.Sprintf("failed to parse combined agent response: %v (raw output length: %d)", err, len(rawOutput)),
		}
	}

	outcome := models.AgentOutcome(data.Outcome)
	switch outcome {
	case models.AgentOutcomeRecommend, models.AgentOutcomeNeedsChanges,
		models.AgentOutcomeFlagHuman, models.AgentOutcomeGaveUp:
		// valid
	default:
		return nil, &AgentError{
			Message: fmt.Sprintf("invalid agent outcome: %q (must be one of: recommend, needs_changes, flag_human, gave_up)", data.Outcome),
		}
	}

	// Validate required fields per outcome.
	switch outcome {
	case models.AgentOutcomeRecommend:
		if strings.TrimSpace(data.RecommendBody) == "" {
			return nil, &AgentError{
				Message: "outcome=recommend requires a non-empty recommend_body",
			}
		}
	case models.AgentOutcomeFlagHuman:
		if strings.TrimSpace(data.FlagReason) == "" {
			return nil, &AgentError{
				Message: "outcome=flag_human requires a non-empty flag_reason",
			}
		}
	}

	return &models.AgentVerdict{
		Outcome:       outcome,
		RecommendBody: data.RecommendBody,
		FlagReason:    data.FlagReason,
		Justification: data.Justification,
	}, nil
}

// appendToLog appends text to a log file, creating it if needed. Best-effort.
func appendToLog(path, text string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(text) //nolint:errcheck
}
