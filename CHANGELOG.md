# Changelog — saves

Generated from git history on 2026-09-15. `git log` stays the source
of truth; this file is the readable summary.

## v0.4.5 — 2026-10-12

- ACNH recovery grants (`/permissions`) expose the current island backup within the authenticated account; device-scoped archive lists distinguish a local backup cache from cross-device recovery
- ACNH island backups and other console archives charge compressed component bytes against quota instead of reserving the expanded save-image size; concurrent uploads, retries and chunked requests enforce the same account allowance

## v0.4.3 — 2026-10-09

- a delta upload keeps the previous archive's untouched chunks: they are shared (not copied) until `finish_upload` makes them part of the new archive, so the delta lists, downloads and exports whole
- a chunk the console replaces (`component_files/<id>/update`) gets its own blob key first; the previous archive's bytes are never overwritten
- retention and deletes remove a blob only when no live or uploading archive still points at it

## v0.4.2 — 2026-10-08

- `start_upload` answers a real `num_of_partitions` (one per 8 MiB of `data_size`, 1..64; a delta repeats its previous archive's)
- `generate_key_seed_package` builds the real KeySeedPackageV0 (AES-128-GCM body, RSA-PSS-SHA256 signature) from keys in files named by `SAVES_KSP_SIGNING_KEY_FILE` and `SAVES_KSP_PACKAGE_KEY_FILE`; without them it answers 503 as before
- the M0 random-package experiment (`SAVES_M0_KEY_SEED`) is gone

## v0.4.1 — 2026-10-08

- `DELETE /internal/accounts/{account}/saves/{platform}/{title}` deletes every backup of a title
- listed versions carry `console` ("Harbor" or "Switch") instead of the device id
- start_upload records which client uploaded (`X-OpenPak-Client`) so the website can say Harbor

## v0.4.0 — 2026-10-08

- breaking: the emulator save API is retired; `PUT`/`GET /internal/accounts/{account}/saves/{platform}/{title}` answer 410 Gone with a JSON reason
- the cloud holds console archives only; versions the old API stored are marked `retired` at start (hidden from lists, downloads, restore and the console)
- `app purge-retired` deletes retired versions, their bytes and emptied save records
- retention is again the plain last 10 per title (no per-kind split); the emulator "newest" is gone

## v0.3.2 — 2026-10-08

- retention keeps the last 10 versions of each kind (console archives, emulator saves) per title, so neither pushes the other out
- the emulator API's newest save is the newest emulator-format save, never a console archive

## v0.3.1 — 2026-10-08

- scsi: the console list holds only archives made through the console protocol; emulator-API saves are no longer sent as incomplete console records (one made firmware 23.0.1 refuse the whole list, 2179-3201)
- scsi: listed records are complete for the 23.0.1 record check (32-byte `encoded_digest`, no `component_files`); an older 8-byte digest is zero-padded
- scsi: error bodies carry the console's `code` (`0001` = no such archive)
- scsi: `component_files/create` answers an index the archive already has instead of adding it twice
- scsi: `finish_upload` keeps `encoded_key_seed` and `encoded_mac` (a delta keeps its series' seed)
- scsi: no account ids in log lines

## v0.3.0 — 2026-09-16

- scsi: v2 and v3 prefixes, POST-only actions, and a fixed archive that stays fixed
- the allowance is reserved by the upload, not recomputed after it
- a stale upload is a conflict at commit, and a conflict is never the current version
- the sweep retries deletions a backend refused
- every version records the storage location it was written to; changing storage no longer moves old backups
- ci: vet, race tests and a Postgres that a skip cannot hide
- a rotated OAuth refresh token is sealed back into the settings it came from
- console backups export whole, as a zip of their components
- player storage on this deployment’s own network is refused unless an administrator allows it
- restore writes a new version over the same bytes; the one it came from keeps its number
- the move verifies every object by reading it back, can be run again, and goes both ways
- the console can delete a backup (firmware 22.5.0: `save_data_archives/<id>/delete`)
- the M0 key-seed experiment is off unless SAVES_M0_KEY_SEED says otherwise
- release: job-scoped registry auth file [989d4b7]
- release: build on the self-hosted runner with podman; private-repo minutes are metered [1f2bc15]
- Go 1.26, the house version [f2b0613]
- ci: a port podman picks, and a release workflow that starts [41f4d25]

## v0.2.1 — 2026-09-10

- WebDAV: create the share's own folder, not only a key's parents [caa9b0e]

## v0.2.0 — 2026-09-10



## v0.1.0 — 2026-09-10



## v0.1.0 — 2026-09-10

- The saves service: index, allowance, and the player's own storage [5e0d425]
- seed: empty saves, AGPL-3.0-only, clean-room gate [2709589]

