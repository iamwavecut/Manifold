#!/bin/sh
set -eu

project=manifold-integration
port=${MANIFOLD_PORT:-18080}
# Every `compose up` below has to publish the same port, or a recreated
# Manifold moves to the default one.
export MANIFOLD_PORT="$port"
url="http://127.0.0.1:${port}"
api_key=${MANIFOLD_BOOTSTRAP_API_KEY:-replace-with-a-long-random-secret}
compose_files="docker-compose.yml:docker-compose.integration.yml"
if [ -n "${MANIFOLD_INTEGRATION_COMPOSE_OVERRIDE:-}" ]; then
	compose_files="$compose_files:$MANIFOLD_INTEGRATION_COMPOSE_OVERRIDE"
fi
compose() {
	COMPOSE_FILE=$compose_files docker compose --project-name "$project" --env-file .env.example "$@"
}
state_file=

cleanup() {
	if [ -n "$state_file" ]; then
		rm -f "$state_file"
	fi
	compose down --volumes --remove-orphans
}

on_exit() {
	status=$?
	if [ "$status" -ne 0 ]; then
		compose ps --all || true
		compose logs --no-color --tail=300 || true
	fi
	cleanup
	exit "$status"
}

run_phase() {
	MANIFOLD_URL=$url \
		MANIFOLD_API_KEY=$api_key \
		MANIFOLD_INTEGRATION_PHASE=$1 \
		MANIFOLD_INTEGRATION_STATE=$state_file \
		go test -tags=integration -count=1 -v ./test/integration
}

wait_for_manifold() {
	attempt=0
	until curl --fail --silent "$url/health" >/dev/null; do
		attempt=$((attempt + 1))
		if [ "$attempt" -ge 60 ]; then
			echo "Manifold did not become healthy" >&2
			exit 1
		fi
		sleep 1
	done
}

provider_control() {
	compose exec -T provider /usr/local/bin/provider control "$1" "$2"
}

trap on_exit EXIT INT TERM
cleanup

compose up --detach --build --wait --wait-timeout 300

curl --fail --silent --show-error "$url/openapi.yaml" >/dev/null
curl --fail --silent --show-error "$url/app/" >/dev/null

MANIFOLD_URL=$url \
MANIFOLD_API_KEY=$api_key \
	go test -tags=integration -count=1 -v ./test/integration

skill_result=$(MANIFOLD_URL=$url \
	MANIFOLD_API_KEY=$api_key \
	sh skills/manifold/scripts/manifold --json remember \
		--new \
		--reason "Runtime-free CLI integration evidence" \
		--id skill-integration-memory \
		--title "Skill integration memory" \
		--folder-path tasks/skill-integration-memory \
		--content "The bundled Manifold skill completed discovery, nested creation, and terminal job verification.")
case "$skill_result" in
	*'"status": "ready"'*) ;;
	*)
		echo "Bundled skill did not return ready canonical memory" >&2
		exit 1
		;;
esac
skill_tree=$(MANIFOLD_URL=$url \
	MANIFOLD_API_KEY=$api_key \
	sh skills/manifold/scripts/manifold --json tree --glob 'tasks/**' --types document)
case "$skill_tree" in
	*'"path": "tasks/skill-integration-memory/skill-integration-memory"'*) ;;
	*)
		echo "Bundled skill tree did not return the nested canonical path" >&2
		exit 1
		;;
esac

# A new, empty index file is seeded from the canonical database at start.
# The rest of the run uses that seeded index.
export MANIFOLD_INTEGRATION_INDEX_PATH=/var/lib/manifold/index-reseeded.db
compose up --detach --no-deps --wait manifold
wait_for_manifold
run_phase reseed_verify

# A provider that refuses extraction for lack of credit: Manifold names the
# cause and, once the provider accepts again, finishes the document by itself.
state_file=$(mktemp)
provider_control refuse 402
run_phase provider_refusal
provider_control refuse 0
run_phase provider_recovery
rm -f "$state_file"

# Manifold restarts while the provider still holds the extraction.
state_file=$(mktemp)
provider_control hold true
run_phase restart_submit
compose restart manifold
wait_for_manifold
provider_control hold false
run_phase restart_verify

# The extraction times out while the provider holds it; once the provider
# answers again, the retry converges on the same revision.
provider_control hold true
export MANIFOLD_INTEGRATION_EXTRACTION_TIMEOUT=15s
compose up --detach --no-deps --wait manifold
unset MANIFOLD_INTEGRATION_EXTRACTION_TIMEOUT
run_phase finished_retry_submit
provider_control hold false
compose up --detach --no-deps --wait manifold
wait_for_manifold
run_phase finished_retry_verify
rm -f "$state_file"

# The provider goes away: the job keeps its semantic error across a Manifold
# restart and completes once the provider is back.
state_file=$(mktemp)
compose stop provider
run_phase create_failure
compose restart manifold
wait_for_manifold
run_phase verify_failure
compose up --detach --no-deps --wait --wait-timeout 120 provider
run_phase recover_failure
rm -f "$state_file"
state_file=

curl --fail --silent --show-error \
	-H "Authorization: Bearer $api_key" \
	"$url/api/v1/documents/integration-note-renamed" >/dev/null

run_phase cleanup
