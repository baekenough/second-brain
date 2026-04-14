.PHONY: sync-env sync-env-test deploy-secret

# Sync .env to k8s secret (local)
sync-env:
	@bash scripts/sync-env.sh

# Sync + auto rollout
deploy-secret:
	@AUTO_ROLLOUT=true bash scripts/sync-env.sh

# Test the sync script
sync-env-test:
	@bash scripts/sync-env_test.sh
