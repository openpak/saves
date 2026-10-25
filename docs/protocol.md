# Protocol: saves

## Who calls it

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
| `DELETE /internal/accounts/{account}/saves` | account erasure (the website finaliser, after the deletion grace period): every backup, the storage credential and every row (200 `{"titles","versions"}`; 503 when bytes on OpenPak storage could not be deleted, retry) |

A deleted backup leaves the console's list at once and its bytes are removed from the backend.
