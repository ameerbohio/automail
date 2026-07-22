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
	@for m in $(GO_MODULES); do echo "vet $$m"; (cd $$m && go vet ./...) || exit 1; done

.PHONY: lint
lint: vet ## Vet Go + typecheck the portal (next lint once configured, Goal T6)
	@echo "portal: tsc --noEmit"; (cd $(PORTAL) && npx --no-install tsc --noEmit)
	@if ls $(PORTAL)/.eslintrc* $(PORTAL)/eslint.config.* >/dev/null 2>&1; then \
		echo "portal: next lint"; (cd $(PORTAL) && npx --no-install next lint); \
	else echo "portal: next lint skipped (no ESLint config yet — added in Goal T6)"; fi

.PHONY: test-unit
test-unit: ## Fast unit tests, both Go modules
	@for m in $(GO_MODULES); do echo "test $$m"; (cd $$m && go test ./... -count=1) || exit 1; done

.PHONY: test-race
test-race: ## Unit tests under the race detector (goroutine-heavy code)
	@for m in $(GO_MODULES); do echo "race $$m"; (cd $$m && go test ./... -race -count=1) || exit 1; done

.PHONY: cover
cover: ## Go coverage with a ratcheting floor (scripts/coverage.sh)
	@bash scripts/coverage.sh

.PHONY: cover-portal
cover-portal: ## Portal (Vitest) coverage with a ratcheting floor
	@bash scripts/coverage-portal.sh

.PHONY: fuzz
fuzz: ## Run fuzz targets briefly (targets populated in Goal T4)
	@bash scripts/fuzz.sh

.PHONY: crypto-contract
crypto-contract: ## Cross-language crypto contract (browser <-> printer), regenerated
	@echo "1/3 Go encrypts a guard vector for the browser…"
	@(cd $(PRINTER) && go test -tags=contract -run '^TestContractGoEncryptForBrowser$$' -count=1 .)
	@echo "2/3 Browser encrypts the production vector + decrypts the guard vector…"
	@(cd $(PORTAL) && npx --no-install vitest run --config vitest.contract.config.ts)
	@echo "3/3 Printer decrypts the browser vector byte-for-byte + rejects tampering…"
	@(cd $(PRINTER) && go test -tags=contract -run '^TestContractPrinterDecryptsBrowser$$' -count=1 -v .)

.PHONY: scan
scan: ## Security scanners: govulncheck + gosec + gitleaks (npm audit is informational)
	@command -v govulncheck >/dev/null || go install golang.org/x/vuln/cmd/govulncheck@latest
	@command -v gosec       >/dev/null || go install github.com/securego/gosec/v2/cmd/gosec@latest
	@command -v gitleaks    >/dev/null || go install github.com/zricethezav/gitleaks/v8@latest
	@echo "── govulncheck (Go stdlib + dep CVEs, reachability) ──"
	@for m in $(GO_MODULES); do echo "$$m:"; (cd $$m && govulncheck ./...) || exit 1; done
	@echo "── gosec (SAST) ── excludes: -exclude-generated (sqlc files); G104 unhandled-err + G706 log-injection (low-value, noisy). Intentional cases are annotated inline with justified #nosec."
	@for m in $(GO_MODULES); do echo "$$m:"; (cd $$m && gosec -quiet -exclude-generated -exclude=G104,G706 ./...) || exit 1; done
	@echo "── gitleaks (secrets in git history; test fixtures allowlisted in .gitleaks.toml) ──"
	@gitleaks git --no-banner -c .gitleaks.toml
	@echo "── npm audit (portal, INFORMATIONAL) ── next@14.2.5 has advisories needing an owner-approved dependency bump; not a blocking gate."
	@(cd $(PORTAL) && npm audit --omit=dev || true)

.PHONY: test-integration
test-integration: ## Integration vs real Postgres/Redis/MinIO — needs Docker (Goal T5)
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ test-integration skipped: no Docker daemon (populated in Goal T5)"; exit 0; fi; \
	for m in $(GO_MODULES); do (cd $$m && go test -tags=integration ./... -count=1) || exit 1; done

.PHONY: test-e2e
test-e2e: ## Portal browser E2E (Playwright vs a clean compose stack) — needs Docker (Goal T7)
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ test-e2e skipped: no Docker daemon (Goal T7 needs the compose stack)"; exit 0; fi; \
	bash scripts/e2e/run.sh

.PHONY: test-e2e-full
test-e2e-full: ## Full-system E2E: two-node stack, real job to delivered + /dev/shm wipe (Goal T8) — needs Docker
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ test-e2e-full skipped: no Docker daemon (Goal T8 needs the compose stack)"; exit 0; fi; \
	bash scripts/e2e/full.sh

.PHONY: chaos
chaos: ## Resilience/chaos: kill each component in turn, prove exactly-once + reconnect (Goal T9) — needs Docker
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ chaos skipped: no Docker daemon (Goal T9 needs the compose stack)"; exit 0; fi; \
	bash scripts/e2e/chaos.sh

.PHONY: demo
demo: ## Public demo: whole stack + Cloudflare tunnel, one URL for a phone — needs Docker (PUBLIC!)
	@bash scripts/demo/up.sh

.PHONY: demo-print
demo-print: ## Same as `demo` but with REAL printing (needs a working host CUPS queue)
	@PRINT=1 bash scripts/demo/up.sh

.PHONY: demo-down
demo-down: ## Tear the public demo down and verify nothing is left exposed
	@bash scripts/demo/down.sh

.PHONY: deploy-smoke
deploy-smoke: ## Deployment parity: production-profile stack driven through the HTTPS edge (Goal T12) — needs Docker
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ deploy-smoke skipped: no Docker daemon (Goal T12 needs the compose stack)"; exit 0; fi; \
	bash scripts/deploy/smoke.sh

.PHONY: load
load: ## Load/perf: submission throughput + SSE fan-out boundedness vs baseline (Goal T10) — needs Docker
	@if ! docker info >/dev/null 2>&1; then \
		echo "⚠ load skipped: no Docker daemon (Goal T10 needs the compose stack)"; exit 0; fi; \
	bash scripts/load/run.sh

.PHONY: load-selftest
load-selftest: ## Prove the load baseline detector catches a regression (no Docker needed)
	@echo "check-baseline.py must FAIL on the deliberately-regressed fixture:"; \
	if python3 scripts/load/check-baseline.py scripts/load/testdata/regressed-summary.json scripts/load/baseline.json; then \
		echo "✗ detector did NOT flag the regression"; exit 1; \
	else echo "✔ regression correctly detected (non-zero exit)"; fi

.PHONY: ci
ci: fmt-check lint test-race cover cover-portal ## Docker-independent local CI gate
	@echo "✔ CI gates passed"

.PHONY: hooks
hooks: ## Install the git pre-commit hook
	@cp scripts/pre-commit .git/hooks/pre-commit && chmod +x .git/hooks/pre-commit \
		&& echo "✔ pre-commit hook installed"
