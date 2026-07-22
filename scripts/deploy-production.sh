#!/usr/bin/env bash
set -Eeuo pipefail

target_sha=${1:-}
repo_dir=${MANIFOLD_REPO_DIR:-/home/wavecut/Manifold}
lock_file=${MANIFOLD_DEPLOY_LOCK:-/tmp/manifold-production-deploy.lock}

has_degraded_dependencies() {
	printf '%s\n' "$1" | grep -Eq '"degraded_dependencies"[[:space:]]*:[[:space:]]*\[[[:space:]]*"'
}

read_env_value() {
	local name=$1 line value
	line=$(grep -E "^${name}=" .env | tail -n 1 || true)
	value=${line#*=}
	case "$value" in
		\"*\"|\'*\') value=${value:1:${#value}-2} ;;
	esac
	printf '%s' "$value"
}

if [[ ! $target_sha =~ ^[0-9a-f]{40}$ ]]; then
	echo "usage: deploy-production.sh <full-commit-sha>" >&2
	exit 2
fi

if ! mkdir "$lock_file" 2>/dev/null; then
	echo "another Manifold deployment is already running" >&2
	exit 1
fi

release_lock() {
	rmdir "$lock_file" 2>/dev/null || true
}
trap release_lock EXIT

cd "$repo_dir"
if [[ $(git branch --show-current) != main ]]; then
	echo "production checkout must be on main" >&2
	exit 1
fi
if [[ -n $(git status --porcelain) ]]; then
	echo "production checkout is not clean" >&2
	exit 1
fi
env_mode=$(stat -c %a .env 2>/dev/null || stat -f %Lp .env 2>/dev/null || true)
if [[ ! -f .env || $env_mode != 600 ]]; then
	echo "production .env must exist with mode 600" >&2
	exit 1
fi
docker compose version >/dev/null
public_url=$(read_env_value MANIFOLD_PUBLIC_URL)
api_key=$(read_env_value MANIFOLD_BOOTSTRAP_API_KEY)
manifold_port=$(read_env_value MANIFOLD_PORT)
: "${public_url:?MANIFOLD_PUBLIC_URL is required}"
: "${api_key:?MANIFOLD_BOOTSTRAP_API_KEY is required}"
health_url=http://127.0.0.1:${manifold_port:-8080}

previous_sha=$(git rev-parse HEAD)
deployed=0

rollback() {
	local status=$?
	trap - EXIT
	trap release_lock EXIT
	if (( status == 0 || deployed == 0 )); then
		release_lock
		trap - EXIT
		exit "$status"
	fi
	echo "deployment failed; rolling back to $previous_sha" >&2
	git reset --hard "$previous_sha" >/dev/null
	previous_version=$(git show "${previous_sha}:VERSION" 2>/dev/null || printf 'dev')
	export MANIFOLD_BUILD_VERSION=$previous_version
	export MANIFOLD_BUILD_COMMIT=$previous_sha
	export MANIFOLD_VERSION=$previous_sha
	docker compose --env-file .env up --detach --build --wait --wait-timeout 300
	curl --fail --silent --show-error "$health_url/health" >/dev/null
	echo "rollback restored $previous_sha" >&2
	release_lock
	trap - EXIT
	exit "$status"
}
trap rollback EXIT

git fetch --no-tags origin "$target_sha"
git fetch --no-tags origin main
git cat-file -e "${target_sha}^{commit}"
if ! git merge-base --is-ancestor "$target_sha" origin/main; then
	echo "target commit is not reachable from origin/main" >&2
	exit 1
fi
git merge --ff-only "$target_sha"
deployed=1

release_version=$(tr -d '[:space:]' < VERSION)
export MANIFOLD_BUILD_VERSION=$release_version
export MANIFOLD_BUILD_COMMIT=$target_sha
docker compose --env-file .env config --quiet
docker compose --env-file .env up --detach --build --wait --wait-timeout 300

skill=skills/manifold/scripts/manifold

curl --fail --silent --show-error "$public_url/health" >/dev/null
curl --fail --silent --show-error "$public_url/ready" >/dev/null
meta=$(curl --fail --silent --show-error "$public_url/api/v1/meta")
case "$meta" in
	*'"version":"'"$release_version"'"'*'"commit":"'"$target_sha"'"'*'"protocol_revision":2'*) ;;
	*)
		echo "public protocol metadata does not match the deployed source" >&2
		exit 1
		;;
esac

status=$(MANIFOLD_URL=$public_url MANIFOLD_API_KEY=$api_key sh "$skill" --json status)
case "$status" in
	*'"state": "ready"'*) ;;
	*)
		echo "authenticated status is not ready" >&2
		exit 1
		;;
esac

search=$(MANIFOLD_URL=$public_url MANIFOLD_API_KEY=$api_key sh "$skill" --json search \
	--scope-glob '**' --type document "deployment verification")
if has_degraded_dependencies "$search"; then
	echo "semantic search is degraded" >&2
	exit 1
fi
case "$search" in
	*'"items"'*) ;;
	*)
		echo "semantic search returned an incomplete response" >&2
		exit 1
		;;
esac

context=$(MANIFOLD_URL=$public_url MANIFOLD_API_KEY=$api_key sh "$skill" --json context \
	--scope-glob '**' --type document "deployment verification")
if has_degraded_dependencies "$context"; then
	echo "context retrieval is degraded" >&2
	exit 1
fi
case "$context" in
	*'"context"'*) ;;
	*)
		echo "context retrieval returned an incomplete response" >&2
		exit 1
		;;
esac

MANIFOLD_URL=$public_url MANIFOLD_API_KEY=$api_key sh "$skill" --json tree \
	--glob '**' --types document >/dev/null

running_commit=$(docker compose --env-file .env exec -T manifold manifold version)
case "$running_commit" in
	*"$release_version"*"$target_sha"*) ;;
	*)
		echo "running binary identity does not match the target commit" >&2
		exit 1
		;;
esac

deployed=0
echo "deployed Manifold $release_version at $target_sha"
