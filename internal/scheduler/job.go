// Package scheduler orchestrates the full CUR pipeline:
// fetch items → analyze → (save to DB) → monthly total → report → notify.
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	awsclient "github.com/claymorepriscilla/aws-cur-scheduler/internal/aws"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/analyzer"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/notifier"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/store"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/logger"
)

// Job encapsulates the full scheduled task.
type Job struct {
	cfg      *config.Config
	log      *logger.Logger
	analyzer *analyzer.Analyzer
	store    store.Store // nil when database.enabled = false
}

// NewJob wires up all dependencies.
// If cfg.Database.Enabled is true it opens a Supabase/PostgreSQL connection;
// if the connection fails the error is returned and the process should stop.
func NewJob(cfg *config.Config, log *logger.Logger) (*Job, error) {
	j := &Job{
		cfg:      cfg,
		log:      log,
		analyzer: analyzer.New(cfg.Report.TopNResources),
	}

	if cfg.Database.Enabled {
		timeout := time.Duration(cfg.Database.TimeoutSec) * time.Second
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		s, err := store.NewPostgres(ctx, cfg.Database.DSN())
		if err != nil {
			log.Errorw("failed to connect to database", "error", err)
			return nil, fmt.Errorf("connect to database: %w", err)
		}
		log.Infow("database connected (Supabase/PostgreSQL)")
		j.store = s
	} else {
		log.Infow("database disabled (database.enabled=false) — using S3/daily mode")
	}

	return j, nil
}

// Run executes the full pipeline for the given targetDate.
//
// Steps:
//  1. Fetch line items (local file or S3)
//  2. Analyze daily cost
//  3. Save snapshot + line items + alerts to DB  (skipped when DB disabled)
//  4. Resolve monthly total                       (DB → S3 → daily fallback)
//  5. Print report to log
//  6. Send Teams card                            (skipped when webhook disabled)
func (j *Job) Run(ctx context.Context, targetDate time.Time) error {
	j.log.Infow("job started",
		"target_date", targetDate.Format("2006-01-02"),
		"source", j.curSource(),
		"webhook_enabled", j.cfg.Teams.EnableWebhook,
		"db_enabled", j.store != nil,
	)

	// ── Step 1: Fetch items ───────────────────────────────────
	items, err := j.fetchItems(ctx, targetDate)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		j.log.Warnw("CUR file contains zero cost items")
	}

	// ── Step 2: Analyze ───────────────────────────────────────
	analysis := j.analyzer.Analyze(items, targetDate)
	j.log.Infow("daily analysis complete",
		"total", fmt.Sprintf("$%.4f", analysis.TotalCost),
		"suspicious", len(analysis.Suspicious),
	)

	// ── Step 3: reportDate = targetDate ──────────────────────
	reportDate := targetDate.UTC().Truncate(24 * time.Hour)

	// ── Step 4: Save to DB (if enabled) ──────────────────────
	j.saveToStore(ctx, analysis, items, reportDate)

	// ── Step 5: Resolve costs ─────────────────────────────────
	// CUR file เป็น cumulative monthly:
	//   monthlyTotal = analysis.TotalCost  (ยอดสะสมตั้งแต่ต้นเดือน)
	//   yesterdayCost = snapshot วันก่อนจาก DB  (เช่น $71)
	//   dailyCost = monthlyTotal - yesterdayCost  (delta วันนี้ เช่น $5)
	monthlyTotal := analysis.TotalCost
	yesterdayCost := j.getYesterdayCost(ctx, reportDate)
	dailyCost := monthlyTotal - yesterdayCost
	if dailyCost < 0 {
		dailyCost = 0
	}
	j.log.Infow("costs resolved",
		"monthly_total", fmt.Sprintf("$%.4f", monthlyTotal),
		"yesterday_cost", fmt.Sprintf("$%.4f", yesterdayCost),
		"daily_cost", fmt.Sprintf("$%.4f", dailyCost),
	)

	// ── Step 6: Print report ──────────────────────────────────
	j.printReport(analysis, monthlyTotal, dailyCost)

	// ── Step 7: Notify Teams ──────────────────────────────────
	return j.maybeNotify(ctx, analysis, monthlyTotal, yesterdayCost, dailyCost)
}

// ── fetchItems ────────────────────────────────────────────────────────────────

// fetchItems reads line items from the configured source (local file or S3).
func (j *Job) fetchItems(ctx context.Context, targetDate time.Time) ([]awsclient.LineItem, error) {
	if j.cfg.CUR.LocalPath != "" {
		j.log.Infow("reading CUR from local file", "path", j.cfg.CUR.LocalPath)
		items, err := awsclient.ReadLocalCURFile(j.cfg.CUR.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("read local CUR file: %w", err)
		}
		j.log.Infow("local CUR file parsed", "line_items", len(items))
		return items, nil
	}

	awsCli, err := awsclient.NewClient(ctx, j.cfg)
	if err != nil {
		return nil, fmt.Errorf("init aws client: %w", err)
	}
	j.log.Infow("AWS client initialised", "region", j.cfg.AWS.Region, "bucket", j.cfg.CUR.S3Bucket)

	curKey, err := awsCli.FindCURFile(ctx, targetDate)
	if err != nil {
		return nil, fmt.Errorf("find CUR file: %w", err)
	}
	j.log.Infow("CUR file found", "key", curKey)

	items, err := awsCli.ReadCURFile(ctx, curKey)
	if err != nil {
		return nil, fmt.Errorf("read CUR file: %w", err)
	}
	j.log.Infow("CUR file parsed", "line_items", len(items))
	return items, nil
}

// ── resolveMonthlyTotal ───────────────────────────────────────────────────────

// getYesterdayCost ดึง cumulative total ของ snapshot ล่าสุดก่อนวันนี้จาก DB
// คืนค่า 0 เมื่อไม่มีข้อมูล (run ครั้งแรก)
func (j *Job) getYesterdayCost(ctx context.Context, date time.Time) float64 {
	if j.store == nil {
		return 0
	}
	cost, found, err := j.store.GetYesterdayCost(ctx, date, j.cfg.App.Env)
	if err != nil {
		j.log.Warnw("could not get previous snapshot from DB", "error", err)
		return 0
	}
	if !found {
		j.log.Infow("no previous snapshot in DB (first run?) — yesterdayCost = 0")
		return 0
	}
	return cost
}

// ── saveToStore ───────────────────────────────────────────────────────────────

// saveToStore persists snapshot, line items, and alerts to DB.
// Errors are logged as warnings — they never abort the job.
func (j *Job) saveToStore(ctx context.Context, a *analyzer.Analysis, rawItems []awsclient.LineItem, date time.Time) {
	if j.store == nil {
		return
	}
	env := j.cfg.App.Env

	if err := j.store.UpsertSnapshot(ctx, toSnapshot(a, env, date)); err != nil {
		j.log.Warnw("DB upsert snapshot failed", "error", err)
	} else {
		j.log.Infow("DB snapshot saved")
	}

	if err := j.store.ReplaceLineItems(ctx, toLineItems(rawItems, date, env)); err != nil {
		j.log.Warnw("DB replace line items failed", "error", err)
	} else {
		j.log.Infow("DB line items saved", "count", len(rawItems))
	}

	alerts := toAlerts(a.Suspicious, date, env)
	if err := j.store.ReplaceAlerts(ctx, alerts); err != nil {
		j.log.Warnw("DB replace alerts failed", "error", err)
	} else {
		j.log.Infow("DB alerts saved", "count", len(alerts))
	}
}

// ── Converters ────────────────────────────────────────────────────────────────

func toSnapshot(a *analyzer.Analysis, env string, date time.Time) *store.DailyCostSnapshot {
	svcCosts := make([]store.ServiceCost, len(a.ByService))
	for i, sc := range a.ByService {
		svcCosts[i] = store.ServiceCost{Service: sc.Service, Cost: sc.Cost}
	}
	return &store.DailyCostSnapshot{
		ReportDate: date, // ใช้ reportDate จากไฟล์ ไม่ใช่ targetDate
		Env:        env,
		TotalCost:  a.TotalCost,
		ItemCount:  a.ItemCount,
		ByService:  svcCosts,
	}
}

func toLineItems(items []awsclient.LineItem, date time.Time, env string) []store.LineItem {
	out := make([]store.LineItem, len(items))
	for i, li := range items {
		out[i] = store.LineItem{
			ReportDate:   date,
			Env:          env,
			Service:      sanitizeUTF8(li.Service),
			ResourceID:   sanitizeUTF8(li.ResourceID),
			Description:  sanitizeUTF8(li.Description),
			UsageType:    sanitizeUTF8(li.UsageType),
			UsageAmount:  li.UsageAmount,
			UsageUnit:    sanitizeUTF8(li.UsageUnit),
			Operation:    sanitizeUTF8(li.Operation),
			InstanceType: sanitizeUTF8(li.InstanceType),
			Region:       sanitizeUTF8(li.Region),
			TagName:      sanitizeUTF8(li.TagName),
			TagOwner:     sanitizeUTF8(li.TagOwner),
			TagEnv:       sanitizeUTF8(li.TagEnv),
			Cost:         li.Cost,
		}
	}
	return out
}

// toAlerts expands each SuspiciousResource into one store.Alert per flag.
func toAlerts(suspicious []analyzer.SuspiciousResource, date time.Time, env string) []store.Alert {
	var out []store.Alert
	for _, s := range suspicious {
		for _, flag := range s.Flags {
			out = append(out, store.Alert{
				ReportDate:   date,
				Env:          env,
				ResourceID:   sanitizeUTF8(s.ResourceID),
				Service:      sanitizeUTF8(s.Service),
				ResourceCost: s.Cost,
				TagOwner:     sanitizeUTF8(s.TagOwner),
				Severity:     parseSeverity(flag),
				Message:      sanitizeUTF8(parseMessage(flag)),
			})
		}
	}
	return out
}

// parseSeverity extracts "critical" | "warning" | "notice" from the flag emoji prefix.
func parseSeverity(flag string) string {
	switch {
	case strings.HasPrefix(flag, "🔴"):
		return "critical"
	case strings.HasPrefix(flag, "🟡"):
		return "warning"
	default:
		return "notice"
	}
}

// parseMessage strips the "🔴 Critical — " / "🟡 Warning — " / "🟠 Notice — " prefix.
func parseMessage(flag string) string {
	const sep = " — "
	if idx := strings.Index(flag, sep); idx >= 0 {
		return flag[idx+len(sep):]
	}
	return flag
}

// ── maybeNotify ───────────────────────────────────────────────────────────────

func (j *Job) maybeNotify(ctx context.Context, analysis *analyzer.Analysis, monthlyTotal, yesterdayCost, dailyCost float64) error {
	if !j.cfg.Teams.EnableWebhook {
		j.log.Infow("webhook disabled (teams.enable_webhook=false) — skipping Teams notification")
		j.log.Infow("job finished successfully (no webhook sent)",
			"daily_cost", fmt.Sprintf("$%.4f", dailyCost),
			"yesterday_cost", fmt.Sprintf("$%.4f", yesterdayCost),
			"monthly_total", fmt.Sprintf("$%.2f", monthlyTotal),
			"budget_pct", fmt.Sprintf("%.1f%%", (monthlyTotal/j.cfg.Budget.LimitUSD)*100),
		)
		return nil
	}

	n := notifier.New(j.cfg, j.log)
	if err := n.Send(ctx, analysis, monthlyTotal, yesterdayCost, dailyCost); err != nil {
		return fmt.Errorf("send teams: %w", err)
	}
	j.log.Infow("job finished successfully",
		"daily_cost", fmt.Sprintf("$%.4f", dailyCost),
		"yesterday_cost", fmt.Sprintf("$%.4f", yesterdayCost),
		"monthly_total", fmt.Sprintf("$%.2f", monthlyTotal),
		"budget_pct", fmt.Sprintf("%.1f%%", (monthlyTotal/j.cfg.Budget.LimitUSD)*100),
	)
	return nil
}

// ── printReport ───────────────────────────────────────────────────────────────

// printReport logs a human-readable cost summary that mirrors the Adaptive Card
// text content line-by-line (same order and format as buildCard in notifier.go).
func (j *Job) printReport(a *analyzer.Analysis, monthlyTotal, dailyCost float64) {
	cfg := j.cfg
	budgetPct := (monthlyTotal / cfg.Budget.LimitUSD) * 100
	budgetRemain := cfg.Budget.LimitUSD - monthlyTotal
	dateStr := a.Date.Format("02 Jan 2006")

	j.log.Infow(fmt.Sprintf("📊 AWS Cost Report — %s", strings.ToUpper(cfg.App.Env)))
	j.log.Infow(dateStr)

	j.log.Infow("📅 วันที่")
	j.log.Infow(dateStr)
	j.log.Infow("🌍 Environment")
	j.log.Infow(cfg.App.Env)
	j.log.Infow("💰 Cost วันนี้")
	j.log.Infow(fmt.Sprintf("$%.4f", dailyCost))
	j.log.Infow("📊 เดือนนี้รวม")
	j.log.Infow(fmt.Sprintf("$%.2f / $%.0f", monthlyTotal, cfg.Budget.LimitUSD))
	j.log.Infow("📈 ใช้ไปแล้ว")
	j.log.Infow(fmt.Sprintf("%.1f%%", budgetPct))
	j.log.Infow("💵 คงเหลือ")
	j.log.Infow(fmt.Sprintf("$%.2f", budgetRemain))
	j.log.Infow("📋 Line items")
	j.log.Infow(fmt.Sprintf("%d", a.ItemCount))

	alertEmoji := "🟢"
	if budgetPct >= 70 {
		alertEmoji = "🟡"
	}
	if budgetPct >= 90 {
		alertEmoji = "🔴"
	}
	filled := int(budgetPct / 5)
	if filled > 20 {
		filled = 20
	}
	progressBar := strings.Repeat("▓", filled) + strings.Repeat("░", 20-filled)
	j.log.Infow(fmt.Sprintf("%s %s %.1f%%", alertEmoji, progressBar, budgetPct))

	j.log.Infow("🏷️ Cost by Service")
	for i, sc := range a.ByService {
		if i >= 8 {
			break
		}
		pct := 0.0
		if a.TotalCost > 0 {
			pct = sc.Cost / a.TotalCost * 100
		}
		bar := strings.Repeat("█", int(pct/5))
		svcName := sc.Service
		if svcName == "" {
			svcName = "Unknown"
		}
		j.log.Infow(fmt.Sprintf("%s $%.4f %.0f%% %s", truncateStr(svcName, 28), sc.Cost, pct, bar))
	}

	j.log.Infow("🔍 Top Resources")
	topN := a.TopResources
	if len(topN) > 8 {
		topN = topN[:8]
	}
	for _, r := range topN {
		label := r.TagName
		if label == "" {
			label = r.ResourceID
		}
		if label == "" {
			label = "unknown"
		}
		svc := r.Service
		if svc == "" {
			svc = "AWS"
		}
		header := fmt.Sprintf("[%s] %s", truncateStr(svc, 15), truncateStr(label, 35))
		if r.InstanceType != "" {
			header += fmt.Sprintf(" (%s)", r.InstanceType)
		}
		if r.TagOwner != "" {
			header += fmt.Sprintf(" [%s]", r.TagOwner)
		}
		header += fmt.Sprintf(" — $%.4f", r.Cost)
		j.log.Infow(header)

		for k, li := range r.LineItems {
			if k >= 2 {
				break
			}
			j.log.Infow(fmt.Sprintf("└ %s", truncateStr(li.Description, 50)))
			j.log.Infow(fmt.Sprintf("%.2f %s", li.UsageAmount, li.UsageUnit))
			j.log.Infow(fmt.Sprintf("$%.4f", li.Cost))
		}
	}

	if len(a.Suspicious) > 0 {
		j.log.Infow("⚠️ ต้องตรวจสอบ")
		for i, s := range a.Suspicious {
			if i >= 5 {
				break
			}
			j.log.Infow(fmt.Sprintf("%s — $%.4f", truncateStr(s.ResourceID, 40), s.Cost))
			for _, f := range s.Flags {
				j.log.Infow("　" + f)
			}
		}
	}

	if budgetPct >= cfg.Budget.AlertThreshPct {
		j.log.Infow(fmt.Sprintf("🚨 ALERT: ใช้งบประมาณไปแล้ว %.1f%% — กรุณาตรวจสอบ resource ที่ไม่จำเป็น", budgetPct))
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────


func (j *Job) curSource() string {
	if j.cfg.CUR.LocalPath != "" {
		return "local:" + j.cfg.CUR.LocalPath
	}
	return "s3://" + j.cfg.CUR.S3Bucket + "/" + j.cfg.CUR.S3Prefix
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// sanitizeUTF8 removes any byte sequence that is not valid UTF-8.
// CUR CSV files from AWS may contain Windows-1252 encoded characters (e.g. 0x94)
// that PostgreSQL rejects with SQLSTATE 22021.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r != utf8.RuneError || size != 1 {
			b.WriteRune(r)
		}
		// skip invalid single bytes (e.g. 0x94)
		i += size
	}
	return b.String()
}

