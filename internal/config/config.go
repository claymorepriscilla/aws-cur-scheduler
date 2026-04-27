// Package config loads and validates application configuration from files and
// environment variables. Config files live in configs/ directory and are named
// by environment: local.yaml, dev.yaml, uat.yaml, prod.yaml.
package config

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// Config is the root configuration struct.
type Config struct {
	App      AppConfig      `mapstructure:"app"`
	AWS      AWSConfig      `mapstructure:"aws"`
	CUR      CURConfig      `mapstructure:"cur"`
	Teams    TeamsConfig    `mapstructure:"teams"`
	Budget   BudgetConfig   `mapstructure:"budget"`
	Report   ReportConfig   `mapstructure:"report"`
	Log      LogConfig      `mapstructure:"log"`
	Database DatabaseConfig `mapstructure:"database"`
}

// AppConfig holds application-level settings.
type AppConfig struct {
	Env     string `mapstructure:"env"`
	Version string `mapstructure:"version"`
}

// AWSConfig holds AWS credentials and region.
// In production these come from IAM role / pod identity; keys only needed for local.
type AWSConfig struct {
	Region          string `mapstructure:"region"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	// UseInstanceProfile = true → ignore key/secret, rely on IRSA / instance profile
	UseInstanceProfile bool `mapstructure:"use_instance_profile"`
}

// CURConfig holds Cost and Usage Report S3 location settings.
// When LocalPath is set (local env only), the job reads from the filesystem
// instead of S3 — useful for offline testing without AWS credentials.
type CURConfig struct {
	S3Bucket  string `mapstructure:"s3_bucket"`
	S3Prefix  string `mapstructure:"s3_prefix"`
	LocalPath string `mapstructure:"local_path"` // local testing only: absolute/relative path to .csv or .csv.gz
}

// TeamsConfig holds Microsoft Teams webhook settings.
type TeamsConfig struct {
	WebhookURL    string `mapstructure:"webhook_url"`
	TimeoutSec    int    `mapstructure:"timeout_sec"`
	EnableWebhook bool   `mapstructure:"enable_webhook"` // false = log report only, do NOT send to Teams
}

// BudgetConfig holds monthly budget and alert threshold settings.
type BudgetConfig struct {
	LimitUSD       float64 `mapstructure:"limit_usd"`
	AlertThreshPct float64 `mapstructure:"alert_threshold_pct"`
}

// ReportConfig holds report display settings.
type ReportConfig struct {
	TopNResources int `mapstructure:"top_n_resources"`
}

// LogConfig holds logging settings.
type LogConfig struct {
	Level  string `mapstructure:"level"`  // debug | info | warn | error
	Format string `mapstructure:"format"` // json | console
}

// DatabaseConfig holds Supabase/PostgreSQL connection settings.
// When Enabled = false the store is not created and the job falls back
// to its original behaviour (S3 monthly total / daily-cost fallback).
type DatabaseConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	Host       string `mapstructure:"host"`
	Port       int    `mapstructure:"port"`
	Database   string `mapstructure:"database"`
	User       string `mapstructure:"user"`
	Password   string `mapstructure:"password"`
	SSLMode    string `mapstructure:"sslmode"`     // require | disable | prefer
	TimeoutSec int    `mapstructure:"timeout_sec"`
}

// DSN builds a PostgreSQL key=value connection string.
// ใช้รูปแบบ DSN แทน URL เพื่อหลีกเลี่ยงปัญหา URL-encoding ของ password
func (d *DatabaseConfig) DSN() string {
	sslmode := d.SSLMode
	if sslmode == "" {
		sslmode = "require"
	}
	port := d.Port
	if port == 0 {
		port = 5432
	}
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, port, d.User, d.Password, d.Database, sslmode,
	)
}

// Load reads the config file for the current ENV and overlays environment variables.
// ENV is determined by the APP_ENV environment variable (default: local).
func Load() (*Config, error) {
	env := strings.ToLower(os.Getenv("APP_ENV"))
	if env == "" {
		env = "local"
	}

	v := viper.New()

	// ── File source ──────────────────────────────────────────
	v.SetConfigName(env)
	v.SetConfigType("yaml")
	v.AddConfigPath("./configs")
	v.AddConfigPath("/etc/aws-cur-scheduler")

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read config file [%s]: %w", env, err)
	}

	// ── Environment variable overrides ───────────────────────
	// Format: APP_AWS_REGION → aws.region
	v.SetEnvPrefix("APP")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Explicit bindings for Secret-sourced values (mounted as env vars in k8s)
	_ = v.BindEnv("aws.access_key_id", "AWS_ACCESS_KEY_ID")
	_ = v.BindEnv("aws.secret_access_key", "AWS_SECRET_ACCESS_KEY")
	_ = v.BindEnv("teams.webhook_url", "TEAMS_WEBHOOK_URL")

	cfg := &Config{}
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation: %w", err)
	}

	return cfg, nil
}

func validate(cfg *Config) error {
	// When using local file, S3 settings are not required.
	if cfg.CUR.LocalPath == "" {
		if cfg.CUR.S3Bucket == "" {
			return fmt.Errorf("cur.s3_bucket is required (or set cur.local_path for local testing)")
		}
		if cfg.CUR.S3Prefix == "" {
			return fmt.Errorf("cur.s3_prefix is required (or set cur.local_path for local testing)")
		}
		if cfg.AWS.Region == "" {
			return fmt.Errorf("aws.region is required")
		}
	}
	// Webhook URL is only required when webhook sending is enabled.
	if cfg.Teams.EnableWebhook && cfg.Teams.WebhookURL == "" {
		return fmt.Errorf("teams.webhook_url is required when teams.enable_webhook is true")
	}
	// DB fields are required when DB is enabled.
	if cfg.Database.Enabled {
		if cfg.Database.Host == "" {
			return fmt.Errorf("database.host is required when database.enabled is true")
		}
		if cfg.Database.User == "" {
			return fmt.Errorf("database.user is required when database.enabled is true")
		}
		if cfg.Database.Password == "" {
			return fmt.Errorf("database.password is required when database.enabled is true")
		}
		if cfg.Database.Database == "" {
			return fmt.Errorf("database.database is required when database.enabled is true")
		}
	}
	if cfg.Budget.LimitUSD <= 0 {
		return fmt.Errorf("budget.limit_usd must be > 0")
	}
	return nil
}
