# saves

Cloud saves for OpenPak. Platform-neutral service on port 20020 ([`ports.md`](../ports.md));
the Switch-facing SCSI handlers are a Go package this repository exports and the Switch
adapter mounts on 21000, per PRD collision rule 2 — they do not get their own console port.

**Status: empty.** Nothing is implemented yet. See [`prds/cloud-saves-prd.md`](../prds/cloud-saves-prd.md).

## Ground rules

- Clean room. Read [`docs/clean-room-policy.md`](../docs/clean-room-policy.md) first.
- AGPL-3.0-only, English only, Go 1.25, no secrets in the repository.

This project is not affiliated with or endorsed by Nintendo or any original service provider.
It contains no proprietary game code, assets, SDK material, binaries, certificates, keys,
firmware, extracted files, credentials, or raw packet captures.
