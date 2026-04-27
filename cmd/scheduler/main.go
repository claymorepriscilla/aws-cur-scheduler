package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/scheduler"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/logger"
)

func main() {
	// ── Load config ──────────────────────────────────────────
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] failed to load config: %v\n", err)
		os.Exit(1)
	}

	// ── Init logger ──────────────────────────────────────────
	log, err := logger.New(cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[FATAL] failed to init logger: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync() //nolint:errcheck

	log.Infow("aws-cur-scheduler starting",
		"env", cfg.App.Env,
		"version", cfg.App.Version,
	)

	// ── Determine target date ────────────────────────────────
	// Priority: CLI arg → env var TARGET_DATE → yesterday (UTC)
	targetDate, err := resolveTargetDate()
	if err != nil {
		log.Fatalw("invalid target date", "error", err)
	}
	log.Infow("target date resolved", "date", targetDate.Format("2006-01-02"))

	// ── Run scheduler job ────────────────────────────────────
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	job, err := scheduler.NewJob(cfg, log)
	if err != nil {
		log.Errorw("failed to initialise scheduler job", "error", err)
		os.Exit(1)
	}
	if err := job.Run(ctx, targetDate); err != nil {
		log.Errorw("scheduler job failed", "error", err)
		os.Exit(1)
	}

	log.Infow("scheduler job completed successfully")
}

// resolveTargetDate parses date from CLI arg or env var, falls back to yesterday UTC.
func resolveTargetDate() (time.Time, error) {
	const layout = "2006-01-02"

	// 1. CLI argument: ./scheduler 2025-04-08
	if len(os.Args) > 1 {
		d, err := time.Parse(layout, os.Args[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid date argument %q (expected YYYY-MM-DD): %w", os.Args[1], err)
		}
		return d, nil
	}

	// 2. Environment variable
	if v := os.Getenv("TARGET_DATE"); v != "" {
		d, err := time.Parse(layout, v)
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid TARGET_DATE %q (expected YYYY-MM-DD): %w", v, err)
		}
		return d, nil
	}

	// 3. Default: yesterday UTC
	return time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour), nil
}
