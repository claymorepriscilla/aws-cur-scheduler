# ============================================================
# Makefile — aws-cur-scheduler
# ============================================================

APP_NAME   := aws-cur-scheduler
IMAGE_NAME := ghcr.io/claymorepriscilla/$(APP_NAME)
BIN_DIR    := ./bin

.PHONY: all build run test lint clean docker-build docker-run \
        k8s-apply-dev k8s-apply-uat k8s-apply-prod help

# ── Default ────────────────────────────────────────────────
all: lint test build

# ── Build ──────────────────────────────────────────────────
build:
	@echo "▶ Building $(APP_NAME)..."
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 go build -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/scheduler
	@echo "✅ Binary: $(BIN_DIR)/$(APP_NAME)"

# ── Run local (requires configs/local.yaml configured) ─────
run:
	APP_ENV=local go run ./cmd/scheduler $(ARGS)

run-date:
	@[ "$(DATE)" ] || (echo "Usage: make run-date DATE=2025-04-08" && exit 1)
	APP_ENV=local go run ./cmd/scheduler $(DATE)

# ── Test ───────────────────────────────────────────────────
test:
	@echo "▶ Running tests..."
	go test ./... -v -race -count=1

test-coverage:
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "✅ Coverage report: coverage.html"

# ── Lint ───────────────────────────────────────────────────
lint:
	@which golangci-lint > /dev/null 2>&1 || \
		(echo "⚠️  golangci-lint not found. Install: https://golangci-lint.run/usage/install/" && exit 1)
	golangci-lint run ./...

# ── Docker ─────────────────────────────────────────────────
docker-build:
	docker build -t $(IMAGE_NAME):local .

docker-run:
	docker run --rm \
		--env-file .env.local \
		$(IMAGE_NAME):local

docker-build-env:
	@[ "$(ENV)" ] || (echo "Usage: make docker-build-env ENV=dev|uat|prod" && exit 1)
	docker build -t $(IMAGE_NAME):$(ENV) .

docker-push:
	@[ "$(ENV)" ] || (echo "Usage: make docker-push ENV=dev|uat|prod" && exit 1)
	docker push $(IMAGE_NAME):$(ENV)

# ── Kubernetes ─────────────────────────────────────────────
k8s-apply-local:
	kubectl apply -f k8s/local/all-in-one.yaml

k8s-apply-dev:
	kubectl apply -f k8s/dev/cronjob.yaml
	kubectl apply -f k8s/dev/configmap.yaml
	@echo "⚠️  Apply secret manually: kubectl apply -f k8s/dev/secret.yaml"

k8s-apply-uat:
	kubectl apply -f k8s/uat/cronjob.yaml
	kubectl apply -f k8s/uat/configmap.yaml
	@echo "⚠️  Apply secret manually: kubectl apply -f k8s/uat/secret.yaml"

k8s-apply-prod:
	kubectl apply -f k8s/prod/cronjob.yaml
	kubectl apply -f k8s/prod/configmap.yaml
	@echo "⚠️  Apply secret manually: kubectl apply -f k8s/prod/secret.yaml"

# Trigger job manually (useful for testing)
k8s-trigger-dev:
	kubectl create job --from=cronjob/aws-cur-scheduler aws-cur-scheduler-manual-$(shell date +%s) \
		-n cost-reporter-dev

k8s-trigger-prod:
	kubectl create job --from=cronjob/aws-cur-scheduler aws-cur-scheduler-manual-$(shell date +%s) \
		-n cost-reporter-prod

# ── Clean ──────────────────────────────────────────────────
clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html

# ── Help ───────────────────────────────────────────────────
help:
	@echo ""
	@echo "  $(APP_NAME) — available targets"
	@echo ""
	@echo "  Build & Run"
	@echo "    make build              Build binary to ./bin/"
	@echo "    make run                Run with local config"
	@echo "    make run-date DATE=YYYY-MM-DD  Run for specific date"
	@echo ""
	@echo "  Test & Lint"
	@echo "    make test               Run all tests"
	@echo "    make test-coverage      Run tests + HTML coverage report"
	@echo "    make lint               Run golangci-lint"
	@echo ""
	@echo "  Docker"
	@echo "    make docker-build       Build local Docker image"
	@echo "    make docker-run         Run Docker image (needs .env.local)"
	@echo "    make docker-build-env ENV=dev|uat|prod"
	@echo "    make docker-push ENV=dev|uat|prod"
	@echo ""
	@echo "  Kubernetes"
	@echo "    make k8s-apply-local    Apply local all-in-one manifest"
	@echo "    make k8s-apply-dev      Apply dev manifests"
	@echo "    make k8s-apply-uat      Apply UAT manifests"
	@echo "    make k8s-apply-prod     Apply PROD manifests"
	@echo "    make k8s-trigger-dev    Manually trigger job in dev"
	@echo "    make k8s-trigger-prod   Manually trigger job in prod"
	@echo ""
