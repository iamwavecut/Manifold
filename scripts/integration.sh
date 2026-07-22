#!/bin/sh
set -eu

project=manifold-integration
port=${MANIFOLD_PORT:-18080}
url="http://127.0.0.1:${port}"
api_key=${MANIFOLD_BOOTSTRAP_API_KEY:-replace-with-a-long-random-secret}
compose="docker compose --project-name ${project} -f docker-compose.yml -f docker-compose.integration.yml --env-file .env.example"
state_file=

has_degraded_dependencies() {
	printf '%s\n' "$1" | grep -Eq '"degraded_dependencies"[[:space:]]*:[[:space:]]*\[[[:space:]]*"'
}

cleanup() {
	if [ -n "$state_file" ]; then
		rm -f "$state_file"
	fi
	$compose down --volumes --remove-orphans
}

on_exit() {
	status=$?
	if [ "$status" -ne 0 ]; then
		$compose ps --all || true
		$compose logs --no-color --tail=300 || true
	fi
	cleanup
	exit "$status"
}

trap on_exit EXIT INT TERM
cleanup

MANIFOLD_PORT=$port $compose up --detach --build --wait --wait-timeout 300

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

# Keep Brain running while SurrealDB restarts. Its scoped pool must restore
# authentication rather than remaining connected as an anonymous session.
$compose restart surrealdb
attempt=0
while :; do
	recovery_search=$(MANIFOLD_URL=$url \
		MANIFOLD_API_KEY=$api_key \
		sh skills/manifold/scripts/manifold --json search "scoped session recovery" 2>/dev/null || true)
	case "$recovery_search" in
		*'"items"'*)
			if ! has_degraded_dependencies "$recovery_search"; then
				break
			fi
			;;
	esac
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 60 ]; then
		echo "Brain did not recover its scoped SurrealDB session" >&2
		exit 1
	fi
	sleep 1
done
recovery_context=$(MANIFOLD_URL=$url \
	MANIFOLD_API_KEY=$api_key \
	sh skills/manifold/scripts/manifold --json context "scoped session recovery")
if has_degraded_dependencies "$recovery_context"; then
	echo "Context remained degraded after SurrealDB restart" >&2
	exit 1
fi
case "$recovery_context" in
	*'"context"'*) ;;
	*)
		echo "Context recovery response was incomplete" >&2
		exit 1
		;;
esac

$compose stop brain
state_file=$(mktemp)
MANIFOLD_URL=$url \
	MANIFOLD_API_KEY=$api_key \
	MANIFOLD_INTEGRATION_PHASE=create_failure \
	MANIFOLD_INTEGRATION_STATE=$state_file \
	go test -tags=integration -count=1 -v ./test/integration

$compose restart manifold

attempt=0
until curl --fail --silent --show-error "$url/health" >/dev/null; do
	attempt=$((attempt + 1))
	if [ "$attempt" -ge 60 ]; then
		$compose logs --no-color manifold
		exit 1
	fi
	sleep 1
done

MANIFOLD_URL=$url \
	MANIFOLD_API_KEY=$api_key \
	MANIFOLD_INTEGRATION_PHASE=verify_failure \
	MANIFOLD_INTEGRATION_STATE=$state_file \
	go test -tags=integration -count=1 -v ./test/integration

curl --fail --silent --show-error \
	-H "Authorization: Bearer $api_key" \
	"$url/api/v1/documents/integration-note-renamed" >/dev/null
