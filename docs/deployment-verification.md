# Production migration — September 16, 2026

Status: deployed and accepted at 2026-09-16 14:12 UTC (16:12 Europe/Warsaw).

The user explicitly authorized deployment after the local implementation
review. Target: `https://manifold.geta.moe`, Compose project `manifold` on
`geta.moe`, source directory `/home/wavecut/Manifold`.

## Source and running artifacts

The deployed source is an immutable archive of the reviewed worktree, not a
published Git commit. No commit, push, PR or release was made. The production
checkout contains that snapshot; its dirty-check guard prevents the automatic
deployment workflow from replacing it with the older remote branch.

- Source archive SHA-256: `57f23ff2fd96b5f2abb217ade3117b7015cba910baf7209e3c1a231232007db1`.
- Public build identity: `snapshot-57f23ff2fd96b5f2abb217ade3117b7015cba910baf7209e3c1a231232007db1`.
- Manifold image: `sha256:cf95148cb775a52492ffeb2689c5f0b50e1c2d13e949169955a6c5345237d4eb`.
- Brain image: `sha256:baaaded91dae32130862399cc31d4925bbb410d3dcf70410329e3c62cde562c5`.
- OpenViking image: `sha256:9906b9af67c788e7727bf6677811309d6f56a034f1c5b7a6b1db073c270bf238`.
- SurrealDB image: `sha256:6e2f7f0134c79f659704986384895226bf84452673404060949b2c67d722da3f`.

Brain v2.2.0, OpenViking v0.4.20 and SurrealDB v3.2.4 retain the existing
provider, chat model, embedding model and 1024-dimensional embedding space.
Credentials, host publication boundaries and persistent volume mounts were
checked before activation. Brain provider calls initially allowed five minutes with one SDK retry.
The final production override allows fifteen minutes per request, retaining
one SDK retry; the upstream environment validator requires a positive retry
count. The outer Manifold extraction attempt allows thirty minutes.

This report is written after the source archive was built; its later evidence
updates do not change the running artifact identity.

## Backup, restore and migration proof

Protected server directory:
`/home/wavecut/manifold-deploy/20260916-refresh-8c0ae330a31b`.
It retains the original source/configuration, exact old image identities and
four coordinated cold volume archives. Access is restricted; document content,
credentials and private graph IDs are excluded from this report.

All four services were stopped for the backup. Every archive was restored into
a separate volume and its complete file-hash manifest matched the original.
The old stack then started against those isolated copies with provider egress
blocked. SQLite integrity, 21 current documents, all 125 historical revisions
belonging to active documents, three old OpenViking snapshot samples and three
Brain source identity samples were checked. The old private graph contained
80 source documents, 26 entities and 36 facts.

The new stack preserved those canonical bytes, snapshot samples and private
record identities. Brain's migration ledger advanced from 68 to 129 entries.
These checks prove the actual production backup can boot under the old stack;
they are additional to the earlier synthetic migration rehearsal. Rollback
must restore the complete four-volume set and matching old code/configuration.

## Acceptance corrections and checks

The first real-provider fixture produced an entity, fact and relation with
canonical graph retrieval. Updating it preserved its immutable first revision.
A Manifold restart during extraction resumed the same job and Brain checkpoint
(attempt 1 to 2) and reached `ready`.

An identical-text revision exposed Brain's distinction between a serving fact
and a corroborating audit row. The sixth patch returns the resolver-selected
serving ID for Manifold candidates, preserving the audit record. Reused edge
projection now trusts the exact committed candidate and endpoint identities;
the edge's first source is not the only document allowed to confirm it.
The upstream regression failed before the patch and passed afterward; all nine
document-pipeline e2e tests, typechecking and focused lint passed.

On the corrected source, `make verify`, Linux amd64/arm64 build stages, Compose
validation and actionlint pass. The complete deterministic integration passed:
graph lifecycle including identical-text update, API/rename, dependency outage,
restart persistence, same-job recovery and cleanup. The final seven-patch
image passed the entire suite. A local Docker address-pool allocation error
was resolved with explicit task-only test subnets; production networks were
unchanged. Its temporary containers, networks and volumes were removed.

The first existing document exceeded Brain's default provider request deadline
on three internal attempts. The new timeout corrects that separate layer. Its
orphaned run is superseded through a reviewed, ETag-protected same-content
revision; other recovery work uses the existing job where resumable.

The subsequent existing-document pass exposed a separate false-success path:
the upstream 1,500-token completion budget truncated JSON, and a null extractor
result was accepted as an empty graph. Seven truncated responses were observed.
The seventh patch makes the budget configurable (16,384 by default), validates
completion and top-level arrays, and throws on failed extraction. Valid empty
responses still succeed. Fourteen regression cases failed before the patch;
all 82 extractor unit tests, typechecking and focused lint pass afterward.
Private Brain hydration limits are configured at 600 requests per minute.

All 21 existing documents were reprocessed using ETag-protected same-content
revisions. The receipt retains previous job history and binds each new job to
the final source batch. A migration-only helper pre-extracted queued revisions
through the existing synchronous Brain ingest API with bounded concurrency
(up to eight model calls), exact canonical origins and revision timestamps.
The durable worker deduplicated its normal async submission onto those source
identities and performed public projection. The helper did not write the public
graph or bypass terminal verification.

One 7,792-character source repeatedly exceeded the provider deadline. Its
interrupted internal run stayed `running` after the queue lease expired: the
queue retry skipped the still-fresh run ledger. One additional ETag-protected
same-content revision recovered that document. Its successful extraction took
321,329 ms, exceeding the old 300,000 ms deadline, and produced 172 staged
entities, 55 facts and 31 relations before resolution and public projection.
The provider and model were unchanged.

A runtime tuning attempt incorrectly set the SDK retry count to zero. Brain's
startup validation rejected it; restoring the supported value of one recovered
the service. The replacement container remained healthy with zero restarts.

## Final public acceptance

The complete final verification exited successfully against the public HTTPS
endpoint and was repeated for status through the installed native skill:

- All 21 current documents and their jobs are `ready`; no current failed,
  partial or incomplete documents and no pending jobs remain.
- Exact original content, hashes, titles, formats, folders, metadata and tags
  match for all 21 documents. All 125 original active-document historical
  revisions match; recovery added 23 same-content revisions.
- Three original OpenViking snapshot hashes and original Brain identities
  remain intact. Brain has 129 applied migrations.
- Paginated public graph reads returned 789 entities, 616 facts and 404
  relations. Graph search returned 45 canonical fact hits with current
  revision provenance; three semantic document checks and context retrieval
  passed without degraded dependencies.
- Public build identity and all four running image hashes match the source
  snapshot. A 396-second final stability interval showed no restart or start
  time changes. Brain, Manifold and SurrealDB have zero container restarts;
  OpenViking's earlier single restart did not recur.

The 52 historical failed jobs remain as audit records; they are not failures
of the current 21 document revisions. Aggregate status graph counters and
paginated visible graph results are recorded separately in the private proof.

All temporary controllers and four restore-test volumes were cleaned up.
The deployment lock was released. Original cold archives, configuration and
rollback images remain in the protected deployment directory. Evidence:
`final-verification.json`, `final-extraction-duration.json`,
`continuous-timeout.json`, `stability-baseline-final.json` and
`deployment-complete.json`. No commit, push or public release was performed.

## Remaining operational boundary

An abrupt Brain termination can leave an internal run `running` until its
separate stale-run policy applies. A queue retry alone may skip that run.
The verified recovery path is an ETag-protected same-content revision; it
preserves canonical content and historical revisions. The superseded private
run is excluded from current public provenance. This deployment does not claim
to change Brain's internal stale-run fencing protocol.
