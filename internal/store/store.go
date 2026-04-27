// Package store defines the persistence interface and domain types for cost data.
// When database.enabled = false the store is nil and all callers fall back to
// their original behaviour (S3 / daily-cost fallback).
package store

import (
	"context"
	"time"
)

// ── Domain types ─────────────────────────────────────────────────────────────

// ServiceCost is a (service, cost) pair stored as JSONB in the snapshot row.
type ServiceCost struct {
	Service string  `json:"service"`
	Cost    float64 `json:"cost"`
}

// DailyCostSnapshot is the daily summary — one row per day per env.
type DailyCostSnapshot struct {
	ReportDate time.Time
	Env        string
	TotalCost  float64
	ItemCount  int
	ByService  []ServiceCost
}

// LineItem is a single CUR CSV row mapped to the cost_line_items table.
type LineItem struct {
	ReportDate   time.Time
	Env          string
	Service      string
	ResourceID   string
	Description  string
	UsageType    string
	UsageAmount  float64
	UsageUnit    string
	Operation    string
	InstanceType string
	Region       string
	TagName      string
	TagOwner     string
	TagEnv       string
	Cost         float64
}

// Alert is a single suspicious-resource flag mapped to the cost_alerts table.
// One resource can produce multiple Alert rows (one per flag / severity message).
type Alert struct {
	ReportDate   time.Time
	Env          string
	ResourceID   string
	Service      string
	ResourceCost float64
	TagOwner     string
	Severity     string // "critical" | "warning" | "notice"
	Message      string // human-readable flag text (without severity prefix)
}

// ── Store interface ───────────────────────────────────────────────────────────

// Store abstracts all persistence operations.
// A nil Store means the DB feature is disabled; callers must guard with != nil.
type Store interface {
	// UpsertSnapshot inserts or updates the daily summary row.
	UpsertSnapshot(ctx context.Context, s *DailyCostSnapshot) error

	// GetYesterdayCost returns the total cost for date-1.
	// found=false when no row exists yet (first run or gap in data).
	GetYesterdayCost(ctx context.Context, date time.Time, env string) (cost float64, found bool, err error)

	// GetMonthlyTotal sums total_cost for every day in the given month.
	GetMonthlyTotal(ctx context.Context, year int, month time.Month, env string) (float64, error)

	// ReplaceLineItems deletes existing rows for the same date+env then
	// bulk-inserts items in a single transaction — safe for re-runs.
	ReplaceLineItems(ctx context.Context, items []LineItem) error

	// ReplaceAlerts deletes existing rows for the same date+env then
	// bulk-inserts alerts in a single transaction — safe for re-runs.
	ReplaceAlerts(ctx context.Context, alerts []Alert) error

	// Close releases the underlying connection pool.
	Close() error
}
