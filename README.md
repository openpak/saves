# saves

Cloud saves for OpenPak. Platform-neutral service on port 20020 ([`ports.md`](../ports.md)):
the index of every save under an account, a small OpenPak-hosted allowance per account, and
the player's own S3-compatible storage for everything beyond it. The plan is
[`prds/cloud-saves-prd.md`](prds/cloud-saves-prd.md); what is known about the console's
protocol, and from where, is [`docs/provenance.md`](docs/provenance.md).

**Status (2026-09-10): built and deployed.** Emulator and website surfaces work end to end.
The Switch surface is served and answers the console's request set; whether a console accepts
a server-minted key seed package (milestone M0) is not yet known, and the log line
`[scsi] M0 key_seed_package` is where that answer will appear.

## What runs

| Piece | Where |
| --- | --- |
| `cmd/saves` | the service: `/internal/*` for the website, `/scsi/*` for the Switch adapter, `/healthz` |
| `internal/store` | Postgres: saves, versions, parts, storage settings, audit; retention (last 10 + pinned) |
| `internal/blob` | backends: `local` (the allowance, a directory) and `s3` (the player's bucket, presigned URLs); sealed credentials |
| `internal/switchscsi` | the console's archives and component files over the same index |
| `internal/api` | the internal HTTP API |

Who calls it:

- **website** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): `/account/saves` and the mobile/emulator
  API `/api/v1/me/saves[/{platform}/{title}]`. An emulator does `GET` on launch (204 means
  nothing stored; `X-Save-Version` says what it got) and `PUT` on exit with `X-Save-Base` set
  to that version, so a save from a stale base is kept as a conflict instead of overwriting.
- **nx-baas** (`SAVES_URL`, `SAVES_INTERNAL_KEY`): forwards every `*.scsi.srv.nintendo.net`
  request here under `/scsi`, with the console's proven account in `X-OpenPak-Account`. The
  console must resolve those names to OpenPak: `console/openpak-nro` redirects
  `*.scsi.srv.nintendo.net` since 2026-09-10.

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

- Clean room. Read [`docs/clean-room-policy.md`](../docs/clean-room-policy.md) first.
- AGPL-3.0-only, English only, Go 1.25, no secrets in the repository.

This project is not affiliated with or endorsed by Nintendo or any original service provider.
It contains no proprietary game code, assets, SDK material, binaries, certificates, keys,
firmware, extracted files, credentials, or raw packet captures.
