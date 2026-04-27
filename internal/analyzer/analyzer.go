// Package analyzer processes CUR LineItems and produces structured reports.
package analyzer

import (
	"sort"
	"strings"
	"time"

	awsclient "github.com/claymorepriscilla/aws-cur-scheduler/internal/aws"
)

// ── Domain types ─────────────────────────────────────────────────────────────

// SuspiciousFlag severity levels.
const (
	SeverityCritical = "🔴 Critical"
	SeverityWarning  = "🟡 Warning"
	SeverityNotice   = "🟠 Notice"
)

// LineItemSummary is a grouped sub-item within a resource.
type LineItemSummary struct {
	Description string
	UsageType   string
	UsageAmount float64
	UsageUnit   string
	Cost        float64
	Operation   string
}

// Resource groups all line items for a single AWS resource.
type Resource struct {
	Service      string
	ResourceID   string
	TagName      string
	TagOwner     string
	TagEnv       string
	InstanceType string
	Region       string
	Cost         float64
	LineItems    []LineItemSummary
}

// SuspiciousResource is a resource with one or more anomaly flags.
type SuspiciousResource struct {
	ResourceID string
	Service    string
	Cost       float64
	TagOwner   string
	Flags      []string // human-readable flag messages
}

// Analysis is the full analysis result for one day.
type Analysis struct {
	Date         time.Time
	TotalCost    float64
	ByService    []ServiceCost // sorted desc
	TopResources []Resource
	Suspicious   []SuspiciousResource
	ItemCount    int
}

// ServiceCost is a (service, cost) pair.
type ServiceCost struct {
	Service string
	Cost    float64
}

// ── Analyzer ─────────────────────────────────────────────────────────────────

// Analyzer performs cost analysis on CUR line items.
type Analyzer struct {
	topN int
}

// New returns a configured Analyzer.
func New(topN int) *Analyzer {
	if topN <= 0 {
		topN = 15
	}
	return &Analyzer{topN: topN}
}

// Analyze processes line items and returns a structured Analysis for date.
func (a *Analyzer) Analyze(items []awsclient.LineItem, date time.Time) *Analysis {
	totalCost := 0.0
	byService := make(map[string]float64)
	byResource := make(map[string]*Resource)

	for i := range items {
		item := &items[i]
		totalCost += item.Cost
		byService[item.Service] += item.Cost

		key := item.ResourceID
		if key == "" {
			key = "__" + item.Service + ":" + item.UsageType
		}

		r, ok := byResource[key]
		if !ok {
			r = &Resource{
				Service:      item.Service,
				ResourceID:   item.ResourceID,
				TagName:      item.TagName,
				TagOwner:     item.TagOwner,
				TagEnv:       item.TagEnv,
				InstanceType: item.InstanceType,
				Region:       item.Region,
			}
			byResource[key] = r
		}
		r.Cost += item.Cost
		r.LineItems = append(r.LineItems, LineItemSummary{
			Description: item.Description,
			UsageType:   item.UsageType,
			UsageAmount: item.UsageAmount,
			UsageUnit:   item.UsageUnit,
			Cost:        item.Cost,
			Operation:   item.Operation,
		})
	}

	// Sort services
	svcList := make([]ServiceCost, 0, len(byService))
	for svc, cost := range byService {
		svcList = append(svcList, ServiceCost{Service: svc, Cost: cost})
	}
	sort.Slice(svcList, func(i, j int) bool { return svcList[i].Cost > svcList[j].Cost })

	// Top N resources
	resList := make([]Resource, 0, len(byResource))
	for _, r := range byResource {
		// Sort line items within each resource by cost desc
		sort.Slice(r.LineItems, func(i, j int) bool { return r.LineItems[i].Cost > r.LineItems[j].Cost })
		resList = append(resList, *r)
	}
	sort.Slice(resList, func(i, j int) bool { return resList[i].Cost > resList[j].Cost })
	if len(resList) > a.topN {
		resList = resList[:a.topN]
	}

	suspicious := detectSuspicious(items)

	return &Analysis{
		Date:         date,
		TotalCost:    totalCost,
		ByService:    svcList,
		TopResources: resList,
		Suspicious:   suspicious,
		ItemCount:    len(items),
	}
}

// ── Suspicious resource detection ────────────────────────────────────────────

func detectSuspicious(items []awsclient.LineItem) []SuspiciousResource {
	byResource := make(map[string]*SuspiciousResource)
	resourceFlags := make(map[string]map[string]bool) // rid → set of flag texts (dedup)

	addFlag := func(item *awsclient.LineItem, flag string) {
		rid := item.ResourceID
		if rid == "" {
			rid = "__" + item.Service + ":" + item.UsageType
		}
		if _, ok := byResource[rid]; !ok {
			byResource[rid] = &SuspiciousResource{
				ResourceID: rid,
				Service:    item.Service,
				TagOwner:   item.TagOwner,
			}
			resourceFlags[rid] = make(map[string]bool)
		}
		byResource[rid].Cost += item.Cost
		if !resourceFlags[rid][flag] {
			resourceFlags[rid][flag] = true
			byResource[rid].Flags = append(byResource[rid].Flags, flag)
		}
	}

	for i := range items {
		item := &items[i]
		ut := strings.ToLower(item.UsageType)
		desc := strings.ToLower(item.Description)
		svc := strings.ToLower(item.Service)
		op := strings.ToLower(item.Operation)

		// ── EC2 / Compute ─────────────────────────────────────────────────────

		// Idle / unassociated Elastic IP
		if strings.Contains(ut, "elasticip") && (strings.Contains(ut, "idle") || strings.Contains(desc, "unassociated")) {
			addFlag(item, SeverityCritical+" — Elastic IP ที่ไม่ได้ใช้งาน ควรลบออก")
		}

		// NAT Gateway — expensive, verify necessity
		if strings.Contains(ut, "natgateway") || strings.Contains(op, "natgateway") {
			addFlag(item, SeverityWarning+" — NAT Gateway มีค่า ~$32/เดือน ตรวจสอบว่ายังจำเป็นไหม")
		}

		// Unattached EBS volume
		if strings.Contains(svc, "ec2") && strings.Contains(ut, "ebs") &&
			(strings.Contains(desc, "unattached") || strings.Contains(desc, "available")) {
			addFlag(item, SeverityCritical+" — EBS Volume ที่ไม่ได้ attach ควรลบหรือ snapshot แล้วลบ")
		}

		// Public IPv4 address — AWS charges since Feb 2024
		if strings.Contains(desc, "in-use public ipv4") || strings.Contains(desc, "public ipv4 address") {
			addFlag(item, SeverityNotice+" — Public IPv4 address มีค่า $3.6/เดือน/IP พิจารณาใช้ IPv6 หรือลดจำนวน")
		}

		// EC2 running 24hr in sandbox
		if strings.Contains(ut, "boxusage") && item.UsageAmount >= 23 {
			addFlag(item, SeverityWarning+" — EC2 รัน 24hr ใน sandbox พิจารณา stop ตอนเลิกงาน")
			addFlag(item, SeverityNotice+" — EC2 On-Demand รันต่อเนื่อง พิจารณาซื้อ Savings Plans ประหยัดได้ ~40-60%")
		}

		// EC2 without Name tag
		if strings.Contains(ut, "boxusage") && item.TagName == "" {
			addFlag(item, SeverityNotice+" — EC2 ไม่มี tag Name ไม่รู้ว่าใครสร้าง")
		}

		// EC2 without Owner tag
		if strings.Contains(ut, "boxusage") && item.TagOwner == "" {
			addFlag(item, SeverityNotice+" — EC2 ไม่มี tag Owner ไม่มีคนรับผิดชอบ resource นี้")
		}

		// Old-generation EC2 instance type (t2.* → should be t3.*)
		if strings.Contains(ut, "boxusage:t2.") {
			addFlag(item, SeverityNotice+" — EC2 ใช้ instance generation เก่า (t2) ควรเปลี่ยนเป็น t3 ราคาถูกกว่าและเร็วกว่า")
		}

		// ── RDS / Aurora ──────────────────────────────────────────────────────

		// RDS running 24hr in sandbox
		if (strings.Contains(svc, "rds") || strings.Contains(svc, "aurora")) && item.UsageAmount >= 23 {
			addFlag(item, SeverityWarning+" — RDS/Aurora รัน 24hr ใน sandbox ควร stop ตอนเลิกงาน ประหยัดได้ ~65%")
		}

		// RDS old-generation instance type (db.t2, db.m3, db.r3)
		if strings.Contains(ut, "db.t2.") || strings.Contains(ut, "db.m3.") || strings.Contains(ut, "db.r3.") {
			addFlag(item, SeverityWarning+" — RDS ใช้ instance generation เก่า ควรอัปเกรดเป็น t3/m5/r5 ราคาถูกกว่าและ performance ดีกว่า")
		}

		// RDS Snapshot accumulation
		if (strings.Contains(svc, "rds") || strings.Contains(svc, "aurora")) && strings.Contains(ut, "snapshot") {
			addFlag(item, SeverityCritical+" — RDS Snapshot สะสม ควรตั้ง retention policy และลบ snapshot ที่ไม่จำเป็น")
		}

		// ── EKS ──────────────────────────────────────────────────────────────

		// EKS extended support (version past standard support)
		if strings.Contains(svc, "eks") && strings.Contains(desc, "extended support") {
			addFlag(item, SeverityCritical+" — EKS Extended Support มีค่า $0.60/cluster/hr ควรอัปเกรด K8s version โดยเร็ว")
		}

		// EKS cluster running 24hr — just awareness
		if strings.Contains(svc, "eks") && strings.Contains(ut, "aks") && item.UsageAmount >= 23 {
			addFlag(item, SeverityNotice+" — EKS Cluster รัน 24hr มีค่า $2.40/วัน ตรวจสอบว่าใช้งานจริงไหม")
		}

		// ── ElastiCache ───────────────────────────────────────────────────────

		// ElastiCache running 24hr in sandbox
		if strings.Contains(svc, "elasticache") && item.UsageAmount >= 23 {
			addFlag(item, SeverityWarning+" — ElastiCache รัน 24hr ใน sandbox พิจารณา stop ตอนเลิกงาน")
		}

		// ── Redshift ──────────────────────────────────────────────────────────

		// Redshift cluster running 24hr
		if strings.Contains(svc, "redshift") && item.UsageAmount >= 23 {
			addFlag(item, SeverityCritical+" — Redshift รัน 24hr ควร pause cluster ตอนไม่ใช้งาน ประหยัดได้ ~75%")
		}

		// ── OpenSearch / Elasticsearch ────────────────────────────────────────

		// OpenSearch / Elasticsearch running 24hr
		if (strings.Contains(svc, "opensearch") || strings.Contains(svc, "elasticsearch") ||
			strings.Contains(svc, "es")) && item.UsageAmount >= 23 {
			addFlag(item, SeverityWarning+" — OpenSearch/Elasticsearch รัน 24hr ราคาสูง ควร pause หรือลด instance size ใน sandbox")
		}

		// ── Lambda ────────────────────────────────────────────────────────────

		// Lambda Provisioned Concurrency — charged even without invocations
		if strings.Contains(svc, "lambda") && strings.Contains(ut, "provisionedconcurrency") {
			addFlag(item, SeverityWarning+" — Lambda Provisioned Concurrency มีค่าแม้ไม่มี invocation ตรวจสอบว่ายังจำเป็นไหม")
		}

		// ── DynamoDB ──────────────────────────────────────────────────────────

		// DynamoDB On-Demand with high cost — suggest Provisioned + Auto Scaling
		if strings.Contains(svc, "dynamodb") && strings.Contains(ut, "requestunits") && item.Cost > 10 {
			addFlag(item, SeverityNotice+" — DynamoDB On-Demand มีค่าสูง พิจารณาเปลี่ยนเป็น Provisioned + Auto Scaling ประหยัดได้ ~50%")
		}

		// ── S3 ────────────────────────────────────────────────────────────────

		// S3 Glacier retrieval fee
		if strings.Contains(svc, "s3") && strings.Contains(ut, "glacier") && strings.Contains(desc, "retrieval") {
			addFlag(item, SeverityWarning+" — S3 Glacier Retrieval มีค่า retrieval fee สูง พิจารณา access pattern และ storage tier")
		}

		// ── ECR ───────────────────────────────────────────────────────────────

		// ECR image storage — old images accumulate silently
		if strings.Contains(svc, "ecr") && strings.Contains(ut, "storage") {
			addFlag(item, SeverityNotice+" — ECR Storage สะสม image เก่า (>90 วัน) ควรตั้ง lifecycle policy ให้ลบ image เก่าอัตโนมัติ")
		}

		// ── API Gateway ───────────────────────────────────────────────────────

		// API Gateway cache enabled — charged even without traffic
		if (strings.Contains(svc, "apigateway") || strings.Contains(svc, "api gateway")) &&
			(strings.Contains(ut, "cache") || strings.Contains(desc, "cache")) {
			addFlag(item, SeverityWarning+" — API Gateway Cache เปิดอยู่ มีค่า $0.02-0.038/hr แม้ไม่มี traffic ตรวจสอบว่ายังจำเป็นไหม")
		}

		// ── CloudFront ────────────────────────────────────────────────────────

		// CloudFront distribution — minimum charge per distribution
		if strings.Contains(svc, "cloudfront") && (strings.Contains(ut, "invalidation") ||
			strings.Contains(ut, "requests") || strings.Contains(ut, "bytestransferred")) {
			if item.Cost > 0 && item.UsageAmount == 0 {
				addFlag(item, SeverityNotice+" — CloudFront Distribution ไม่มี request แต่มีค่า minimum ต่อ distribution ตรวจสอบว่ายังใช้งานอยู่จริง")
			}
		}
		// Catch CloudFront with very low usage (likely idle)
		if strings.Contains(svc, "cloudfront") && item.Cost > 0 && item.UsageAmount < 1 {
			addFlag(item, SeverityNotice+" — CloudFront Distribution มี traffic น้อยมาก อาจเป็น distribution ที่ไม่ได้ใช้งานแล้ว")
		}

		// ── SageMaker ─────────────────────────────────────────────────────────

		// SageMaker endpoint running 24hr
		if strings.Contains(svc, "sagemaker") &&
			(strings.Contains(ut, "endpoint") || strings.Contains(op, "endpoint")) &&
			item.UsageAmount >= 23 {
			addFlag(item, SeverityCritical+" — SageMaker Endpoint รัน 24hr ราคาแพงมาก ควร delete endpoint เมื่อไม่ใช้งาน")
		}

		// SageMaker notebook instance running 24hr
		if strings.Contains(svc, "sagemaker") && strings.Contains(ut, "notebookinstance") && item.UsageAmount >= 23 {
			addFlag(item, SeverityWarning+" — SageMaker Notebook Instance รัน 24hr ควร stop เมื่อเลิกใช้งาน")
		}

		// ── Secrets Manager ───────────────────────────────────────────────────

		// Secrets Manager — charged per secret per month
		if strings.Contains(svc, "secretsmanager") && strings.Contains(ut, "secret") {
			addFlag(item, SeverityNotice+" — Secrets Manager มีค่า $0.40/secret/เดือน ตรวจสอบว่า secret ทั้งหมดยังใช้งานอยู่จริง")
		}

		// ── CloudWatch ────────────────────────────────────────────────────────

		// CloudWatch Logs storage — likely missing retention policy
		if strings.Contains(svc, "cloudwatch") && strings.Contains(ut, "logs") && strings.Contains(desc, "storage") {
			addFlag(item, SeverityWarning+" — CloudWatch Logs Storage อาจไม่มี retention policy ควรตั้ง expire ภายใน 30-90 วัน")
		}

		// ── WAF ───────────────────────────────────────────────────────────────

		// WAF WebACL — charged per ACL
		if strings.Contains(svc, "waf") || strings.Contains(svc, "wafv2") {
			addFlag(item, SeverityNotice+" — WAF WebACL มีค่า $5/เดือน/ACL ตรวจสอบว่า attach กับ resource จริงและ rule ยังใช้งานอยู่")
		}

		// ── Data Transfer ─────────────────────────────────────────────────────

		// Inter-AZ data transfer — often overlooked
		if strings.Contains(ut, "regionaldata") || strings.Contains(desc, "inter az") ||
			strings.Contains(desc, "inter-az") {
			addFlag(item, SeverityWarning+" — Inter-AZ Data Transfer มีค่า $0.01/GB พิจารณา architecture ให้ traffic อยู่ใน AZ เดียวกัน")
		}

		// ── CodeCommit ───────────────────────────────────────────────────────

		// CodeCommit per-user fee — AWS announced EOS July 2024
		if strings.Contains(svc, "codecommit") {
			addFlag(item, SeverityWarning+" — AWS CodeCommit ประกาศ End-of-Sale ควรย้ายไป GitHub/GitLab")
		}
	}

	result := make([]SuspiciousResource, 0, len(byResource))
	for _, s := range byResource {
		result = append(result, *s)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Cost > result[j].Cost })
	return result
}
