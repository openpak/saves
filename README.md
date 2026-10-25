# Cloud Saves Server

OpenPak's Cloud Saves Server.

| | |
| --- | --- |
| Network | Switch (`*.scsi.srv.nintendo.net`), Harbor |
| Role | Cloud saves: the index of every save under an account, an OpenPak-hosted allowance, and the player's own S3-compatible or WebDAV storage |
| Depends on | PostgreSQL |
| Used by | [`nx-baas`](https://github.com/openpak/nx-baas), website |
| Image | `ghcr.io/openpak/saves` |
| Licence | AGPL-3.0-only |

## What it serves

| Endpoint | What |
| --- | --- |
| `/internal/*` | Save index and deletion for the website (`X-Internal-Key`) |
| `/scsi/*` | Switch archives and component files, forwarded by nx-baas |
| `/healthz` | Health check |

## Run

```sh
cp example.env .env   # fill in; never commit it
set -a; . ./.env; set +a
go run ./cmd/saves
```

## Docs

- [Protocol](docs/protocol.md)
- [Evidence](docs/evidence.md)
- [Deploy](docs/deploy.md)
- [Notes](docs/notes.md)
- [Provenance](docs/provenance.md)

## Licence

AGPL-3.0-only. See [LICENSE](LICENSE).
