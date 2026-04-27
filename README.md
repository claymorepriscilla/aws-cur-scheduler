# 📊 aws-cur-scheduler

> **Scheduler Service** — ดึง AWS Cost & Usage Report (CUR) จาก S3 → วิเคราะห์ค่าใช้จ่าย → บันทึกลง Supabase → แจ้งเตือนทีมผ่าน Microsoft Teams อัตโนมัติทุกวัน 08:00 น. (Bangkok Time)

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Docker](https://img.shields.io/badge/Docker-distroless-2496ED?logo=docker)](Dockerfile)

---

## Table of Contents

- [ภาพรวม](#ภาพรวม)
- [สถาปัตยกรรม](#สถาปัตยกรรม)
- [Flow การทำงาน](#flow-การทำงาน)
- [Business Rules](#business-rules)
- [โครงสร้างโปรเจค](#โครงสร้างโปรเจค)
- [Tech Stack](#tech-stack)
- [การตั้งค่า Config](#การตั้งค่า-config)
- [Supabase Setup](#supabase-setup)
- [การรัน Local](#การรัน-local)
- [Docker](#docker)
- [Kubernetes Deployment](#kubernetes-deployment)
- [ตัวอย่าง Teams Notification](#ตัวอย่าง-teams-notification)
- [Tests](#tests)
- [Environment Variables](#environment-variables)

---

## ภาพรวม

`aws-cur-scheduler` เป็น Go service ที่ออกแบบมาเพื่อรันเป็น **Kubernetes CronJob** ทุกวัน โดยทำหน้าที่:

1. **ดึงข้อมูล** Cost & Usage Report (`.csv.gz`) จาก S3 bucket (หรือ local file สำหรับ dev)
2. **วิเคราะห์** ค่าใช้จ่าย แยกตาม Service และ Resource
3. **ตรวจจับ** resource ที่น่าสงสัย (25+ กฎ: Elastic IP idle, EBS unattached, RDS 24hr, CodeCommit EOS ฯลฯ)
4. **บันทึก** snapshot รายวัน, line items ทั้งหมด, และ alerts ลง Supabase PostgreSQL
5. **คำนวณ** cost เมื่อวาน / cost วันนี้ (delta) / ยอดสะสมรายเดือน จาก DB
6. **ส่งรายงาน** เข้า Microsoft Teams ผ่าน Adaptive Card

> CUR ของ AWS เป็น **cumulative monthly file** (ยอดสะสมตั้งแต่ต้นเดือน)
> ดังนั้น `cost วันนี้ = monthlyTotal − snapshot เมื่อวาน`

---

## สถาปัตยกรรม

```
┌────────────────────────────────────────────────────────────────┐
│                        AWS Environment                         │
│                                                                │
│  ┌─────────────┐   CUR Export      ┌────────────────────────┐ │
│  │ AWS Billing │ ────────────────▶ │ S3 Bucket              │ │
│  │ & Cost Mgmt │  (monthly .csv.gz)│ YOUR_S3_BUCKET_NAME    │ │
│  └─────────────┘                   └────────────────────────┘ │
└───────────────────────────────────────────┬────────────────────┘
                                            │ GetObject (AWS SDK v2)
                                            ▼
┌────────────────────────────────────────────────────────────────┐
│                   Kubernetes Cluster (EKS)                     │
│                                                                │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │               CronJob  0 1 * * *  (01:00 UTC)            │  │
│  │                                                          │  │
│  │   ┌──────────────────────────────────────────────────┐  │  │
│  │   │              aws-cur-scheduler Pod               │  │  │
│  │   │                                                  │  │  │
│  │   │  cmd/scheduler/main.go                           │  │  │
│  │   │    └─▶ internal/scheduler/job.go                 │  │  │
│  │   │          ├─▶ internal/aws/client.go  (S3/local)  │  │  │
│  │   │          ├─▶ internal/analyzer/      (CUR)       │  │  │
│  │   │          ├─▶ internal/store/         (DB)        │  │  │
│  │   │          └─▶ internal/notifier/      (Teams)     │  │  │
│  │   └──────────────────────────────────────────────────┘  │  │
│  └──────────────────────────────────────────────────────────┘  │
└──────────┬────────────────────────────────────┬────────────────┘
           │ pgx/v5 (PostgreSQL)                │ HTTPS POST
           ▼                                    ▼
┌─────────────────────┐              ┌────────────────────────┐
│  Supabase           │              │   Microsoft Teams      │
│  PostgreSQL         │              │   Channel Notification │
│  - daily_cost_*     │              └────────────────────────┘
│  - cost_line_items  │
│  - cost_alerts      │
└─────────────────────┘
```

---

## Flow การทำงาน

```
Cron 01:00 UTC (08:00 BKK)
         │
         ▼
   main.go
   resolveTargetDate()
   ├── CLI arg        → go run ./cmd/scheduler 2026-04-10
   ├── TARGET_DATE    → env var
   └── default        → yesterday UTC
         │
         ▼
   scheduler.NewJob(cfg, log)   ← ถ้า database.enabled=true และต่อ DB ไม่ได้ → error + exit(1)
         │
         ▼
   scheduler.Job.Run(ctx, targetDate)
   │
   ├── Step 1: fetchItems()
   │     ├── LocalPath ≠ "" → ReadLocalCURFile()  (dev/test)
   │     └── S3 → FindCURFile() → ReadCURFile()
   │
   ├── Step 2: analyzer.Analyze()
   │     ├── group by service (sorted desc)
   │     ├── group by resource → top N
   │     └── detectSuspicious()  (25+ กฎ)
   │
   ├── Step 3: reportDate = targetDate.Truncate(24h)
   │
   ├── Step 4: saveToStore()   [ถ้า database.enabled=true]
   │     ├── UpsertSnapshot    → daily_cost_snapshots
   │     ├── ReplaceLineItems  → cost_line_items (DELETE + COPY)
   │     └── ReplaceAlerts     → cost_alerts     (DELETE + COPY)
   │
   ├── Step 5: getYesterdayCost()  [จาก DB หรือ 0 ถ้าไม่มีข้อมูล]
   │     monthlyTotal = analysis.TotalCost   (cumulative CUR)
   │     yesterdayCost = snapshot ล่าสุดก่อน reportDate
   │     dailyCost = monthlyTotal − yesterdayCost
   │
   ├── Step 6: printReport()   → log human-readable summary
   │
   └── Step 7: maybeNotify()  [ถ้า teams.enable_webhook=true]
         ├── buildCard() → Adaptive Card JSON
         └── PostJSON()  → Teams Webhook (retry 2 ครั้ง)
```

---

## Business Rules

### Cost Calculation

| ตัวแปร | คำอธิบาย |
|--------|---------|
| `monthlyTotal` | `analysis.TotalCost` — ยอดสะสมจาก CUR file (cumulative) |
| `yesterdayCost` | snapshot ล่าสุดจาก DB ก่อน reportDate |
| `dailyCost` | `monthlyTotal − yesterdayCost` (delta ของวันนี้) |

### Suspicious Resource Detection (25+ กฎ)

| สัญญาณ | เงื่อนไข | ระดับ |
|--------|----------|-------|
| Elastic IP ไม่ได้ใช้ | `elasticip` + `idle/unassociated` | 🔴 Critical |
| EBS Volume ไม่ได้ attach | `ebs` + `available/unattached` | 🔴 Critical |
| RDS Snapshot สะสม | `rds` + `snapshot` ใน usage_type | 🔴 Critical |
| EKS Extended Support | `eks` + `extended support` ใน description | 🔴 Critical |
| SageMaker Endpoint 24hr | `sagemaker` + `endpoint` + usage ≥ 23hr | 🔴 Critical |
| Redshift 24hr | `redshift` + usage ≥ 23hr | 🔴 Critical |
| NAT Gateway | `natgateway` ใน usage_type หรือ operation | 🟡 Warning |
| RDS/Aurora 24hr | service=RDS + usage ≥ 23hr | 🟡 Warning |
| RDS Old-gen instance | `db.t2./db.m3./db.r3.` ใน usage_type | 🟡 Warning |
| ElastiCache 24hr | `elasticache` + usage ≥ 23hr | 🟡 Warning |
| OpenSearch 24hr | `opensearch/elasticsearch` + usage ≥ 23hr | 🟡 Warning |
| Lambda Provisioned Concurrency | `lambda` + `provisionedconcurrency` | 🟡 Warning |
| S3 Glacier Retrieval | `glacier` + `retrieval` | 🟡 Warning |
| API Gateway Cache | `apigateway` + `cache` | 🟡 Warning |
| CloudWatch Logs (no retention) | `cloudwatch` + `logs` + `storage` | 🟡 Warning |
| Inter-AZ Data Transfer | `regionaldata` หรือ `inter az` | 🟡 Warning |
| CodeCommit (End-of-Sale) | service=codecommit | 🟡 Warning |
| EC2 24hr sandbox | `boxusage` + usage ≥ 23hr | 🟡 Warning |
| EC2 ไม่มี tag Name | `boxusage` + TagName="" | 🟠 Notice |
| EC2 ไม่มี tag Owner | `boxusage` + TagOwner="" | 🟠 Notice |
| EC2 Old-gen t2 | `boxusage:t2.` ใน usage_type | 🟠 Notice |
| Public IPv4 | `in-use public ipv4` ใน description | 🟠 Notice |
| ECR Storage เก่า | `ecr` + `storage` | 🟠 Notice |
| CloudFront ไม่มี traffic | `cloudfront` + usage=0 + cost>0 | 🟠 Notice |
| DynamoDB On-Demand สูง | `dynamodb` + `requestunits` + cost>10 | 🟠 Notice |
| Secrets Manager | `secretsmanager` + `secret` | 🟠 Notice |
| WAF WebACL | service=WAF | 🟠 Notice |
| EC2 + Savings Plans | `boxusage` + usage ≥ 23hr | 🟠 Notice |
| SageMaker Notebook 24hr | `sagemaker` + `notebookinstance` + usage ≥ 23hr | 🟡 Warning |
| EKS Cluster 24hr | `eks` + `aks` + usage ≥ 23hr | 🟠 Notice |

### Budget Alert

| เงื่อนไข | พฤติกรรม |
|---------|---------|
| `monthlyTotal / budget_limit < 70%` | 🟢 รายงานปกติ |
| `monthlyTotal / budget_limit ≥ 70%` | 🟡 Warning color |
| `monthlyTotal / budget_limit ≥ 90%` | 🔴 Attention color |
| `≥ alert_threshold_pct` (default 70%) | 🚨 Alert Banner ใน Teams card |

---

## โครงสร้างโปรเจค

```
aws-cur-scheduler/
├── cmd/
│   └── scheduler/
│       └── main.go              # entrypoint — parse args, init, run job
│
├── internal/                    # business logic (not exported)
│   ├── config/
│   │   ├── config.go            # Config struct + Viper loader + DSN()
│   │   └── config_test.go
│   ├── aws/
│   │   └── client.go            # S3 client: FindCURFile, ReadCURFile, ReadLocalCURFile
│   ├── analyzer/
│   │   ├── analyzer.go          # Analyze(), detectSuspicious() (25+ rules)
│   │   └── analyzer_test.go
│   ├── notifier/
│   │   ├── notifier.go          # Teams Adaptive Card builder + sender
│   │   └── notifier_test.go
│   ├── store/
│   │   ├── store.go             # Store interface + domain types
│   │   └── postgres.go          # PostgreSQL implementation (pgx/v5)
│   └── scheduler/
│       ├── job.go               # Pipeline orchestrator (7 steps)
│       └── job_test.go
│
├── pkg/                         # reusable packages
│   ├── logger/
│   │   └── logger.go            # zap SugaredLogger wrapper
│   └── httpclient/
│       └── client.go            # HTTP client with retry
│
├── configs/                     # config files per environment
│   ├── local.yaml               # all-in-one local (gitignored when has real creds)
│   ├── dev.yaml
│   ├── uat.yaml
│   └── prod.yaml
│
├── k8s/                         # Kubernetes manifests
│   ├── local/
│   ├── dev/
│   ├── uat/
│   └── prod/
│
├── Dockerfile
├── Makefile
├── go.mod
└── go.sum
```

---

## Tech Stack

| Component | Technology |
|-----------|-----------|
| Language | Go 1.25 |
| AWS SDK | aws-sdk-go-v2 |
| Config | spf13/viper (YAML + env override) |
| Logger | uber-go/zap (SugaredLogger) |
| Database | Supabase PostgreSQL + jackc/pgx v5 |
| Container | golang:1.25-alpine → distroless/static:nonroot |
| Orchestration | Kubernetes CronJob |
| Notification | Microsoft Teams Adaptive Card v1.4 |

---

## การตั้งค่า Config

Config ใช้ระบบ **2 ชั้น**:

1. **YAML file** — `configs/{env}.yaml` (non-sensitive defaults)
2. **Environment Variables** — override ค่าใดก็ได้ รวมถึง secrets จาก K8s Secret

### ลำดับความสำคัญ (สูง → ต่ำ)
```
ENV VAR  >  configs/{env}.yaml
```

### เลือก Environment

```bash
APP_ENV=local   # default
APP_ENV=dev
APP_ENV=uat
APP_ENV=prod
```

### ค่าที่ต้องตั้ง

| Key | ENV VAR | คำอธิบาย |
|-----|---------|---------|
| `aws.region` | `APP_AWS_REGION` | AWS Region ของ S3 bucket |
| `aws.access_key_id` | `AWS_ACCESS_KEY_ID` | ไม่ต้องใส่ถ้าใช้ IRSA |
| `aws.secret_access_key` | `AWS_SECRET_ACCESS_KEY` | ไม่ต้องใส่ถ้าใช้ IRSA |
| `cur.s3_bucket` | `APP_CUR_S3_BUCKET` | ชื่อ S3 bucket ที่เก็บ CUR |
| `cur.s3_prefix` | `APP_CUR_S3_PREFIX` | Prefix path ของ CUR files |
| `cur.local_path` | — | (optional) อ่านจาก local file แทน S3 |
| `teams.webhook_url` | `TEAMS_WEBHOOK_URL` | Microsoft Teams Webhook URL |
| `teams.enable_webhook` | — | `true` = ส่ง Teams จริง, `false` = แค่ log |
| `budget.limit_usd` | — | งบรายเดือน (USD) |
| `budget.alert_threshold_pct` | — | % ที่จะส่ง alert (default 70) |
| `database.enabled` | — | `true` = ใช้ Supabase DB — ถ้าต่อไม่ได้จะ error และหยุด process ทันที |
| `database.host` | `APP_DATABASE_HOST` | Supabase host |
| `database.port` | `APP_DATABASE_PORT` | PostgreSQL port (default 5432) |
| `database.database` | `APP_DATABASE_DATABASE` | Database name (default "postgres") |
| `database.user` | `APP_DATABASE_USER` | Database user |
| `database.password` | `APP_DATABASE_PASSWORD` | Database password (ใส่ใน Secret) |
| `database.sslmode` | — | `require` (default) |
| `database.timeout_sec` | — | Connection timeout in seconds (default 10) |

---

## Supabase Setup

**Security & Secrets**

- Do NOT commit real secrets. Use `configs/local.yaml.example` as the template and create your local file from it:

```bash
cp configs/local.yaml.example configs/local.yaml
# edit configs/local.yaml (NOT committed) or use .env.local for env-based secrets
```

- Prefer environment variables / Kubernetes Secrets / IRSA for production. `configs/*.example` should live in the repo; `configs/local.yaml` must be gitignored.

- If you accidentally commit secrets to Git: rotate the credentials immediately (AWS IAM keys, DB passwords, webhook tokens), then purge the secret from history. Example steps:

```bash
# 1) Rotate keys in AWS/IAM immediately (disable/delete old key)

# 2) Remove file from git history (example using git filter-repo):
git clone --mirror git@github.com:your/repo.git
cd repo.git
git filter-repo --path configs/local.yaml --invert-paths
git push --force

# Alternative: BFG (simpler for common cases)
bfg --delete-files configs/local.yaml
git reflog expire --expire=now --all && git gc --prune=now --aggressive
git push --force
```

- Install `pre-commit` hooks locally to prevent committing secrets and large files:

```bash
pip install pre-commit
pre-commit install
pre-commit run --all-files
```

- I added a `.pre-commit-config.yaml` with `detect-secrets` and a local check to block `configs/local.yaml` commits. Keep `.secrets.baseline` only if you want to allow known false positives.

**Docker / Build notes**

- The `Dockerfile` was updated to avoid copying real configs into the final image. Do not bake secrets into images. Use env injection or mount Kubernetes Secrets at runtime:

```bash
# run with env file (local):
docker run --rm --env-file .env.local aws-cur-scheduler:local

# in k8s, mount secrets or use IRSA (recommended for AWS prod)
```

### 1. สร้าง Tables

รัน SQL นี้ใน Supabase SQL Editor:

```sql
-- Daily cost snapshot (one row per day per env)
CREATE TABLE IF NOT EXISTS daily_cost_snapshots (
    id          BIGSERIAL PRIMARY KEY,
    report_date DATE        NOT NULL,
    env         TEXT        NOT NULL,
    total_cost  NUMERIC(14,6) NOT NULL DEFAULT 0,
    item_count  INT         NOT NULL DEFAULT 0,
    by_service  JSONB       NOT NULL DEFAULT '[]',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (report_date, env)
);

-- All CUR line items (replace each day)
CREATE TABLE IF NOT EXISTS cost_line_items (
    id            BIGSERIAL PRIMARY KEY,
    report_date   DATE          NOT NULL,
    env           TEXT          NOT NULL,
    service       TEXT,
    resource_id   TEXT,
    description   TEXT,
    usage_type    TEXT,
    usage_amount  NUMERIC(18,6),
    usage_unit    TEXT,
    operation     TEXT,
    instance_type TEXT,
    region        TEXT,
    tag_name      TEXT,
    tag_owner     TEXT,
    tag_env       TEXT,
    cost          NUMERIC(14,6) NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_cost_line_items_date_env ON cost_line_items (report_date, env);

-- Suspicious resource alerts
CREATE TABLE IF NOT EXISTS cost_alerts (
    id            BIGSERIAL PRIMARY KEY,
    report_date   DATE          NOT NULL,
    env           TEXT          NOT NULL,
    resource_id   TEXT,
    service       TEXT,
    resource_cost NUMERIC(14,6),
    tag_owner     TEXT,
    severity      TEXT,  -- 'critical' | 'warning' | 'notice'
    message       TEXT
);

CREATE INDEX IF NOT EXISTS idx_cost_alerts_date_env ON cost_alerts (report_date, env);
```

### 2. ตั้งค่า Config

ใน `configs/local.yaml`:
```yaml
database:
  enabled: true
  host: "db.YOUR_PROJECT_ID.supabase.co"
  port: 5432
  database: "postgres"
  user: "postgres"
  password: "YOUR_PASSWORD"
  sslmode: "require"
  timeout_sec: 10
```

---

## การรัน Local

### Prerequisites

```bash
go version   # ต้องการ >= 1.25
```

### 1. Clone & Setup

```bash
git clone https://github.com/claymorepriscilla/aws-cur-scheduler.git
cd aws-cur-scheduler
go mod download
```

### 2. ตั้งค่า config

แก้ไข `configs/local.yaml`:

```yaml
aws:
  access_key_id: "YOUR_ACCESS_KEY_ID"
  secret_access_key: "YOUR_SECRET_ACCESS_KEY"

# หรือใช้ local file แทน S3 (ไม่ต้องการ AWS credentials)
cur:
  local_path: "/path/to/your-cur-file.csv.gz"

teams:
  webhook_url: "YOUR_TEAMS_WEBHOOK_URL"
  enable_webhook: false   # ← false = แค่ print log (ปลอดภัยสำหรับ dev)

database:
  enabled: true
  host: "db.YOUR_PROJECT.supabase.co"
  password: "YOUR_DB_PASSWORD"
```

### 3. รัน

```bash
# รันสำหรับเมื่อวาน (default)
make run

# หรือระบุวันที่
make run-date DATE=2025-04-10

# หรือตรง
APP_ENV=local go run ./cmd/scheduler 2025-04-10
```

### 4. Build binary

```bash
make build
./bin/aws-cur-scheduler 2025-04-10
```

---

## Docker

### Build

```bash
make docker-build
```

### Run

```bash
cp .env.local.example .env.local
# แก้ไขค่าจริงใน .env.local

make docker-run
# หรือ
docker run --rm --env-file .env.local aws-cur-scheduler:local 2025-04-10
```

---

## Kubernetes Deployment

### DEV / UAT

```bash
# 1. Apply ConfigMap
kubectl apply -f k8s/dev/configmap.yaml

# 2. สร้าง secret (AWS keys, Teams webhook, DB password)
cp k8s/dev/secret.yaml.example k8s/dev/secret.yaml
# แก้ไข REPLACE_ME ในไฟล์
kubectl apply -f k8s/dev/secret.yaml

# 3. Deploy CronJob
kubectl apply -f k8s/dev/cronjob.yaml
```

### PROD (IRSA)

```bash
# IRSA — ไม่ต้องใส่ AWS keys, DB password มาจาก K8s Secret
make k8s-apply-prod
```

### Trigger งานทันที

```bash
kubectl create job --from=cronjob/aws-cur-scheduler manual-$(date +%s) \
  -n cost-reporter-dev

kubectl logs -l app=aws-cur-scheduler -n cost-reporter-dev --tail=200
```

---

## ตัวอย่าง Teams Notification

Card ที่ส่งเข้า Microsoft Teams ช่อง cost-alert:

```
┌─────────────────────────────────────────────────────────────┐
│  📊 AWS Cost Report — DEV                                   │
│  26 Apr 2026                                                │
├─────────────────────────────────────────────────────────────┤
│  📅 วันที่          26 Apr 2026                             │
│  🌍 Environment     dev                                     │
│  💰 Cost วันนี้     $35.2289                                │
│  📊 เดือนนี้รวม     $128.91 / $500                         │
│  📈 ใช้ไปแล้ว       25.8%                                   │
│  💵 คงเหลือ         $371.09                                 │
│  📋 Line items      360                                     │
├─────────────────────────────────────────────────────────────┤
│  🟢 ▓▓▓▓▓░░░░░░░░░░░░░░░ 25.8%                             │
├─────────────────────────────────────────────────────────────┤
│  🏷️ Cost by Service                                        │
│  AmazonEC2              $82.4216  64%  ████████████        │
│  AmazonEKS              $36.4435  28%  █████               │
│  AmazonVPC               $6.4223   5%                      │
│  AmazonSageMaker          $1.4510   1%                      │
│  AWSCodeCommit            $1.0000   1%                      │
│  AmazonRDS                $0.8179   1%                      │
│  AWSSecretsManager        $0.3457   0%                      │
│  AmazonS3                 $0.0024   0%                      │
├─────────────────────────────────────────────────────────────┤
│  🔍 Top Resources                                           │
│  1. [AmazonEC2] arn:aws:ec2:…/nat-gateway — $36.9041       │
│     └ $0.059 per NAT Gateway Hour          24.00 Hrs       │
│  2. [AmazonEKS] arn:aws:eks:…/cluster     — $36.4435       │
│     └ Amazon EKS extended support usage   24.00 Hours      │
│  3. [AmazonEC2] i-06d78de4d1d18758d (t2.xlarge) — $23.6664│
│     └ $0.2336 per On Demand Linux t2.xlarge   24.00 Hrs   │
├─────────────────────────────────────────────────────────────┤
│  ⚠️ ต้องตรวจสอบ                                            │
│  i-06d78de4d1d18758d — $82.1735                            │
│    🟠 EC2 ไม่มี tag Name ไม่รู้ว่าใครสร้าง                │
│    🟠 EC2 ไม่มี tag Owner ไม่มีคนรับผิดชอบ resource นี้   │
│    🟠 EC2 ใช้ instance generation เก่า (t2)                │
│    🟡 EC2 รัน 24hr ใน sandbox                              │
│  arn:aws:ec2:…/nat-gateway — $36.8902                      │
│    🟡 NAT Gateway มีค่า ~$32/เดือน                         │
│  arn:aws:eks:…/cluster — $22.0161                          │
│    🔴 EKS Extended Support $0.60/cluster/hr                 │
└─────────────────────────────────────────────────────────────┘
```

> **Budget color:** 🟢 Good (<70%) · 🟡 Warning (≥70%) · 🔴 Attention (≥90%)
> เมื่อถึง `alert_threshold_pct` จะมี 🚨 Alert Banner เพิ่มที่ด้านล่าง card

---

## Tests

### รัน unit tests

```bash
make test
```

### รันพร้อม coverage report

```bash
make test-coverage
open coverage.html
```

### Test coverage

| Package | Test cases | เป้าหมาย |
|---------|-----------|---------|
| `internal/config` | ValidConfig, EnvVarOverride, MissingBucket/Prefix/Region, WebhookRequired, ZeroBudget, LocalPathBypass, DatabaseValidation (4 cases), DSN() | >95% |
| `internal/analyzer` | TotalCost, ByService sort, TopN, ItemCount, EmptyItems, ZeroTopN, EmptyResourceID, Suspicious (25+ detection rules), FlagDedup, SortedByCost | >90% |
| `internal/notifier` | Send success/error, BudgetWarning/Attention/OverFull, NoSuspicious, ZeroTotalCost, ResourceFallbacks, ManyServices/Resources/Suspicious | >90% |
| `internal/scheduler` | sanitizeUTF8, truncateStr, parseSeverity, parseMessage, toSnapshot, toLineItems, toAlerts, curSource, NewJob, getYesterdayCost, saveToStore, printReport, maybeNotify, Run (local file) | >85% |

---

## Environment Variables

### ConfigMap (non-sensitive)

| Variable | Default | คำอธิบาย |
|----------|---------|---------|
| `APP_ENV` | `local` | Environment: `local` / `dev` / `uat` / `prod` |
| `APP_AWS_REGION` | `ap-southeast-1` | AWS region |
| `APP_CUR_S3_BUCKET` | — | S3 bucket ที่เก็บ CUR |
| `APP_CUR_S3_PREFIX` | — | S3 prefix path |
| `APP_BUDGET_LIMIT_USD` | — | งบรายเดือน USD |
| `APP_BUDGET_ALERT_THRESHOLD_PCT` | `70` | % ที่ trigger alert |
| `APP_REPORT_TOP_N_RESOURCES` | `15` | จำนวน resource ใน report |
| `APP_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `APP_LOG_FORMAT` | `json` | `json` / `console` |
| `APP_DATABASE_HOST` | — | Supabase host |
| `APP_DATABASE_PORT` | `5432` | PostgreSQL port |
| `APP_DATABASE_DATABASE` | `postgres` | Database name |
| `APP_DATABASE_USER` | `postgres` | Database user |

### Secret (sensitive)

| Variable | คำอธิบาย |
|----------|---------|
| `AWS_ACCESS_KEY_ID` | AWS Access Key (ไม่ต้องใส่ถ้าใช้ IRSA) |
| `AWS_SECRET_ACCESS_KEY` | AWS Secret Key (ไม่ต้องใส่ถ้าใช้ IRSA) |
| `TEAMS_WEBHOOK_URL` | Microsoft Teams Webhook URL |
| `APP_DATABASE_PASSWORD` | Supabase database password |

### Runtime (optional)

| Variable | คำอธิบาย |
|----------|---------|
| `TARGET_DATE` | วันที่ต้องการรัน (YYYY-MM-DD) — ถ้าไม่ระบุใช้เมื่อวาน UTC |

---

> **หมายเหตุ:** CUR data จาก AWS จะพร้อมประมาณ ~24 ชั่วโมงหลังจากสิ้นสุดวันนั้น
> CUR file เป็น **cumulative monthly** — ยอด `total_cost` ในไฟล์คือยอดสะสมตั้งแต่ต้นเดือน
> `cost วันนี้` = ยอด CUR วันนี้ − snapshot ล่าสุดจาก DB ก่อน reportDate
