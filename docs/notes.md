# Notes: saves

Cloud saves for OpenPak. Platform-neutral service on port 20020 ([`ports.md`](https://docs.openpak.org/contributors/ports/)):
the index of every save under an account, a small OpenPak-hosted allowance per account, and
the player's own S3-compatible storage for everything beyond it. The plan is
[`../prds/cloud-saves-prd.md`](../prds/cloud-saves-prd.md); what is known about the console's
protocol, and from where, is [`provenance.md`](provenance.md).

**Status (2026-10-08): built and deployed.** The cloud holds console archives only: the emulator
save API is retired (410 Gone) since v0.4.0. The Switch surface is served and answers the console's request set; `generate_key_seed_package` builds the real, signed KeySeedPackageV0 when
`SAVES_KSP_SIGNING_KEY_FILE` and `SAVES_KSP_PACKAGE_KEY_FILE` name key files (see
`docs/provenance.md`); without them it answers 503.

Since v0.4.4, the console list exposes the newest complete backup per title and datatype,
rather than every retained revision. The native Switch upload/download path rejects a
third per-title record. Website history and archive-id downloads keep all retained revisions.

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
nobody uploads to again (50 saves per pass). It was ten.

The quota is `SAVES_ALLOWANCE_BYTES` per account (default 1 GiB during the alpha phase, was 256 MiB; counted over live and
uploading backups on the OpenPak-hosted allowance). It counts compressed component bytes,
not the expanded save-image `data_size`; ACNH island backups can therefore upload even when
their expanded image exceeds the remaining allowance. Each blob PUT holds the account's
quota lock through the write and answers 403 if the component does not fit. A retry replaces
its component's charge, and an oversized chunked request preserves the previous blob. The
console's own "cloud full" error code is not known; a
player who connects their own storage is not limited by it. Which client uploaded a backup is
read from `X-OpenPak-Client` (`harbor/...` means Harbor, anything else a Switch).

## Ground rules

- Clean room. Read [clean-room policy](https://docs.openpak.org/contributors/clean-room/) first.
- AGPL-3.0-only, English only, Go 1.25, no secrets in the repository.

This project is not affiliated with or endorsed by Nintendo or any original service provider.
It contains no proprietary game code, assets, SDK material, binaries, certificates, keys,
firmware, extracted files, credentials, or raw packet captures.
