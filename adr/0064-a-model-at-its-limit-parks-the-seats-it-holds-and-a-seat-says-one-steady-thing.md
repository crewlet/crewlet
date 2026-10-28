# ADR-0064: A model at its limit parks the seats it holds, and a seat says one steady thing

- **Status:** accepted
- **Authority:** `internal/engine`
- **Enforced-by:** `internal/engine.TestASpentModelParksTheSeatUntilItsResetAndThenDeliversOnce`, `internal/engine.TestAProbeThatMeetsTheLimitArmsTheParkAgain`, `internal/engine.TestAFortyHourLimitDeliversTheHeldMailOnceWithItsDeliveriesIntact`, `internal/engine.TestSeatsOnOneLimitedEntryShareOneProbe`, `internal/engine.TestAResumeThatMeetsALimitMidTurnWaitsForTheParksRelease`, `internal/engine.TestAPublishedCompanyResumesACompletionKeptForALimit`, `internal/engine.TestASpentHeadWithAFailingFallbackIsNotParked`, `internal/engine.TestNewModelsReleaseTheParkAtOnce`, `internal/engine.TestAResumeAtItsModelsLimitIsAWaitNotAFailure`, `internal/engine.TestACompletionDuringALimitIsNeverHandedBackAndResumesOnce`, `internal/sandbox.TestACompletionWhoseResumeMeetsALimitIsKeptAndResumedOnce`, `internal/sandbox.TestAnAnswerWhoseResumeMeetsALimitWaitsUncharged`, `internal/providers/llm/chain.TestAChainIsLimitedOnlyWhenEveryMemberIs`, `internal/providers/llm/cliagent.TestClaudeCodesLimitIsReadFromItsStatusWithTheReset`, `internal/providers/llm/cliagent.TestAnAnswerOpeningWithTheLimitsWordsIsAnAnswer`, `internal/providers/llm/cliagent.TestAClockResetIsTheNextOccurrenceInItsZone`, `internal/providers/llm/cliagent.TestAnUnnamedResetBacksOffAndStartsAgainOnceAnswered`, `internal/api/livestate.TestAParkedSeatReadsStoppedThroughEveryProbe`, `internal/api/livestate.TestTheBroadcastLimitCarriesNoConfigKey`, `internal/api/livestate.TestNoBroadcastFieldCarriesAConfigKeyOrARawFailure`, `internal/api/queries.TestTheSpendByEntryIsAnAdminsAlone`, `internal/api/stream.TestTheSpendPushNamesEntriesToAnAdminAlone`, `internal/api/livestate.TestASpendRecordIsNeverNamedByItsEntry`, `internal/api/queries.TestTheScreenOffersAnAdminsSplitsToAnAdminAlone`, `internal/engine.TestTheLastSeatOutEndsTheProbesCall`, `internal/engine.TestAStopWaitsForAProbesCall`, `internal/engine.TestAnOlderProbeCallLeavesTheNewerOnesCancel`, `internal/api/livestate.TestAnExhaustedChainIsAFailedTurnNotAStop`, `internal/config.TestEveryBuiltInCLIHasAProductName`, `internal/agent/turn.TestAResumeAnswersOnlyForWhatThisAttemptWrote`, `internal/agent/runner.TestAResumedRecordMarksTheParkedRoundsCallsAsInherited`, `internal/engine.TestARetriedResumeWaitsForALimitItsModelsKnow`, `internal/caveat.TestTheDashboardKnowsEverySettingARemedyNames`, the dashboard's `LimitRemedy.test.tsx`, `Inbox.test.tsx` ("a burst of live pushes"), `providerLimit.test.ts` and `store.test.ts`
- **Measured:** a five-seat company on one Claude Code subscription, 2026-10-27, from 16:16 local when the five-hour window ran out until its 16:30 reset: 60 to 93 `batch_handler_failed` a minute, one partition failing 64 times in eight minutes, and every failed turn also failing a prefetch, a reflection and a compaction call. A forty-second sample of the dashboard socket carried 1207 frames (thirty a second), of which 529 were seat pushes (thirteen a second) and 64 were turns started and failed; each seat moved between working, idle with a failed turn and stopped about once a second. With the park, the storm test runs one refused turn where it ran 25.
- **Cost-when-tried:** the limit was classified as a server failure (Claude Code had reworded it, and nothing read its 429), so nothing sat the entry out; the dispatcher NAKed every refused turn on a backoff of one second doubling to thirty, per partition, with new notifications joining each; and the seat state read `stopped`/`provider` from the last turn's `llm_unavailable` until the next turn began a few tens of milliseconds later. The Inbox drew each seat as a "stopped" row or a "last turn failed" row under two ids, so every flip removed one row and inserted another, which moved every row below it. Two coding runs resumed into the limit were abandoned with their results: the abandon rule read the writes the attempt that parked had made as the resume's own. In review of the first version of this record: its probe was the held delivery's own turn, parked again by a deferral that spends one of the message's deliveries, so a limit named 40 hours out would have dead-lettered the held mail after 24 hourly probes (about five and a half hours for a reset nobody named); and a completion resumed into the limit was reverted and NAKed through its 25 deliveries in about ten minutes.
- **Tag-status:** unreleased

## The decision

**A model's limit is read from what its CLI writes, and its end from what the
vendor says.** A profile's `limit_events` match the CLI's own structured
report (Claude Code: a `result` event with `is_error` and `api_error_status:
429`) before any sentinel, on fields a model cannot write; a reset reads as an
epoch, a delay, or a time of day in a named zone (`reset_unit: clock`), taken
as its next occurrence (one passed by less than five minutes, the window
turning over as the report is read, is no instant). The words that open a
report are not a sentinel where they open an answer as readily ("You've hit
your"). A limit whose reset is not read sits the entry out for 1, 2, 5, then
15 minutes per probe that meets it again, and from the first step once a call
answers.

**A seat whose whole chain is limited is parked, not retried.** When every
member of the chain a turn ran on refused for a rate limit with an instant it
is tried again (`chain.Limited`), the dispatcher parks the seat until the
earliest of them, with the machinery the budget park uses (one `seatParks`,
two kinds): a hold on its inbox under its own reason and the delivery deferred
so it keeps its place. Mail that arrives meanwhile waits behind it. **One probe
per limited chain, outside any delivery**: seats parked on the same entries of
the same registry share one alarm, and at each retry instant one call goes
through the members that refused, run as the parked seat that sorts first and
charged and recorded through the auxiliary seam (the background stage,
`provider_probe`). A limit that still holds arms the alarm again and publishes
`seat_provider_parked` again for each seat, and no inbox is touched; an answer,
or a failure that is not a limit, lifts every hold on the chain, publishes
`seat_provider_released` with why, and each delivery runs once, in order, the
held one having spent only the deferral its park took. **What a probe costs**:
a full run of the entry's CLI, a process launched and a request carrying the
CLI's own fixed overhead, which a one-word prompt does not shrink; one per retry
instant (hourly at most for a named reset, 1, 2, 5, then every 15 minutes for an
unnamed one) for the whole company's seats on that chain, where one per seat
would multiply it by the seats. A chain with one member
that failed otherwise is retried as any failed turn is. An apply that builds
new models releases every park at once. Every release, and every published
company, signals the resumes waiting on a limit (`waitProvider`).

**A seat's state says one steady thing for as long as the limit lasts.** The
projection holds `provider_limit` from the park or the rate-limit hand-off
until a phase completes through the seat's models, a release, or a new
instance; a parked seat reads `stopped`/`provider` through every probe.
Livestate reaches every reader, so the condition carries only what a person is
told: the CLI it is of and whether it signs in on a plan, whether the seat
waits or runs on a fallback (unnamed), when the vendor said it lifts, and since
when. The entry keys that move it are held and never broadcast, and neither is
the vendor's sentence. **The same holds for every live field**: a failure's own
text (`last_error` and a failed call's `error`, as `detail`) and a CLI run's
`provider_key` reach only a reader of the company's configuration
(`livestate.Reach.Config`, an admin), whatever their reach to the seat's body;
everybody else is told a failure by its kind, which the dashboard words, and
the full text stays in the engine's log and on the turn's record. So does the
spend breakdown by entry (`by_provider` on the `tokens` answer and push, and a
`token_series` banded by `provider`), which is everybody's question but whose
per-entry half is an admin's. A seat's history read is not a broadcast: it is
held to `audit.seat.read`, which a lead holds over that seat's trail.
`llm_unavailable` is a failed turn and no longer a stop: nothing says such a
failure lasts.

**A person is shown one condition per kind of limit, in their words, and a
list that does not move when nothing it shows did.** The Inbox draws "Claude
Code subscription limit reached. Agents resume at 4:30 PM." (the time in the
reader's zone; "Resume time unknown." where the vendor did not say; who runs on
a fallback model where one carries them) from `provider_limit` alone, the CLI
named through `CLI_PRODUCTS`, one condition per CLI and sign-in kind. **The
event carries the CLI and whether it signs in on a plan** (`seat_provider_parked`'s
`cli` and `subscription`, `provider_fallback`'s `from_cli` and
`from_subscription`) rather than the reader resolving the entry against the org
projection's `models` list: which entries a company configures is an admin's to
read (the org projection hands only an admin each seat's chain), and a reader
that looked the entry up would need that map, while naming the fallback would
hand every reader it. A seat's own condition
keeps one id whether it is stopped or its last turn failed. The store keeps a
row a push leaves as it was and tells nobody of a push that moved nothing, and
the Inbox keeps the rows it drew while they say the same thing.

**A resume answers for what its own attempt wrote.** A resumed phase's record
opens with the calls the round that parked made before it suspended
(`turn.Work.Inherited`), so the record stays whole for the delivery check and
the ledger, while `turn.Result.Acted` (what `turn.Abandon` reads) counts only
the calls this attempt made. A resume that broke before a new round (a usage
limit, a runner it could not build) is reverted and retried, never abandoned;
one that wrote again and then broke is abandoned as before. **A resume never
runs into a limit its models already know, and one that meets a limit is a
wait:** the turn gate every resume passes (ADR-0063) asks the seat's spent
budget and its models' limit after the node's admission and before the claim,
and refuses with `waitBudget` or `waitProvider` until the instant either
names; a resume that meets either before it writes is reverted and refused the
same way. A completion so refused is acknowledged, kept, and re-fired when the
condition's signal comes (the waiter announcing it again meanwhile), and an
answer waits uncharged; neither spends deliveries or attempts on the wait.

**Amended by [ADR-0066](0066-every-executor-is-its-coding-clis-own-run.md), for
an executor's run.** No round parks with its calls in its conversation: a
resumed executor's record is its run's calls over the bridge, made before the
segment that collects it, and those are the calls it inherits. The resume's
first model call is its reviewer's, and an answer's is the relaunch that
carries the run on, whose every runner member sitting out is the same limited
chain. So the turn gate is told the resume's route and asks each of its own
first call: a completion of its reviewer's chain and a full window, an answer
of the runner members its relaunch can take and a window long enough for that
launch. A budget that refuses either a round later is waited out on its
window, ahead of the abandon: a completion is held collected in its paused box
(`sandbox.ErrResumeHeld`), and an answer is reverted and waits uncharged in
its own series.

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
- **Let the held delivery be the probe.** It is the simplest probe there is,
  and every one spends a delivery of the message it holds: a long limit
  dead-letters the mail the park exists to keep.
- **Name the limited entry on the condition.** The dashboard could group by it
  directly, but the condition reaches every reader, and the company's
  configuration is an admin's.
- **Debounce the flicker in the dashboard.** It hides the churn on one screen
  while every other reader, and the engine's own spend, still pays for it.
