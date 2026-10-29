# ADR-0051: A retired reference keeps its object for a grace

- **Status:** accepted
- **Authority:** `internal/objstore/references`
- **Enforced-by:** `internal/objstore.TestADeclarationStatesItsStanding`, `internal/objstore.TestAWalkReadsOnlyTheStandingsItsPassIsOwed`, `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestEveryDeclarationStatesItsStanding`, `internal/objstore/references.TestARetiredTableIsSweptAndOutlivesItsLongestRead`, `internal/objstore/collect.TestARetiredReferenceKeepsItsObjectFromCollection`, `internal/objstore/collect.TestTheAuditAsksOnlyAfterRequiredReferences`, `internal/objstore/collect.TestADerivedReferenceIsKeptAndNeverAudited`, `internal/objstore/collect.TestAStoreMoveWalksTheRequiredAndDerivedTablesAlone`, `internal/objstore/migrate.TestAStoreMoveCopiesADerivedObjectVerified`, `internal/objstore/migrate.TestADerivedObjectTheSourceCannotGiveBackHoldsNothingUp`, `internal/backup.TestABackupCarriesNoRetiredReference`, `internal/backup.TestABackupCarriesNoDerivedObject`, `internal/backup.TestABackupRefusesADeclarationThatStatesNoStanding`
- **Cost-when-tried:** the object store's page and four comments promised a grace from un-naming that the collector never gave — "the older copy goes a day after nothing names it" — so an operator could have planned a recovery from the bucket that was not there (corrected in ce9454996). The collector's grace is an UPLOAD's; before this record the only way to keep an object past its un-naming was to keep a live row naming it, which the audit checks and every backup carries
- **Tag-status:** unreleased

## The decision

Every table declared in `internal/objstore/references` states a **standing**
(`objstore.Standing`), and there is no default:

- **`Required`** — a live reference. It keeps its object from collection, the
  collector's daily audit asks the store for it (`objects_missing`), a backup
  carries it — copied into `objects/` on `s3`, asked after once the bucket's
  stream snapshot is taken on `nats` — and a store move
  (`crewlet objects migrate`) copies it into the store the fleet moves to.
- **`Retired`** — keeps its object from collection and does nothing else. The
  audit never asks after it, and a backup neither copies it, asks after it nor
  records it lost; on `nats` its bytes ride the bucket's snapshot like every
  other object the bucket holds, uncounted. A store move leaves it behind: the
  move ends with every node restarted onto the new store, so no reader is left
  that has not caught up with what replaced it.
- **`Derived`** — names an object the engine can build again from what
  `Required` rows name, so losing one costs a rebuild and no data. It keeps its
  object from collection while a row names it, exactly as a `Required` row
  does, and a store move copies it as it copies a `Required` one; it does
  nothing else: the audit never asks after it, since `objects_missing` is the
  alarm for lost data and would page somebody about a loss nobody has to
  repair, and a backup never copies, counts or asks after it — on `nats` its
  bytes ride the bucket's snapshot uncounted, as a retired object's do — since
  a restore can do without it. A store move that cannot read one back whole
  counts it apart and leaves it behind, naming no file and holding nothing up.

The zero value is refused by `ReferenceTable.Validate`, so a declaration that
says none of the three fails the references gate, every statement built from
it, the collector's construction at boot and every backup handed it, and the
gate holds each declaration's standing to a reviewed row naming why it has
that one. Each of the three passes that walk the declared tables builds its
walk for itself (`objstore.Pass`, whose `Reads` is the table of obligations'
walk columns): the audit's and a backup's read `Required` tables alone and
refuse the other two, and a store move's reads `Required` and `Derived` tables
and refuses a retired one — so a consumer that forgets to leave a table out
fails on its first pass rather than carrying it.

A backup and a store move part over a derived object for what each costs. A
backup is taken again and again and restored rarely, so carrying an object the
engine can build again would cost its bytes at every backup to spare a rebuild
after a restore. A move is made once, and a derived object it left behind would
be built again by every node of the moved fleet, at a cost that grows with what
it was built from rather than with anything that changed, where a copy costs
its bytes once, through the same verified read the move makes of every object.

The grace is the retired **row's lifetime**. Whatever retires a live row writes
the retired one; its own domain deletes that row once the domain's grace has
passed; the collector's next pass then deletes the object as it deletes
anything nothing names. No retirement timestamp reaches the collector and it
still takes no lock — ADR-0027's judgement is unchanged. The declaring domain
owns the sweep and a grace that outlasts the longest read of what its table
names. The first retired table is `code_packs_retired`, the code host's
retired packs, swept by the code-retention duty's `retire_sweep` records
([ADR-0060](0060-a-compaction-merges-packs-without-judging-reachability.md)); the
references gate refuses any further one declared until it has a reviewed
entry naming its grace, the largest object its rows name and what sweeps them
— a job of the engine's maintenance worker, or a duty whose records delete
them — and holds the grace above the longest read of that object. A restore
on `s3` brings retired rows back without their objects, and nothing is owed
them.

A derived row owes **no grace**, because its domain owes every reader of the
object a way on without it instead: a reader whose read of it fails reads the
rows again for what replaced it, or builds it. So the domain may delete a
derived row the moment it replaces what the row names, the collector's next
pass deletes the object, and a derived declaration needs no reviewed entry. A
restore on `s3` brings derived rows back without their objects, and the engine
builds them again.

Four packages carry it besides the list: `objstore` (the type, the refusal,
each pass's walk), `objstore/collect` (every table shields, Required tables are
audited, and a store move walks Required and Derived ones), `objstore/migrate`
(a store move copies both, and a derived object the source cannot give back
never holds it up, as it never pages anybody) and `backup` (every declaration
is validated, and Required tables are read from the copy).

## Why the obvious alternatives are wrong

**A bool `Required`.** Its zero value is one of the two answers: a new live
table whose author left the field out reads as retired, and is audited by
nobody and accounted for by no backup with nothing failing. Nor could it hold
the third standing.

**Declare an object the engine builds again `Required`.** A lost one would
raise `objects_missing`, the alarm for lost data, about something nobody has
to restore, and every backup would carry bytes a restore can do without.

**…or `Retired`.** A retired row keeps its object only for a grace its domain
must sweep, and its declaration owes a reviewed entry; a derived object is
live for as long as a row names it, and goes the moment its row does.

**Leave a derived object behind when the store moves, as a backup does.** The
moved fleet would then hold rows naming objects its store lacks, and every node
would build each of them again — for an index over a repository's history, a
pass over that whole history on every node, repeated for every node added
later — where the move had the bytes in hand and a verified read away.

**Keep the replaced object named by its live row**, under a state column. It
gives the grace and costs everything else: the audit pages somebody when a
replaced object goes missing although nobody needs it, and every backup
accounts for bytes no restore wants — a GET per retired object per backup on
`s3`.

**Rely on `collect.PendingGrace`.** Both instants it judges date the upload, so
an object older than a day goes at the first hourly pass after its last row
stops naming it — the gap the Cost line records.

**A retirement timestamp the collector reads** (keep an unnamed object until
`retired_at` plus a grace). That puts a second clock, and a domain's notion of
"no longer needed", inside the one judgement ADR-0027 keeps to "is it named";
the row's presence is the whole signal.

## What this does not decide

How long a grace is, how a domain stamps a retired row, or who sweeps it — the
declaring domain's; so is what a derived object is built from, and how its
readers go on without it. Which objects exist and where they are kept
([ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md));
when an unnamed object may be deleted
([ADR-0027](0027-a-deletion-from-the-shared-store-needs-no-lock.md)). A
project's file rows are `Required` and never retired.

## Amendment — the first derived table is a repository's published layers

[ADR-0061](0061-a-derived-object-is-published-kept-and-never-audited.md)
gives `Derived` its first table, `code_layer_parts`: the parts of a
repository's published multi-pack-index layer, its bitmap and its commit-graph
layer, which one git node publishes and every other installs. For it, "the
engine builds them again" is that record's rule: a git node whose layer is
missing or damaged serves from the layers below it, and the repository's
maintainer publishes a replacement.
