#!/bin/sh
# Apply Manifold's Brain patches to a checkout of the pinned upstream commit,
# in the order that deploy/brain/series states. The image build, CI, and local
# verification all use this script.
set -eu
if [ "$#" -ne 1 ]; then
	echo "usage: $0 <brain-checkout>" >&2
	exit 2
fi
patches=$(CDPATH= cd -- "$(dirname -- "$0")/../deploy/brain" && pwd)
while IFS= read -r name; do
	[ -n "$name" ] || continue
	if ! git -C "$1" apply --check "$patches/$name.patch" || ! git -C "$1" apply "$patches/$name.patch"; then
		echo "brain-apply-patches: $name.patch does not apply" >&2
		exit 1
	fi
done <"$patches/series"
