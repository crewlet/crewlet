# ADR-0119: Operating insights are typed weekly records under a consent of their own

- **Status:** accepted
- **Authority:** `internal/insights`
- **Enforced-by:** `internal/insights.TestARecordHoldsNoPlainString`, `internal/insights.TestARecordBuiltOutsideThePackageFailsValid`, `internal/insights.FuzzNoFindingTextReachesARecord`, `internal/insights.TestTwoCompaniesWithOnePatternSendTheSameBytes`, `internal/insights.TestEveryClassIsSmallAndClosed`, `internal/insights.TestEveryClassIsReadOffItsDetectorsOwnFindings`, `internal/insights.TestEveryFindingOfAClassFoldsIntoOneRecord`, `internal/insights.TestARecordNamesTheBuildByItsTagOrMain`, `internal/insights.TestARecordOutsideTheCatalogueIsRefused`, `internal/insights.TestAPublishedClassShapeIsImmutable`, `internal/insights.TestEveryRecordFitsItsBound`, `cmd/crewlet.TestTheCheckedInInsightsCatalogueIsWhatTheGeneratorEmits`, `internal/insights.TestReviewModeSendsNothingUntilSend`, `internal/insights.TestOffMeansOffAcrossARestart`, `internal/insights.TestAnUnreadConsentForgetsNothing`, `internal/insights.TestAnUnacceptedSchemaSharesNothing`, `internal/insights.TestOneClassesWeekLeavesOnceAcrossNodes`, `internal/insights.TestAStoredPayloadOutsideTheCatalogueNeverLeaves`, `internal/api/configapi.TestOnlyAnAdministratorMayTurnInsightsOn`
- **Cost-when-tried:** not tried here. Before this record the only way to learn which operating patterns are common across companies was a manual analysis of one company's own data, and the defect reporter (ADR-0101) excludes usage by design.
- **Tag-status:** unreleased

## The decision

**How a company's agents and people work is shared, if at all, as typed weekly
records under a switch of its own, never through the defect reporter.** ADR-0101
promises that a defect report carries nothing about usage, and an operating
pattern is usage, so widening that consent would break the promise every
company that turned defects on has already accepted.

**A record is an allowlist, held by its type.** `insights.Record` is sealed: its
fields are unexported, so only `insights.Records` makes one, and a record built
or decoded anywhere else is the zero record, which fails `Record.Valid`. It holds
no plain string: a class of the catalogue, the ISO week, the build's release tag
or `main`, and at most four facts, each a label of a closed set of at most six. A
fact is a bucket of a share or a count, never the count. A class has at most 64
combinations of facts, so a record is a handful of bits. Never sent: a name of a
role, seat, person, unit, server or host; an item key or title; any text; a raw
count or duration; the commit; an instant finer than the ISO week; anything
about the topology or the configuration.

**Only the advisor's detectors feed it.** `insights.Records` reads the findings
the advisor's deterministic detectors raised from the company's own records
(ADR-0115) and nothing else. A model may explain an insight locally and draft a
fix through its previewed approval (ADR-0051); it may never choose a class, a
fact or a bucket, or reach the path that sends, because a value a model chose
would be a channel that company content could steer.

**One record per class per week.** Every finding of a class folds into one
record: the worst finding's facts and how many seats or people raised it, as a
bucket. Two companies with the same pattern send the same bytes, and no record
carries an install id, a nonce or a sequence, so no field joins one record to
another.

**One catalogue both ends vendor.** The table in `internal/insights` is the
catalogue; `schema/insights-catalogue.json` is its generated export. The relay's
insights route, a separate change, is to validate against its vendored copy, and
its CI must compare that copy to this file. A published class's shape (its
facts, labels, floors and the evidence they read) is immutable: each schema
is frozen in `schema/insights-history/`, a change is a new class id or a schema
bump, and the consent names the schema it accepted, so a build whose catalogue
is newer shares nothing until an administrator accepts it again. Only a public
build sends, by ADR-0101's own origin rule.

**Consent is its own, an administrator's, and off by default.** The switch is
separate from the defect reporter's. Turning it on shows the whole catalogue and
the never-sent list. Its default mode is review: each eligible record waits
beside its local evidence until an administrator sends it, declines it, or
always sends its class; sending without review is a separate explicit choice.
Off means off, across restarts: every record that waits is forgotten, and a
consent that cannot be read is never taken for off. A class is eligible when
its finding holds on two days in a row of one ISO week, at most one record per
class per company per week (a compare-and-set record in the coordination store,
as for defects), each sent at its own random delay within a day.

**The relay keeps aggregates, never reports.** A route of its own on the
existing relay validates a record schema-exactly against the vendored
catalogue, files no issue, keeps no per-report row and no client address,
increments one private counter per week, class, fact tuple and version, and
reads any cell under 3 as "under 3". Its traffic never consumes the defect
reporter's slots.

## Why the obvious alternative is wrong

The obvious alternative is a usage section on the defect report. It is one
consent for two different promises: an administrator who agreed to report the
engine's own crashes would find their company's working patterns leaving under
the same switch.

The second alternative is counts and free-form context, scrubbed before
sending. A count is a fingerprint (a company with 37 agent comments in a week is
findable across weeks), and a scrubber fails open on the words a company
writes. Buckets of a closed set carry the pattern and nothing that names the
company.

A record per finding, or per seat, would send a company's shape: how many
agents it runs and which of them misbehave. Folding by class sends the pattern
alone.

## What this does not decide

Which patterns the advisor detects and their thresholds (ADR-0115), what an
administrator sees locally (the advisor's own page), or how maintainers read the
aggregates beyond the small-cell rule. Counts are forgeable, since the engine is
open source and holds no credential, so an aggregate is advisory and never
drives an automatic action.
