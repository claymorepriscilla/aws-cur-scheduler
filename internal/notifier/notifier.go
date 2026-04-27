// Package notifier builds and sends Microsoft Teams Adaptive Card messages.
package notifier

import (
	"context"
	"fmt"
	"strings"

	"github.com/claymorepriscilla/aws-cur-scheduler/internal/analyzer"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/httpclient"
	"github.com/claymorepriscilla/aws-cur-scheduler/pkg/logger"
)

// Notifier sends cost reports to Microsoft Teams.
type Notifier struct {
	cfg  *config.Config
	http *httpclient.Client
	log  *logger.Logger
}

// New creates a Notifier.
func New(cfg *config.Config, log *logger.Logger) *Notifier {
	return &Notifier{
		cfg:  cfg,
		http: httpclient.New(cfg.Teams.TimeoutSec, 2),
		log:  log,
	}
}

// Send builds an Adaptive Card from analysis + monthly total and POSTs it to Teams.
// dailyCost = actual charges for the day (today's cumulative - yesterday's cumulative).
func (n *Notifier) Send(ctx context.Context, analysis *analyzer.Analysis, monthlyTotal, yesterdayCost, dailyCost float64) error {
	card := n.buildCard(analysis, monthlyTotal, dailyCost)
	n.log.Infow("sending Teams notification",
		"date", analysis.Date.Format("2006-01-02"),
		"daily_cost", fmt.Sprintf("$%.4f", dailyCost),
		"yesterday_cost", fmt.Sprintf("$%.4f", yesterdayCost),
		"monthly_total", fmt.Sprintf("$%.2f", monthlyTotal),
	)

	if err := n.http.PostJSON(ctx, n.cfg.Teams.WebhookURL, card); err != nil {
		return fmt.Errorf("teams post: %w", err)
	}
	return nil
}

// ac is a shorthand type for Adaptive Card JSON elements.
type ac = map[string]interface{}

func (n *Notifier) buildCard(a *analyzer.Analysis, monthlyTotal, dailyCost float64) map[string]interface{} {
	cfg := n.cfg
	dateStr := a.Date.Format("02 Jan 2006")
	budgetPct := (monthlyTotal / cfg.Budget.LimitUSD) * 100
	budgetRemain := cfg.Budget.LimitUSD - monthlyTotal
	isAlert := budgetPct >= cfg.Budget.AlertThreshPct
	env := strings.ToUpper(cfg.App.Env)

	// Budget color and emoji
	alertColor := "Good"
	alertEmoji := "🟢"
	if budgetPct >= 70 {
		alertColor = "Warning"
		alertEmoji = "🟡"
	}
	if budgetPct >= 90 {
		alertColor = "Attention"
		alertEmoji = "🔴"
	}

	// Progress bar (20 chars)
	filled := int(budgetPct / 5)
	if filled > 20 {
		filled = 20
	}
	progressBar := strings.Repeat("▓", filled) + strings.Repeat("░", 20-filled)

	body := []interface{}{
		// ── Header ───────────────────────────────────────────────
		ac{
			"type":  "Container",
			"style": "emphasis",
			"bleed": true,
			"items": []interface{}{
				ac{
					"type": "TextBlock",
					"text": fmt.Sprintf("📊 AWS Cost Report — %s", env),
					"weight": "Bolder", "size": "Large", "wrap": true,
				},
				ac{
					"type": "TextBlock", "text": dateStr,
					"spacing": "None", "isSubtle": true,
				},
			},
		},

		// ── Summary FactSet ───────────────────────────────────────
		ac{
			"type": "FactSet",
			"facts": []interface{}{
				ac{"title": "📅 วันที่", "value": dateStr},
				ac{"title": "🌍 Environment", "value": cfg.App.Env},
				ac{"title": "💰 Cost วันนี้", "value": fmt.Sprintf("$%.4f", dailyCost)},
				ac{"title": "📊 เดือนนี้รวม", "value": fmt.Sprintf("$%.2f / $%.0f", monthlyTotal, cfg.Budget.LimitUSD)},
				ac{"title": "📈 ใช้ไปแล้ว", "value": fmt.Sprintf("%.1f%%", budgetPct)},
				ac{"title": "💵 คงเหลือ", "value": fmt.Sprintf("$%.2f", budgetRemain)},
				ac{"title": "📋 Line items", "value": fmt.Sprintf("%d", a.ItemCount)},
			},
		},

		// ── Budget progress bar ───────────────────────────────────
		ac{
			"type": "Container", "separator": true,
			"items": []interface{}{
				ac{
					"type":     "TextBlock",
					"text":     fmt.Sprintf("%s `%s` **%.1f%%**", alertEmoji, progressBar, budgetPct),
					"fontType": "Monospace", "wrap": true, "color": alertColor,
				},
			},
		},
	}

	// ── Cost by Service ───────────────────────────────────────────
	body = append(body, ac{
		"type": "TextBlock", "text": "🏷️ **Cost by Service**",
		"weight": "Bolder", "separator": true, "spacing": "Medium",
	})
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
		body = append(body, ac{
			"type":     "TextBlock",
			"text":     fmt.Sprintf("`%-28s` **$%.4f** %.0f%% %s", truncate(svcName, 28), sc.Cost, pct, bar),
			"fontType": "Monospace", "spacing": "None", "wrap": true,
		})
	}

	// ── Top Resources ─────────────────────────────────────────────
	body = append(body, ac{
		"type": "TextBlock", "text": "🔍 **Top Resources**",
		"weight": "Bolder", "separator": true, "spacing": "Medium",
	})
	// Limit to top 8 in Teams card to stay within 28KB Adaptive Card size limit
	topN := a.TopResources
	if len(topN) > 8 {
		topN = topN[:8]
	}
	for i, r := range topN {
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

		header := fmt.Sprintf("%d. **[%s]** `%s`", i+1, truncate(svc, 15), truncate(label, 35))
		if r.InstanceType != "" {
			header += fmt.Sprintf(" (%s)", r.InstanceType)
		}
		if r.TagOwner != "" {
			header += fmt.Sprintf(" [%s]", r.TagOwner)
		}
		header += fmt.Sprintf(" — **$%.4f**", r.Cost)

		body = append(body, ac{
			"type": "TextBlock", "text": header,
			"wrap": true, "spacing": "Small",
		})
		for j, li := range r.LineItems {
			if j >= 2 { // max 2 line items per resource to reduce payload size
				break
			}
			// ColumnSet aligns description | usage | cost in fixed columns
			body = append(body, ac{
				"type": "ColumnSet", "spacing": "None",
				"columns": []interface{}{
					ac{
						"type": "Column", "width": "10px",
						"items": []interface{}{
							ac{"type": "TextBlock", "text": "└", "isSubtle": true, "size": "Small"},
						},
					},
					ac{
						"type": "Column", "width": "stretch",
						"items": []interface{}{
							ac{"type": "TextBlock", "text": truncate(li.Description, 50),
								"isSubtle": true, "size": "Small", "wrap": true},
						},
					},
					ac{
						"type": "Column", "width": "90px",
						"items": []interface{}{
							ac{"type": "TextBlock",
								"text":                fmt.Sprintf("%.2f %s", li.UsageAmount, li.UsageUnit),
								"isSubtle":            true, "size": "Small",
								"horizontalAlignment": "Right"},
						},
					},
					ac{
						"type": "Column", "width": "75px",
						"items": []interface{}{
							ac{"type": "TextBlock",
								"text":                fmt.Sprintf("**$%.4f**", li.Cost),
								"size":                "Small",
								"horizontalAlignment": "Right"},
						},
					},
				},
			})
		}
	}

	// ── Suspicious ────────────────────────────────────────────────
	if len(a.Suspicious) > 0 {
		body = append(body, ac{
			"type": "TextBlock", "text": "⚠️ **ต้องตรวจสอบ**",
			"weight": "Bolder", "color": "Warning",
			"separator": true, "spacing": "Medium",
		})
		for i, s := range a.Suspicious {
			if i >= 5 {
				break
			}
			body = append(body, ac{
				"type":    "TextBlock",
				"text":    fmt.Sprintf("`%s` — **$%.4f**", truncate(s.ResourceID, 40), s.Cost),
				"color":   "Warning", "wrap": true, "spacing": "Small",
			})
			for _, f := range s.Flags {
				body = append(body, ac{
					"type": "TextBlock", "text": "　" + f,
					"isSubtle": true, "size": "Small", "wrap": true, "spacing": "None",
				})
			}
		}
	}

	// ── Budget alert ──────────────────────────────────────────────
	if isAlert {
		body = append(body, ac{
			"type":   "TextBlock",
			"text":   fmt.Sprintf("🚨 **ALERT: ใช้งบประมาณไปแล้ว %.1f%%** — กรุณาตรวจสอบ resource ที่ไม่จำเป็น", budgetPct),
			"color":  "Attention", "weight": "Bolder", "wrap": true,
			"separator": true, "spacing": "Medium",
		})
	}

	return map[string]interface{}{
		"type": "message",
		"attachments": []interface{}{
			map[string]interface{}{
				"contentType": "application/vnd.microsoft.card.adaptive",
				"content": map[string]interface{}{
					"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
					"type":    "AdaptiveCard",
					"version": "1.4",
					"body":    body,
				},
			},
		},
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
