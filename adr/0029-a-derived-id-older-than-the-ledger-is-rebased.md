# ADR-0029 — A derived operation id older than the ledger is rebased, by every attempt, onto an instant the fleet records

- **Status:** accepted
- **Authority:** `internal/statelog`
- **Enforced-by:** `internal/statelog.TestAnAttemptRebasesOnlyPastTheHorizon`, `internal/engine.TestALaterAttemptIsJudgedAtItsOwnClock`, `internal/engine.TestALaterAttemptInheritsTheRebaseWhileItCan`, `internal/engine.TestADispatchPastTheHorizonMintsAtItsAttempt`, `internal/engine.TestAResumePastTheHorizonMintsAtItsAttemptOrItsHalfsRebase`, `internal/coord/coordtest.TestTheRetentionsOutlastWhatTheyCover`
- **Cost-when-tried:** the first fix decided the rebase once, at a resume's first attempt, and kept that instant on the parked run's row until the run's next launch cleared it. A resume that failed at twenty-eight days and succeeded at forty, on a person's second answer, minted at the work's start and lost every write it made on every node; and the second half of a rebased turn, judged against the work's start again, rebased onto its own instant two hours after the first half's and wrote twice what the first half had written and the second repeated. A dispatch of a month-old backlog had the same loss and no fix at all.
- **Tag-status:** unreleased

## The decision

A derived operation id carries the instant its unit of work began, so a retry
reproduces it — until that instant is further behind the attempt than the
operation ledger remembers. Then the attempt **rebases**: it mints at its own
instant and records that instant in the fleet's coordination store under the
ids' seed (`coord.Rebases`) before it writes anything, and every later attempt
at the same work — the re-run a redelivery is, a retried resume, the next half
of a parked turn, on any node — inherits it while it is recent enough to.

**Every attempt is judged against its own clock**, by one rule
(`statelog.MintAt`, with `statelog.MintHorizon` as the line), applied by one
function in the engine (`rebaseFor`) from the one frame each path builds the
turn its tools read from — the dispatch's and the resume's. The rule and the
horizon's sizing are stated at the authority. The record and how long it is
kept are coordination's (`coord.Rebases`, `coord.RebaseRetention`), and
`coordtest`'s retention guard holds that age to the horizon and to the ledger's
retention.

## Why the obvious alternative is wrong

The obvious alternative is to decide once and carry the decision with the run:
stamp the instant on the parked run's row at the resume's first attempt and let
every retry reuse it. It is wrong on both ends. A retry is not bounded in time
— a person's next answer can come weeks later, and a completion is re-fired
for as long as a provider is down — so an instant judged recent at the first
attempt is as far past the ledger as the start was by the attempt that runs,
and the loss returns. And a row is the wrong owner: the instant belongs to the
unit of work, which outlives one launch, so a relaunch that cleared it was
judged against the start again and gave up the collapse between two halves an
hour apart. Recording it under the seed in the coordination store is what lets
a dispatch's re-run, a resume and the next half all read one answer — the dispatch
path has no row at all.

The other alternative, minting every attempt at its own clock, is the one the
derived id exists to prevent: a re-run would write again whatever the attempt
before it wrote.

## What this does not decide

How long the ledger keeps its rows (`statelog.OpsRetention`), or whether it can
vouch for an id ([ADR-0002](0002-the-stream-is-the-write-ahead-log.md)'s
framework does). Which identity a turn's ids are seeded from — the work key or
the run — is [ADR-0017](0017-a-turn-id-names-one-run.md)'s. And it does not make
the collapse survive the line: an attempt judged just short of the horizon and
a retry judged just past it write under two instants, which is the cost this
decision accepts against every write lost.
