#!/usr/bin/env bash
# Runs the model switch deploy/ptah/README.md describes, end to end, on a
# throwaway stack: second-brain's own PostgreSQL image and migrations, a demo
# corpus, Ollama serving bge-m3, and the released Ptah image. It ends with the
# second-brain server answering /api/v1/search from the Ptah generations.
#
#   deploy/ptah/demo/run.sh
#
# Needs Docker with compose, Go, curl and python3. The stack publishes two
# ports on 127.0.0.1 (DEMO_PG_PORT, DEMO_OLLAMA_PORT) and is removed on exit;
# KEEP=1 leaves it running. Against a remote Docker daemon, set DEMO_BIND to
# an address that daemon's host listens on and DEMO_HOST to the address this
# machine reaches it by.
set -euo pipefail

demo_dir=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$demo_dir/../../.." && pwd)
export DEMO_PG_PORT=${DEMO_PG_PORT:-55432}
export DEMO_OLLAMA_PORT=${DEMO_OLLAMA_PORT:-55433}
export DEMO_BIND=${DEMO_BIND:-127.0.0.1}
host=${DEMO_HOST:-127.0.0.1}
server_port=${DEMO_SERVER_PORT:-18080}
db_host="postgres://brain:brain@$host:$DEMO_PG_PORT/second_brain?sslmode=disable"
db_net="postgres://brain:brain@postgres:5432/second_brain?sslmode=disable"
work=$(mktemp -d)
server_pid=

compose() { docker compose -f "$demo_dir/compose.yaml" "$@"; }
psql_db() { compose exec -T postgres psql -U brain -d second_brain -X -q -v ON_ERROR_STOP=1 "$@"; }
ptah() { compose run --rm -T ptah inference "$@" --db-url "$db_net"; }
sparsectx() { (cd "$repo" && DATABASE_URL="$db_host" go run ./cmd/sparsectx --recipe=v1-full --dry-run=false "$@" 2>&1 | tail -1); }
step() { printf '\n== %s\n' "$*"; }

cleanup() {
	status=$?
	[ -n "$server_pid" ] && kill "$server_pid" 2>/dev/null || true
	if [ "${KEEP:-}" != 1 ]; then
		compose --profile tools down -v --rmi local >/dev/null 2>&1 || true
	fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT

families=(documents summaries chunks)

step "PostgreSQL (pgvector + pg_bigm) and Ollama with bge-m3"
compose up -d --build --wait postgres ollama
compose exec -T ollama ollama pull bge-m3 >/dev/null

step "second-brain migrations, the way store.RunMigrations applies them"
for f in "$repo"/migrations/*.sql; do
	case "$(basename "$f")" in
	011_* | 015_*) { printf 'BEGIN;\nSET LOCAL app.embedding_dim = 1536;\n'; cat "$f"; printf '\nCOMMIT;\n'; } ;;
	*) cat "$f" ;;
	esac | psql_db >/dev/null 2>&1
done
echo "applied $(ls "$repo"/migrations/*.sql | wc -l | tr -d ' ') migrations"

step "demo corpus, and chunk context from cmd/sparsectx"
psql_db <"$demo_dir/seed.sql"
sparsectx

step "prepare and backfill: three generations beside the application's columns"
for f in "${families[@]}"; do
	ptah prepare --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3" | head -1
	ptah backfill --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3"
done

step "the application keeps writing: a rename, a new mail, a summary, a delete"
psql_db <"$demo_dir/writes.sql"
sparsectx --sweep

step "catch up, index, verify"
for f in "${families[@]}"; do
	ptah catchup --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3" | head -1
	ptah index --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3"
	ptah verify --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3" | head -2
done

step "cutover, each approval bound to the plan it was shown"
for f in "${families[@]}"; do
	# Without an approval the cutover is refused, and the refusal prints the
	# digest of the plan it built. That refusal is the expected answer here.
	digest=$({ ptah cutover --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3" 2>&1 || true; } |
		sed -n 's/^plan \([0-9a-f]*\)$/\1/p' | head -1)
	ptah cutover --spec "/specs/$f.bge-m3.yaml" --run-id "$f-bge-m3" \
		--approve "$digest" --approver "run.sh" | head -1
done

step "second-brain server with VECTOR_SOURCE=ptah"
(cd "$repo" && go build -o "$work/server" ./cmd/server)
(
	cd "$repo"
	exec env DATABASE_URL="$db_host" PORT="$server_port" API_KEY=demo \
		EMBEDDING_PROVIDER=local LOCAL_EMBEDDING_ENDPOINT="http://$host:$DEMO_OLLAMA_PORT" \
		LOCAL_EMBEDDING_MODEL=bge-m3 EMBEDDING_DIM=1024 VECTOR_SOURCE=ptah \
		"$work/server" >"$work/server.log" 2>&1
) &
server_pid=$!
for _ in $(seq 1 60); do
	curl -sf "http://127.0.0.1:$server_port/health" >/dev/null && break
	sleep 1
done
curl -sf "http://127.0.0.1:$server_port/health" >/dev/null || {
	tail -20 "$work/server.log"
	exit 1
}

for q in '워크숍 숙소는 어디로 잡았어?' '계약 위약금 얼마로 줄이기로 했지?' '면접관이 누구야?'; do
	printf '\nQ: %s\n' "$q"
	curl -s -G "http://127.0.0.1:$server_port/api/v1/search" -H 'Authorization: Bearer demo' \
		--data-urlencode "q=$q" --data-urlencode "limit=3" |
		python3 -c 'import json,sys
for r in json.load(sys.stdin)["results"][:3]: print("  " + r["title"])'
done
printf '\n'
grep -o '"msg":"ptah vectors: [^"]*"' "$work/server.log" | sed 's/"msg":"//; s/"$//'
