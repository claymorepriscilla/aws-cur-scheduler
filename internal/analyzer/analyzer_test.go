package analyzer_test

import (
	"testing"
	"time"

	awsclient "github.com/claymorepriscilla/aws-cur-scheduler/internal/aws"
	"github.com/claymorepriscilla/aws-cur-scheduler/internal/analyzer"
)

var testDate = time.Date(2025, 4, 8, 0, 0, 0, 0, time.UTC)

func makeItems() []awsclient.LineItem {
	return []awsclient.LineItem{
		{
			LineItemType: "Usage",
			Service:      "Amazon EC2",
			ResourceID:   "i-0abc123def456",
			UsageType:    "BoxUsage:t3.medium",
			Description:  "Linux/UNIX t3.medium",
			UsageAmount:  24,
			UsageUnit:    "Hrs",
			Cost:         1.1136,
			InstanceType: "t3.medium",
			Region:       "ap-southeast-1",
			TagName:      "dev-api-server",
			TagOwner:     "john.doe",
		},
		{
			LineItemType: "Usage",
			Service:      "Amazon RDS",
			ResourceID:   "arn:aws:rds:ap-southeast-1:123456789:db:sandbox-postgres",
			UsageType:    "RDS:db.t3.small",
			Description:  "db.t3.small Multi-AZ",
			UsageAmount:  24,
			UsageUnit:    "Hrs",
			Cost:         2.89,
			InstanceType: "db.t3.small",
			Region:       "ap-southeast-1",
			TagName:      "sandbox-postgres",
		},
		{
			LineItemType: "Usage",
			Service:      "Amazon EC2",
			ResourceID:   "i-0nametag000",
			UsageType:    "BoxUsage:t3.large",
			Description:  "Linux/UNIX t3.large",
			UsageAmount:  24,
			UsageUnit:    "Hrs",
			Cost:         0.96,
			InstanceType: "t3.large",
			Region:       "ap-southeast-1",
			TagName:      "", // no tag — suspicious
		},
		{
			LineItemType: "Usage",
			Service:      "Amazon EC2",
			ResourceID:   "nat-077f03f67a3b4c1d2",
			UsageType:    "NatGateway-Hours",
			Description:  "NAT Gateway hours",
			UsageAmount:  24,
			UsageUnit:    "Hrs",
			Cost:         0.96,
		},
	}
}

// ── Analyze basic tests ───────────────────────────────────────────────────────

func TestAnalyze_TotalCost(t *testing.T) {
	a := analyzer.New(15)
	items := makeItems()
	result := a.Analyze(items, testDate)

	want := 1.1136 + 2.89 + 0.96 + 0.96
	if diff := result.TotalCost - want; diff > 0.0001 || diff < -0.0001 {
		t.Errorf("TotalCost = %.4f, want %.4f", result.TotalCost, want)
	}
}

func TestAnalyze_ByServiceSortedDesc(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze(makeItems(), testDate)

	for i := 1; i < len(result.ByService); i++ {
		if result.ByService[i].Cost > result.ByService[i-1].Cost {
			t.Errorf("ByService not sorted desc at index %d", i)
		}
	}
}

func TestAnalyze_ItemCount(t *testing.T) {
	a := analyzer.New(15)
	items := makeItems()
	result := a.Analyze(items, testDate)
	if result.ItemCount != len(items) {
		t.Errorf("ItemCount = %d, want %d", result.ItemCount, len(items))
	}
}

func TestAnalyze_TopNRespected(t *testing.T) {
	a := analyzer.New(2) // only top 2
	result := a.Analyze(makeItems(), testDate)
	if len(result.TopResources) > 2 {
		t.Errorf("TopResources len = %d, want <= 2", len(result.TopResources))
	}
}

func TestNew_ZeroTopN_DefaultsTo15(t *testing.T) {
	a := analyzer.New(0) // topN <= 0 → default 15
	items := makeItems()
	result := a.Analyze(items, testDate)
	if result.ItemCount != len(items) {
		t.Errorf("expected analyzer to work with topN=0 default, ItemCount=%d", result.ItemCount)
	}
}

func TestAnalyze_EmptyResourceID_GroupsByServiceUsage(t *testing.T) {
	// Items with empty ResourceID should be grouped by "__Service:UsageType" key.
	items := []awsclient.LineItem{
		{Service: "Amazon S3", ResourceID: "", UsageType: "S3-Standard-Storage", Cost: 0.5, UsageAmount: 10},
		{Service: "Amazon S3", ResourceID: "", UsageType: "S3-Standard-Storage", Cost: 0.3, UsageAmount: 5},
	}
	a := analyzer.New(15)
	result := a.Analyze(items, testDate)

	if result.TotalCost < 0.79 || result.TotalCost > 0.81 {
		t.Errorf("TotalCost = %.2f, want ~0.80", result.TotalCost)
	}
	if len(result.TopResources) != 1 {
		t.Errorf("expected 1 grouped resource (same key), got %d", len(result.TopResources))
	}
}

func TestAnalyze_EmptyItems(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze([]awsclient.LineItem{}, testDate)
	if result.TotalCost != 0 {
		t.Errorf("empty items should give 0 total cost, got %.4f", result.TotalCost)
	}
	if len(result.Suspicious) != 0 {
		t.Errorf("empty items should give 0 suspicious, got %d", len(result.Suspicious))
	}
}

// ── Suspicious detection — existing tests ─────────────────────────────────────

func TestDetectSuspicious_RDSRunning24h(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze(makeItems(), testDate)

	for _, s := range result.Suspicious {
		if s.Service == "Amazon RDS" {
			return // found — test passes
		}
	}
	t.Error("expected RDS to be flagged as suspicious (24hr run)")
}

func TestDetectSuspicious_EC2NoTag(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze(makeItems(), testDate)

	found := false
	for _, s := range result.Suspicious {
		if s.ResourceID == "i-0nametag000" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected EC2 with no tag to be flagged as suspicious")
	}
}

func TestDetectSuspicious_NATGateway(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze(makeItems(), testDate)

	found := false
	for _, s := range result.Suspicious {
		if s.ResourceID == "nat-077f03f67a3b4c1d2" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected NAT Gateway to be flagged as suspicious")
	}
}

// ── Suspicious detection — comprehensive coverage ─────────────────────────────

// makeSuspiciousItems returns items designed to trigger every detection branch.
func makeSuspiciousItems() []awsclient.LineItem {
	return []awsclient.LineItem{
		// Elastic IP idle
		{Service: "Amazon EC2", ResourceID: "eip-idle-001",
			UsageType: "ElasticIP:IdleAddress", Description: "Elastic IP idle", Cost: 0.005, UsageAmount: 720},

		// EBS volume unattached
		{Service: "Amazon EC2", ResourceID: "vol-unattached-001",
			UsageType: "EBS:VolumeUsage.gp3", Description: "EBS volume available unattached", Cost: 0.8, UsageAmount: 730},

		// Public IPv4 address
		{Service: "Amazon EC2", ResourceID: "ip-public-001",
			UsageType: "PublicIPv4Usage", Description: "In-use public IPv4 address", Cost: 0.005, UsageAmount: 720},

		// Old-gen EC2 t2 — also has no Owner to trigger that notice too
		{Service: "Amazon EC2", ResourceID: "i-t2micro-001",
			UsageType: "BoxUsage:t2.micro", Description: "Linux/UNIX t2.micro",
			Cost: 0.46, UsageAmount: 24, TagName: "old-srv", TagOwner: ""},

		// NAT Gateway via operation field (separate from UsageType path)
		{Service: "Amazon VPC", ResourceID: "nat-via-op-001",
			UsageType: "VPC-NatGateway-Bytes", Operation: "NatGateway",
			Description: "NAT gateway data processed", Cost: 0.1, UsageAmount: 100},

		// RDS old-gen db.t2
		{Service: "Amazon RDS", ResourceID: "db-t2-001",
			UsageType: "RDS:db.t2.micro", Description: "db.t2.micro Single-AZ", Cost: 0.3, UsageAmount: 1},

		// RDS Snapshot accumulation
		{Service: "Amazon RDS", ResourceID: "db-snap-001",
			UsageType: "RDS:SnapshotUsage", Description: "RDS snapshot storage", Cost: 0.1, UsageAmount: 1},

		// EKS Extended Support
		{Service: "Amazon EKS", ResourceID: "eks-ext-001",
			UsageType: "EKS:Cluster", Description: "Amazon EKS Extended Support cluster hours",
			Cost: 0.6, UsageAmount: 1},

		// EKS cluster running 24hr (UsageType must contain "aks")
		{Service: "Amazon EKS", ResourceID: "eks-24hr-001",
			UsageType: "EKS:AKS-ClusterHours", Description: "EKS cluster hours",
			Cost: 2.4, UsageAmount: 24},

		// ElastiCache running 24hr
		{Service: "Amazon ElastiCache", ResourceID: "cache-001",
			UsageType: "ElastiCache:NodeUsage:cache.r6g.large", Description: "ElastiCache node",
			Cost: 1.2, UsageAmount: 24},

		// Redshift cluster running 24hr
		{Service: "Amazon Redshift", ResourceID: "redshift-001",
			UsageType: "Redshift:Node:dc2.large", Description: "Redshift cluster node",
			Cost: 5.0, UsageAmount: 24},

		// OpenSearch running 24hr
		{Service: "Amazon OpenSearch Service", ResourceID: "os-001",
			UsageType: "OpenSearch:Instance:m5.large.search", Description: "OpenSearch node",
			Cost: 1.5, UsageAmount: 24},

		// Lambda Provisioned Concurrency
		{Service: "AWS Lambda", ResourceID: "arn:aws:lambda:ap-southeast-1:123:function:my-fn",
			UsageType: "Lambda:ProvisionedConcurrency-GB-Second", Description: "Lambda provisioned concurrency",
			Cost: 0.5, UsageAmount: 100},

		// DynamoDB On-Demand with high cost
		{Service: "Amazon DynamoDB", ResourceID: "arn:aws:dynamodb:ap-southeast-1:123:table/my-table",
			UsageType: "DynamoDB:RequestUnits-WriteUnits", Description: "DynamoDB write request units",
			Cost: 15.0, UsageAmount: 10000},

		// S3 Glacier retrieval fee
		{Service: "Amazon S3", ResourceID: "my-archive-bucket",
			UsageType: "S3-Glacier-Retrieval-Tier1", Description: "S3 Glacier standard retrieval",
			Cost: 0.3, UsageAmount: 1},

		// ECR image storage
		{Service: "Amazon ECR", ResourceID: "arn:aws:ecr:ap-southeast-1:123:repository/my-app",
			UsageType: "ECR-Storage-GB-Month", Description: "ECR image storage",
			Cost: 0.5, UsageAmount: 50},

		// API Gateway cache
		{Service: "Amazon API Gateway", ResourceID: "api-xyz-001",
			UsageType: "ApiGateway:CacheUsage-0.5GB", Description: "API Gateway cache",
			Cost: 0.7, UsageAmount: 720},

		// CloudFront idle distribution (usage=0, cost>0)
		{Service: "Amazon CloudFront", ResourceID: "E1XXXXXXXXXXXXXX",
			UsageType: "CloudFront:Requests", Description: "CloudFront HTTP requests",
			Cost: 0.01, UsageAmount: 0},

		// SageMaker endpoint running 24hr
		{Service: "Amazon SageMaker", ResourceID: "arn:aws:sagemaker:ap-southeast-1:123:endpoint/my-endpoint",
			UsageType: "SageMaker:Endpoint-Hours:ml.t3.medium", Description: "SageMaker real-time inference endpoint",
			Cost: 5.0, UsageAmount: 24},

		// SageMaker notebook running 24hr
		{Service: "Amazon SageMaker", ResourceID: "arn:aws:sagemaker:ap-southeast-1:123:notebook-instance/my-nb",
			UsageType: "SageMaker:NotebookInstance-Hours:ml.t3.medium", Description: "SageMaker notebook instance",
			Cost: 0.5, UsageAmount: 24},

		// Secrets Manager secrets
		{Service: "AWSSecretsManager", ResourceID: "arn:aws:secretsmanager:ap-southeast-1:123:secret:my-secret",
			UsageType: "SecretsManager:Secret", Description: "Secrets Manager secret per month",
			Cost: 0.4, UsageAmount: 1},

		// CloudWatch Logs storage (no retention policy)
		{Service: "Amazon CloudWatch", ResourceID: "arn:aws:logs:ap-southeast-1:123:log-group:/aws/lambda/my-fn",
			UsageType: "CloudWatch:Logs-GB", Description: "CloudWatch logs storage GB",
			Cost: 0.5, UsageAmount: 10},

		// WAF WebACL
		{Service: "AWS WAF", ResourceID: "arn:aws:wafv2:ap-southeast-1:123:regional/webacl/my-waf",
			UsageType: "WAF:WebACL-V2", Description: "WAFv2 WebACL monthly fee",
			Cost: 5.0, UsageAmount: 1},

		// Inter-AZ data transfer
		{Service: "Amazon EC2", ResourceID: "transfer-ec2-001",
			UsageType: "EC2-RegionalDataTransfer-In-Bytes", Description: "Data transfer inter az",
			Cost: 0.5, UsageAmount: 50000},

		// CodeCommit (End-of-Sale)
		{Service: "AWS CodeCommit", ResourceID: "arn:aws:codecommit:ap-southeast-1:123:my-repo",
			UsageType: "CodeCommit:Repositories-Month", Description: "CodeCommit repository",
			Cost: 0.1, UsageAmount: 1},
	}
}

func TestDetectSuspicious_Comprehensive(t *testing.T) {
	a := analyzer.New(15)
	items := makeSuspiciousItems()
	result := a.Analyze(items, testDate)

	if len(result.Suspicious) == 0 {
		t.Fatal("expected suspicious resources, got none")
	}

	// Build a set of all resource IDs that were flagged
	flagged := make(map[string]bool)
	for _, s := range result.Suspicious {
		flagged[s.ResourceID] = true
	}

	checks := []struct {
		rid  string
		desc string
	}{
		{"eip-idle-001", "Elastic IP idle"},
		{"vol-unattached-001", "EBS unattached"},
		{"ip-public-001", "Public IPv4"},
		{"i-t2micro-001", "Old-gen EC2 t2"},
		{"nat-via-op-001", "NAT via operation"},
		{"db-t2-001", "RDS old-gen t2"},
		{"db-snap-001", "RDS snapshot"},
		{"eks-ext-001", "EKS extended support"},
		{"eks-24hr-001", "EKS 24hr"},
		{"cache-001", "ElastiCache 24hr"},
		{"redshift-001", "Redshift 24hr"},
		{"os-001", "OpenSearch 24hr"},
	}
	for _, c := range checks {
		if !flagged[c.rid] {
			t.Errorf("expected %s (%s) to be flagged as suspicious", c.desc, c.rid)
		}
	}
}

func TestDetectSuspicious_ComprehensiveServices(t *testing.T) {
	// A second batch covering more detection rules.
	a := analyzer.New(15)
	items := makeSuspiciousItems()
	result := a.Analyze(items, testDate)

	flagged := make(map[string]bool)
	for _, s := range result.Suspicious {
		flagged[s.ResourceID] = true
	}

	checks := []struct {
		rid  string
		desc string
	}{
		{"arn:aws:lambda:ap-southeast-1:123:function:my-fn", "Lambda ProvisionedConcurrency"},
		{"arn:aws:dynamodb:ap-southeast-1:123:table/my-table", "DynamoDB high cost"},
		{"my-archive-bucket", "S3 Glacier retrieval"},
		{"arn:aws:ecr:ap-southeast-1:123:repository/my-app", "ECR storage"},
		{"api-xyz-001", "API Gateway cache"},
		{"E1XXXXXXXXXXXXXX", "CloudFront idle"},
		{"arn:aws:sagemaker:ap-southeast-1:123:endpoint/my-endpoint", "SageMaker endpoint"},
		{"arn:aws:sagemaker:ap-southeast-1:123:notebook-instance/my-nb", "SageMaker notebook"},
		{"arn:aws:secretsmanager:ap-southeast-1:123:secret:my-secret", "Secrets Manager"},
		{"arn:aws:logs:ap-southeast-1:123:log-group:/aws/lambda/my-fn", "CloudWatch Logs storage"},
		{"arn:aws:wafv2:ap-southeast-1:123:regional/webacl/my-waf", "WAF WebACL"},
		{"transfer-ec2-001", "Inter-AZ data transfer"},
		{"arn:aws:codecommit:ap-southeast-1:123:my-repo", "CodeCommit"},
	}
	for _, c := range checks {
		if !flagged[c.rid] {
			t.Errorf("expected %s (%s) to be flagged as suspicious", c.desc, c.rid)
		}
	}
}

func TestDetectSuspicious_FlagDeduplication(t *testing.T) {
	// The same flag should not appear twice for the same resource.
	items := []awsclient.LineItem{
		{Service: "Amazon EC2", ResourceID: "i-dup-001",
			UsageType: "BoxUsage:t3.large", Description: "Linux t3.large",
			Cost: 0.48, UsageAmount: 24, TagName: "", TagOwner: ""},
		// Same resource, same usage type — should not duplicate flags
		{Service: "Amazon EC2", ResourceID: "i-dup-001",
			UsageType: "BoxUsage:t3.large", Description: "Linux t3.large",
			Cost: 0.48, UsageAmount: 24, TagName: "", TagOwner: ""},
	}
	a := analyzer.New(15)
	result := a.Analyze(items, testDate)

	for _, s := range result.Suspicious {
		if s.ResourceID == "i-dup-001" {
			seen := make(map[string]int)
			for _, f := range s.Flags {
				seen[f]++
				if seen[f] > 1 {
					t.Errorf("flag %q appears %d times — should be deduplicated", f, seen[f])
				}
			}
		}
	}
}

func TestDetectSuspicious_SortedByCostDesc(t *testing.T) {
	a := analyzer.New(15)
	result := a.Analyze(makeItems(), testDate)

	for i := 1; i < len(result.Suspicious); i++ {
		if result.Suspicious[i].Cost > result.Suspicious[i-1].Cost {
			t.Errorf("Suspicious not sorted desc at index %d: %.4f > %.4f",
				i, result.Suspicious[i].Cost, result.Suspicious[i-1].Cost)
		}
	}
}
