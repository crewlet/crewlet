# Architecture decision records

A decision that binds **more than one package** is recorded here. A decision
that lives inside one package is recorded in that package's own doc comment,
where `go doc` puts it beside the code — and it does not get a record here.

That single criterion is the whole of the filing rule, and it exists because
this repository has already paid for the alternative. `internal/textcut`,
`internal/whsec`, `internal/jsprovision`, `internal/httpx`,
`internal/api/httpjson`, `internal/tokens`, `internal/procgroup`,
`internal/runtoken`, `internal/clientsource`, `internal/hostbox` and
`internal/solo` all exist for one reason: a rule was written down twice, the
copies disagreed, and nothing compared them. `internal/jsprovision`'s own doc
names the failure exactly — *"five decisions spelled separately, each doc
comment asserting it matched the other with nothing enforcing that it did."*
A tree of records that restated package docs would be that failure with a
directory of its own. So an ADR here never restates its authority; it names it.

## Why these exist at all, when the package docs are this good

They do not exist because the reasoning was missing. Before the first record
below was written, the rule it carries was already stated correctly in
`internal/statelog/doc.go`, in `docs/guides/replication.md`, in
`docs/concepts/architecture.md` and in `CLAUDE.md`. It was still not taken into
account, and the reason is visible in the tree: **every rule here that has a
gate has never been re-litigated, and every rule held by prose alone has
decayed.**

The table below is the evidence, **as it stood when this directory was
created**. It is deliberately a historical record rather than a live one — two
of its rows acquired a gate in the same change that wrote it, and a table
edited to keep up would lose the only thing it is for, which is that the two
columns correlate.

| Rule | Held by, then | What had happened |
|---|---|---|
| No statement names both estates | a static walk | never broken |
| One store driver in the binary | a test | never broken |
| One outbound HTTP transport | a static walk | never broken |
| One writer of a live stream's config | a static walk | never broken |
| A skip is not a pass | `internal/skipgate` | never broken since |
| Every commit is signed off | a script | 61 of 361 commits missed it before the script |
| Fleet-agreement state belongs in coordination | prose | eight tables, four repair migrations — and a ninth found while writing this |
| The replicated estate is written only by an applier | prose | three bypasses, none reviewed as one |
| Every node-local table is swept on every node | prose, at the field that implements it | six of seven sweeps took the wrong default, including the audit log's |
| The Makefile matches `ci.yml` | prose | unchecked, and nothing would notice |

Every rule in the top half was still true. Every rule in the bottom half had
decayed, and the last of them is the sharpest: the rule was written out in full
at the definition of the very field that carries it, and six of the seven call
sites ignored it anyway. Reading is not enforcement.

So the load-bearing field of a record here is **`Enforced-by:`**, and the
record is the preface to the gate rather than a substitute for it. An ADR
whose `Enforced-by:` is `nothing` is allowed, and it must say so in that word,
and it is then listed in `adr.Unenforced` with a reason — two-sided,
so an entry that stops being true fails the build. Writing `nothing` is the
honest answer often enough that hiding it would be the worse outcome; what is
not allowed is leaving the question out.

## The fields, and how they differ from a generic template

A generic ADR template asks for Context, Options Considered, Decision and
Consequences. Four of those survive here under other names; three fields are
particular to this repository and one is missing on purpose.

| Field | What it is for |
|---|---|
| **Status** | `accepted` or `superseded`. A superseded record keeps its number and its text, and gains `Superseded-by:`. Numbers are never reused and a record is never edited into a different decision — the reasoning somebody acted on has to stay readable. |
| **Authority** | The ONE package or page whose own doc holds the detail. The record states the decision and why the obvious alternative is wrong; everything else stays at the authority, and the record is forbidden to restate it. This is the field that stops the tree becoming a second copy. |
| **Enforced-by** | A named test, a compile error, or the literal `nothing`. A test named here must exist. |
| **Measured** | The numbers the decision was settled on. This repository argues with measurements — 1.7 ms to create a consumer, 1 min 44 s against 50 ms for an index, K1 at 1.2, a delivery budget of 25 — and a Pros/Cons list discards the only thing that makes a decision hard to re-litigate later. Omit it only when there was genuinely nothing to measure. |
| **Cost-when-tried** | What the obvious alternative cost *when it was tried here*. Forensic rather than speculative, which is the difference from a generic "Considered Options" section: this repository has usually already run the experiment, and the incident is the argument. |
| **Tag-status** | `unreleased`, or the tag that first shipped the decision. A `v*` tag is the only compatibility boundary this project has, so a surface no tag has shipped can be changed outright — see `CONTRIBUTING.md`. Today every record reads `unreleased`; re-check with `git tag --merged` rather than trusting that sentence. |

There is no Consequences section. What a decision costs belongs in the same
paragraph as the decision, not in a list underneath it that nobody updates.

## Writing one

Copy `0000-template.md`, take the next free number, and name the file
`NNNN-a-short-imperative-title.md`. Then add the record's id to its authority's
doc — a package doc comment for a package, the page itself for a page — because
the anchor is checked in both directions:

- a record naming an authority whose doc does not carry the record's id fails;
- a doc carrying an id that no record declares fails.

That is what puts the decision in front of the person about to violate it.
`go doc ./internal/statelog` shows `ADR-0002` on its first screen, and the
record it names is a page long rather than the framework's whole argument.

The gate is `internal/adr`, it runs inside `make check`, and it is the thing
without which this directory becomes `docs/reference/design-decisions.md` —
which was this repository's ADR register under another name, carried fifteen
decisions, was referenced by nothing, was read by nobody, and had drifted into
six false statements by the time anybody checked.

## The records

| # | Decision | Enforced by |
|---|---|---|
| [0001](0001-one-broker-carries-the-stream-and-the-coordination-store.md) | One broker carries both the stream and the coordination store | `TestNoPackageBuildsASubjectByHand`, `queuetest` |
| [0002](0002-the-stream-is-the-write-ahead-log.md) | The stream is the write-ahead log; the SQL estate is derived | `TestOnlyTheApplierWritesTheReplicatedEstate` |
| [0003](0003-fleet-agreement-state-lives-in-coordination.md) | Fleet-agreement state lives in coordination, never a node's own file | `TestEveryNodeTableSaysWhoHasToAgreeOnIt` |
| [0004](0004-a-node-keeps-two-database-files.md) | A node keeps two database files, and nothing spans them | `TestNoStatementNamesBothEstates` |
| [0005](0005-ownership-is-three-valued.md) | "Do I hold this?" has three answers, never two | the `(value, error)` signature |
| [0006](0006-event-evolution-is-additive-only.md) | Event evolution is additive-only and unknown types round-trip | `TestAnUnknownTypesLargeIntegersSurviveARoundTrip` |
| [0007](0007-turso-is-the-only-store-driver.md) | Turso is the only store driver, and it bounds the release matrix | `TestTursoIsTheOnlyDriverInTheBinary` |
| [0008](0008-a-shared-rule-gets-one-implementation.md) | A rule more than one package needs gets exactly one implementation | `TestNoClientSitsOnTheProcessGlobalPool` and its siblings |
| [0009](0009-one-activation-pointer-applied-per-node.md) | Which revision is current is fleet-wide; applying it is per node | nothing — declared |
| [0010](0010-tracing-is-configured-by-the-standard-otel-environment.md) | Tracing is configured by the standard OTel environment, not by Tier A | nothing — declared |
| [0011](0011-tier-a-is-the-root-of-trust.md) | Tier A resolves from the environment and nothing else | `TestTierAIsNeverResolvedFromTheSecretStore` |
| [0012](0012-a-wake-is-derived-not-published.md) | A wake is derived from a durable record, never published by the writer | `TestAnUnreachableClaimStorePublishesAnyway` |
| [0013](0013-a-seats-identity-is-derived.md) | A seat's identity is derived, never looked up | `TestDeriveAgentIDIsStable` |
| [0014](0014-a-compacted-changelog-is-the-fourth-answer.md) | A compacted changelog is the fourth answer to "who has to agree on it?" | `TestASeatsMemoryCrossesToANodeThatHasNeverRunIt` |
| [0015](0015-an-alarm-borrows-its-threshold.md) | An alarm fires at a threshold another decision already made | `TestTheBackupAlarmFiresAtTheAgeThePolicyNames` |
| [0016](0016-a-protocol-bump-refuses-where-an-envelope-round-trips.md) | A coordination protocol bump refuses where an event envelope round-trips | nothing — declared |
| [0017](0017-a-turn-id-names-one-run.md) | A turn id names one RUN; the work key names the unit of work | `TestARedeliveredTriggerRunsUnderItsOwnIdentity` |
| [0018](0018-a-node-without-data-reaches-the-estate-through-one-that-holds-it.md) | A node without data reaches the estate through a node that holds it | `TestASeatOnAStatelessNodeWritesThroughADataNode` |
