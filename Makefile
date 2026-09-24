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

.PHONY: build-server build-collector build-mcp build-eval build-all

BIN_DIR := bin

# 로컬 빌드 산출물은 레포 루트가 아닌 bin/(gitignore 대상)에 둔다.
# 루트에서 `go build ./cmd/eval`을 실행하면 실패하는데, /eval이 추적 중인
# 픽스처 디렉터리이기 때문이다(internal/askeval/judge_remote.go가 그 정확한
# 경로에 의존하므로 이름을 바꾸지 말 것); bin/에 빌드하면 이 이름 충돌을
# 피할 수 있다(#274).
build-server:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/server ./cmd/server

build-collector:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/collector ./cmd/collector

build-mcp:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/mcp ./cmd/mcp

build-eval:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/eval ./cmd/eval

build-all: build-server build-collector build-mcp build-eval
