package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/claymorepriscilla/aws-cur-scheduler/internal/config"
)

// writeConfig writes a temp yaml config file and sets APP_ENV accordingly.
func writeConfig(t *testing.T, name, content string) func() {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "configs"), 0755) //nolint:errcheck
	os.WriteFile(filepath.Join(dir, "configs", name+".yaml"), []byte(content), 0600) //nolint:errcheck
	orig, _ := os.Getwd()
	os.Chdir(dir) //nolint:errcheck
	t.Setenv("APP_ENV", name)
	return func() { os.Chdir(orig) } //nolint:errcheck
}

const validYAML = `
app:
  env: test
  version: "1.0.0"
aws:
  region: ap-southeast-1
  use_instance_profile: false
cur:
  s3_bucket: "my-bucket"
  s3_prefix: "cur/prefix"
teams:
  webhook_url: "https://example.com/webhook"
  timeout_sec: 15
  enable_webhook: false
budget:
  limit_usd: 500.0
  alert_threshold_pct: 70.0
report:
  top_n_resources: 15
log:
  level: info
  format: json
database:
  enabled: false
  host: ""
  port: 5432
  database: ""
  user: ""
  password: ""
  sslmode: "require"
  timeout_sec: 10
`

// ── Happy path ────────────────────────────────────────────────────────────────

func TestLoad_ValidConfig(t *testing.T) {
	cleanup := writeConfig(t, "test", validYAML)
	defer cleanup()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.CUR.S3Bucket != "my-bucket" {
		t.Errorf("S3Bucket = %q, want %q", cfg.CUR.S3Bucket, "my-bucket")
	}
	if cfg.Budget.LimitUSD != 500.0 {
		t.Errorf("LimitUSD = %.1f, want 500.0", cfg.Budget.LimitUSD)
	}
	if cfg.Teams.EnableWebhook {
		t.Error("EnableWebhook should be false")
	}
}

func TestLoad_EnvVarOverride(t *testing.T) {
	cleanup := writeConfig(t, "test", validYAML)
	defer cleanup()

	t.Setenv("TEAMS_WEBHOOK_URL", "https://overridden.example.com/hook")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Teams.WebhookURL != "https://overridden.example.com/hook" {
		t.Errorf("WebhookURL not overridden, got %q", cfg.Teams.WebhookURL)
	}
}

// ── Validation failures ────────────────────────────────────────────────────────

func TestLoad_MissingBucket_Fails(t *testing.T) {
	yaml := `
app:
  env: test
aws:
  region: ap-southeast-1
cur:
  s3_bucket: ""
  s3_prefix: "prefix"
teams:
  webhook_url: "https://example.com"
  enable_webhook: false
budget:
  limit_usd: 100.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	_, err := config.Load()
	if err == nil {
		t.Error("expected validation error for empty s3_bucket")
	}
}

func TestLoad_MissingPrefix_Fails(t *testing.T) {
	yaml := `
app:
  env: test
aws:
  region: ap-southeast-1
cur:
  s3_bucket: "my-bucket"
  s3_prefix: ""
teams:
  enable_webhook: false
budget:
  limit_usd: 100.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	_, err := config.Load()
	if err == nil {
		t.Error("expected validation error for empty s3_prefix")
	}
}

func TestLoad_MissingRegion_Fails(t *testing.T) {
	yaml := `
app:
  env: test
aws:
  region: ""
cur:
  s3_bucket: "my-bucket"
  s3_prefix: "cur/prefix"
teams:
  enable_webhook: false
budget:
  limit_usd: 100.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	_, err := config.Load()
	if err == nil {
		t.Error("expected validation error for empty aws.region")
	}
}

func TestLoad_WebhookRequired_Fails(t *testing.T) {
	yaml := `
app:
  env: test
aws:
  region: ap-southeast-1
cur:
  s3_bucket: "my-bucket"
  s3_prefix: "cur/prefix"
teams:
  webhook_url: ""
  enable_webhook: true
budget:
  limit_usd: 100.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	_, err := config.Load()
	if err == nil {
		t.Error("expected validation error: webhook_url required when enable_webhook=true")
	}
}

func TestLoad_ZeroBudget_Fails(t *testing.T) {
	yaml := `
app:
  env: test
aws:
  region: ap-southeast-1
cur:
  s3_bucket: "my-bucket"
  s3_prefix: "cur/prefix"
teams:
  enable_webhook: false
budget:
  limit_usd: 0.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	_, err := config.Load()
	if err == nil {
		t.Error("expected validation error for budget.limit_usd = 0")
	}
}

func TestLoad_LocalPath_BypassesS3Validation(t *testing.T) {
	yaml := `
app:
  env: test
  version: "1.0.0"
aws:
  region: ""
cur:
  local_path: "/tmp/test.csv"
  s3_bucket: ""
  s3_prefix: ""
teams:
  enable_webhook: false
budget:
  limit_usd: 500.0
log:
  level: info
  format: json
database:
  enabled: false
`
	cleanup := writeConfig(t, "test", yaml)
	defer cleanup()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() should succeed with local_path set, got: %v", err)
	}
	if cfg.CUR.LocalPath != "/tmp/test.csv" {
		t.Errorf("LocalPath = %q, want /tmp/test.csv", cfg.CUR.LocalPath)
	}
}

// ── Database validation ────────────────────────────────────────────────────────

func dbEnabledYAML(host, user, password, dbname string) string {
	return `
app:
  env: test
aws:
  region: ap-southeast-1
cur:
  s3_bucket: "my-bucket"
  s3_prefix: "cur/prefix"
teams:
  enable_webhook: false
budget:
  limit_usd: 500.0
log:
  level: info
  format: json
database:
  enabled: true
  host: "` + host + `"
  port: 5432
  database: "` + dbname + `"
  user: "` + user + `"
  password: "` + password + `"
  sslmode: "require"
`
}

func TestLoad_Database_MissingHost_Fails(t *testing.T) {
	cleanup := writeConfig(t, "test", dbEnabledYAML("", "postgres", "pass", "mydb"))
	defer cleanup()
	_, err := config.Load()
	if err == nil {
		t.Error("expected error: database.host required when enabled=true")
	}
}

func TestLoad_Database_MissingUser_Fails(t *testing.T) {
	cleanup := writeConfig(t, "test", dbEnabledYAML("db.host.com", "", "pass", "mydb"))
	defer cleanup()
	_, err := config.Load()
	if err == nil {
		t.Error("expected error: database.user required when enabled=true")
	}
}

func TestLoad_Database_MissingPassword_Fails(t *testing.T) {
	cleanup := writeConfig(t, "test", dbEnabledYAML("db.host.com", "postgres", "", "mydb"))
	defer cleanup()
	_, err := config.Load()
	if err == nil {
		t.Error("expected error: database.password required when enabled=true")
	}
}

func TestLoad_Database_MissingDBName_Fails(t *testing.T) {
	cleanup := writeConfig(t, "test", dbEnabledYAML("db.host.com", "postgres", "pass", ""))
	defer cleanup()
	_, err := config.Load()
	if err == nil {
		t.Error("expected error: database.database required when enabled=true")
	}
}

func TestLoad_Database_AllFields_Passes(t *testing.T) {
	cleanup := writeConfig(t, "test", dbEnabledYAML("db.host.com", "postgres", "pass", "mydb"))
	defer cleanup()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !cfg.Database.Enabled {
		t.Error("database.enabled should be true")
	}
	if cfg.Database.Host != "db.host.com" {
		t.Errorf("Host = %q, want db.host.com", cfg.Database.Host)
	}
}

// ── DSN() method ──────────────────────────────────────────────────────────────

func TestDatabaseConfig_DSN(t *testing.T) {
	d := config.DatabaseConfig{
		Host:     "db.example.com",
		Port:     5432,
		User:     "postgres",
		Password: "s3cr3t",
		Database: "mydb",
		SSLMode:  "require",
	}
	dsn := d.DSN()
	want := "host=db.example.com port=5432 user=postgres password=s3cr3t dbname=mydb sslmode=require"
	if dsn != want {
		t.Errorf("DSN() = %q, want %q", dsn, want)
	}
}

func TestDatabaseConfig_DSN_Defaults(t *testing.T) {
	d := config.DatabaseConfig{
		Host:     "db.example.com",
		User:     "postgres",
		Password: "pass",
		Database: "mydb",
		// Port = 0 → default 5432
		// SSLMode = "" → default "require"
	}
	dsn := d.DSN()
	if !strings.Contains(dsn, "port=5432") {
		t.Errorf("DSN() should default port to 5432, got %q", dsn)
	}
	if !strings.Contains(dsn, "sslmode=require") {
		t.Errorf("DSN() should default sslmode to require, got %q", dsn)
	}
}
