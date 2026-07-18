#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

HOME=$tmp npx --yes "skills@${SKILLS_CLI_VERSION:-1.5.19}" add "$root" \
	--skill manifold \
	--global \
	--copy

installed="$tmp/.agents/skills/manifold"
test -f "$installed/SKILL.md"
test -f "$installed/bin/linux-amd64/manifold"
test -f "$installed/bin/linux-arm64/manifold"
test -f "$installed/bin/darwin-amd64/manifold"
test -f "$installed/bin/darwin-arm64/manifold"
sh "$installed/scripts/manifold" --version >/dev/null

echo "Standard skills CLI installation is valid."
