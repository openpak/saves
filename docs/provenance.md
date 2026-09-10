# Provenance: the Switch save-data cloud, as far as it is known

Every non-obvious fact the saves service is built on, and where it came from. Per
[`docs/clean-room-policy.md`](../../docs/clean-room-policy.md): the earlier server's `nx-scsi` is read
for facts about the console and the wire, never for code or shape. Nothing below is theirs to
own; a path, a field name or a status string is a fact about the console.

Sources:

- **N** — Reference `nx-scsi` (source-available): `handlers.go`, `main.go`, `replay_test.go`.
  Evidence of what a console sent them and what they answered that the console accepted, as
  far as their own comments claim ("to be validated console-side" is repeated there).
- **K** — Kinnay's NintendoClients wiki, pages *OLSC Servers* and *Server List*.
- **C** — our own console, through the Switch adapter's debug log (2026-09-07).
- **B** — `Openpak/nx-baas/services.go`, what the adapter answers today.

## Hostnames

| Host | Role | Source |
| --- | --- | --- |
| `storage.hac.lp1.scsi.srv.nintendo.net` | the REST API, `/api/console/v3/...` | C, N |
| `scsi-download.lp1.scsi.srv.nintendo.net` | where `get_url` points; the console GETs blobs here | N, K |
| `scsi-upload-lp1.s3.us-west-2.amazonaws.com` | where Nintendo's `put_url` points: a plain S3 presigned PUT on a public host | K |
| `scsi-upload.lp1.scsi.srv.nintendo.net` | the name Reference substituted for the S3 host; the console followed it | N |
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

`start_upload` metadata, as sent by a console in N's replay: `datatype` (0), `data_size`,
`saved_at_as_unixtime`, `launch_required_version`, `series_id`, `encoded_digest`, `desc`
(null), `auto_backup`, `acd_index`. `application_id` was **absent** on a delta upload and
taken from the previous archive. C saw `datatype=0` on the list; N's code also knows `3`.
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
(N). The console PUTs the body raw and GETs it raw, `application/octet-stream`.

**Unexplained, and the risk that decides the design:** the console encrypts each component
before upload. `generate_key_seed_package` answers a 16-byte challenge with a 512-byte
package the console turns into a key; `finish_upload` carries a 16-byte MAC. Reference did not
implement this: they replay a package captured from Nintendo, and their own comment says
whether the console binds the package to the challenge or to the account is unknown. 512
bytes is the size of an RSA-4096 signature or ciphertext. If the package must be signed by a
key in the console's firmware, no server can mint one and the answer is the same as News:
a console-side key patch. That is milestone M0's question and nothing else is built on it
until it is answered.

## Policy

`GET policy.hac.lp1.scsi.../api/console/v2/application_policy/<title>/<version>?dtoken=...`
answers `{"policy_type": "ALL_OK"}` or `"ALL_NG"` (K, N: Nintendo blocks some titles, Splatoon
among them; Reference allows all). The adapter today answers a differently shaped object
(`{"application_policy": {"save_data_backup": false}}`, B); the console's reaction to that
shape is unrecorded. `dtoken` is an edge token (K).

## Emulators

Citron (earlier build) pulls `GET /api/save/<title hex>` at launch (204 = nothing stored)
and pushes `POST /api/save/<title hex>` with the raw save as the body, bearer = the account
token. That is the whole emulator contract Reference offers: newest wins, no versions, no
conflict. Evidence for what a client will do, not a design to keep.
