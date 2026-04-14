.PHONY: sync-env sync-env-dry-run sync-env-test deploy-secret

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
