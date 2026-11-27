#!/bin/sh
# Print the config catalog of a Brain builder image (the `builder` stage of
# deploy/brain/Dockerfile) in the JSON form scripts/brain-flags.py reads.
set -eu
if [ "$#" -ne 1 ]; then
	echo "usage: $0 <brain-builder-image>" >&2
	exit 2
fi
docker run --rm --entrypoint node "$1" -e '
const { CONFIG_CATALOG } = require("/app/dist/admin/config-catalog.data.js");
const entries = CONFIG_CATALOG.map((e) => [
  e.key,
  { default: e.defaultValue ?? null, boolean: Boolean(e.isBooleanFlag) },
]).sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
console.log(JSON.stringify(Object.fromEntries(entries), null, 1));
'
