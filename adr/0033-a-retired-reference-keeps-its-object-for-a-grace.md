# ADR-0033 — A retired reference keeps its object for a grace

- **Status:** accepted
- **Authority:** `internal/objstore/references`
- **Enforced-by:** `internal/objstore.TestADeclarationStatesItsStanding`, `internal/objstore.TestOnlyARequiredTableIsWalked`, `internal/objstore/references.TestEveryTableThatNamesAnObjectIsDeclared`, `internal/objstore/references.TestARetiredTableIsSweptAndOutlivesItsLongestRead`, `internal/objstore/collect.TestARetiredReferenceKeepsItsObjectFromCollection`, `internal/objstore/collect.TestTheAuditAsksOnlyAfterRequiredReferences`, `internal/backup.TestABackupCarriesNoRetiredReference`, `internal/backup.TestABackupRefusesADeclarationThatStatesNoStanding`
- **Cost-when-tried:** the object store's page and four comments promised a grace from un-naming that the collector never gave — "the older copy goes a day after nothing names it" — so an operator could have planned a recovery from the bucket that was not there (corrected in ce9454996). The collector's grace is an UPLOAD's; before this record the only way to keep an object past its un-naming was to keep a live row naming it, which the audit checks and every backup carries
- **Tag-status:** unreleased

## The decision

Every table declared in `internal/objstore/references` states a **standing**
(`objstore.Standing`), and there is no default:

- **`Required`** — a live reference. It keeps its object from collection, the
  collector's daily audit asks the store for it (`objects_missing`), and a
  backup carries it: copied into `objects/` on `s3`, asked after once the
  bucket's stream snapshot is taken on `nats`.
- **`Retired`** — keeps its object from collection and does nothing else. The
  audit never asks after it, and a backup neither copies it, asks after it nor
  records it lost; on `nats` its bytes ride the bucket's snapshot like every
  other object the bucket holds, uncounted.

The zero value is refused by `ReferenceTable.Validate`, so a declaration that
says neither fails the references gate, every statement built from it, the
collector's construction at boot and every backup handed it. The reference
walk the audit and the backup share (`ReferenceTable.ReferencesAfter`) is
built only for a `Required` table and refused for a retired one, so a consumer
that forgets to leave a retired table out fails on its first pass rather than
carrying it.

The grace is the retired **row's lifetime**. Whatever retires a live row writes
the retired one; its own domain deletes that row once the domain's grace has
passed; the collector's next pass then deletes the object as it deletes
anything nothing names. No retirement timestamp reaches the collector and it
still takes no lock — ADR-0027's judgement is unchanged. The declaring domain
owns the sweep and a grace that outlasts the longest read of what its table
names. No table is retired yet; the references gate refuses the first one
declared until it has a reviewed entry naming its grace, the largest object
its rows name and the job that sweeps them, and holds the grace above the
longest read of that object. A restore on `s3` brings retired rows back
without their objects, and nothing is owed them.

Three packages carry it besides the list: `objstore` (the type, the refusal,
the Required-only walk), `objstore/collect` (every table shields, Required
tables are audited) and `backup` (every declaration is validated, and
Required tables are read from the copy).

## Why the obvious alternatives are wrong

**A bool `Required`.** Its zero value is one of the two answers: a new live
table whose author left the field out reads as retired, and is audited by
nobody and accounted for by no backup with nothing failing.

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
declaring domain's. Which objects exist and where they are kept
([ADR-0026](0026-the-estate-names-an-object-and-a-store-keeps-its-bytes.md));
when an unnamed object may be deleted
([ADR-0027](0027-a-deletion-from-the-shared-store-needs-no-lock.md)). A
project's file rows are `Required` and never retired.
