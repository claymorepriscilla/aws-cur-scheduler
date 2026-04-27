package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// postgresStore is the Supabase/PostgreSQL implementation of Store.
type postgresStore struct {
	pool *pgxpool.Pool
}

// NewPostgres opens a connection pool to the given PostgreSQL URL and pings it.
// Call Close() when the application exits.
func NewPostgres(ctx context.Context, url string) (Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("pgxpool.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return &postgresStore{pool: pool}, nil
}

func (s *postgresStore) Close() error {
	s.pool.Close()
	return nil
}

// ── UpsertSnapshot ────────────────────────────────────────────────────────────

func (s *postgresStore) UpsertSnapshot(ctx context.Context, snap *DailyCostSnapshot) error {
	byService, err := json.Marshal(snap.ByService)
	if err != nil {
		return fmt.Errorf("marshal by_service: %w", err)
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO daily_cost_snapshots
		    (report_date, env, total_cost, item_count, by_service)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (report_date, env) DO UPDATE SET
		    total_cost = EXCLUDED.total_cost,
		    item_count = EXCLUDED.item_count,
		    by_service = EXCLUDED.by_service,
		    updated_at = NOW()
	`, snap.ReportDate, snap.Env, snap.TotalCost, snap.ItemCount, byService)
	if err != nil {
		return fmt.Errorf("upsert snapshot: %w", err)
	}
	return nil
}

// ── GetYesterdayCost ──────────────────────────────────────────────────────────

// GetYesterdayCost returns the most recent snapshot total_cost that is strictly
// before the given date. Using "most recent before" instead of "date - 1 exactly"
// handles gaps in data (e.g. weekends, first run of the month).
func (s *postgresStore) GetYesterdayCost(ctx context.Context, date time.Time, env string) (float64, bool, error) {
	var cost float64
	err := s.pool.QueryRow(ctx, `
		SELECT total_cost FROM daily_cost_snapshots
		WHERE env = $1 AND report_date < $2
		ORDER BY report_date DESC
		LIMIT 1
	`, env, date).Scan(&cost)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("query previous snapshot cost: %w", err)
	}
	return cost, true, nil
}

// ── GetMonthlyTotal ───────────────────────────────────────────────────────────

func (s *postgresStore) GetMonthlyTotal(ctx context.Context, year int, month time.Month, env string) (float64, error) {
	monthStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	var total float64
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(total_cost), 0)
		FROM daily_cost_snapshots
		WHERE env = $1 AND report_date >= $2 AND report_date < $3
	`, env, monthStart, monthEnd).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("query monthly total: %w", err)
	}
	return total, nil
}

// ── ReplaceLineItems ──────────────────────────────────────────────────────────

func (s *postgresStore) ReplaceLineItems(ctx context.Context, items []LineItem) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	date := items[0].ReportDate
	env := items[0].Env

	_, err = tx.Exec(ctx,
		`DELETE FROM cost_line_items WHERE report_date = $1 AND env = $2`,
		date, env,
	)
	if err != nil {
		return fmt.Errorf("delete line items: %w", err)
	}

	rows := make([][]interface{}, 0, len(items))
	for _, li := range items {
		rows = append(rows, []interface{}{
			li.ReportDate, li.Env,
			li.Service, li.ResourceID, li.Description,
			li.UsageType, li.UsageAmount, li.UsageUnit,
			li.Operation, li.InstanceType, li.Region,
			li.TagName, li.TagOwner, li.TagEnv,
			li.Cost,
		})
	}

	_, err = tx.CopyFrom(ctx,
		pgx.Identifier{"cost_line_items"},
		[]string{
			"report_date", "env",
			"service", "resource_id", "description",
			"usage_type", "usage_amount", "usage_unit",
			"operation", "instance_type", "region",
			"tag_name", "tag_owner", "tag_env",
			"cost",
		},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy line items: %w", err)
	}

	return tx.Commit(ctx)
}

// ── ReplaceAlerts ─────────────────────────────────────────────────────────────

func (s *postgresStore) ReplaceAlerts(ctx context.Context, alerts []Alert) error {
	if len(alerts) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	date := alerts[0].ReportDate
	env := alerts[0].Env

	_, err = tx.Exec(ctx,
		`DELETE FROM cost_alerts WHERE report_date = $1 AND env = $2`,
		date, env,
	)
	if err != nil {
		return fmt.Errorf("delete alerts: %w", err)
	}

	rows := make([][]interface{}, 0, len(alerts))
	for _, a := range alerts {
		rows = append(rows, []interface{}{
			a.ReportDate, a.Env,
			a.ResourceID, a.Service, a.ResourceCost, a.TagOwner,
			a.Severity, a.Message,
		})
	}

	_, err = tx.CopyFrom(ctx,
		pgx.Identifier{"cost_alerts"},
		[]string{
			"report_date", "env",
			"resource_id", "service", "resource_cost", "tag_owner",
			"severity", "message",
		},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("copy alerts: %w", err)
	}

	return tx.Commit(ctx)
}
