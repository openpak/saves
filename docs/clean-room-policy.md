# Clean-room policy

> Trimmed copy for this repository; the canonical document lives at
> `Openpak/docs/clean-room-policy.md` and governs. Last synchronised 2026-09-15.

OpenPak replaces console network services. Some of what it replaces already has a
closed-to-us implementation. This is how we stay clear of it.

## The wall

the earlier network repositories are **source-available**, and OpenPak is AGPL-3.0-only.
We cannot fork them, vendor them, or carry their code across. What we can do is read them for
**facts**, and rewrite.

**Allowed — read these freely and write them down:**

- Ports, hostnames, endpoint paths, method and opcode numbers, RMC/protocol IDs.
- Wire formats: field names and types a console sends or expects, error and result codes.
- Flows: what talks to what, in what order, and what a working run looks like.
- Status: what actually runs today, what is a stub, what was never finished.

Those are facts about a console and a protocol. A port number is not authorship.

**Not allowed:**

- Copying, translating or vendoring their source, in whole or in part.
- Mirroring their structure: file layout, package split, type and function names, table
  schemas, the shape of their handlers.
- Lifting their comments, docs or commit messages verbatim.

The test: could you have written this line having only read a *description* of what the
server does? If yes, write it. If you are reproducing how they chose to express it, stop.

Practically: read to learn the endpoint, close the file, then write OpenPak's version in
OpenPak's shape — our `account`/`nx-baas` integration, our `ports.md` blocks, our error
handling, Go 1.25, AGPL-3.0-only.

## What is allowed as input

1. **Observed console behaviour.** A packet a real Switch sends is a fact about the Switch.
   Captures we take ourselves, error codes the console prints, endpoints it resolves.
2. **Public protocol documentation.** Kinnay's NintendoClients docs and wiki, the Pretendo
   wiki, ErrorCodes/switchbrew, published reverse-engineering write-ups.
3. **Compatibly licensed code.** Pretendo's AGPL-3.0 libraries (`nex-go`,
   `nex-protocols-go`, `nex-protocols-common-go`), Ryubing's MIT `LdnServer`. AGPL-3.0-only
   is OpenPak's licence, so AGPL-3.0 dependencies are fine; check each one's exact version
   and notices before adding it.
4. **Our own repositories.** `account`, `nx-baas`, `servers/*`.

## Record where a fact came from

Every ported component keeps a `docs/provenance.md`: each non-obvious fact (a port, an
endpoint, a field, an opcode), and where it came from — the earlier repo and file that
records it, a capture of ours, or a public document. That is what makes "we rewrote it from
the facts" checkable later instead of a claim.

Record what you could not explain, too. A `docs/provenance.md` is not only for settled
facts: an observation that made no sense, a value that seemed arbitrary, a behaviour with no
cause you could find — write it down and mark it unexplained. Another component's measurement
is often the missing half, and no one can pair them if only one was ever recorded. Two facts
that contradict each other are a finding, not a problem to resolve before writing.

## The working-directory trap

A session started in a Shield-licensed tree has one as its working directory, so any bare
`ls`, `grep` or `find` lands there without anyone intending it. Start work in
`~/REPOS/Openpak`, and pin every command to an absolute path until you have. A directory
listing of top-level names is not source and does not contaminate anyone — but it is the
near miss that tells you the next command might.

## In practice, for an agent

Your working set is your own repository, everything under `Openpak/`, the public
documentation above, and the earlier repo you are replacing — read for facts, never copied.
Write the facts into your own docs as you go, then build from your notes rather than with
their file open beside you.
