#!/bin/bash
# ============================================================
# scripts/run-manual-job.sh
# Trigger a one-off Kubernetes Job from the CronJob template
# Usage: ./scripts/run-manual-job.sh [ENV] [DATE]
#   ENV  : dev | uat | prod  (default: dev)
#   DATE : YYYY-MM-DD        (optional, overrides target date)
# ============================================================

set -euo pipefail

ENV="${1:-dev}"
DATE="${2:-}"

case "$ENV" in
  dev)  NS="cost-reporter-dev" ;;
  uat)  NS="cost-reporter-uat" ;;
  prod) NS="cost-reporter-prod" ;;
  *)
    echo "❌ Unknown ENV: $ENV. Use: dev | uat | prod"
    exit 1
    ;;
esac

JOB_NAME="aws-cur-scheduler-manual-$(date +%s)"

echo "▶ Creating manual Job in namespace: $NS"
echo "  Job name : $JOB_NAME"
[ -n "$DATE" ] && echo "  Date     : $DATE"

kubectl create job \
  --from=cronjob/aws-cur-scheduler \
  "$JOB_NAME" \
  -n "$NS"

if [ -n "$DATE" ]; then
  # Patch the job to pass TARGET_DATE as env var
  kubectl patch job "$JOB_NAME" -n "$NS" --type=json -p "[
    {\"op\": \"add\", \"path\": \"/spec/template/spec/containers/0/env/-\",
     \"value\": {\"name\": \"TARGET_DATE\", \"value\": \"$DATE\"}}
  ]"
fi

echo ""
echo "✅ Job created. Tail logs with:"
echo "   kubectl logs -n $NS -l job-name=$JOB_NAME -f"
