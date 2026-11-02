# ADR-0036: Bytes are reserved before they move, and a carried object is named again only by a record that finds it named

- **Status:** accepted
- **Authority:** `internal/artifactupload`
- **Enforced-by:** `internal/artifactupload.TestNoByteIsAcceptedBeforeItsReservation`, `internal/artifactupload.TestFinalizeRefusesBytesThatDoNotMatchTheirDigest`, `internal/artifactupload.TestUnnamedBytesNeverExceedTheReservationBudget`, `internal/artifactsdomain.TestACarriedObjectMustBeNamedWhereItIsCarriedFrom`, `internal/artifactsdomain.TestNoCarryCrossesAnArtifact`
- **Cost-when-tried:** a project file upload stores its object, then writes its row, and holds no quota over what the company stores at all. An upload refused at the row, abandoned by its client, or cut off by a node leaves its object in the store for the collector's day, and nothing bounds how many such objects one principal can leave: a looping client at the 1 GiB file limit can fill a `nats` bucket that shares its broker with the fleet's leases and ledgers before the first collection runs.
- **Tag-status:** unreleased

## The decision

Two rules govern how bytes become a version, and both bind the upload
pipeline, the object store, its collector and the artifacts domain together.

**Bytes are reserved before they move.** Every artifact upload starts with an
intent, and the intent decides everything a publish can decide without the
bytes, in one snapshot: the caller's authority, the paths, the media types, the
per-file and per-publish limits, which files the artifact already holds, and
whether the artifact usage already named, plus every outstanding reservation,
plus the new bytes, fits the company's quota. It then mints one key per new
file at the engine's own clock and records a **reservation** in coordination
whose lifetime runs from the key's minting to past the point where the
collector could have deleted the object (`collect.PendingGrace` plus the
collection interval). One principal may hold `artifacts.limits.pending_bytes`
reserved, and a company four times that; past it the intent is refused `429
uploads_outstanding`, naming the figure and when the oldest reservation
expires. So **bytes that no record names never exceed the reservation budget**,
where before they were bounded only by the collector's day.

The bytes then move one of two ways, and the client never branches on which:
streamed through the engine (always available, and the only way on `nats`), or
straight to an S3 bucket on a presigned `PUT` the engine issues only when its
boot probe proved the bucket honours the bindings: the exact length, the served
content type, the declared SHA-256 as a signed checksum and `if-none-match: *`,
expiring well inside `objstore.RecordWithin`. An agent's sandbox is never handed
a presigned URL; the engine reads the box itself. Nor is the dashboard, whose
own policy (`connect-src 'self'`) reaches only its origin, so its uploads
always stream through the engine. **The engine mints every key;
a client never names one.**

**Finalize reads every new object back** through the object store's verifying
reader before the record is decided: a mismatch is `422 upload_mismatch`, an
object that never arrived `409 upload_missing`, and a key older than
`objstore.RecordWithin` is refused by the decide as it is for every write. The
row's SHA-256 is therefore the digest of the stored bytes, established by the
engine before the record exists, whatever transport carried them. The
reservation is released for every object the decided record names.

**A carried object is named again only by a record that finds it named.** A
publish names each file by its SHA-256, and a hash the artifact already holds is
CARRIED: the new version names the existing object and no bytes move. That is
the one way a key is named by a record other than the write that uploaded it,
so the collector's invariant (once no row names a key at some applied prefix,
no later record names it) is kept by three facts:

- the applier performs a carry only by finding the artifact's own reference
  row for that object, with a live reference count, **in the apply transaction
  itself**; finding none, it skips the record identically on every data node and
  the writer, reading its own rows, answers `409 carried_object_gone` naming
  the paths to upload again;
- every record that adds or removes an object reference (a publish, a
  restore, a version prune, a purge) is arbitrated on the artifact's own
  subject, so the broker orders every carry against every removal;
- both artifact reference tables belong to one domain, which the collector
  reads at one position.

**No carry crosses an artifact**, and none crosses a domain. A third-party file
vendored at publish is copied into the publishing artifact as an object of its
own, and an asset is never carried at all.

## Why the obvious alternatives are wrong

**Checking the quota after the bytes land** is the shape the project file
upload has, and the cost line above is what it buys: the refusal comes after
the store has already paid for the bytes, and only the collector, a day later,
gets them back.

**Letting the client choose the key**, or upload under a name it computed, is
what makes a direct upload tempting to design and what ADR-0027 cannot survive:
a key two writers can name is a key the collector can delete under one of them.
A presigned `PUT` writes a key the engine minted, once, under a precondition
that refuses a second put.

**Trusting the bucket's own checksum instead of reading back** would save one
read of at most a publish's 64 MiB, and the pipeline reads every text byte
anyway: the linter, the nonce placeholders, the inline handler hashes, the
credential scan, vendoring and search text all parse the files. The read is
what makes the row's digest the engine's statement rather than the client's.

**Deduplicating across the company** by content hash saves storage for shared
libraries and turns every publish into an existence oracle: a writer of one
artifact could learn whether any artifact in the company, including ones they
cannot open, holds a file with a given digest. It would also judge a carry and
its removal in two artifacts' orders at once, which no single subject
arbitrates.

## Amendments to earlier records

[ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md):
the row's digest is established by the engine, by a verified read of the stored
bytes, before the record that names it is decided, whatever carried the bytes;
and a read path that cannot check what it serves (a presigned `GET`) is not
offered.

[ADR-0027](0027-a-deletion-from-the-shared-store-needs-no-lock.md): a key is
named only by the write that uploaded it, **or by a record of the same artifact
whose apply finds it named**. A presigned transfer writes a key the engine
minted, under `if-none-match: *` and a signed checksum, so a name is still put
once when the client does the put. The reservation outlives the object until
the collector could have deleted it.

## What the spikes confirm

Spike S3 streams 64 MiB out of a sandbox through the engine-streamed leg into
both object backends. Spike S7 measures state-log append latency at the 99th
percentile while a 64 MiB publish streams into the `nats` bucket, which is what
the bucket's reservation and `artifacts.limits.pending_bytes` protect. The
method and the numbers are in
[`docs/reference/artifact-capacity.md`](../docs/reference/artifact-capacity.md).

## What this does not decide

It does not decide **where the bytes live**, which is ADR-0026's store and its
operator's choice of backend, nor **an object name grammar** beyond the
existing `files/` corner. It applies the reservation to artifact versions and
assets; a project file upload keeps the path its cost line describes until it
is moved onto an intent, which is this rule applied to another consumer and not
a new decision.
