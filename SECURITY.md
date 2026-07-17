# Security policy

## Supported versions

Security fixes are applied to the latest `main` revision until the first tagged release. After releases begin, the latest minor release is supported.

## Reporting

Do not open a public issue for a suspected vulnerability. Use GitHub's private vulnerability reporting for `iamwavecut/Manifold`. Include the affected revision, impact, minimal reproduction, and whether secrets or private knowledge may have been exposed.

Do not include real API keys, session cookies, CSRF tokens, provider payloads, database files, or private document content.

## Security model

- Only Manifold is published by the default Compose stack.
- API keys are bearer secrets stored as Argon2id hashes.
- Browser sessions are HttpOnly, SameSite=Strict, time-limited, and paired with CSRF tokens.
- Capabilities are checked per operation.
- Idempotency records are scoped to the authenticated API-key record.
- Upstream responses and identifiers remain internal.
- Error responses are allowlisted semantic fields and never raw exceptions.

Operators must rotate every placeholder in `.env.example`, use TLS at the ingress, restrict data-volume access, and back up secrets separately from data.
