package config

import (
	"testing"

	"github.com/mozilla-releng/dependabot-sweeper/internal/models"
)

// baseValidConfig returns a Config with all numeric fields in valid ranges, so
// each test can mutate the single field under test.
func baseValidConfig() *Config {
	return &Config{
		Concurrency:             1,
		MaxImplIterations:       30,
		MaxNoProgressIterations: 8,
		MaxImplTime:             100,
		MaxImplBudget:           1,
		CIVerifyMaxWait:         100,
		MaxReviewRetries:        1,
		MinBumpToEngage:         models.BumpMajor,
	}
}

func TestValidateMinBumpToEngage(t *testing.T) {
	cases := []struct {
		bump    models.BumpType
		wantErr bool
	}{
		{models.BumpMajor, false},
		{models.BumpMinor, false},
		{models.BumpPatch, false},
		{models.BumpUnknown, true}, // not a threshold one would set
		{models.BumpType("nonsense"), true},
		{models.BumpType(""), true}, // FromEnv defaults empty→major; validate() itself rejects empty
	}
	for _, c := range cases {
		cfg := baseValidConfig()
		cfg.MinBumpToEngage = c.bump
		err := cfg.validate()
		if (err != nil) != c.wantErr {
			t.Errorf("validate() with MinBumpToEngage=%q: err=%v, wantErr=%v", c.bump, err, c.wantErr)
		}
	}
}
