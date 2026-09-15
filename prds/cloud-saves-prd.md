# OpenPak cloud saves PRD (platform-neutral store, per-platform adapters, your own storage)

> **2026-09-14 audit:** The status below is historical. See the [cloud-saves completion PRD](cloud-saves-completion-prd.md) for verified implementation, correctness gaps, Reference nx-scsi review and release gates. “Built and deployed” does not mean all acceptance journeys have passed.

Status: v2, 2026-09-10; supersedes the 2026-09-07 draft. **Built and deployed the same day**
(M1, M2, M5 and the console surface; M0 awaits a console run). Two things changed in the plan. The Switch
save-data-cloud protocol is now written down from evidence
([`saves/docs/provenance.md`](../docs/provenance.md)), including the one part nobody has
reproduced. And storage is two-tier: OpenPak keeps a small allowance per account, and a
player who wants more connects their own cloud storage, which then holds their saves.

Design rules:

- A platform-neutral service is not platform-neutral saves. Every blob is keyed by account,
  platform and title; the service never overwrites or merges across platforms.
- **OpenPak is the index, never the warehouse.** Metadata lives in OpenPak's Postgres for every
  save; the bytes live wherever the account's storage setting says. A console never learns
  which, and the protocol adapters never care.
- Nothing newer is overwritten silently (Switch expansion PRD, section 10.6).

## 1. Product objective

Progress follows the account. An emulator pulls the newest save on launch and pushes on
exit; a console's Save Data Cloud screen lists, backs up and restores; the website shows every
platform's saves under one account. Storage costs OpenPak next to nothing, because the
allowance is small and anyone with more saves than that brings a bucket of their own.

## 2. Relationship to existing plans

- The Switch adapter keeps owning the scsi hostnames. With `SAVES_URL` set it forwards every
  request on them to this service under `/scsi`, before reading the body, with the console's
  proven account attached (CS-1, CS-2). Without it the empty-cloud answers of before remain.
- Ports: 20020 HTTP, 20021 gRPC, 20022 metrics (`ports.md`). No console port of its own.
- Federation: none. Saves are not latency-bound and are not served from region nodes.
- Companion app: read-only saves list, already stubbed (`app/lib/saves_page.dart`).

## 3. Evidence and current baseline

The whole console protocol, with sources, is in `saves/docs/provenance.md`. The short form:

- Four hosts: the REST API (`storage.hac.lp1.scsi`), blob download (`scsi-download.lp1.scsi`),
  blob upload (Nintendo uses a **presigned S3 PUT on amazonaws.com**), and per-title policy.
- A backup is an *archive* with *component files* (index 0 = metadata, 4096 and up = save
  partitions). Upload is a dance: `start_upload` (optionally against the previous archive),
  `component_files/create` or `update` for a `put_url`, raw PUT, per-component
  `finish_upload` with size and digest, archive `finish_upload` with a MAC. Download is
  `start_download` for `get_url`s, raw GETs, `finish_download`.
- Listing is `GET save_data_archives?datatype=0`, which is what our console sends today.
- **The console encrypts every component.** The key comes from a 512-byte
  `key_seed_package` the server returns for a 16-byte challenge. Nobody outside Nintendo
  has generated one; Reference replays a captured one. Whether a server can mint an
  acceptable package is unknown and is the first thing this PRD finds out.
- Emulator (evidence): Citron pulls `GET /api/save/<title>` and pushes `POST /api/save/<title>`
  with the raw body; newest wins, no versions.

## 4. Users and journeys

1. Emulator player: launch, newest save restored; quit, save uploaded; a second machine
   receives it.
2. Console player: Save Data Cloud lists backups per title; back up; restore on another
   console linked to the same account.
3. Conflict: two devices with divergent saves. The newer stays current, the older is kept as
   a version, the website shows both and lets the player pick.
4. **Bring your own storage:** the player pastes an S3-compatible endpoint, bucket and key
   pair on the website, presses *Test*, and from then on every new save version lands in
   their bucket. Their allowance no longer applies. They can move existing versions across.
5. Out of allowance: an upload is refused with a message that names the limit and links to
   the storage settings. Nothing already stored is touched.
6. Storage unreachable: the player's bucket refuses us. Uploads fail loudly, the website
   shows the storage as *not answering*, and no save is quietly written elsewhere.
7. Disconnecting storage: the player removes their bucket. Versions stored there stay
   listed as *unavailable* until they either reconnect it or delete them; nothing is copied
   back without asking, because it may not fit.
8. Restore after an OpenPak database restore: no player loses a newer save.

## 5. Scope and non-goals

In: versioned blob store per account, platform and title; allowance and quotas; listing,
restore, pin, delete; integrity (SHA-256, size); retention; the emulator API; the Switch
save-data-cloud handlers as an adapter package; storage backends: OpenPak-hosted and
S3-compatible bring-your-own; website pages for saves and storage settings; app read-only.

Planned, later milestones in this PRD: WebDAV and OAuth consumer clouds (Google Drive,
Dropbox, OneDrive) as further backends behind the same interface.

Out: game-specific save formats, cross-platform conversion or merging (the hook stays
defined, CS-10), sharing saves between accounts, Wii U/3DS/PlayStation/Xbox adapters until
those platform layers exist.

## 6. Independent implementation rules (apply verbatim)

- Every earlier network repository is source-available; its noncompete clause forbids
  using that code in a competing product. earlier code, routes, schemas and prose are
  **evidence of observable behaviour only**, never implementation input. Do not copy,
  translate, or structurally mirror them. Cite them as evidence, then design from scratch.
- Go 1.26, module `github.com/openpak/<repo>`, AGPL-3.0-only, `LICENSE` verbatim from
  gnu.org. Server-rendered templates and minimal browser JavaScript for web surfaces.
  Native mobile code is the recorded non-Go exception.
- Talk to the account core **only** through its versioned contracts (gRPC: Identity, Links,
  Social, Sessions, Invitations, Events; HTTP `/v1/*`). Never read its database. If a
  contract is missing, write the proposal in this PRD's "Contract requests" section and
  raise it as an `account` issue; do not fork the core.
- Do not touch `Openpak/nx-baas` (the Switch adapter, owned by the T4/T5 agent) or the
  console-facing hostnames it serves. Console-side needs go into "Contract requests".
- No secrets, tokens, saves, or copyrighted game material in any repository. `example.env` only.
- Definition of done for every milestone: `go vet ./...` and `go test -race ./...` green in
  CI, a runbook in `README.md`, and the acceptance journey below reproduced by the owner.

## 7. Functional requirements

Store and rules:

- CS-01 Model, Postgres: `save` (account, platform, title, created) has `version`s (number,
  size, SHA-256, device, created, pinned, conflict flag, **storage location**: which backend
  and which key). A Switch archive is one version; its component files are that version's
  parts, each with its own size and digest.
- CS-02 Write rule: a PUT carries the base version it was loaded from; if the server has a
  newer version, the PUT is stored as a new version flagged `conflict`, never discarded.
  On the Switch, `previous_save_data_archive_id` is the base version.
- CS-03 Integrity: size and digest checked when a part finishes; a mismatch fails the
  upload and the version is never listed. Downloads carry the digest.
- CS-04 Retention: last 10 versions plus pinned, per save; a scheduled sweep deletes
  blobs from whichever backend holds them; an audit row per deletion.
- CS-05 Platform isolation: every read and write carries a platform; no API returns or
  accepts a blob without one; CS-02 is per platform.
- CS-10 Title-owned conversion hook: interface defined, no implementation.

Storage:

- CS-11 **Backend interface**, one per account: `Put(key, size, reader)`, `Get(key)`,
  `Delete(key)`, `Stat(key)`, and optionally `PresignPut(key, ttl)` / `PresignGet(key, ttl)`.
  Implementations in these milestones: `local` (a directory, OpenPak's allowance) and
  `s3` (any S3-compatible endpoint: AWS, Cloudflare R2, Backblaze B2, Wasabi, MinIO, Garage).
  Later: `webdav`, and OAuth consumer clouds.
- CS-12 **Allowance:** `SAVES_ALLOWANCE_BYTES` (default 256 MiB) of OpenPak-hosted storage
  per account, counted over live versions. An upload that would exceed it is refused with
  `quota_exceeded`, the limit, current usage and a link to storage settings. An account with
  its own storage connected has no allowance; its bucket is its limit.
- CS-13 **Bring your own storage:** website settings page: endpoint, region, bucket, optional
  prefix, access key, secret. *Test* writes and deletes a marker object and reports the
  result before anything is saved. Credentials are encrypted at rest with
  `SAVES_CREDENTIAL_KEY` (AES-256-GCM, key per deployment, rotation documented); the secret
  is never shown again after entry and never logged.
- CS-14 **Direct transfer where the backend allows it.** When the account's backend presigns,
  the console's `put_url` and `get_url` are presigned URLs on the player's own bucket, and
  no save byte passes through OpenPak. Nintendo already sends the console to `amazonaws.com`
  for the PUT, so the shape is one the console accepts (provenance). For `local`, and for
  any backend without presigning, the URLs point at the blob hosts the adapter forwards
  (CS-2) and the service streams the body.
- CS-15 **Failure semantics:** a backend that does not answer fails the operation with a
  clear error; the service never falls back to another backend on its own. The website
  shows the storage as *not answering* with the last error and time.
- CS-16 **Moving house:** *Move my saves to my storage* copies every live version from
  `local` to the connected backend, verifies digests, then frees the allowance. Disconnecting
  leaves versions in place, listed as *unavailable*, with a *copy back* action that respects
  the allowance.
- CS-17 Usage is shown per account and per title, both surfaces, from the metadata, so it
  costs no backend call.

Adapters:

- CS-06 Emulator API (website tokens): list saves, get newest, get version, put with base
  version, pin. Streaming bodies, not JSON.
- CS-07 Switch save-data cloud: the request set in `saves/docs/provenance.md`, served under
  `/scsi` and reached through the adapter, which validates the BAAS bearer and passes the
  account. Policy: `ALL_OK` for every title unless `SAVES_SWITCH_TITLES` narrows it.
- CS-08 Restore drill documented: database and object store restore, then a returning-player
  check per Switch PRD section 10.6.
- CS-09 Website: saves per platform and title with versions, pin, restore-as-current,
  download, delete; storage settings (CS-13); usage (CS-17). App: read-only list.

## 8. Architecture and ownership

Repo `saves`, module `github.com/openpak/saves`:

```
cmd/saves              the service: gRPC Saves on 20021, HTTP emulator API on 20020, /healthz
store/                 model, rules (CS-02..05), Postgres schema, retention sweep
blob/                  Backend interface; blob/local, blob/s3 (minio-go), later blob/webdav
internal/switchscsi/   the console handlers under /scsi; nx-baas forwards the scsi hosts here (CS-1)
internal/api/          the internal HTTP API the website (and through it the emulators and the app) uses
```

Postgres, database `saves`: `saves`, `versions`, `parts`, `storage_settings` (account,
backend kind, encrypted config, last test result), `audit`. The service keeps no blob bytes
in the database ever.

How a Switch upload flows, for an account with its own bucket:

```
console ──443──▶ nx-baas (storage.hac.lp1.scsi) ──HTTP /scsi + X-OpenPak-Account──▶ saves
                 start_upload → new version, parts pending
                 component update → PresignPut on the player's bucket
console ──443──▶ r2.cloudflarestorage.com  (PUT, straight into the bucket)
                 component finish_upload → Stat, size and digest checked
                 archive finish_upload → version live, older ones retained per CS-04
```

For the OpenPak allowance the PUT goes to `scsi-upload.lp1.scsi` instead, which the adapter
forwards to the service (CS-2), and the body lands in `SAVES_LOCAL_DIR`.

Why S3-compatible first and the others later: it is the only backend where the console can
talk to the storage directly, it is one client library already in the servers (minio-go), and
the free tiers of R2 and B2 are larger than any allowance OpenPak will offer. WebDAV covers
Nextcloud and the Hetzner boxes with a few hundred lines of stdlib HTTP. OAuth consumer clouds
need OpenPak-held client ids and a consent flow on the website; when they come, one library
that already speaks all of them (rclone's backend packages, MIT) is preferred over three SDKs.

## 9. Contract requests

Done 2026-09-10, a forward rather than a package because it decouples the two deploys:

- CS-1 The adapter forwards every `*.scsi.srv.nintendo.net` request to `SAVES_URL/scsi`
  with `X-OpenPak-Account`, `X-OpenPak-NSA` and `X-OpenPak-Device` once the bearer is proven,
  and answers policy through the service (`policy_type`, as the evidence says).
- CS-2 The forward happens before the adapter reads the body, so the blob hosts stream.
- CS-3 Website: `/account/saves` and the storage settings consume the service's internal HTTP
  API; the settings form tests before storing and never displays a stored secret.
- CS-4 App and emulators: `GET /api/v1/me/saves`, `GET`/`PUT /api/v1/me/saves/{platform}/{title}`.
- CS-5 Account core: nothing. Allowance and storage settings are the saves service's own.

## 10. Milestones and acceptance

- **CS-M0 The key seed question (blocking).** Serve the console's `generate_key_seed_package`
  with a server-minted package and read its reaction; try a replayed genuine package for
  contrast; establish whether the console verifies a signature on the package. Output: a
  written finding in `saves/docs/provenance.md` and a go/no-go for the console surface. A
  no-go means the console surface waits for a console-side key patch, as News did, and the
  emulator and website surfaces proceed alone.
- **CS-M1 Store and emulator round trip on the allowance.** `store`, `blob/local`, the
  emulator API, CS-02 and CS-12 exercised by tests. Gate: a second Citron profile receives
  the first profile's save; an upload past the allowance is refused with the documented error.
- **CS-M2 Your own storage.** `blob/s3`, storage settings page with *Test*, encrypted
  credentials, *Move my saves*, CS-15 and CS-16. Gate: the owner's account on a free R2 or
  B2 bucket completes M1's journey with the service holding no blob bytes.
- **CS-M3 Console listing.** Save Data Cloud lists versions from the store (empty is fine).
  Gate: no error on the console.
- **CS-M4 Console backup and restore** (only after an M0 go). Gate: a backup from console A
  restores on console B; on an S3 account, the console talks to the bucket directly.
- **CS-M5 Website saves pages and app list.** Gate: every platform's versions under one
  account, pin, restore, download; the app shows the same list.
- **CS-M6 Retention and restore drill.** Gate: CS-04 sweep proven against both backends;
  drill run once and written up.

## 11. Risks

- M0 may find that the key seed package is signed by a firmware key. Then the console
  surface needs a patch and the schedule for M3/M4 moves; everything else is unaffected.
- A player's bucket is a player's responsibility: a public bucket exposes their saves.
  *Test* checks that the bucket is not publicly readable and warns; it cannot enforce it.
- Presigned URLs on non-AWS S3 endpoints vary in signature quirks (R2 and B2 both need
  path-style and region `auto`/`us-west-004`); the settings page offers presets for the
  common providers rather than a free-text region.
- Save blobs can be hundreds of MB: every path streams; the adapter's body buffering has
  to be lifted for the blob hosts (CS-2).
- NSO membership gating: the console shows cloud saves only with an active membership; the
  adapter already reports one.

## 12. Evidence references

`saves/docs/provenance.md` (2026-09-10): the console's request set from the adapter's debug
log, Reference `nx-scsi` handlers and replay test (paths, fields, statuses, blob URL shape,
policy reply), Kinnay wiki *OLSC Servers* and *Server List* (v2 API, field sizes, the S3
upload host, `dtoken`), Citron client paths.
