// Package aws wraps AWS SDK interactions for reading CUR files from S3.
// It also provides ReadLocalCURFile for reading CUR files from the local filesystem
// (intended for local testing without AWS credentials).
package aws

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
)

// LineItem represents one CUR CSV row normalised for our use.
type LineItem struct {
	LineItemType     string
	UsageStartDate   time.Time // parsed from lineItem/UsageStartDate (v1) or line_item_usage_start_date (v2)
	Service          string
	ResourceID       string
	UsageType        string
	Description      string
	UsageAmount      float64
	UsageUnit        string
	Cost             float64
	Region           string
	InstanceType     string
	Operation        string
	TagName          string
	TagOwner         string
	TagEnv           string
	AvailabilityZone string
}

// Client is a thin wrapper around the AWS S3 service client.
type Client struct {
	s3     *s3.Client
	bucket string
	prefix string
}

// NewClient builds an S3 Client from the application config.
func NewClient(ctx context.Context, cfg *config.Config) (*Client, error) {
	var opts []func(*awscfg.LoadOptions) error

	opts = append(opts, awscfg.WithRegion(cfg.AWS.Region))

	if !cfg.AWS.UseInstanceProfile && cfg.AWS.AccessKeyID != "" {
		opts = append(opts, awscfg.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(
				cfg.AWS.AccessKeyID,
				cfg.AWS.SecretAccessKey,
				"",
			),
		))
	}

	awsCfg, err := awscfg.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	return &Client{
		s3:     s3.NewFromConfig(awsCfg),
		bucket: cfg.CUR.S3Bucket,
		prefix: cfg.CUR.S3Prefix,
	}, nil
}

// FindCURFile looks for the latest .csv.gz file in S3 for the given date.
// Tries CUR v2 path first (Data Exports): {prefix}/data/BILLING_PERIOD=YYYY-MM/
// Falls back to CUR v1 path: {prefix}/YYYYMMDD-YYYYMMDD/
func (c *Client) FindCURFile(ctx context.Context, date time.Time) (string, error) {
	year, month, _ := date.Date()

	// CUR v2 (Data Exports) — BILLING_PERIOD partition
	folderV2 := fmt.Sprintf("%s/data/BILLING_PERIOD=%d-%02d/", c.prefix, year, int(month))
	if key, err := c.findCSVInFolder(ctx, folderV2); err == nil {
		return key, nil
	}

	// CUR v1 — legacy date-range folder
	monthStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	nextMonth := monthStart.AddDate(0, 1, 0)
	folderV1 := fmt.Sprintf("%s/%s-%s/",
		c.prefix,
		monthStart.Format("20060102"),
		nextMonth.Format("20060102"),
	)
	if key, err := c.findCSVInFolder(ctx, folderV1); err == nil {
		return key, nil
	}

	return "", fmt.Errorf("no CUR file found in s3://%s under %s (tried v2 and v1 paths)", c.bucket, c.prefix)
}

// findCSVInFolder lists objects under the given S3 prefix and returns the first .csv.gz or .csv key found.
func (c *Client) findCSVInFolder(ctx context.Context, folder string) (string, error) {
	out, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(folder),
	})
	if err != nil {
		return "", fmt.Errorf("list objects [%s]: %w", folder, err)
	}
	for _, obj := range out.Contents {
		key := aws.ToString(obj.Key)
		if strings.HasSuffix(key, ".csv.gz") || strings.HasSuffix(key, ".csv") {
			return key, nil
		}
	}
	return "", fmt.Errorf("no CSV file in %s", folder)
}

// ReadCURFile downloads and parses the CUR CSV (optionally gzip-compressed) from S3.
// Only rows with lineItemType in {Usage, Fee, Tax} and cost > 0.0001 are returned.
func (c *Client) ReadCURFile(ctx context.Context, s3Key string) ([]LineItem, error) {
	obj, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		return nil, fmt.Errorf("get object [%s]: %w", s3Key, err)
	}
	defer obj.Body.Close() //nolint:errcheck

	var reader io.Reader = obj.Body
	if strings.HasSuffix(s3Key, ".gz") {
		gz, err := gzip.NewReader(obj.Body)
		if err != nil {
			return nil, fmt.Errorf("open gzip: %w", err)
		}
		defer gz.Close() //nolint:errcheck
		reader = gz
	}

	return parseCURReader(reader)
}

// ReadLocalCURFile reads and parses a CUR CSV file from the local filesystem.
// Supports both plain .csv and gzip-compressed .csv.gz files.
// Intended for local testing without AWS credentials.
func ReadLocalCURFile(filePath string) ([]LineItem, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open local CUR file [%s]: %w", filePath, err)
	}
	defer f.Close() //nolint:errcheck

	var reader io.Reader = f
	if strings.HasSuffix(filePath, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("open gzip [%s]: %w", filePath, err)
		}
		defer gz.Close() //nolint:errcheck
		reader = gz
	}

	return parseCURReader(reader)
}

// curColumns holds the resolved column names for a given CUR format version.
// CUR v1 uses slash-separated names (lineItem/LineItemType).
// CUR v2 (Data Exports) uses underscore-separated names (line_item_line_item_type).
type curColumns struct {
	LineItemType     string
	UsageStartDate   string
	UnblendedCost    string
	ProductName      string
	ProductCode      string // fallback when ProductName is empty
	ResourceID       string
	UsageType        string
	Description      string
	UsageAmount      string
	PricingUnit      string
	Region           string
	InstanceType     string
	Operation        string
	TagName          string
	TagOwner         string
	TagEnv           string
	AvailabilityZone string
}

// detectColumns inspects the header row and returns column name mappings
// that match the actual CUR format (v1 slash-style vs v2 underscore-style).
func detectColumns(idx map[string]int) curColumns {
	if _, ok := idx["lineItem/LineItemType"]; ok {
		// CUR v1 format
		return curColumns{
			LineItemType:     "lineItem/LineItemType",
			UsageStartDate:   "lineItem/UsageStartDate",
			UnblendedCost:    "lineItem/UnblendedCost",
			ProductName:      "product/ProductName",
			ProductCode:      "lineItem/ProductCode",
			ResourceID:       "lineItem/ResourceId",
			UsageType:        "lineItem/UsageType",
			Description:      "lineItem/LineItemDescription",
			UsageAmount:      "lineItem/UsageAmount",
			PricingUnit:      "pricing/unit",
			Region:           "product/region",
			InstanceType:     "product/instanceType",
			Operation:        "lineItem/Operation",
			TagName:          "resourceTags/user:Name",
			TagOwner:         "resourceTags/user:Owner",
			TagEnv:           "resourceTags/user:Environment",
			AvailabilityZone: "lineItem/AvailabilityZone",
		}
	}
	// CUR v2 format (AWS Data Exports)
	return curColumns{
		LineItemType:     "line_item_line_item_type",
		UsageStartDate:   "line_item_usage_start_date",
		UnblendedCost:    "line_item_unblended_cost",
		ProductName:      "product_product_name",
		ProductCode:      "line_item_product_code",
		ResourceID:       "line_item_resource_id",
		UsageType:        "line_item_usage_type",
		Description:      "line_item_line_item_description",
		UsageAmount:      "line_item_usage_amount",
		PricingUnit:      "pricing_unit",
		Region:           "product_region",
		InstanceType:     "product_instance_type",
		Operation:        "line_item_operation",
		TagName:          "resource_tags_user_name",
		TagOwner:         "resource_tags_user_owner",
		TagEnv:           "resource_tags_user_environment",
		AvailabilityZone: "line_item_availability_zone",
	}
}

// parseCURReader is the shared CSV parsing logic for both S3 and local sources.
func parseCURReader(reader io.Reader) ([]LineItem, error) {
	csvReader := csv.NewReader(reader)
	csvReader.LazyQuotes = true
	csvReader.TrimLeadingSpace = true

	headers, err := csvReader.Read()
	if err != nil {
		return nil, fmt.Errorf("read csv header: %w", err)
	}
	idx := buildIndex(headers)
	cols := detectColumns(idx)

	var items []LineItem
	for {
		row, err := csvReader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read csv row: %w", err)
		}

		lineItemType := col(row, idx, cols.LineItemType)
		if lineItemType != "Usage" && lineItemType != "Fee" && lineItemType != "Tax" {
			continue
		}

		cost := parseFloat(col(row, idx, cols.UnblendedCost))
		if cost <= 0.0001 {
			continue
		}

		service := col(row, idx, cols.ProductName)
		if service == "" {
			service = col(row, idx, cols.ProductCode)
		}

		items = append(items, LineItem{
			LineItemType:     lineItemType,
			UsageStartDate:   parseDate(col(row, idx, cols.UsageStartDate)),
			Service:          service,
			ResourceID:       col(row, idx, cols.ResourceID),
			UsageType:        col(row, idx, cols.UsageType),
			Description:      col(row, idx, cols.Description),
			UsageAmount:      parseFloat(col(row, idx, cols.UsageAmount)),
			UsageUnit:        col(row, idx, cols.PricingUnit),
			Cost:             cost,
			Region:           col(row, idx, cols.Region),
			InstanceType:     col(row, idx, cols.InstanceType),
			Operation:        col(row, idx, cols.Operation),
			TagName:          col(row, idx, cols.TagName),
			TagOwner:         col(row, idx, cols.TagOwner),
			TagEnv:           col(row, idx, cols.TagEnv),
			AvailabilityZone: col(row, idx, cols.AvailabilityZone),
		})
	}

	return items, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func buildIndex(headers []string) map[string]int {
	m := make(map[string]int, len(headers))
	for i, h := range headers {
		m[h] = i
	}
	return m
}

func col(row []string, idx map[string]int, name string) string {
	i, ok := idx[name]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func parseFloat(s string) float64 {
	if s == "" {
		return 0
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// parseDate parses a CUR date string into time.Time.
// CUR uses RFC3339 format: "2026-04-08T00:00:00Z"
// Returns zero time when the string is empty or unparseable.
func parseDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try RFC3339 first (most common in CUR)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC()
	}
	// Fallback: date-only "2006-01-02"
	if t, err := time.Parse("2006-01-02", s[:min(len(s), 10)]); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// DetectReportDate returns the usage date found in the CUR line items.
// It reads UsageStartDate from the first item that has a non-zero value,
// then truncates to day boundary (UTC) — suitable for comparing with targetDate.
// Returns (time.Time{}, false) when no date can be determined.
func DetectReportDate(items []LineItem) (time.Time, bool) {
	for _, item := range items {
		if !item.UsageStartDate.IsZero() {
			return item.UsageStartDate.Truncate(24 * time.Hour), true
		}
	}
	return time.Time{}, false
}
