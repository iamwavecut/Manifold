#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
config=$(docker compose --project-directory "$root" \
	--env-file "$root/.env.example" config --format json)

printf '%s' "$config" | jq -e '
	.networks.private.internal == true
	and ((.networks.provider.internal // false) == false)
	and (.services.openviking.networks | has("private") and has("provider"))
	and (.services.brain.networks | has("private") and has("provider"))
	and (.services.surrealdb.networks | has("private") and (has("provider") | not))
	and ((.services.openviking.ports // []) | length == 0)
	and ((.services.brain.ports // []) | length == 0)
	and ((.services.surrealdb.ports // []) | length == 0)
	and ((.services.manifold.ports // []) | length == 1)
' >/dev/null

echo "Compose network and publication boundaries are valid."
