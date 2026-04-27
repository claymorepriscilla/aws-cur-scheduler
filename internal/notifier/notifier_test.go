package notifier_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/claymorepriscilla/aws-cur-scheduler/internal/analyzer"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/notifier"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/logger"
)

func newTestConfig(webhookURL string) *config.Config {
	return &config.Config{
		App:    config.AppConfig{Env: "test", Version: "0.0.1"},
		AWS:    config.AWSConfig{Region: "ap-southeast-1"},
		Teams:  config.TeamsConfig{WebhookURL: webhookURL, TimeoutSec: 5},
		Budget: config.BudgetConfig{LimitUSD: 500, AlertThreshPct: 70},
		Report: config.ReportConfig{TopNResources: 5},
		Log:    config.LogConfig{Level: "debug", Format: "console"},
	}
}

func newTestAnalysis() *analyzer.Analysis {
	return &analyzer.Analysis{
		Date:      time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC),
		TotalCost: 12.38,
		ByService: []analyzer.ServiceCost{
			{Service: "Amazon EC2", Cost: 6.21},
			{Service: "Amazon RDS", Cost: 2.89},
		},
		TopResources: []analyzer.Resource{
			{
				Service:      "Amazon EC2",
				ResourceID:   "i-0abc",
				TagName:      "dev-api",
				TagOwner:     "john",
				InstanceType: "t3.medium",
				Cost:         6.21,
				LineItems: []analyzer.LineItemSummary{
					{Description: "Linux t3.medium", UsageAmount: 24, UsageUnit: "Hrs", Cost: 6.21},
				},
			},
		},
		Suspicious: []analyzer.SuspiciousResource{
			{ResourceID: "i-0nametag", Service: "Amazon EC2", Cost: 0.96, Flags: []string{"🟠 Notice — EC2 ไม่มี tag Name"}},
		},
		ItemCount: 42,
	}
}

// TestSend_Success verifies a well-formed POST with "type":"message" is sent.
func TestSend_Success(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	err := n.Send(context.Background(), newTestAnalysis(), 287.45, 71.0, 5.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
	if received["type"] != "message" {
		t.Errorf("card type = %v, want 'message'", received["type"])
	}
}

// TestSend_ServerError_ReturnsError verifies HTTP 500 surfaces as an error.
func TestSend_ServerError_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	err := n.Send(context.Background(), newTestAnalysis(), 100.0, 50.0, 50.0)
	if err == nil {
		t.Error("expected error for HTTP 500, got nil")
	}
}

// TestSend_BudgetWarning exercises the yellow (Warning) budget color path (70%).
func TestSend_BudgetWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	// monthlyTotal = 350, limitUSD = 500 → 70% → Warning color
	err := n.Send(context.Background(), newTestAnalysis(), 350.0, 300.0, 50.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_BudgetAttention exercises the red (Attention) budget color path (≥90%) + budget alert banner.
func TestSend_BudgetAttention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	// monthlyTotal = 450, limitUSD = 500 → 90% → Attention color + alert
	err := n.Send(context.Background(), newTestAnalysis(), 450.0, 400.0, 50.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_BudgetOverFull exercises the progress bar cap (filled > 20 → capped at 20).
func TestSend_BudgetOverFull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	// monthlyTotal = 600 > limitUSD = 500 → >100% → filled capped at 20
	err := n.Send(context.Background(), newTestAnalysis(), 600.0, 550.0, 50.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_NoSuspicious verifies card builds correctly when Suspicious is nil.
func TestSend_NoSuspicious(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	a.Suspicious = nil

	err := n.Send(context.Background(), a, 12.38, 7.0, 5.38)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_ZeroTotalCost exercises the pct=0.0 branch when TotalCost is zero.
func TestSend_ZeroTotalCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	a.TotalCost = 0
	// Also add a service with empty name to hit the "Unknown" fallback
	a.ByService = append(a.ByService, analyzer.ServiceCost{Service: "", Cost: 0})

	err := n.Send(context.Background(), a, 0.0, 0.0, 0.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_ResourceFallbacks exercises ResourceID and "unknown" fallbacks when TagName is empty.
func TestSend_ResourceFallbacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	// Resource with no TagName → falls back to ResourceID
	a.TopResources[0].TagName = ""
	a.TopResources[0].TagOwner = ""
	a.TopResources[0].InstanceType = ""
	// Extra resource with no TagName AND no ResourceID AND no Service → "unknown" / "AWS"
	a.TopResources = append(a.TopResources, analyzer.Resource{
		Service: "", ResourceID: "", TagName: "", Cost: 0.5,
		LineItems: []analyzer.LineItemSummary{
			{Description: "desc", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.5},
		},
	})

	err := n.Send(context.Background(), a, 12.38, 7.0, 5.38)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_ManyServices exercises the i >= 8 break in the services loop.
func TestSend_ManyServices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	a.ByService = make([]analyzer.ServiceCost, 9)
	for i := 0; i < 9; i++ {
		a.ByService[i] = analyzer.ServiceCost{Service: fmt.Sprintf("Service%d", i+1), Cost: float64(9 - i)}
	}

	err := n.Send(context.Background(), a, 45.0, 20.0, 25.0)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_ManyResources exercises the len(topN) > 8 truncation and j >= 2 line-item break.
func TestSend_ManyResources(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	// Reset and add 10 resources (> 8 limit); each has 3 line items (> 2 limit)
	a.TopResources = make([]analyzer.Resource, 10)
	for i := 0; i < 10; i++ {
		a.TopResources[i] = analyzer.Resource{
			Service:    "Amazon EC2",
			ResourceID: fmt.Sprintf("i-%04d", i),
			Cost:       1.0,
			LineItems: []analyzer.LineItemSummary{
				{Description: "d1", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.4},
				{Description: "d2", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.3},
				{Description: "d3", UsageAmount: 1, UsageUnit: "Hrs", Cost: 0.3}, // triggers j >= 2 break
			},
		}
	}

	err := n.Send(context.Background(), a, 12.38, 7.0, 5.38)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}

// TestSend_ManySuspicious exercises the i >= 5 break in the suspicious loop.
func TestSend_ManySuspicious(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log, _ := logger.New("debug", "console")
	n := notifier.New(newTestConfig(srv.URL), log)

	a := newTestAnalysis()
	// Add 6 more suspicious items (> 5 limit)
	for i := 0; i < 6; i++ {
		a.Suspicious = append(a.Suspicious, analyzer.SuspiciousResource{
			ResourceID: fmt.Sprintf("i-%04d", i),
			Service:    "Amazon EC2",
			Cost:       1.0,
			Flags:      []string{"🟡 Warning — test flag"},
		})
	}

	err := n.Send(context.Background(), a, 12.38, 7.0, 5.38)
	if err != nil {
		t.Fatalf("Send() unexpected error: %v", err)
	}
}
