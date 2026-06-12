package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseWorkerFlagsDefaults(t *testing.T) {
	opts, err := parseWorkerFlags([]string{"--repos", "owner/name"})
	if err != nil {
		t.Fatalf("parseWorkerFlags error: %v", err)
	}
	if opts.repo != "owner/name" {
		t.Errorf("repo = %q, want owner/name", opts.repo)
	}
	if opts.interval != 30*time.Minute {
		t.Errorf("interval = %v, want 30m", opts.interval)
	}
	if opts.db == "" {
		t.Errorf("db default should not be empty")
	}
	want := filepath.Join(os.TempDir(), "sweeper-agent-logs")
	if opts.logDir != want {
		t.Errorf("logDir = %q, want %q", opts.logDir, want)
	}
}

func TestParseWorkerFlagsOverrides(t *testing.T) {
	opts, err := parseWorkerFlags([]string{
		"--repos", "o/r",
		"--interval", "5m",
		"--concurrency", "4",
		"--accept-author", "test-bot",
		"--db", "/tmp/test.db",
		"--log-dir", "/tmp/logs",
	})
	if err != nil {
		t.Fatalf("parseWorkerFlags error: %v", err)
	}
	if opts.interval != 5*time.Minute {
		t.Errorf("interval = %v, want 5m", opts.interval)
	}
	if opts.concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", opts.concurrency)
	}
	if len(opts.acceptAuthors) != 1 || opts.acceptAuthors[0] != "test-bot" {
		t.Errorf("acceptAuthors = %v", opts.acceptAuthors)
	}
	if opts.db != "/tmp/test.db" {
		t.Errorf("db = %q, want /tmp/test.db", opts.db)
	}
	if opts.logDir != "/tmp/logs" {
		t.Errorf("logDir = %q, want /tmp/logs", opts.logDir)
	}
}

func TestParseWorkerFlagsRequiresRepo(t *testing.T) {
	_, err := parseWorkerFlags([]string{"--interval", "5m"})
	if err == nil {
		t.Errorf("expected an error when --repo is missing")
	}
}

func TestParseWebFlagsDefaults(t *testing.T) {
	opts, err := parseWebFlags([]string{})
	if err != nil {
		t.Fatalf("parseWebFlags error: %v", err)
	}
	if opts.listenAddr != "localhost:8080" {
		t.Errorf("listenAddr = %q, want localhost:8080", opts.listenAddr)
	}
	if opts.pollInterval != time.Second {
		t.Errorf("pollInterval = %v, want 1s", opts.pollInterval)
	}
	if opts.db == "" {
		t.Errorf("db default should not be empty")
	}
	want := filepath.Join(os.TempDir(), "sweeper-agent-logs")
	if opts.logDir != want {
		t.Errorf("logDir = %q, want %q", opts.logDir, want)
	}
}

func TestParseWebFlagsOverrides(t *testing.T) {
	opts, err := parseWebFlags([]string{
		"--listen-addr", "0.0.0.0:9090",
		"--db", "/tmp/w.db",
		"--log-dir", "/var/log/sweeper",
		"--poll-interval", "500ms",
	})
	if err != nil {
		t.Fatalf("parseWebFlags error: %v", err)
	}
	if opts.listenAddr != "0.0.0.0:9090" {
		t.Errorf("listenAddr = %q", opts.listenAddr)
	}
	if opts.db != "/tmp/w.db" {
		t.Errorf("db = %q", opts.db)
	}
	if opts.logDir != "/var/log/sweeper" {
		t.Errorf("logDir = %q", opts.logDir)
	}
	if opts.pollInterval != 500*time.Millisecond {
		t.Errorf("pollInterval = %v, want 500ms", opts.pollInterval)
	}
}
