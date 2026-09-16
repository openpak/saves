# Changelog — saves

Generated from git history on 2026-09-15. `git log` stays the source
of truth; this file is the readable summary.

## Unreleased

- scsi: v2 and v3 prefixes, POST-only actions, and a fixed archive that stays fixed
- the allowance is reserved by the upload, not recomputed after it
- a stale upload is a conflict at commit, and a conflict is never the current version
- the sweep retries deletions a backend refused
- every version records the storage location it was written to; changing storage no longer moves old backups
- ci: vet, race tests and a Postgres that a skip cannot hide
- a rotated OAuth refresh token is sealed back into the settings it came from
- console backups export whole, as a zip of their components
- player storage on this deployment’s own network is refused unless an administrator allows it
- release: job-scoped registry auth file [989d4b7]
- release: build on the self-hosted runner with podman; private-repo minutes are metered [1f2bc15]
- Go 1.26, the house version [f2b0613]


## v0.2.0 — 2026-09-10



## v0.1.0 — 2026-09-10



## v0.1.0 — 2026-09-10

- The saves service: index, allowance, and the player's own storage [5e0d425]
- seed: empty saves, AGPL-3.0-only, clean-room gate [2709589]

