.PHONY: sync-env sync-env-dry-run sync-env-test deploy-secret tidy check install-hooks

tidy:
	go mod tidy -tags setup

# Dry-run sync (safe — prints rendered secret without applying)
sync-env-dry-run:
	@DRY_RUN=true bash scripts/sync-env.sh

# Actual sync (writes to cluster)
sync-env:
	@bash scripts/sync-env.sh

# Sync + rollout
deploy-secret:
	@AUTO_ROLLOUT=true bash scripts/sync-env.sh

# Test (now safe — never touches real cluster)
sync-env-test:
	@bash scripts/sync-env_test.sh

.PHONY: check install-hooks

# Run all CI hygiene + go checks locally (parity with CI yaml-lint + go-lint-build-test jobs)
check:
	@bash scripts/ci-checks.sh
	@go vet ./...
	@go build ./...
	@go test -race -count=1 ./...

# One-time: tell git to use repo-tracked hooks
install-hooks:
	@git config core.hooksPath .githooks
	@echo "Git hooks installed. Pre-push now runs scripts/ci-checks.sh + go checks."
	@echo "Bypass with: git push --no-verify  (emergencies only)"
