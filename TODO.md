# second-brain TODO

## Path A — enable OpenAI embeddings (deferred)

Current state: path B (BM25 only). Embeddings disabled via empty EMBEDDING_API_KEY + empty CLIPROXY_AUTH_FILE in the k8s Secret (internal/search/embed.go:NewEmbedClient auto-disables when both are empty regardless of EMBEDDING_API_URL).

To enable later:
1. Obtain an OpenAI API key from https://platform.openai.com/account/api-keys.
2. Inject into the k8s Secret on ubuntu1:
   ```bash
   ssh ubuntu1 'sudo microk8s kubectl -n second-brain patch secret second-brain-secret --type=merge -p "{\"stringData\":{\"EMBEDDING_API_KEY\":\"sk-...\"}}"'
   ```
3. Rollout the pods to pick up the new secret:
   ```bash
   ssh ubuntu1 'sudo microk8s kubectl -n second-brain rollout restart deployment/second-brain deployment/second-brain-collector'
   ```
4. Also set the same key on laptop (`docker-compose.laptop-collector.yml`) and ubuntu2 (`/home/baekenough/second-brain-collector/docker-compose.yml`) EMBEDDING_API_KEY env.
5. Restart those compose stacks.
6. Trigger a full re-embed: reset collected_at on all filesystem docs so the next scheduler tick re-processes them:
   ```bash
   ssh ubuntu1 'sudo microk8s kubectl -n second-brain exec postgres-0 -- psql -U brain -d second_brain -c "UPDATE documents SET collected_at='\''1970-01-01'\'' WHERE source_type='\''filesystem'\'' AND embedding IS NULL;"'
   ```
7. Expect ~$1-4 OpenAI cost for initial embed of ~4k docs at text-embedding-3-small rates.

## Other future work

- **[DONE 2026-04-24] Watermark per-collector-instance**: implemented via migrations/009_collector_state.sql, store/scheduler/config/cmd code changes, and per-instance `COLLECTOR_INSTANCE` env applied to ubuntu1/laptop/ubuntu2. Each collector now tracks its own max(collected_at) watermark; cross-host scan suppression resolved.
- **[DONE 2026-04-24] Cleanup of obsolete laptop docker-compose.yml**: the root `docker-compose.yml` now carries a DEPRECATED banner (top comment + `name: second-brain-deprecated-build-only` + inline warning) and is retained ONLY for `docker compose build server collector`. Live runtime is k8s on ubuntu1 + `docker-compose.laptop-collector.yml` on laptop.
  - Also renamed orphaned k8s manifests (not referenced by `deploy/k8s/kustomization.yaml`) to `.unused` suffix to prevent accidental `kubectl apply -f`:
    - `deploy/k8s/eval-cronjob.yaml` → `eval-cronjob.yaml.unused`
    - `deploy/k8s/second-brain-pv.yaml` → `second-brain-pv.yaml.unused`
    - `deploy/k8s/second-brain-web-deployment.yaml` → `second-brain-web-deployment.yaml.unused`
    - `deploy/k8s/second-brain-web-service.yaml` → `second-brain-web-service.yaml.unused`
  - Originals preserved (renamed, not deleted) — useful as design-pattern references.
