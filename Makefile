# Automail — developer & CI task runner.
# Spec: docs/testing-plan.md (Part 0). One command runs the gates; the same
# gates run in .github/workflows/ci.yml so regressions can't merge silently.
#
# Docker-independent gates (fmt-check, lint, test-race, cover) make up `ci`.
# test-integration / test-e2e need the compose stack and no-op without Docker.

SHELL   := bash
CLOUD   := services/cloud
PRINTER := services/printer
PORTAL  := services/portal
GO_MODULES := $(CLOUD) $(PRINTER)

.DEFAULT_GOAL := help

.PHONY: help
help: ## Show available targets
	@grep -hE '^[a-z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| sort | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: fmt
fmt: ## Format all Go code
	@for m in $(GO_MODULES); do (cd $$m && gofmt -w .); done

.PHONY: fmt-check
fmt-check: ## Fail if any Go code is not gofmt-clean
	@bad=$$(gofmt -l $(GO_MODULES)); \
	if [ -n "$$bad" ]; then echo "gofmt needed:"; echo "$$bad"; exit 1; fi; \
	echo "✔ gofmt clean"

.PHONY: vet
vet: ## go vet both Go modules
	@for m in $(GO_MODULES); do echo "vet $$m"; (cd $$m && go vet ./...); done

.PHONY: lint
lint: vet ## Vet Go + typecheck the portal (next lint once configured, Goal T6)
	@echo "portal: tsc --noEmit"; (cd $(PORTAL) && npx --no-install tsc --noEmit)
	@if ls $(PORTAL)/.eslintrc* $(PORTAL)/eslint.config.* >/dev/null 2>&1; then \
		echo "portal: next lint"; (cd $(PORTAL) && npx --no-install next lint); \
	else echo "portal: next lint skipped (no ESLint config yet — added in Goal T6)"; fi

.PHONY: test-unit
test-unit: ## Fast unit tests, both Go modules
	@for m in $(GO_MODULES); do echo "test $$m"; (cd $$m && go test ./... -count=1); done

.PHONY: test-race
test-race: ## Unit tests under the race detector (goroutine-heavy code)
	@for m in $(GO_MODULES); do echo "race $$m"; (cd $$m && go test ./... -race -count=1); done

.PHONY: cover
cover: ## Coverage with a ratcheting floor (scripts/coverage.sh)
	@bash scripts/coverage.sh

.PHONY: fuzz
fuzz: ## Run fuzz targets briefly (targets populated in Goal T4)
	@bash scripts/fuzz.sh

.PHONY: test-integration
test-integration: ## Integration vs real Postgres/Redis/MinIO — needs Docker (Goal T5)
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ test-integration skipped: no Docker daemon (populated in Goal T5)"; exit 0; fi; \
	for m in $(GO_MODULES); do (cd $$m && go test -tags=integration ./... -count=1); done

.PHONY: test-e2e
test-e2e: ## Full-stack / browser E2E — needs Docker (Goals T7/T8)
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ test-e2e skipped: no Docker daemon (populated in Goals T7/T8)"; exit 0; fi; \
	echo "e2e harness added in Goals T7/T8"

.PHONY: ci
ci: fmt-check lint test-race cover ## Docker-independent local CI gate
	@echo "✔ CI gates passed"

.PHONY: hooks
hooks: ## Install the git pre-commit hook
	@cp scripts/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit \
		&& echo "✔ pre-commit hook installed"
