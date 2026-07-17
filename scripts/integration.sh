#!/bin/sh
set -eu

project=manifold-integration
port=${MANIFOLD_PORT:-18080}
url="http://127.0.0.1:${port}"
api_key=${MANIFOLD_BOOTSTRAP_API_KEY:-replace-with-a-long-random-secret}
compose="docker compose --project-name ${project} -f docker-compose.yml -f docker-compose.integration.yml --env-file .env.example"
state_file=

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
