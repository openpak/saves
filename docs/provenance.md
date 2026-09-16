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
- **F** — the console's own firmware, 22.5.0: the `olsc` system module (the save-data cloud
  client) from the local audit tree, `all/olsc/strings.txt`, read 2026-09-16. Offsets in
  brackets are string-table offsets in that dump. This is first-party evidence and outranks
  everything else here: where F and N disagree, F is the console and N is somebody's server.

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

N answers the same resource set under both the `v2` and the `v3` prefix, and hands out
`timeout_at_as_unixtime` of now+3600 on `GET save_data_archives/<id>` and on `start_download`,
now+600 on `extend_upload_timeout` (N). It returns no structured error body at all: a bare 404
for an unknown path, archive or component, and a 500 with plain text when a blob write fails
(N). **Unexplained:** what the console shows for each, and whether it needs a body.

**Corrected 2026-09-16:** an earlier note here said there is no delete route, on the evidence
that N never implemented one. F has it — `save_data_archives/%llu/delete?force=true` [1468615]
— and the adapter now answers it. Absence from somebody else's server was never evidence of
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
| `permissions` | **no** | 1470346 |
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
**Unexplained:** what `permissions` answers, what `save_data_type` and `$.revision` carry, and
where `encoded_key_seed`/`encoded_sek` are sent — none of those are on a backup path we have
watched.

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
(N), served for GET on the download host and PUT on the upload host, with no bearer. The
console PUTs the body raw and GETs it raw, `application/octet-stream`. That path shape is N's
own choice, not the console's requirement: Nintendo's `put_url` is an S3 presigned URL on
`amazonaws.com` (K), so the console follows whatever URL the archive hands it. OpenPak mints
`/blob/<part id>?exp&sig` on the same two hosts.

**Unexplained, and the risk that decides the design:** the console encrypts each component
before upload. `generate_key_seed_package` answers a 16-byte challenge with a 512-byte
package the console turns into a key; `finish_upload` carries a 16-byte MAC. Reference did not
implement this: they replay a package captured from Nintendo, and their own comment says
whether the console binds the package to the challenge or to the account is unknown. 512
bytes is the size of an RSA-4096 signature or ciphertext. If the package must be signed by a
key in the console's firmware, no server can mint one and the answer is the same as News:
a console-side key patch. That is milestone M0's question and nothing else is built on it
until it is answered.

**Where the answer is not (F, 2026-09-16).** `olsc` does not do this cryptography. It is a
courier: it fetches the package over HTTP, reads it out as `$.encoded_key_seed_package`
[1456375], and hands it to the filesystem service — `SetKeySeedPackage` [1473492],
`GetKeySeed` [1460916], `GetInitialDataMacKeyGeneration` [1480208], through
`nn::olsc::srv::adaptor::KeySeedPackageAdaptor` [1616276] onto
`nn::fssrv::sf::ISaveDataTransferManagerForSaveDataRepair` [1502470] and
`SaveDataTransferManagerVersion2` [1464317]. Whatever validates the package runs in **fs**,
which lives in package2 and is not in the audit tree. Reading `olsc` further cannot answer
M0; extracting fs is the next step, and it is the only one that can.

**fs is out (F, 2026-09-16).** `dump-all.sh` only walks Program NCAs, so fs was never in the
audit tree: it lives in INI1 inside package2, in the BootImagePackage NCA (`0100000000000819`,
content type Data). On this firmware INI1 is **embedded in the kernel blob**, not a package2
section of its own, so `hactool --intype=pk21 --ini1dir=` reports `Number of Processes: 00`
and writes nothing — dump the kernel, find the `INI1` magic inside it (0x8d000 here), carve
from there, and feed that to hactool. `scripts/extract-fs.sh` in the audit tree does all of it
and leaves `FS.bin` (2,499,188 bytes decompressed) plus the other six initial processes.

What that does **not** give: fs is a release build. 467 strings over twelve characters, no
assert paths, and HIPC dispatches by command id rather than by name, so no amount of grepping
finds the handler. A scan of the whole blob for an RSA public exponent (0x10001) adjacent to a
256- or 512-byte high-entropy modulus found **none** — which is suggestive and not evidence,
because a verify routine can hardcode the exponent and keep only the modulus.

The handler to read is `ISaveDataTransferManagerVersion2` **command 16, `SetKeySeedPackage`**,
taking one type-0x5 buffer (switchbrew, *Filesystem services*). That is a Ghidra job on
`FS.bin`, and it is the last step between here and M0.

**And one fact that narrows M0 either way (switchbrew):** `SetLocalKeySeedPackage` is command
17, added in 20.0.0, and it *"can only be used after SetKeySeedPackage, in certain
conditions"*. So the console's own local package does not stand in for the server's — the
server package comes first, always. Whatever validates it is not optional.

**One thing worth chasing there (F, inference, not fact):** `olsc` also has
`GetLocalKeySeedPackage` [1479304] — the console can produce a key seed package *itself*, for
a transfer with no server in it. If the local package and the server package are the same
shape, then the format is one a console mints, which is an argument against it being a
Nintendo signature. If they are different shapes, that is the answer the other way. Either
way it is a question about fs, not about this service.

**The Ghidra pass on FS.bin (F, 2026-09-16).** `FS.bin` is a KIP1 whose three segments
hactool leaves uncompressed and concatenated after a 0x100 header (text@0, ro@0x1db000,
data@0x244000); read flat, every absolute address is wrong. Wrapped at true vaddrs into
`FS.elf` and auto-analysed (5319 functions). Reusable workspace, project and scan tooling:
`~/openpak-firmware-audit-22.5.0/m0/` (see its `README.md`). What the pass established:

- FS opens **`set:cal`** (settings calibration — device-unique key material) at
  `FUN_001d07d0`, via the sm GetServiceHandle-by-name path `FUN_001c38d0(&h, "set:cal")`.
  Device-key access is present in FS itself.
- The **`spl:fs` string (vaddr 0x1f738c) is unreferenced** — vestigial. FS's save crypto is
  not reached through that service name, so the earlier "spl:fs delegation" read is withdrawn.
- The cmif dispatch here is **by numeric command id**, not the libstratosphere
  `{ver,ver,cmd,handler}` `ServiceCommandMeta` layout: static table-scanning for that struct
  finds nothing, and many handler targets are not even auto-recognised as functions (they are
  only data-pointer-referenced). This is the wall the release build was expected to be.
- The dispatch is a `.data` array of pointers (~0x2520a0+, 8 bytes each) into per-command
  **argument-metadata** blocks in .rodata (~0x1dcc00-0x1df400, stride 0x22). **Command 16's
  metadata block is at ~0x1dd128** (cmd_id `0x10` at 0x1dd130, an input-buffer descriptor
  follows). Command 17 is gated to hosver **20.0.0** (`0x00140000`), confirming this firmware
  carries `SetLocalKeySeedPackage` next to `SetKeySeedPackage`. Each metadata block has one
  `.text` referrer (its handler) and one `.data` referrer (its dispatch slot).

**Still open, and now a narrow interactive step.** Correlate cmd 16's dispatch slot -> its
metadata block -> the `.text` handler, confirm the input is one ~512-byte (0x200) type-5
buffer, then read that handler: does it verify a signature against an embedded key, call
device crypto (calibration/SPL keyslot) — both mean no server can mint a package a given
console accepts — or merely store the bytes? That read is the M0 answer and is best done in
the Ghidra GUI on the saved `FSm0` project.

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
