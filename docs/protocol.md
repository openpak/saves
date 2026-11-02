# Protocol: saves

## Who calls it

Who calls it:

- **website** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): `/account/saves`, over `/internal/accounts/{account}/...`.
  The emulator save routes (`PUT`/`GET .../saves/{platform}/{title}`) answer 410 Gone: the
  cloud is console archives made by a real Switch or Harbor, nothing else. What the old API
  stored is marked `retired` (hidden, kept) at start; `app purge-retired` deletes it, bytes and rows.
- **nx-baas** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): forwards every `*.scsi.srv.nintendo.net`
  request here under `/scsi`, with the console's proven account in `X-OpenPak-Account`. The
  console must resolve those names to OpenPak: the Switch Setup app (`switch-patcher`) redirects
  `*.scsi.srv.nintendo.net` since 2026-09-10.

## Managing the cloud

ACNH island recovery uses the console API on both Harbor and a real Switch. `GET
/scsi/api/console/v3/permissions` grants the authenticated account access to its newest
complete island archive (`datatype=3`) per title, including an archive uploaded by another
device. Recovery joins those grants with the archive list, then downloads metadata and
chunks through the existing archive and signed component routes. Ownership is checked
again on every archive and component request. `device_id=me` scopes a device's backup cache;
recovery omits that filter. These recovery changes ship with the compressed-byte quota fix
in the single `ghcr.io/openpak/saves:0.4.5` release image (also tagged `latest`).

The account owner sees and deletes console backups on the website (`/account/saves`) and, later,
in the app. The website decides who the account is and calls these internal routes
(`X-Internal-Key`); the app reaches them through the website's authenticated API
(`/api/v1/me/saves...`, see the website README).

| Route | What |
| --- | --- |
| `GET /internal/accounts/{account}/saves` | every title's live backups: `id`, `number`, `size`, `saved_at`, `created_at`, `console` (`Harbor` or `Switch`, never a device id), `conflict`, `pinned`, `parts`; plus `usage` (`allowance_used`, `allowance`, `total`) |
| `DELETE /internal/accounts/{account}/versions/{id}` | delete one backup (204) |
| `DELETE /internal/accounts/{account}/saves/{platform}/{title}` | delete every backup of a title, pinned too (200 `{"deleted":n}`, 404 when none) |
| `DELETE /internal/accounts/{account}/saves` | account erasure (the website finaliser, after the deletion grace period): every backup, the storage credential and every row (200 `{"titles","versions"}`; 503 when bytes on OpenPak storage could not be deleted, retry) |

A deleted backup leaves the console's list at once and its bytes are removed from the backend.

## Service shape

Cloud saves for OpenPak. Platform-neutral service on port 20020 ([ports](https://docs.openpak.org/contributors/ports/)): the index of every save under an account, a small OpenPak-hosted allowance per account, and the player's own S3-compatible storage for everything beyond it. The Switch surface answers the console's request set; `generate_key_seed_package` builds the real, signed KeySeedPackageV0 when `SAVES_KSP_SIGNING_KEY_FILE` and `SAVES_KSP_PACKAGE_KEY_FILE` name key files, and answers 503 without them.

## What runs

| Piece | Where |
| --- | --- |
| `cmd/saves` | the service: `/internal/*` for the website, `/scsi/*` for the Switch adapter, `/healthz` |
| `internal/store` | Postgres: saves, versions, parts, storage settings, audit; retention (last 10 + pinned) |
| `internal/blob` | backends: `local` (the allowance, a directory) and `s3` (the player's bucket, presigned URLs); sealed credentials |
| `internal/switchscsi` | the console's archives and component files over the same index |
| `internal/api` | the internal HTTP API |

## Retention and quota

Retention (alpha phase): each save keeps the newest **two** live backups of each kind (a kind is the console's
datatype, the origin console/emulator, and whether the digest is one the console accepts), plus any pinned
backup and any unresolved conflict. It is applied when an upload completes and by the periodic sweep for saves
nobody uploads to again (50 saves per pass).

The quota is `SAVES_ALLOWANCE_BYTES` per account (default 1 GiB during the alpha phase; counted over live and
uploading backups on the OpenPak-hosted allowance). It counts compressed component bytes,
not the expanded save-image `data_size`; ACNH island backups can therefore upload even when
their expanded image exceeds the remaining allowance. Each blob PUT holds the account's
quota lock through the write and answers 403 if the component does not fit. A retry replaces
its component's charge, and an oversized chunked request preserves the previous blob. The
console's own "cloud full" error code is not known; a
player who connects their own storage is not limited by it. Which client uploaded a backup is
read from `X-OpenPak-Client` (`harbor/...` means Harbor, anything else a Switch).

# Console wire reference

Facts below about the console and the wire come from these sources, tagged in the tables:
**N** earlier research notes on the save-backup wire, **K** Kinnay's NintendoClients wiki
(*OLSC Servers*, *Server List*), **C** our own console through the adapter's debug log
(2026-09-07), **F** the console's own firmware 22.5.0 `olsc` module (offsets in brackets are
string-table offsets). F outranks the rest. Per the
[clean-room policy](https://docs.openpak.org/contributors/clean-room/), third-party
implementations are read only for facts about the console and the wire.

## Current backup versus history (23.0.1, 2026-10-11)

Our own firmware inspection shows that the native download lookup (`olsc` 0x1e67c0) calls
the archive-list helper (0x1e61e0) with capacity two. Upload paths (0x210ae0, 0x212ed0) use
the same bound. The list sink (0x1e7630 / 0x1e76e0) returns 0x1900b3 (module 179,
description 3200) on a third record. Live Switch retries for titles with three retained revisions stopped after an HTTP 200 list,
without requesting an archive, key-seed package or blob.

Since v0.4.4, the console list returns only the newest complete live archive per title and
datatype. This is OpenPak's current-backup policy, based on the client bound; it does not
establish Nintendo's retention policy. The website still lists every retained revision,
and archive-id routes still serve them. No backup is deleted to meet the client bound.

## ACNH island recovery (23.0.1, 2026-10-12)

Firmware research establishes the application restore contract. `olsc:u` command 25 takes a transfer-memory size and user ID, fetches
`/permissions`, then joins grants with fixed `datatype=3` archives for the caller's title.
Each permission requires `id`, `save_data_archive_id`, `nsa_id`, `application_id`, and
`created_at_as_unixtime`; a missing key returns 2179-3320. The list capacity is the memory
size divided by 0x438; public records are 0x410 bytes (archive ID, finished time, 0x400-byte
custom description). Command 26 takes the user ID and archive ID, checks the launch
version, registers a device-save download, and sets extra-data Restore flag 0x10 after import.

OpenPak's recovery policy grants the authenticated owner access on any of their devices.
Grants expose only the newest complete island revision per title, matching the current-backup
list: native OLSC bounds the permission list before joining it, so advertising retained
history could overflow the caller's transfer-memory capacity. Account ownership is checked
independently on all archive and component routes. `device_id=me` selects the caller's
device for the local backup cache; the recovery list intentionally omits this restriction.
These server changes ship in v0.4.5. Database tests prove the HTTP policy and
cross-account rejection; Harbor's synthetic transfer checks prove listing, stream recovery,
version refusal, and cancellation before import. An actual ACNH restore on a real Switch
or Android device has not been verified by these tests.

## Hostnames

| Host | Role | Source |
| --- | --- | --- |
| `storage.hac.lp1.scsi.srv.nintendo.net` | the REST API, `/api/console/v3/...` | C, N |
| `scsi-download.lp1.scsi.srv.nintendo.net` | where `get_url` points; the console GETs blobs here | N, K |
| `scsi-upload-lp1.s3.us-west-2.amazonaws.com` | where Nintendo's `put_url` points: a plain S3 presigned PUT on a public host | K |
| `scsi-upload.lp1.scsi.srv.nintendo.net` | a substitute name for the S3 host; the console followed it | N |
| `policy.hac.lp1.scsi.srv.nintendo.net` | per-title policy, `/api/console/v2/application_policy/<title>/<version>?dtoken=` | N, K |
| `migration.lp1.scsi.srv.nintendo.net`, `*.sata.srv.nintendo.net` | account migration and save repair; not this service | K |

Kinnay documents an `/api/nx/v2/` prefix on `storage.lp1.scsi`; the console we have calls
`/api/console/v3/` on `storage.hac.lp1.scsi`. Same resources, newer firmware. **Unexplained:**
whether v2 and v3 differ in more than the prefix; Kinnay's v2 `start_upload` carries
`has_thumbnail`, the v3 body seen by N does not.

The console reaches the S3 host on its own certificate store: Nintendo sends it to
`amazonaws.com`, so a `put_url` on a host we do not serve is a shape the console already accepts.

## The REST API (`storage.hac.lp1.scsi`, `/api/console/v3`)

Bearer is the BAAS access token; `sub` names the network service account. Every reply is
JSON and wraps its object: `{"save_data_archive": {...}}`, `{"component_file": {...}}`,
`{"save_data_archives": [...]}`.

| Method and path | Request | Reply | Source |
| --- | --- | --- | --- |
| `POST network_service_accounts/<nsa>/notification_tokens` | `{"notification_token": <36 hex>}` | the same token back, 200 | C, N, K |
| `GET save_data_archives?datatype=0[&application_id=<title>]` | — | `save_data_archives`: fixed archives only, **without** `component_files` | C, N |
| `GET save_data_archives/<id>` | — | the archive; while `uploading`, each pending component carries `put_url` and the archive a `timeout_at_as_unixtime` | N |
| `POST save_data_archives/start_upload` | metadata (below) | 201, a new archive in `uploading` with one `meta` component in `hand_over` | N |
| `POST save_data_archives/<prev>/start_upload` | metadata | 201, a new archive whose components mirror `<prev>`'s, all `hand_over`; `previous_save_data_archive_id` = `<prev>` | N |
| `POST save_data_archives/<id>/component_files/create` | `{"index", "datatype"}` | 201, the component with `put_url` | N |
| `POST save_data_archives/<id>/extend_upload_timeout` | — | the archive, timeout pushed out | N, K |
| `POST save_data_archives/<id>/finish_upload` | `{"encoded_mac": <16 bytes b64url>}` | the archive, `status: fixed`, `finished_at_as_unixtime` set, never-uploaded components dropped | N, K |
| `POST save_data_archives/<id>/generate_key_seed_package` | `{"encoded_challenge": <16 bytes b64url>}` | `{"encoded_key_seed_package": <512 bytes b64url>}` | N, K |
| `POST save_data_archives/<id>/start_download` | — | the archive with `get_url` on every fixed component and a timeout | N |
| `POST save_data_archives/<id>/finish_download` | — | the archive | N |
| `POST component_files/<id>/signed_uri` | — | the component with `get_url` | N |
| `POST component_files/<id>/update` | — | the component, `status: uploading`, with `put_url`, size and digest cleared | N |
| `POST component_files/<id>/finish_upload` | `{"archive_size", "encoded_archive_digest"}` | the component, `status: fixed` | N |

N answers the same resource set under both the `v2` and the `v3` prefix, and hands out
`timeout_at_as_unixtime` of now+3600 on `GET save_data_archives/<id>` and on `start_download`,
now+600 on `extend_upload_timeout` (N). It returns no structured error body at all: a bare 404
for an unknown path, archive or component, and a 500 with plain text when a blob write fails
(N). **Unexplained:** what the console shows for each, and whether it needs a body.

**Corrected 2026-09-16:** an earlier note here said there is no delete route, on the evidence
that N never covered one. F has it — `save_data_archives/%llu/delete?force=true` [1468615]
— and the adapter now answers it. Absence from another server was never evidence of
absence from the console.

### The whole request set the console can build (F)

Every URL `olsc` formats, in the two forms it uses: `%s/api/%s/v%d/...`, where the API segment
and the version are runtime values (ours sends `console` and `3`), and `%s/api/nx/v%u/...`
with the segment fixed. Same resources either way, which is why the adapter takes `console/v3`,
`console/v2` and `nx/v2` on one dispatcher.

| Path | Ours | Offset |
| --- | --- | --- |
| `network_service_accounts/%016llx/notification_tokens` | yes | 1458794 |
| `save_data_archives?datatype=%hhu[&application_id=%016llx]` | yes | 1460216, 1463856 |
| `save_data_archives/%llu` | yes | 1456337 |
| `save_data_archives/start_upload[?force=true]` | yes (`force` ignored) | 1466565, 1460080 |
| `save_data_archives/%llu/start_upload` | yes | 1464042 |
| `save_data_archives/%llu/component_files/create` | yes | 1457180 |
| `save_data_archives/%llu/extend_upload_timeout` | yes | 1466611 |
| `save_data_archives/%llu/finish_upload` | yes | 1471075 |
| `save_data_archives/%llu/start_download`, `/finish_download` | yes | 1475914, 1463926 |
| `save_data_archives/%llu/generate_key_seed_package` | gated (M0) | 1471607 |
| `save_data_archives/%llu/delete?force=true` | **yes, since 2026-09-16** | 1468615 |
| `component_files/%llu/signed_uri`, `/update`, `/finish_upload` | yes | 1469557, 1479976, 1462612 |
| `application_policy/%016llx/%05u?dtoken=` | yes | 1470372 |
| `permissions` | yes (v0.4.5, 2026-10-12) | 1470346 |
| `save_data_sets`, `?update=true`, `?survey_request_id=%llu&repair_status=%s` | **no** | 1474085, 1460691, 1464913 |
| `save_data_sets/generate_key_package` | **no** | 1456214 |
| `customer_operations`, `?survey_request_id=%llu`, `/update_action_completed` | **no** | 1472350, 1469294, 1471465 |

The `save_data_sets` and `customer_operations` families are save-data **repair** — the survey
flow behind `SaveDataTransferManagerForSaveDataRepair` and the `migration`/`sata` hosts, not
the backup this service implements. Note `save_data_sets/generate_key_package`: a *key
package*, not a *key seed package*, and a different endpoint from the backup one.

Bodies and fields F formats, beyond what N showed: `{"encoded_challenge": "%s"}` [1471579],
`{"encoded_key_seed": "%s","encoded_mac": "%s"}` [1463995], `{"encoded_mac": "%s"}` [1479872],
`{"encoded_sek":"%s","encoded_mac":"%s","mac_key_generation":%llu}` [1469395],
`{"permissions": ["read"]}` [1480044] (the body of a `signed_uri` request),
`{"save_data_archive_id":%llu,"nsa_id":"%016llx","save_data_type":"%s"}` [1463752]. Replies are
read by path: `$.encoded_key_seed_package` [1456375], `$.format_version`, `$.component_file`,
`$.revision`, `$.nsa_id`. The console also logs `x-nintendo-akamai-reference-id` [1479894].
**Unexplained:** what `save_data_type` and `$.revision` carry, and
where `encoded_key_seed`/`encoded_sek` are sent — none of those are on a backup path we have
watched.

`start_upload` metadata, as sent by a console in N: `datatype` (0), `data_size`,
`saved_at_as_unixtime`, `launch_required_version`, `series_id`, `encoded_digest`, `desc`
(null), `auto_backup`, `acd_index`. `application_id` was **absent** on a delta upload and
taken from the previous archive. C saw `datatype=0` on the list; N also knows `3`.
**Unexplained:** what `datatype` 3 is, and `acd_index`.

## The model

An **archive** is one backup of one title for one account:

`id`, `nsa_id`, `application_id`, `device_id`, `device_serial`, `data_size`, `desc`,
`series_id`, `datatype`, `status` (`fixed` | `uploading`), `auto_backup`, `num_of_partitions`,
`launch_required_version`, `previous_save_data_archive_id`, `encoded_digest`,
`saved_at_as_unixtime`, `finished_at_as_unixtime`, `timeout_at_as_unixtime`,
`platform_key_generation`, `component_files`.

`num_of_partitions` counts the `save` components, not all components (N, after getting it
wrong first).

A **component file** is one blob of an archive:

`id`, `index` (0 = meta, 4096 and up = save partitions), `datatype` (`meta` | `save`),
`status` (`fixed` | `hand_over` | `uploading`), `archive_size`, `encoded_archive_digest`,
and transiently `get_url` / `put_url`.

Blob URLs seen working: `https://<host>/<nsa>/<title>/<component id>_<index>.bin?Expires=..`
(N), served for GET on the download host and PUT on the upload host, with no bearer. The
console PUTs the body raw and GETs it raw, `application/octet-stream`. That path shape is N's
choice, not the console's requirement: Nintendo's `put_url` is an S3 presigned URL on
`amazonaws.com` (K), so the console follows whatever URL the archive hands it. OpenPak mints
`/blob/<part id>?exp&sig` on the same two hosts.

**Encryption.** The console encrypts each component before upload. `generate_key_seed_package`
answers a 16-byte challenge with a 512-byte package the console turns into a key; `finish_upload`
carries a 16-byte MAC. `olsc` does not do this cryptography: it fetches the package as
`$.encoded_key_seed_package` and hands it to the filesystem service (`SetKeySeedPackage`,
`GetKeySeed`, `GetInitialDataMacKeyGeneration`, through `KeySeedPackageAdaptor` onto
`ISaveDataTransferManagerVersion2`). The validation runs in fs, which lives in package2. The
format that fs accepts is under "Key seed package and division count" below. Because fs
verifies an RSA signature, a console only accepts OpenPak packages if its fs is patched to trust
OpenPak's key: fs is a KIP in package2, so the route is `atmosphere/kip_patches/<name>/` (applied
at boot), with two fs KIPs per firmware (FS and FS-exFAT), each patched against its own build.
While patched, Nintendo's own save-data cloud stops verifying on that console. `SetLocalKeySeedPackage`
(command 17, since 20.0.0) only works after `SetKeySeedPackage`, so the server package always comes first.

## Policy

`GET policy.hac.lp1.scsi.../api/console/v2/application_policy/<title>/<version>?dtoken=...`
answers `{"policy_type": "ALL_OK"}` or `"ALL_NG"` (K, N: Nintendo blocks some titles, Splatoon
among them; N allows all). The adapter today answers a differently shaped object
(`{"application_policy": {"save_data_backup": false}}`, B); the console's reaction to that
shape is unrecorded. `dtoken` is an edge token (K).

## Console archives only

### Compressed storage quota (2026-10-12)

An owned Harbor ACNH island backup (`datatype: 3`) reached `start_upload` but was refused
with HTTP 403, `storage_quota_exceeded`, code `0000`. Its expanded save image exceeded the
account's remaining hosted allowance, despite the island files occupying far less space.
The expanded image size must remain in `data_size` and determine the division count; it
does not describe the compressed files stored by this service. See Harbor's
`docs/harbor/save-cloud-interop.md` for independent native-console evidence of this distinction.

New console uploads carry an internal `quota_by_parts` marker and initially charge no bytes.
Quota then counts component sizes for uploading versions and stored sizes for live versions.
Each allowance PUT holds the existing account advisory lock through writing and recording
its component, so concurrent writes cannot spend the same remaining bytes. Overwriting a
component replaces its charge. Unknown-length bodies use a reader that fails before the
local backend publishes its temporary file. Older uploads retain their declared reservation
until completion or timeout. No console wire fields change. The database integration test
`TestCompressedIslandQuota` covers an expanded island image, concurrent writes, retries,
chunked overflow, preservation of previous bytes, exact quota exhaustion and final metadata.

The cloud holds console archives and nothing else: backups made through the console upload
protocol by a real Switch or by Harbor (which acts exactly like a real console), marked
`origin: console` in the version's meta. The same account sees the same archives on every
device. Retention is described under Retention and quota.

The emulator save API (`PUT`/`GET /internal/accounts/{account}/saves/{platform}/{title}`, and
the website's `/api/v1/me/saves/{platform}/{title}` over it) is retired since v0.4.0: both
answer 410 Gone, `{"error": "emulator saves are retired; OpenPak's Save Data Cloud is console
archives only"}`. Saves the old API stored are disregarded:
at start every live version without the console mark becomes `retired`, which hides it from every
list, download, restore and console route while keeping it on disk. Nothing converts between
formats; the server could not (it never has the console keys).

## Key seed package and division count

`start_upload` answers a real `num_of_partitions`: `ceil(data_size / 8 MiB)` within 1..64 for a
full upload (Nintendo's own rule is not visible; the importer reads chunk sizes from the initial
data, so any value works), and a delta upload repeats its previous archive's count, as the
console requires (else 2179-0301). The count is stored with the archive.

`generate_key_seed_package` builds the 512-byte KeySeedPackageV0 that the 23.0.1 fs
accepts through `SetKeySeedPackage` (Harbor private notes, §9.2):

| Offset | Field |
| --- | --- |
| `+0x000` | u32 Version = 0 (fs needs < 0x100) |
| `+0x008` | u8 KeyGeneration (`SAVES_KSP_KEY_GENERATION`, at least 4) |
| `+0x010` | Iv[16], fresh per package |
| `+0x020` | GCM tag[16] (fs: 2-7002 when wrong) |
| `+0x030` | 80 zero bytes |
| `+0x080` | AES-128-GCM body, 0x180 bytes, no AAD, key = the package key for KeyGeneration, over Signature[256] then Content[128] |

Signature is RSASSA-PSS-SHA256 (MGF1-SHA256, salt 32, e = 65537) over Content with OpenPak's
private key (fs: 2-7003). Content is `{16 zero bytes, TransferKeySeed[16] (the archive's stored
encoded_key_seed, first 16 bytes), TransferInitialDataMac[16] (the stored encoded_mac), Challenge[16]
(the request's encoded_challenge; fs: 2-7004 when it is not this session's), local flag 0, 63 zero
bytes}`. The server cannot check the MAC (it is keyed inside fs); it returns it as stored.

Both keys are files on the box, named by `SAVES_KSP_SIGNING_KEY_FILE` (PEM RSA-2048 private key,
PKCS#8 or PKCS#1) and `SAVES_KSP_PACKAGE_KEY_FILE` (the 16-byte package key, as 16 raw bytes or
32 hex characters). Neither is ever in the repo, the image or a log. With either unset or
unreadable the endpoint answers 503 and the log says the keys are not configured.

## Delta uploads

A delta (`POST save_data_archives/<P>/start_upload` against a fixed archive P) mirrors P's
components as `hand_over` parts that still point at P's blobs (same key, size, digest). The
console then touches only the chunks that changed and the meta: `component_files/<id>/update`
gives the part a blob key of its own (so P's bytes are never overwritten) and an upload URL, the
console PUTs and finishes it. A chunk it never touches has no upload URL while the archive
uploads. `finish_upload {"encoded_mac"}` turns every still-shared part into a fixed part of the
new archive, so the delta is a complete archive of its own: the console list, `start_download`
and the website's export return every chunk. Retention is safe without materialising anything:
a blob is deleted only when no live or uploading version still points at its key, so P can be
pruned or deleted while a delta needs its unchanged chunks, and the last version to hold a key
removes the blob. The division count of a delta is P's (else the console fails 2179-0301).
