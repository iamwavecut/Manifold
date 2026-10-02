# Migration verification

These checks used synthetic data in isolated local Compose projects on 2026-09-16. They did not connect to production or use a hosted model provider.

## Brain and SurrealDB

The baseline used the Manifold repository at `527273a001e3189f4730b73f072f7cfa4cc3b72c` and its Brain Dockerfile, which builds upstream Brain [`v0.8.1`](https://github.com/inite-ai/inite-brain-service/tree/v0.8.1) at `557babacfad9ffd856107a49d0cfc20d27016afe` with the two baseline patches. The built Brain image ID was `sha256:9f948a95c625c8dff025974346c0b197c2702434aa7afff300411918bd760f8a`; the database was `surrealdb/surrealdb:v3.1.5` (image ID `sha256:3ad3fe6160ada702242506d7134744e065052e380b716a00cd98893b61d05bc8`). The synthetic document was ingested synchronously through Brain using the local deterministic provider (`sha256:42fa699dfde5d98027daa1a8f2d5bcaa4d2f6ed5b8762a0e9122dc95b9362311`) and the text `Mira Chen works at Meridian Labs.`

The baseline contained one committed source document, two entities, and one active `works_at` fact. The document's content hash was `fe7a04e2a850772b6abce0d7d3488c3455e907d9bca3b621672fb184bce644bd`; this matches an independent SHA-256 of the normalized synthetic text. Its canonical origin was `synthetic://migration-check/MIRA-001`. The fact referenced the same `source_document` record and stored `source.originKey = doc:<contentHash>`.

With Brain v2.2.0 at upstream commit `b19b209fec92069d91df22af61db18b0a810ce82` and SurrealDB `v3.2.4`, the same named SurrealDB and Brain-cache volumes started successfully in the isolated `manifold-migration-check` project (networks `192.168.247.0/24` and `192.168.249.0/24`). The database image ID was `sha256:16ad1cd7d6b4fce53fd979202a63b2b42f45b9a6340d5c01922220225f665d17`. The provider endpoint was local (`http://provider:8081/v1`) with deterministic chat and embedding models. A document GET triggered the tenant migration; `schema_migrations` contains 129 rows, 61 more than the old baseline (`0069`–`0129`). The API read and direct aggregate queries preserved the same document ID, hash, source URI, title, committed status, entity/fact counts, fact ID, and canonical document/origin pointers.

The first forward check used v2.2.0 image `sha256:3dd276cd5db957e73b7135e01e78204040d5017d079c377c65e97e9e7e969b2c`, built with the OpenAI base-URL, scoped-session-recovery, and document-origin-identity patches. After adding `failed-run-retry.patch`, the four-patch image (`sha256:2146dd280208b6aaa8b81e793e9d83140e342f2bfb452eb91395f21c0833eaa6`) was recreated against the same migrated volume. It reached ready state and the document, source pointer, fact/entity counts, and 129-row migration ledger remained unchanged.

The subsequent retry version/content guards and worker startup-order patch do not add schema migrations. The final five-patch image is checked by the fresh-stack integration suite; the old-volume migration rehearsal above used the exact four-patch image recorded here.

A cold backup was taken while Brain, SurrealDB, and the provider were stopped. The SurrealDB volume archive is 285,184 bytes with SHA-256 `35b2964b3ce8aac1b15b59c2aa38b8f6a6ddca44f381eebb0bd670cfcfb1ea57`; the Brain-cache archive is 1,536 bytes with SHA-256 `674a76809a8a7e80b896a36ca8d1ff072e612c345cec264e2017933f588d0dfd`. Both archives were restored into separate replacement volumes, then read with Brain v0.8.1 and SurrealDB v3.1.5. The old stack reached ready state and returned the same document and fact pointer; SQL confirmed one document, two entities, one active fact, and 68 schema migrations. The migrated volume was never downgraded in place.

The migration check used only the one synthetic document in a separate tenant. It demonstrates the tested version path and data pointers for this fixture; it does not establish behavior for arbitrary production data or application traffic.

## OpenViking

In separate project `manifold-openviking-migration` on subnet `192.168.246.0/24` with test port `19331`, OpenViking `v0.4.10` (`sha256:0cf1c6fc75214a2b093b3a8e5df6c8dc2e6b1802c60dee25b5285ac28dbdc988`) stored the same synthetic sentence with a newline and returned a snapshot. The snapshot was read back byte-for-byte; its content hash was `4cf135e21e39670d5a17a122ebc28818424ca96fed81c2479748143d7acfbd49`.

After stopping OpenViking, the service was recreated on the same data volume with `v0.4.20` (`sha256:b9827753d035f4157b5b318865907fd18f738924ad6398c86feb1182f209825b`). The original snapshot ID and bytes remained readable. A replacement write created a different snapshot ID, and the original retained its prior content. The verified markers were `BASELINE_SNAPSHOT_READBACK_OK`, `UPGRADE_PRESERVES_OLD_OID_AND_CONTENT_OK`, and `NEW_WRITE_AND_IMMUTABLE_HISTORY_OK`. A full OpenViking backup restore was not tested.
