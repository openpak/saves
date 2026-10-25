# Deploy: saves

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
