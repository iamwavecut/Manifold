#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
config=$(docker compose --project-directory "$root" \
	--env-file "$root/.env.example" config --format json)

# One container holds the canonical database and the derived index; only the
# API port is published, and the runtime image has no shell for a probe.
printf '%s' "$config" | jq -e '
	(.services | keys) == ["manifold"]
	and ((.services.manifold.ports // []) | length == 1)
	and (.services.manifold.ports[0].target == 8080)
	and (.services.manifold.healthcheck.test == ["CMD", "/usr/local/bin/manifold", "healthcheck"])
	and ((.services.manifold.mem_limit // 0) > 0)
	and ((.services.manifold.volumes // []) | map(select(.type == "bind")) | length == 0)
	and (.services.manifold.environment.MANIFOLD_INDEX_DB_PATH | startswith("/var/lib/manifold/"))
' >/dev/null

echo "Compose service and publication boundaries are valid."
