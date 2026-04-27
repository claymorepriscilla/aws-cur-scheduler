package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	awsclient "github.com/claymorepriscilla/aws-cur-scheduler/internal/aws"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/analyzer"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/store"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/logger"
)

// ── mockStore ─────────────────────────────────────────────────────────────────

type mockStore struct {
	snapshots        []*store.DailyCostSnapshot
	lineItems        []store.LineItem
	alerts           []store.Alert
	yesterdayCost    float64
	found            bool
	yesterdayCostErr error
	upsertErr        error
	lineItemErr      error
	alertErr         error
}

func (m *mockStore) UpsertSnapshot(_ context.Context, s *store.DailyCostSnapshot) error {
	if m.upsertErr != nil {
		return m.upsertErr
	}
	m.snapshots = append(m.snapshots, s)
	return nil
}

func (m *mockStore) GetYesterdayCost(_ context.Context, _ time.Time, _ string) (float64, bool, error) {
	return m.yesterdayCost, m.found, m.yesterdayCostErr
}

func (m *mockStore) GetMonthlyTotal(_ context.Context, _ int, _ time.Month, _ string) (float64, error) {
	return 0, nil
}

func (m *mockStore) ReplaceLineItems(_ context.Context, items []store.LineItem) error {
	if m.lineItemErr != nil {
		return m.lineItemErr
	}
	m.lineItems = items
	return nil
}

func (m *mockStore) ReplaceAlerts(_ context.Context, alerts []store.Alert) error {
	if m.alertErr != nil {
		return m.alertErr
	}
	m.alerts = alerts
	return nil
}

func (m *mockStore) Close() error { return nil }

// ── test helpers ──────────────────────────────────────────────────────────────

func testLogger(t *testing.T) *logger.Logger {
	t.Helper()
	log, err := logger.New("debug", "console")
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}
	return log
}

func testConfig() *config.Config {
	return &config.Config{
		App:    config.AppConfig{Env: "test"},
		Teams:  config.TeamsConfig{EnableWebhook: false, TimeoutSec: 5},
		Budget: config.BudgetConfig{LimitUSD: 500, AlertThreshPct: 70},
		Report: config.ReportConfig{TopNResources: 15},
	}
}

// createTempCSV writes a minimal CUR v1 CSV to a temp dir and returns the path.
func createTempCSV(t *testing.T) string {
	t.Helper()
	content := `lineItem/LineItemType,lineItem/UnblendedCost,product/ProductName,lineItem/ResourceId,lineItem/UsageType,lineItem/LineItemDescription,lineItem/UsageAmount,pricing/unit,product/region,product/instanceType,lineItem/Operation,resourceTags/user:Name,resourceTags/user:Owner,resourceTags/user:Environment,lineItem/UsageStartDate,lineItem/AvailabilityZone
Usage,1.12,Amazon EC2,i-0abc123,BoxUsage:t3.medium,Linux/UNIX t3.medium,24,Hrs,ap-southeast-1,t3.medium,RunInstances,dev-api,john,dev,2025-04-10T00:00:00Z,ap-southeast-1a
Usage,2.89,Amazon RDS,arn:aws:rds:ap-southeast-1:123:db:test,RDS:db.t3.small,db.t3.small Multi-AZ,24,Hrs,ap-southeast-1,db.t3.small,CreateDBInstance,test-db,john,dev,2025-04-10T00:00:00Z,
`
	f := filepath.Join(t.TempDir(), "test.csv")
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatalf("create temp CSV: %v", err)
	}
	return f
}

func testAnalysis() *analyzer.Analysis {
	return &analyzer.Analysis{
		Date:      time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC),
		TotalCost: 12.38,
		ByService: []analyzer.ServiceCost{
			{Service: "Amazon EC2", Cost: 6.21},
			{Service: "Amazon RDS", Cost: 2.89},
		},
		TopResources: []analyzer.Resource{
			{
				Service: "Amazon EC2", ResourceID: "i-0abc", TagName: "api",
				TagOwner: "john", InstanceType: "t3.medium", Cost: 6.21,
				LineItems: []analyzer.LineItemSummary{
					{Description: "Linux t3.medium", UsageAmount: 24, UsageUnit: "Hrs", Cost: 6.21},
				},
			},
		},
		Suspicious: []analyzer.SuspiciousResource{
			{ResourceID: "i-notag", Service: "Amazon EC2", Cost: 1.0,
				Flags: []string{"🟡 Warning — EC2 24hr", "🟠 Notice — EC2 ไม่มี tag Name"}},
		},
		ItemCount: 2,
	}
}

// ── sanitizeUTF8 ─────────────────────────────────────────────────────────────

func TestSanitizeUTF8_ValidASCII(t *testing.T) {
	s := "hello world"
	if got := sanitizeUTF8(s); got != s {
		t.Errorf("sanitizeUTF8(%q) = %q, want %q", s, got, s)
	}
}

func TestSanitizeUTF8_ValidThai(t *testing.T) {
	s := "สวัสดี"
	if got := sanitizeUTF8(s); got != s {
		t.Errorf("sanitizeUTF8(%q) = %q, want %q", s, got, s)
	}
}

func TestSanitizeUTF8_SingleInvalidByte(t *testing.T) {
	// 0x94 = Windows-1252 "right double quotation mark", invalid in UTF-8
	s := "hello\x94world"
	got := sanitizeUTF8(s)
	want := "helloworld"
	if got != want {
		t.Errorf("sanitizeUTF8(%q) = %q, want %q", s, got, want)
	}
}

func TestSanitizeUTF8_MultipleInvalidBytes(t *testing.T) {
	s := "\x94\x96\x97"
	if got := sanitizeUTF8(s); got != "" {
		t.Errorf("sanitizeUTF8(all invalid) = %q, want empty", got)
	}
}

func TestSanitizeUTF8_MixedValid(t *testing.T) {
	s := "cost\x94report\x96summary"
	got := sanitizeUTF8(s)
	want := "costreportsummary"
	if got != want {
		t.Errorf("sanitizeUTF8(%q) = %q, want %q", s, got, want)
	}
}

// ── truncateStr ───────────────────────────────────────────────────────────────

func TestTruncateStr_ShortString(t *testing.T) {
	s := "hello"
	if got := truncateStr(s, 10); got != s {
		t.Errorf("truncateStr(%q, 10) = %q, want %q", s, got, s)
	}
}

func TestTruncateStr_ExactLength(t *testing.T) {
	s := "hello"
	if got := truncateStr(s, 5); got != s {
		t.Errorf("truncateStr(%q, 5) = %q, want %q", s, got, s)
	}
}

func TestTruncateStr_LongString(t *testing.T) {
	s := "hello world"
	got := truncateStr(s, 8)
	want := "hello w…"
	if got != want {
		t.Errorf("truncateStr(%q, 8) = %q, want %q", s, got, want)
	}
}

// ── parseSeverity ─────────────────────────────────────────────────────────────

func TestParseSeverity_Critical(t *testing.T) {
	if got := parseSeverity("🔴 Critical — some message"); got != "critical" {
		t.Errorf("parseSeverity = %q, want critical", got)
	}
}

func TestParseSeverity_Warning(t *testing.T) {
	if got := parseSeverity("🟡 Warning — some message"); got != "warning" {
		t.Errorf("parseSeverity = %q, want warning", got)
	}
}

func TestParseSeverity_Notice(t *testing.T) {
	if got := parseSeverity("🟠 Notice — some message"); got != "notice" {
		t.Errorf("parseSeverity = %q, want notice", got)
	}
}

func TestParseSeverity_Default(t *testing.T) {
	if got := parseSeverity("unknown flag without emoji"); got != "notice" {
		t.Errorf("parseSeverity(unknown) = %q, want notice", got)
	}
}

// ── parseMessage ──────────────────────────────────────────────────────────────

func TestParseMessage_WithSeparator(t *testing.T) {
	got := parseMessage("🔴 Critical — Elastic IP ที่ไม่ได้ใช้งาน")
	want := "Elastic IP ที่ไม่ได้ใช้งาน"
	if got != want {
		t.Errorf("parseMessage = %q, want %q", got, want)
	}
}

func TestParseMessage_WithoutSeparator(t *testing.T) {
	flag := "plain message without separator"
	if got := parseMessage(flag); got != flag {
		t.Errorf("parseMessage = %q, want %q", got, flag)
	}
}

// ── toSnapshot ────────────────────────────────────────────────────────────────

func TestToSnapshot(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	a := &analyzer.Analysis{
		TotalCost: 12.5,
		ItemCount: 10,
		ByService: []analyzer.ServiceCost{
			{Service: "Amazon EC2", Cost: 10.0},
			{Service: "Amazon RDS", Cost: 2.5},
		},
	}
	snap := toSnapshot(a, "dev", date)

	if snap.ReportDate != date {
		t.Errorf("ReportDate = %v, want %v", snap.ReportDate, date)
	}
	if snap.Env != "dev" {
		t.Errorf("Env = %q, want dev", snap.Env)
	}
	if snap.TotalCost != 12.5 {
		t.Errorf("TotalCost = %.2f, want 12.5", snap.TotalCost)
	}
	if snap.ItemCount != 10 {
		t.Errorf("ItemCount = %d, want 10", snap.ItemCount)
	}
	if len(snap.ByService) != 2 {
		t.Errorf("ByService len = %d, want 2", len(snap.ByService))
	}
	if snap.ByService[0].Service != "Amazon EC2" {
		t.Errorf("ByService[0].Service = %q, want Amazon EC2", snap.ByService[0].Service)
	}
}

// ── toLineItems ───────────────────────────────────────────────────────────────

func TestToLineItems_AllFields(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	items := []awsclient.LineItem{
		{
			Service:      "Amazon EC2",
			ResourceID:   "i-0abc",
			Description:  "Linux t3.medium\x94invalid", // contains invalid UTF-8 byte
			UsageType:    "BoxUsage:t3.medium",
			UsageAmount:  24,
			UsageUnit:    "Hrs",
			Operation:    "RunInstances",
			InstanceType: "t3.medium",
			Region:       "ap-southeast-1",
			TagName:      "dev-api",
			TagOwner:     "john",
			TagEnv:       "dev",
			Cost:         1.12,
		},
	}
	out := toLineItems(items, date, "test")

	if len(out) != 1 {
		t.Fatalf("toLineItems len = %d, want 1", len(out))
	}
	li := out[0]
	if li.ReportDate != date {
		t.Errorf("ReportDate = %v, want %v", li.ReportDate, date)
	}
	if li.Env != "test" {
		t.Errorf("Env = %q, want test", li.Env)
	}
	if li.Service != "Amazon EC2" {
		t.Errorf("Service = %q, want Amazon EC2", li.Service)
	}
	// The \x94 byte should be stripped by sanitizeUTF8
	if li.Description != "Linux t3.mediuminvalid" {
		t.Errorf("Description = %q, expected invalid byte stripped", li.Description)
	}
	if li.Cost != 1.12 {
		t.Errorf("Cost = %.2f, want 1.12", li.Cost)
	}
	if li.TagName != "dev-api" {
		t.Errorf("TagName = %q, want dev-api", li.TagName)
	}
	if li.TagOwner != "john" {
		t.Errorf("TagOwner = %q, want john", li.TagOwner)
	}
	if li.TagEnv != "dev" {
		t.Errorf("TagEnv = %q, want dev", li.TagEnv)
	}
}

func TestToLineItems_Empty(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	out := toLineItems([]awsclient.LineItem{}, date, "test")
	if len(out) != 0 {
		t.Errorf("toLineItems(empty) len = %d, want 0", len(out))
	}
}

// ── toAlerts ──────────────────────────────────────────────────────────────────

func TestToAlerts_MultipleFlags(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	suspicious := []analyzer.SuspiciousResource{
		{
			ResourceID: "i-0abc",
			Service:    "Amazon EC2",
			Cost:       1.5,
			TagOwner:   "john",
			Flags: []string{
				"🔴 Critical — EBS unattached",
				"🟡 Warning — EC2 running 24hr",
				"🟠 Notice — EC2 no Owner tag",
			},
		},
	}
	alerts := toAlerts(suspicious, date, "test")

	if len(alerts) != 3 {
		t.Fatalf("toAlerts len = %d, want 3", len(alerts))
	}
	if alerts[0].Severity != "critical" {
		t.Errorf("alerts[0].Severity = %q, want critical", alerts[0].Severity)
	}
	if alerts[1].Severity != "warning" {
		t.Errorf("alerts[1].Severity = %q, want warning", alerts[1].Severity)
	}
	if alerts[2].Severity != "notice" {
		t.Errorf("alerts[2].Severity = %q, want notice", alerts[2].Severity)
	}
	if alerts[0].Message != "EBS unattached" {
		t.Errorf("alerts[0].Message = %q, want 'EBS unattached'", alerts[0].Message)
	}
	if alerts[0].ResourceCost != 1.5 {
		t.Errorf("alerts[0].ResourceCost = %.1f, want 1.5", alerts[0].ResourceCost)
	}
	if alerts[0].Env != "test" {
		t.Errorf("alerts[0].Env = %q, want test", alerts[0].Env)
	}
	if alerts[0].TagOwner != "john" {
		t.Errorf("alerts[0].TagOwner = %q, want john", alerts[0].TagOwner)
	}
}

func TestToAlerts_Empty(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	if alerts := toAlerts(nil, date, "test"); len(alerts) != 0 {
		t.Errorf("toAlerts(nil) = %d alerts, want 0", len(alerts))
	}
}

func TestToAlerts_MultipleSuspicious(t *testing.T) {
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	suspicious := []analyzer.SuspiciousResource{
		{ResourceID: "r1", Service: "EC2", Cost: 1.0, Flags: []string{"🔴 Critical — flag1"}},
		{ResourceID: "r2", Service: "RDS", Cost: 2.0, Flags: []string{"🟡 Warning — flag2", "🟠 Notice — flag3"}},
	}
	alerts := toAlerts(suspicious, date, "prod")
	if len(alerts) != 3 {
		t.Errorf("toAlerts len = %d, want 3", len(alerts))
	}
}

// ── curSource ─────────────────────────────────────────────────────────────────

func TestCurSource_LocalPath(t *testing.T) {
	cfg := testConfig()
	cfg.CUR = config.CURConfig{LocalPath: "/tmp/test.csv"}
	j := &Job{cfg: cfg, log: testLogger(t)}
	got := j.curSource()
	want := "local:/tmp/test.csv"
	if got != want {
		t.Errorf("curSource() = %q, want %q", got, want)
	}
}

func TestCurSource_S3(t *testing.T) {
	cfg := testConfig()
	cfg.CUR = config.CURConfig{S3Bucket: "my-bucket", S3Prefix: "cur/prefix"}
	j := &Job{cfg: cfg, log: testLogger(t)}
	got := j.curSource()
	want := "s3://my-bucket/cur/prefix"
	if got != want {
		t.Errorf("curSource() = %q, want %q", got, want)
	}
}

// ── NewJob ────────────────────────────────────────────────────────────────────

func TestNewJob_DBDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Database = config.DatabaseConfig{Enabled: false}
	j, err := NewJob(cfg, testLogger(t))
	if err != nil {
		t.Fatalf("NewJob returned unexpected error: %v", err)
	}
	if j == nil {
		t.Fatal("NewJob returned nil job")
	}
	if j.store != nil {
		t.Error("store should be nil when DB disabled")
	}
}

func TestNewJob_DBEnabled_ConnectionFails(t *testing.T) {
	// Port 9 (Discard) is almost always closed → pgx Ping gets "connection refused" immediately.
	// NewJob must return an error and nil job when DB is enabled but connection fails.
	cfg := testConfig()
	cfg.Database = config.DatabaseConfig{
		Enabled:    true,
		Host:       "127.0.0.1",
		Port:       9, // Discard port — connection refused immediately
		Database:   "postgres",
		User:       "postgres",
		Password:   "wrong",
		SSLMode:    "disable",
		TimeoutSec: 2,
	}
	j, err := NewJob(cfg, testLogger(t))
	if err == nil {
		t.Fatal("expected error when DB connection fails, got nil")
	}
	if j != nil {
		t.Error("expected nil job when DB connection fails")
	}
}

func TestNewJob_DBEnabled_ZeroTimeout(t *testing.T) {
	// TimeoutSec = 0 exercises the "if timeout <= 0 { timeout = 10s }" branch.
	// Connection still fails fast (port 9), so NewJob returns an error.
	cfg := testConfig()
	cfg.Database = config.DatabaseConfig{
		Enabled:    true,
		Host:       "127.0.0.1",
		Port:       9,
		Database:   "postgres",
		User:       "postgres",
		Password:   "wrong",
		SSLMode:    "disable",
		TimeoutSec: 0, // triggers default 10s path — but connection fails fast so test stays quick
	}
	j, err := NewJob(cfg, testLogger(t))
	if err == nil {
		t.Fatal("expected error when DB connection fails, got nil")
	}
	if j != nil {
		t.Error("expected nil job when DB connection fails")
	}
}

// ── getYesterdayCost ──────────────────────────────────────────────────────────

func TestGetYesterdayCost_NilStore(t *testing.T) {
	j := &Job{cfg: testConfig(), log: testLogger(t), store: nil}
	cost := j.getYesterdayCost(context.Background(), time.Now())
	if cost != 0 {
		t.Errorf("expected 0 with nil store, got %f", cost)
	}
}

func TestGetYesterdayCost_Found(t *testing.T) {
	ms := &mockStore{yesterdayCost: 71.0, found: true}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	cost := j.getYesterdayCost(context.Background(), time.Date(2025, 4, 11, 0, 0, 0, 0, time.UTC))
	if cost != 71.0 {
		t.Errorf("expected 71.0, got %f", cost)
	}
}

func TestGetYesterdayCost_NotFound(t *testing.T) {
	ms := &mockStore{found: false}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	cost := j.getYesterdayCost(context.Background(), time.Now())
	if cost != 0 {
		t.Errorf("expected 0 when not found, got %f", cost)
	}
}

func TestGetYesterdayCost_Error(t *testing.T) {
	ms := &mockStore{yesterdayCostErr: fmt.Errorf("db error")}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	cost := j.getYesterdayCost(context.Background(), time.Now())
	if cost != 0 {
		t.Errorf("expected 0 on error, got %f", cost)
	}
}

// ── saveToStore ───────────────────────────────────────────────────────────────

func TestSaveToStore_NilStore(t *testing.T) {
	j := &Job{cfg: testConfig(), log: testLogger(t), store: nil}
	// Should not panic
	j.saveToStore(context.Background(), testAnalysis(), nil, time.Now())
}

func TestSaveToStore_WithStore(t *testing.T) {
	ms := &mockStore{}
	cfg := testConfig()
	j := &Job{cfg: cfg, log: testLogger(t), store: ms}

	rawItems := []awsclient.LineItem{
		{Service: "Amazon EC2", ResourceID: "i-0abc", Cost: 1.0, UsageAmount: 24},
	}
	date := time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)
	j.saveToStore(context.Background(), testAnalysis(), rawItems, date)

	if len(ms.snapshots) != 1 {
		t.Errorf("expected 1 snapshot saved, got %d", len(ms.snapshots))
	}
	if len(ms.lineItems) != 1 {
		t.Errorf("expected 1 line item saved, got %d", len(ms.lineItems))
	}
}

func TestSaveToStore_UpsertError(t *testing.T) {
	ms := &mockStore{upsertErr: fmt.Errorf("upsert fail")}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	// Should log warning but not panic
	j.saveToStore(context.Background(), testAnalysis(), nil, time.Now())
}

func TestSaveToStore_LineItemError(t *testing.T) {
	ms := &mockStore{lineItemErr: fmt.Errorf("line item fail")}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	rawItems := []awsclient.LineItem{{Service: "EC2", Cost: 1.0}}
	j.saveToStore(context.Background(), testAnalysis(), rawItems, time.Now())
}

func TestSaveToStore_AlertError(t *testing.T) {
	ms := &mockStore{alertErr: fmt.Errorf("alert fail")}
	j := &Job{cfg: testConfig(), log: testLogger(t), store: ms}
	j.saveToStore(context.Background(), testAnalysis(), nil, time.Now())
}

// ── printReport ───────────────────────────────────────────────────────────────

func TestPrintReport_Normal(t *testing.T) {
	j := &Job{cfg: testConfig(), log: testLogger(t)}
	a := testAnalysis()
	// Should not panic
	j.printReport(a, 12.38, 5.38)
}

func TestPrintReport_AllBranches(t *testing.T) {
	j := &Job{cfg: testConfig(), log: testLogger(t)}

	// Build analysis with edge cases
	a := &analyzer.Analysis{
		Date:      time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC),
		TotalCost: 0, // pct = 0.0 branch
		ItemCount: 10,
		ByService: func() []analyzer.ServiceCost {
			svcs := make([]analyzer.ServiceCost, 9)
			svcs[0] = analyzer.ServiceCost{Service: "", Cost: 5} // empty name → "Unknown"
			for i := 1; i < 9; i++ {
				svcs[i] = analyzer.ServiceCost{Service: fmt.Sprintf("Svc%d", i), Cost: float64(9 - i)}
			}
			return svcs
		}(),
		TopResources: func() []analyzer.Resource {
			res := make([]analyzer.Resource, 9)
			// resource with no TagName, no ResourceID, no Service → "unknown", "AWS"
			res[0] = analyzer.Resource{
				Service: "", ResourceID: "", TagName: "", Cost: 1.0,
				LineItems: []analyzer.LineItemSummary{
					{Description: "d1", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.4},
					{Description: "d2", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.3},
					{Description: "d3", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.3}, // triggers k >= 2
				},
			}
			for i := 1; i < 9; i++ {
				res[i] = analyzer.Resource{
					Service: "EC2", ResourceID: fmt.Sprintf("i-%04d", i),
					TagOwner: "alice", InstanceType: "t3.medium", Cost: 1.0,
					LineItems: []analyzer.LineItemSummary{
						{Description: "d", UsageAmount: 1, UsageUnit: "Hrs", Cost: 1.0},
					},
				}
			}
			return res
		}(),
		Suspicious: func() []analyzer.SuspiciousResource {
			sus := make([]analyzer.SuspiciousResource, 6)
			for i := range sus {
				sus[i] = analyzer.SuspiciousResource{
					ResourceID: fmt.Sprintf("r%d", i),
					Flags:      []string{"🟡 Warning — test"},
				}
			}
			return sus
		}(),
	}

	// budgetPct = 0 (TotalCost=0)
	j.printReport(a, 0, 0)

	// budgetPct = 70% → Warning emoji
	j.printReport(a, 350.0, 350.0)

	// budgetPct = 90% → Attention emoji + budget alert
	j.printReport(a, 450.0, 450.0)

	// budgetPct > 100% → progress bar capped at 20
	j.printReport(a, 600.0, 600.0)
}

// ── maybeNotify ───────────────────────────────────────────────────────────────

func TestMaybeNotify_WebhookDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Teams.EnableWebhook = false
	j := &Job{cfg: cfg, log: testLogger(t)}

	err := j.maybeNotify(context.Background(), testAnalysis(), 12.38, 0, 12.38)
	if err != nil {
		t.Errorf("maybeNotify (webhook disabled) unexpected error: %v", err)
	}
}

func TestMaybeNotify_WebhookEnabled_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Teams = config.TeamsConfig{
		WebhookURL: srv.URL, TimeoutSec: 5, EnableWebhook: true,
	}
	j := &Job{cfg: cfg, log: testLogger(t)}

	err := j.maybeNotify(context.Background(), testAnalysis(), 12.38, 0, 12.38)
	if err != nil {
		t.Errorf("maybeNotify (webhook enabled) unexpected error: %v", err)
	}
}

func TestMaybeNotify_WebhookEnabled_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := testConfig()
	cfg.Teams = config.TeamsConfig{
		WebhookURL: srv.URL, TimeoutSec: 5, EnableWebhook: true,
	}
	j := &Job{cfg: cfg, log: testLogger(t)}

	err := j.maybeNotify(context.Background(), testAnalysis(), 12.38, 0, 12.38)
	if err == nil {
		t.Error("expected error for HTTP 500, got nil")
	}
}

// ── Run (integration with local file) ────────────────────────────────────────

func TestRun_LocalFile_WebhookDisabled(t *testing.T) {
	csvPath := createTempCSV(t)

	cfg := testConfig()
	cfg.CUR = config.CURConfig{LocalPath: csvPath}

	ms := &mockStore{found: true, yesterdayCost: 0}
	j := &Job{
		cfg:      cfg,
		log:      testLogger(t),
		analyzer: analyzer.New(15),
		store:    ms,
	}

	err := j.Run(context.Background(), time.Date(2025, 4, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Errorf("Run() unexpected error: %v", err)
	}
	// Snapshot and line items should have been saved
	if len(ms.snapshots) != 1 {
		t.Errorf("expected 1 snapshot, got %d", len(ms.snapshots))
	}
	if len(ms.lineItems) != 2 {
		t.Errorf("expected 2 line items (2 CSV rows), got %d", len(ms.lineItems))
	}
}

func TestRun_LocalFile_ZeroItems(t *testing.T) {
	// CSV with no matching rows (all filtered out as non-Usage or zero cost)
	content := `lineItem/LineItemType,lineItem/UnblendedCost,product/ProductName,lineItem/ResourceId,lineItem/UsageType,lineItem/LineItemDescription,lineItem/UsageAmount,pricing/unit,product/region,product/instanceType,lineItem/Operation,resourceTags/user:Name,resourceTags/user:Owner,resourceTags/user:Environment,lineItem/UsageStartDate,lineItem/AvailabilityZone
RIFee,0.0,Amazon EC2,i-0abc,BoxUsage:t3.medium,Reserved,720,Hrs,ap-southeast-1,t3.medium,RunInstances,,,, ,
`
	f := filepath.Join(t.TempDir(), "empty.csv")
	if err := os.WriteFile(f, []byte(content), 0600); err != nil {
		t.Fatalf("write CSV: %v", err)
	}

	cfg := testConfig()
	cfg.CUR = config.CURConfig{LocalPath: f}

	j := &Job{
		cfg:      cfg,
		log:      testLogger(t),
		analyzer: analyzer.New(15),
		store:    nil,
	}

	err := j.Run(context.Background(), time.Now())
	if err != nil {
		t.Errorf("Run() with zero items should not fail, got: %v", err)
	}
}

func TestRun_LocalFileNotFound(t *testing.T) {
	cfg := testConfig()
	cfg.CUR = config.CURConfig{LocalPath: "/nonexistent/file.csv"}

	j := &Job{
		cfg:      cfg,
		log:      testLogger(t),
		analyzer: analyzer.New(15),
	}

	err := j.Run(context.Background(), time.Now())
	if err == nil {
		t.Error("expected error for nonexistent local file, got nil")
	}
}

func TestRun_LocalFile_NilStore(t *testing.T) {
	// Run with nil store — saveToStore and getYesterdayCost should be skipped gracefully
	csvPath := createTempCSV(t)

	cfg := testConfig()
	cfg.CUR = config.CURConfig{LocalPath: csvPath}

	j := &Job{
		cfg:      cfg,
		log:      testLogger(t),
		analyzer: analyzer.New(15),
		store:    nil,
	}

	err := j.Run(context.Background(), time.Date(2025, 4, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Errorf("Run() (nil store) unexpected error: %v", err)
	}
}
