#!/bin/sh
set -eu

: "${BRAIN_API_KEY:?set BRAIN_API_KEY before running this script}"

if command -v sha256sum >/dev/null 2>&1; then
  hash=$(printf '%s' "$BRAIN_API_KEY" | sha256sum | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  hash=$(printf '%s' "$BRAIN_API_KEY" | shasum -a 256 | awk '{print $1}')
elif command -v openssl >/dev/null 2>&1; then
  hash=$(printf '%s' "$BRAIN_API_KEY" | openssl dgst -sha256 -r | awk '{print $1}')
else
  echo "A SHA-256 utility is required: sha256sum, shasum, or openssl." >&2
  exit 1
fi

printf 'BRAIN_API_KEYS_JSON=[{"keyHash":"sha256:%s","companyId":"manifold","scopes":["brain:read","brain:write"]}]\n' "$hash"
