# saves

Cloud saves for OpenPak. Platform-neutral service on port 20020 ([`ports.md`](https://github.com/openpak/docs/blob/main/ports.md)):
the index of every save under an account, a small OpenPak-hosted allowance per account, and
the player's own S3-compatible storage for everything beyond it. The plan is
[`prds/cloud-saves-prd.md`](prds/cloud-saves-prd.md); what is known about the console's
protocol, and from where, is [`docs/provenance.md`](docs/provenance.md).

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

Who calls it:

- **website** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): `/account/saves`, over `/internal/accounts/{account}/...`.
  The emulator save routes (`PUT`/`GET .../saves/{platform}/{title}`) answer 410 Gone: the
  cloud is console archives made by a real Switch or Harbor, nothing else. What the old API
  stored is marked `retired` (hidden, kept) at start; `app purge-retired` deletes it, bytes and rows.
- **nx-baas** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): forwards every `*.scsi.srv.nintendo.net`
  request here under `/scsi`, with the console's proven account in `X-OpenPak-Account`. The
  console must resolve those names to OpenPak: `console/openpak-nro` redirects
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

A deleted backup leaves the console's list at once and its bytes are removed from the backend.
The quota is `SAVES_ALLOWANCE_BYTES` per account (default 1 GiB during the alpha phase, was 256 MiB; counted over live and
uploading backups on the OpenPak-hosted allowance). It counts compressed component bytes,
not the expanded save-image `data_size`; ACNH island backups can therefore upload even when
their expanded image exceeds the remaining allowance. Each blob PUT holds the account's
quota lock through the write and answers 403 if the component does not fit. A retry replaces
its component's charge, and an oversized chunked request preserves the previous blob. The
console's own "cloud full" error code is not known; a
player who connects their own storage is not limited by it. Which client uploaded a backup is
read from `X-OpenPak-Client` (`harbor/...` means Harbor, anything else a Switch).

## Running it

```sh
cp example.env .env   # fill in; never commit it
set -a; . ./.env; set +a
go run ./cmd/saves
```

Tests need a Postgres:

```sh
podman run -d --name openpak-saves-test -e POSTGRES_PASSWORD=test -e POSTGRES_DB=saves_test \
  -p 23001:5432 docker.io/library/postgres:18-alpine
SAVES_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:23001/saves_test?sslmode=disable' \
  go test -race ./...
```

## Deploying

Tag `v*`; the workflow publishes `ghcr.io/openpak/saves` and the host's quadlet
(`openpak-saves.container`, volume `openpak-saves` at `/app/data`) auto-updates. The service
needs `SAVES_DATABASE_URL`, `SAVES_INTERNAL_KEY` and `SAVES_CREDENTIAL_KEY` in the shared
`.env`; the website and adapter need `SAVES_URL` and the same `SAVES_INTERNAL_KEY`.

`SAVES_CREDENTIAL_KEY` seals every player's bucket credentials. Losing it disconnects them all;
rotating it means every player re-enters their secret. Back it up with the database.

## Ground rules

- Clean room. Read [`docs/clean-room-policy.md`](https://github.com/openpak/docs/blob/main/clean-room-policy.md) first.
- AGPL-3.0-only, English only, Go 1.25, no secrets in the repository.

This project is not affiliated with or endorsed by Nintendo or any original service provider.
It contains no proprietary game code, assets, SDK material, binaries, certificates, keys,
firmware, extracted files, credentials, or raw packet captures.
