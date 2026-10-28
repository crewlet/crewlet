# ADR-0064: A model at its limit parks the seats it holds, and a seat says one steady thing

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestASpentModelParksTheSeatUntilItsResetAndThenDeliversOnce`, `internal/engine.TestAProbeThatMeetsTheLimitParksTheSeatAgain`, `internal/engine.TestASpentHeadWithAFailingFallbackIsNotParked`, `internal/engine.TestNewModelsReleaseTheParkAtOnce`, `internal/providers/llm/chain.TestAChainIsLimitedOnlyWhenEveryMemberIs`, `internal/providers/llm/cliagent.TestClaudeCodesLimitIsReadFromItsStatusWithTheReset`, `internal/providers/llm/cliagent.TestAClockResetIsTheNearestOccurrenceInItsZone`, `internal/providers/llm/cliagent.TestAnUnnamedResetBacksOffAndStartsAgainOnceAnswered`, `internal/providers/llm/cliagent.TestTheLimitReportHoldsUntilAnAnswerOrTheNamedReset`, `internal/api/livestate.TestAParkedSeatReadsStoppedThroughEveryProbe`, `internal/api/livestate.TestAnExhaustedChainIsAFailedTurnNotAStop`, `internal/config.TestEveryBuiltInCLIHasAProductName`, `internal/agent/turn.TestAResumeAnswersOnlyForWhatThisAttemptWrote`, `internal/agent/runner.TestAResumedRecordMarksTheParkedRoundsCallsAsInherited`, `internal/engine.TestARetriedResumeWaitsForALimitItsModelsKnow`, `internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames`, the dashboard's `LimitRemedy.test.tsx`, `Inbox.test.tsx` ("a burst of live pushes"), `providerLimit.test.ts` and `store.test.ts`
- **Measured:** a five-seat company on one Claude Code subscription, 2026-10-27, from 16:16 local when the five-hour window ran out until its 16:30 reset: 60 to 93 `batch_handler_failed` a minute, one partition failing 64 times in eight minutes, and every failed turn also failing a prefetch, a reflection and a compaction call. A forty-second sample of the dashboard socket carried 1207 frames (thirty a second), of which 529 were seat pushes (thirteen a second) and 64 were turns started and failed; each seat moved between working, idle with a failed turn and stopped about once a second. With the park, the storm test runs one refused turn where it ran 25.
- **Cost-when-tried:** the limit was classified as a server failure (Claude Code had reworded it, and nothing read its 429), so nothing sat the entry out; the dispatcher NAKed every refused turn on a backoff of one second doubling to thirty, per partition, with new notifications joining each; and the seat state read `stopped`/`provider` from the last turn's `llm_unavailable` until the next turn began a few tens of milliseconds later. The Inbox drew each seat as a "stopped" row or a "last turn failed" row under two ids, so every flip removed one row and inserted another, which moved every row below it. Two coding runs resumed into the limit were abandoned with their results: the abandon rule read the writes the attempt that parked had made as the resume's own.
- **Tag-status:** unreleased

## The decision

**A model's limit is read from what its CLI writes, and its end from what the
vendor says.** A profile's `limit_events` match the CLI's own structured
report (Claude Code: a `result` event with `is_error` and `api_error_status:
429`) before any sentinel, on fields a model cannot write; a reset reads as an
epoch, a delay, or a time of day in a named zone (`reset_unit: clock`), taken
as the occurrence nearest the report. A limit whose reset is not read sits the
entry out for 1, 2, 5, then 15 minutes per probe that meets it again, and from
the first step once a call answers. The provider holds the report (`llm.Limit`)
from the first refusal until a call through the entry answers or the named
reset passes: the window ends at every probe, the fact does not.

**A seat whose whole chain is limited is parked, not retried.** When every
member of the chain a turn ran on refused for a rate limit with an instant it
is tried again (`chain.Limited`), the dispatcher parks the seat until the
earliest of them, with the machinery the budget park uses (one `seatParks`,
two kinds): a hold on its inbox under its own reason, the delivery deferred so
it keeps its place, and an alarm that lifts the hold. Mail that arrives
meanwhile waits behind it, and each delivery runs once, in order, when the
park lifts. The turn that runs then is the probe: a limit that still holds
parks the seat again. A chain with one member that failed otherwise is retried
as any failed turn is. An apply that builds new models releases every park at
once. The park publishes `seat_provider_parked` at each park and
`seat_provider_released` only for a release other than a probe.

**A seat's state says one steady thing for as long as the limit lasts.** The
projection holds `provider_limit` (the entry, the CLI it drives and whether it
signs in on a plan, whether the seat waits or runs on a fallback, when the
vendor said it lifts, since when) from the park or the
rate-limit hand-off until a phase completes through the seat's models, a
release, or a new instance; a parked seat reads `stopped`/`provider` through
every probe. `llm_unavailable` is a failed turn and no longer a stop: nothing
says such a failure lasts.

**A person is shown one condition per limited entry, in their words, and a
list that does not move when nothing it shows did.** The Inbox draws "Claude
Code subscription limit reached. Agents resume at 4:30 PM." (the time in the
reader's zone; "Resume time unknown." where the vendor did not say; who runs on
a fallback model where one carries them) from `provider_limit` alone, the CLI
named through `CLI_PRODUCTS`. The limit carries the CLI's name and sign-in kind
rather than the reader looking the entry up: which entries a company configures
is an admin's to read (the org projection hands only an admin each seat's
chain), and naming the fallback would hand every reader that map. A seat's own condition
keeps one id whether it is stopped or its last turn failed. The store keeps a
row a push leaves as it was and tells nobody of a push that moved nothing, and
the Inbox keeps the rows it drew while they say the same thing.

**A resume answers for what its own attempt wrote.** A resumed phase's record
opens with the calls the round that parked made before it suspended
(`turn.Work.Inherited`), so the record stays whole for the delivery check and
the ledger, while `turn.Result.Acted` (what `turn.Abandon` reads) counts only
the calls this attempt made. A resume that broke before a new round (a usage
limit, a runner it could not build) is reverted and retried, never abandoned;
one that wrote again and then broke is abandoned as before. A retried resume
also waits for a limit its models already know (`waitProvider`, beside
`waitBudget` in the resume's admission) rather than running a turn to meet it.

**The way out is the remedy #427 introduced.** The condition carries the
`model_providers` remedy (a `caveat.Setting`), which the dashboard's link table
resolves to Settings › Models, behind the same admin gate as that screen; a
reader without Admin is told an admin can change the company's models.

## Why not the alternatives

- **A longer NAK backoff.** It spaces the storm without ending it, keeps
  spending the message's delivery budget toward the dead-letter queue, and
  still knows nothing of the reset the vendor named.
- **Park on the head's limit alone.** A seat whose fallback answers is
  working, and parking it would stop work the company is paying the fallback
  for; one whose fallback failed for a passing reason would wait an hour for
  a blip.
- **Debounce the flicker in the dashboard.** It hides the churn on one screen
  while every other reader, and the engine's own spend, still pays for it.
