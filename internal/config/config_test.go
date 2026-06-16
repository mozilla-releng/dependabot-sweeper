package config

import (
	"strings"
	"testing"
	"time"
)

// TestValidateRejectsZeroConcurrency verifies that a zero Concurrency is rejected.
func TestValidateRejectsZeroConcurrency(t *testing.T) {
	cfg := &Config{
		Concurrency:             0,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
		CIStaleness:             12 * time.Hour,
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "--concurrency") {
		t.Errorf("expected concurrency error, got %v", err)
	}
}

// TestValidateRejectsZeroMaxImplIterations covers the MaxImplIterations guard.
func TestValidateRejectsZeroMaxImplIterations(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       0,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--max-impl-iterations") {
		t.Errorf("expected max-impl-iterations error, got %v", err)
	}
}

// TestValidateRejectsZeroMaxNoProgressIterations covers the MaxNoProgressIterations guard.
func TestValidateRejectsZeroMaxNoProgressIterations(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 0,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--max-no-progress-iterations") {
		t.Errorf("expected max-no-progress-iterations error, got %v", err)
	}
}

// TestValidateRejectsZeroMaxImplTime covers the MaxImplTime guard.
func TestValidateRejectsZeroMaxImplTime(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             0,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--max-impl-time") {
		t.Errorf("expected max-impl-time error, got %v", err)
	}
}

// TestValidateRejectsZeroMaxImplBudget covers the MaxImplBudget guard.
func TestValidateRejectsZeroMaxImplBudget(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--max-impl-budget") {
		t.Errorf("expected max-impl-budget error, got %v", err)
	}
}

// TestValidateRejectsZeroCIVerifyMaxWait covers the CIVerifyMaxWait guard.
func TestValidateRejectsZeroCIVerifyMaxWait(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         0,
		MaxReviewRetries:        0,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--ci-verify-max-wait") {
		t.Errorf("expected ci-verify-max-wait error, got %v", err)
	}
}

// TestValidateRejectsNegativeMaxReviewRetries covers the MaxReviewRetries guard.
func TestValidateRejectsNegativeMaxReviewRetries(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        -1,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--max-review-retries") {
		t.Errorf("expected max-review-retries error, got %v", err)
	}
}

// TestValidateThinkingBudgetRejectsNegative covers the negative-budget guard.
func TestValidateThinkingBudgetRejectsNegative(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
		AnalyserThinkingBudget:  -1,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--analyser-thinking-budget") {
		t.Errorf("expected analyser-thinking-budget error, got %v", err)
	}
}

// TestValidateThinkingBudgetRejectsBelowMinimum covers the 1–1023 invalid range
// that the Anthropic API rejects.
func TestValidateThinkingBudgetRejectsBelowMinimum(t *testing.T) {
	cfg := &Config{
		Concurrency:             1,
		MaxImplIterations:       3,
		MaxNoProgressIterations: 2,
		MaxImplTime:             300,
		MaxImplBudget:           1.0,
		CIVerifyMaxWait:         60,
		MaxReviewRetries:        0,
		ReviewerThinkingBudget:  512,
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "--reviewer-thinking-budget") {
		t.Errorf("expected reviewer-thinking-budget error for value in 1-1023 range, got %v", err)
	}
}

// TestValidateAcceptsValidConfig verifies that a well-formed config passes validation.
func TestValidateAcceptsValidConfig(t *testing.T) {
	cfg := &Config{
		Concurrency:             2,
		MaxImplIterations:       5,
		MaxNoProgressIterations: 3,
		MaxImplTime:             600,
		MaxImplBudget:           5.0,
		CIVerifyMaxWait:         120,
		MaxReviewRetries:        2,
		AnalyserThinkingBudget:  0,    // 0 = disabled, valid
		ReviewerThinkingBudget:  1024, // minimum valid non-zero value
	}
	if err := cfg.validate(); err != nil {
		t.Errorf("expected no error for valid config, got %v", err)
	}
}
