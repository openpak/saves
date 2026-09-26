# Cloud saves completion PRD

Status: implementation plan, audited 2026-09-14. This document supersedes the completion claims and remaining-work plan in [the original cloud-saves PRD](cloud-saves-prd.md). It preserves the platform-neutral service and bring-your-own-storage direction.

## Outcome and release decision

A player can back up progress, retrieve it on a compatible second device, resolve divergent progress without losing either copy, and recover after storage or service failure. Connecting, replacing, or disconnecting storage must never silently change where an existing backup is read from or deleted.

**Current assessment: substantial implementation exists; cloud saves are not complete.** The backend, website, mobile settings and emulator client are present. Native Switch backup/restore remains experimental. Source inspection found correctness and recovery gaps that block a reliable release even for emulator saves.

Ship two explicitly named capabilities:

- **Cloud saves for emulators:** release after milestones C1–C4 and C6 below pass. Publish a tested client/OS/format matrix.
- **Native Switch Save Data Cloud:** release only after those shared foundations and C5 pass, including a real two-console restore. A passing HTTP replay is insufficient.

No percentage-complete estimate: implemented routes, successful tests and verified user journeys are different evidence.

## Audit scope and confidence

Reviewed `saves`, website saves handlers/templates/client, the Flutter saves page, shared `openpak-client` API/sync implementation, selected emulator integration call sites, `nx-baas` forwarding, existing PRD and provenance, and public legacy/nx-scsi source.

The local `saves` HEAD was `989d4b77a2e90340d10c0d6d4b96b18d0a15a623`. `internal/api/flow_test.go` already had uncommitted changes; the audit preserved them and tested the working tree. This workspace is a collection of repositories; its root and `prds` are not Git repositories.

This is a source and local-test audit, not production certification. The old README reports deployment on September 10; production image identity, account journeys, real provider credentials, console logs and hardware round trips were not verified in this review. Emulator forks were sampled for integration, not all built or exercised.

Validation performed:

| Check | Result | What it establishes |
| --- | --- | --- |
| `go vet ./...` in `saves` | Passed | Static checks on current service code |
| `go test -race -v ./...` with a fresh PostgreSQL 18 database | Passed: TestFlow, TestWiiUFlow, TestWebDAVBackend, TestGoogleDriveBackend | Existing synthetic service flows and mocked provider behavior; no hardware encryption or real cloud proof |
| Same service tests without database environment | Database flows skip | A plain green run can omit the principal integration tests |
| Website `go test ./internal/pages ./internal/api -run 'Saves|Save|Storage' -count=1` | Passed page test; API package reported no matching tests | Saves page behavior against a stub service |

The disposable audit database/container was removed after verification. No production save bytes or external storage were modified.

## What exists today

| Surface | Implemented | Qualification / remaining work |
| --- | --- | --- |
| Save index | PostgreSQL saves, numbered versions, component parts, account/platform/title keys, pin, conflict flag, deletion and audit | Current-version selection, concurrent writes and immutable revision semantics need repair |
| Hosted storage | Local filesystem backend, default 256 MiB allowance, streaming server uploads and SHA-256 calculation | Quota checks are not reservations; interrupted and concurrent transfers need stronger accounting |
| User storage | S3-compatible backend and presigned URLs; WebDAV; Google Drive and OneDrive OAuth implementations; encrypted settings; test-before-connect | This is ahead of the old PRD. Real-provider certification, stable storage identities, token rotation and safe migration remain |
| Website | Save/version list, pin, restore, delete, single-part download, storage connection and move controls | Multi-part console export returns 409; recovery and explicit conflict/current semantics incomplete |
| Companion app | Saves/version list plus S3/WebDAV/OAuth connection, move and disconnect UI | More than the old read-only stub claim; game-save restore/delete remains website work |
| Emulator API | Authenticated website list and GET/PUT endpoints, version/base/device headers, shared C++ client | Shared pull skips existing local content; base state is memory-only and indexed by title; a conflict upload is treated as success |
| Emulator integration | Citron launch/exit calls, Eden manual restore path, Dolphin shared API calls found | A generic Wii U API test does not prove a Cemu client or real Wii U console journey. Every supported fork needs its own matrix row |
| Switch adapter | `nx-baas` strips supplied identity headers, derives account from bearer, forwards SCSI before body buffering; signed blob URLs in saves | Hardware auth, routing, TLS, upload and restore must be verified together |
| Switch service | Archive/component routes, listing, policy, upload/download, timeout routes, delta component descriptions | Key package is random bytes; digest/MAC verification, delta preservation and state enforcement incomplete |
| Operations | Docker/release workflow, health endpoint, five-minute expired-upload sweep | Release workflow inspected has no test job; no gRPC or metrics listener in current main despite old PRD architecture |

## Reference nx-scsi review

“Reference nx-scsi” was interpreted as legacy/nx-scsi (archived source record). GitHub API HEAD on review: `0f2eaf71fbae2a27d8c23d78d23690235d7be91f`, committed 2026-07-22. Findings below concern that public revision, not an unseen deployment.

It provides useful observable protocol evidence: archive/component requests, delta-upload sequencing, policy and blob transfer shapes. It remains a narrow Switch service with local persistence. OpenPak already has broader product surfaces and storage choices. Source: main.go (archived source record).

The key-package response reuses a captured package; challenge binding and console acceptance are explicitly unresolved in its source. It does not supply a demonstrated independent key-package generator. Do not treat it as the answer to OpenPak’s cryptographic blocker. Source: keyseed.go (archived source record).

Its replay test sends synthetic save bytes and checks HTTP responses during a delta upload. It does not establish decryption, reboot persistence, or restore on another console. It was read, not executed in this audit. Source: replay_test.go (archived source record).

Component completion records client-supplied size/digest; archive completion discards the MAC body. These observations are protocol experiments, not a production integrity model to adopt. Source: handlers.go (archived source record).

Follow [OpenPak’s clean-room policy](https://github.com/openpak/docs/blob/main/clean-room-policy.md): retain facts and citations only; do not copy source, captured cryptographic material, implementation structure or prose. This review adds no earlier implementation to OpenPak.

## Verified gaps and their consequences

These are source findings unless explicitly described as a passing test. They are not claims of observed production data loss.

| ID / priority | Finding and evidence | Consequence |
| --- | --- | --- |
| G01 / P0 | `NewVersion` flags a stale base, but `Newest` returns the highest live number, including conflicts; `Finish` does not recheck the base. [store.go](../internal/store/store.go) | A stale device’s upload can become the next device’s default download. Overlapping uploads can miss conflict detection |
| G02 / P0 | `NewVersion` allocates `max(number)+1` without serializing per save; `Restore` renumbers an existing version. [store.go](../internal/store/store.go) | Concurrent allocation can fail; restore changes the meaning of previously issued base numbers |
| G03 / P0 | Versions record backend kind only; `BackendNamed` opens the account’s current settings for every non-local version. [service.go](../internal/service/service.go) | Switching bucket/provider makes historical reads/deletes target the new configuration, losing the original location association |
| G04 / P0 | `putURL/getURL` fall back to signed service URLs, but `putBlob/getBlob` always use `Svc.Local`. [scsi.go](../internal/switchscsi/scsi.go) | Native transfers on non-presigning backends use the wrong storage; S3 signing failures can silently redirect writes locally |
| G05 / P0 | `generate_key_seed_package` returns fresh random 512-byte data. Component finish checks size, accepts the supplied digest; archive MAC is audited, not verified. [scsi.go](../internal/switchscsi/scsi.go) | HTTP success is not proof of valid encrypted or restorable console backups |
| G06 / P0 | Part update/blob PUT do not enforce an uploading parent; Finish can finalize again and drops every non-fixed part. Delta setup creates fresh pending parts without preserving old bytes. [scsi.go](../internal/switchscsi/scsi.go), [store.go](../internal/store/store.go) | A still-valid upload URL can mutate completed data; incomplete/delta archives may become live without required content |
| G07 / P0 | Quota uses current live usage and caller size, with no atomic reservation. Switch blob writes are not bounded by allowance; emulator uploads have no expiry. [service.go](../internal/service/service.go), [api.go](../internal/api/api.go) | Concurrent/unknown-size or interrupted uploads can consume unaccounted disk and evade intended limits |
| G08 / P0 | Shared ZIP extraction joins untrusted entry names directly to destination and writes in place, with incomplete error propagation. [save_sync.cpp](../../openpak-client/qt/openpak/qt/save_sync.cpp) | Malformed archives can escape the restore directory or leave partially replaced progress; manual restore lacks transactional recovery |
| G09 / P1 | Pull skips nonempty local directories; client stores bases only in a title-keyed memory map and ignores conflict in upload success. [save_sync.cpp](../../openpak-client/qt/openpak/qt/save_sync.cpp), [api.cpp](../../openpak-client/client/openpak/api.cpp) | Existing devices do not implement promised automatic reconciliation; restart/account/format context must be handled explicitly |
| G10 / P1 | `Move` checks destination size, not digest, changes backend then deletes source; process-local goroutine and state check are not a durable job. No copy-back route. [service.go](../internal/service/service.go), [api.go](../internal/api/api.go) | Corruption and crashes during migration have insufficient recovery; duplicate jobs/settings changes can race |
| G11 / P1 | Failed deletion leaves tombstones, but `Sweep` enumerates only expired uploads. Finish deletes pending part rows before blob cleanup. [service.go](../internal/service/service.go), [store.go](../internal/store/store.go) | Claimed deletion retries do not occur; orphaned objects and unaccounted physical usage can accumulate |
| G12 / P1 | API downloads reject multiple parts; platform is arbitrary text and shared client defaults to `citron`. [api.go](../internal/api/api.go), [api.cpp](../../openpak-client/client/openpak/api.cpp) | Console backups cannot be exported whole; format compatibility is not represented or enforced |
| G13 / P1 | OAuth refresh caches access token but does not persist a returned replacement refresh token. [oauth.go](../internal/blob/oauth.go) | Providers that rotate refresh credentials can stop working after subsequent refresh/restart |
| G14 / P1 | Current main exposes static health only; release workflow builds/pushes without vet/race/integration steps. [main.go](../cmd/saves/main.go), [release.yml](../.github/workflows/release.yml) | Healthy HTTP/build success does not establish storage readiness or release correctness |

## Product requirements

### C1 — Preserve progress and stable identity

Owner: saves; website and client owners consume the contract. Addresses G01–G03, G06, G07, G11.

- Give each save an explicit current revision. Revisions have immutable IDs, format metadata and immutable committed parts; version numbers never change meaning.
- Commit using an atomic comparison with the revision the client actually loaded. A stale/unknown base against existing progress creates a retained conflict, leaves current unchanged, and returns an explicit conflict result. Concurrent first uploads must follow the same rule. Never use client clocks to select a winner.
- Restore creates a new current revision referencing verified immutable content, with provenance back to the selected revision; preserve the replaced current and source revision. Use reference-aware blob deletion if content is shared.
- Bind every object to an immutable storage-location ID, including endpoint/bucket/prefix/provider account. Credential rotation may update credentials for that location; choosing a different destination creates a different location. Disconnect retains location metadata and marks content unavailable.
- Reserve hosted bytes atomically across in-flight uploads; reconcile actual bytes on commit/abort. Reject invalid lengths, bound streaming bodies and temporary storage, expire all uploads, and release reservations after recovery. Keep physical pending-deletion usage visible to operators.
- Enforce legal state transitions and idempotent completion/retries. Completed objects must remain immutable even while previously issued upload URLs exist; stage uploads separately or freeze a verified object version before publishing it.
- Retain ten recent ordinary revisions plus pinned revisions. Never automatically prune current or unresolved conflicts. Use durable deletion jobs with retry, audit and orphan reconciliation; release references only after deletion succeeds.

Acceptance: two devices based on revision 1 upload divergent saves concurrently; one becomes current, the other is visible as a conflict; a third downloads current. Restore does not invalidate historical revision references. Twenty concurrent uploads cannot exceed quota or collide on numbering. Restart during every commit/deletion boundary loses no committed content.

### C2 — Reliable storage and movement

Owner: saves; settings UX owned by website/app. Addresses G03, G04, G10, G13.

- Proxy bytes through the backend belonging to the part’s storage location. Presigning failure returns a clear retryable error unless an explicitly supported proxy path to that same location succeeds. Never silently switch storage.
- Migration is a durable, resumable, per-account job with immutable source/destination, per-object progress, retries and cancellation. Verify cryptographic digest and size before changing references; delete the source only after a committed handover and recovery window.
- Implement copy-back subject to hosted quota. Keep disconnected versions listed with reconnect/migrate/delete actions and explain which storage holds them.
- Persist rotated OAuth refresh credentials securely and atomically. Exercise revoked consent, expiry, provider quota, throttling, partial multipart upload and unavailable storage. Record operational failures separately from the last connection test.
- Validate destination URLs, redirect behavior and outbound access policy so user-configured storage cannot access infrastructure metadata/internal services; allow intentional private storage only through explicit administrator configuration. Do not reuse stored credentials when the destination identity changes.
- Certify local, one S3-compatible target, WebDAV, Google Drive and OneDrive separately; hide or label uncertified providers. Dropbox remains out of scope.

Acceptance: replace bucket A with B, including two buckets on the same provider; old revisions still resolve to A. Interrupt/restart migration and corrupt equal-length destination bytes: source remains recoverable. Native proxy tests reach WebDAV/OAuth fakes without touching local bytes. Real-provider runs demonstrate upload/download/delete and credential refresh across restart.

### C3 — Safe emulator sync and format contracts

Owner: openpak-client and each emulator owner. Addresses G08, G09, G12.

- Persist sync state by server/account/platform/title/local profile/save format: last applied revision, content digest, pending upload and error state. Set base only after verified local application or acknowledged commit.
- Before play, compare local digest, last applied revision and remote current. Remote-only change downloads safely; local-only change preserves local progress for upload; both changed produce a visible choice and retain both. Offline play remains available, with durable retry after reconnect.
- Snapshot after the game closes its save handles; upload retries survive application exit. Show synced, local changes, uploading, offline/retry, conflict, quota and storage-unavailable states. Do not gate cloud backups solely on multiplayer-compatible title lists.
- Verify downloaded integrity; reject absolute paths, parent traversal, links escaping the destination, malformed/truncated entries and decompression bombs. Extract into a bounded staging directory, validate, then atomically swap with a recoverable local backup. Cover Windows and libarchive paths.
- Define canonical platform/title normalization, format/version and emulator/profile metadata. Keep legacy namespaces readable; migrate through aliases only after proving format compatibility. Never automatically feed opaque native Switch components into an emulator ZIP restore.

Acceptance: machine B already has an older local save and safely receives A’s newer progress; divergent offline progress remains available; account/profile switches do not reuse bases. A crash during extraction leaves the original playable. A client/OS matrix records the actual binary revision and round-trip fixture for every advertised emulator.

### C4 — Complete web and mobile management

Owner: website/app. Addresses G01, G09, G10, G12.

- Identify current separately from history/conflicts. Show device, format, timestamp, size, storage location and availability. Offer explicit keep-current, promote-conflict and download-both choices.
- Restore and delete show exactly which revision is affected; mutations remain account-scoped and protected by existing authentication/CSRF boundaries.
- Export every part of a console backup with a manifest, checksums and required metadata, clearly labeled as an opaque native archive. Single-part download remains supported; do not present an incomplete export as a full backup.
- Show durable migration progress, cancellation/retry, copy-back quota preview and reconnect guidance. The mobile app keeps version management read-only but retains its existing storage controls and links to web recovery.

Acceptance: two accounts cannot access each other’s lists, revision actions, storage settings or transfer URLs. Website and app show identical current/conflict/availability data. Complete multi-part export verifies against the manifest; every error leaves a clear retry/recovery route.

### C5 — Prove native Switch compatibility

Owner: saves protocol work; nx-baas integration owner; console/hardware owner. Addresses G04–G06, G12.

- Gate the current random-key experiment behind an explicit development capability. Production must not advertise successful native backup support until encrypted restore is proven.
- Record firmware, title, device pairing, request sequence and console result codes for key-package experiments. Separate challenge binding, account/device/series binding and persistence across restart. A 512-byte length alone proves neither an RSA signature nor its validation rules.
- Establish an independently supportable key lifecycle. If hardware requires a console patch, specify its compatibility, rollout and recovery before proceeding. Persist whatever archive/series metadata the validated flow needs; no captured third-party key material in repositories.
- Derive digest/MAC semantics from documented or measured behavior; distinguish transport SHA-256 from protocol cryptographic fields. Verify integrity where possible and document opaque fields honestly. Do not describe storing a submitted digest as checking it.
- Prove full and delta uploads, unchanged component reuse, required metadata, archive finalization, cancellation, retries, timeout, title policy and version/format isolation. Do not permit unrelated account/platform/title revisions as delta bases.
- Verify DNS, TLS and signed URL behavior for each supported provider on hardware. Nintendo’s use of S3 does not prove arbitrary provider certificate/redirect compatibility.

Acceptance: console A uploads, service restarts, console B on the same OpenPak account downloads and the game loads the expected progress. Repeat a delta backup, retry after network loss, and restore an older revision. Repeat on hosted storage and a supported user bucket. Include mismatched digest, expired URL, wrong-account and mutation-after-commit cases. Publish a sanitized evidence report; listing alone does not pass.

### C6 — Operations, rollout and recovery

Owner: saves deployment owner, with website/client release owners. Addresses G07, G10, G11, G14.

- Add mandatory CI vet, race tests and fresh-PostgreSQL integration runs; fail the job if database tests skip. Add backend contract tests including S3 and OneDrive behavior plus focused failure/concurrency/state tests for the findings above.
- Introduce versioned schema migrations and a documented rollback/forward recovery path. Backfill storage IDs from known configurations; mark ambiguous historical locations unresolved rather than guessing. Preserve existing objects and revision identities.
- Add readiness checks and metrics for upload/restore success, conflicts, bytes/reservations, provider failures, oldest unfinished job, deletion backlog and orphan storage. Define alerts and an operator runbook. Keep tokens, signed URLs and save contents out of logs.
- Retain HTTP on 20020; gRPC is not a release requirement without a consumer. Implement observability on the allocated metrics port or explicitly update the port contract; do not claim reserved ports are running services.
- Back up database, hosted objects, storage mappings and credential-encryption keys coherently. Perform a restore drill including a client whose known revision is ahead of restored database state; refuse silent rollback and preserve its local save.
- Roll out schema/read compatibility first, then guarded writes to test accounts, then supported emulator/provider cohorts; release native Switch independently after C5. Rollback disables affected writes while preserving reads and recovery.

Acceptance: CI results and real client/provider/hardware evidence are linked to the exact release. A restart resumes jobs. Database/object recovery is demonstrated with hashes and recovered revisions, not only a runbook. Adopt provisional recovery targets of RPO ≤24 hours and RTO ≤4 hours for hosted data; confirm them through the drill before publishing them. External-provider availability is reported separately.

## Delivery order and contracts

| Milestone | Dependencies | Exit artifact |
| --- | --- | --- |
| C1 correctness and metadata | First; preserve existing data during migration | New revision/storage contracts, migrations, concurrency and recovery tests |
| C2 storage | C1 location identity and job model | Backend matrix, resumable migration/copy-back, provider evidence |
| C3 client sync | C1 revision contract | Safe restore implementation, persistent sync state, client matrix |
| C4 management | C1/C2 contracts; C3 state vocabulary | Conflict/recovery/export journeys |
| C5 native Switch | Research can begin immediately; release depends on C1/C2 | Key-lifecycle finding and two-console evidence |
| C6 operations | CI work starts immediately; final gate follows C1–C4 | Release evidence, restore drill and rollout record |

Request these cross-repository contracts before implementation: immutable revision/current/conflict response semantics and backward-compatible base headers; storage-location IDs and durable job/status endpoints; format manifest/export contract; client capability advertisement; adapter protocol capability/error mapping. The account core remains the identity authority; no direct account database access. Console-facing forwarding belongs to the nx-baas owner.

This PRD does not prescribe a calendar estimate before key-lifecycle research and revision/storage migrations are scoped. Each milestone needs one accountable owner. Existing implementation should be repaired incrementally; completion means the acceptance evidence exists, not merely that handlers have been added.

## Explicit exclusions and open decisions

Cross-platform conversion, game-save merging, cross-account sharing, Dropbox, and new native Wii U/3DS/PlayStation/Xbox protocols are excluded. Existing generic platform storage remains supported. Native Switch and emulator saves share an account/index, not an assumed interchangeable byte format.

Decisions to record during implementation: supported console firmware and title matrix; independently workable key lifecycle or required patch; legacy namespace compatibility; provider-specific integrity mechanisms for direct transfer; maximum compressed/uncompressed save sizes and transfer limits; protection/expiry policy for unresolved conflicts; verified production recovery targets. None may be replaced with a success claim based on HTTP status alone.
