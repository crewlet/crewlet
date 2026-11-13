# ADR-0141: The advisor lists what the atlas shows needs a person, and names entries only to a holder

- **Status:** accepted
- **Authority:** `internal/advisor`
- **Enforced-by:** `internal/advisor.TestEachAtlasClassNamesTheEntriesItsRuleReads`, `internal/advisor.TestAReaderWithoutTheAtlasAppIsToldHowManyAndNeverWhich`, `internal/advisor.TestAnAtlasFindingNamesAtMostFiveEntriesAndCountsThemAll`, `internal/advisor.TestAnEntryNobodyStandsBehindRaisesNothing`, `internal/advisor.TestADisagreementIsListedAfterAWeekAndNotBefore`, `internal/advisor.TestASourceIsStaleAtThreeReadsAndNotBefore`, `internal/advisor.TestAnUnreadAtlasRaisesNothingAndIsNamed`, `internal/advisor.TestEveryAtlasClassIsRaised`, `internal/api/queries.TestTheAdvisorNamesAnAtlasEntryOnlyToAHolderOfTheAtlasApp`, `internal/api/queries.TestTheAdvisorListsASourceThatStoppedReading`, `internal/insights.TestAnAtlasFindingMakesNoRecord`
- **Cost-when-tried:** not tried. The atlas shows each of these conditions on the entry it is about, and nothing gathers them: a service nobody owns looks like every other card until somebody opens it.
- **Tag-status:** unreleased

## The decision

**The atlas's findings are the setup advisor's, in its own shape.** Five classes join the one list ([ADR-0115](0115-a-setup-finding-carries-its-fix-and-only-a-person-sends-it.md)), each the owner's to act on, none blocking, under the area `atlas` and the source `atlas`:

| Class | Read as | Costs |
|---|---|---|
| `atlas.owner_gone` | an entry on the map whose owner is a seat or a unit the chart no longer holds | `costs` |
| `atlas.unmonitored` | a confirmed service that exposes an address outside the company's networks, where no monitor is recorded for it or for that address | `costs` |
| `atlas.source_stale` | a source read whole at least once and not for three of its reads | `costs` |
| `atlas.unowned` | a confirmed service with no owner a person or the engine's records name | `note` |
| `atlas.disagreement` | an entry holding a fact or a relation its sources have disagreed on for 7 days | `note` |

**Each is read, never stored.** The classes over entries are read off the folded view of the asker's own read of the atlas, and the stale source off the standings the `atlas_sources` question answers. No finding is written anywhere, so one ends when its condition does, and two nodes never raise it twice ([ADR-0116](0116-an-administrator-notice-is-read-off-its-condition.md)'s reasoning).

**Only what the company stands behind is advised about.** A service is counted when it is confirmed: a report nobody checked, or an entry somebody retired, raises nothing. An owner that left is read on every entry still on the map, since an owner is named on whatever it answers for.

**Public is what the record says.** A service is open to the internet when it has an `exposes` relation to an endpoint whose host is not on a private network, the reading `atlas.Ref.Private` already gives. It is watched when a `monitored_by` relation is recorded on it or on an endpoint it exposes. Nothing is guessed from a name or a vendor.

**The thresholds are tied to rules the atlas already keeps.** A disagreement is listed after 7 days, the advisor's own window: every weekday has passed, so whoever reviews the atlas has had a working week to decide it. A source is stale at three of its reads with none whole: an entry is retired after two whole reads stop seeing it ([ADR-0129](0129-an-integrations-read-records-what-changed-and-retires-what-two-reads-missed.md)), so at three what should have left the map still stands on it. A source never read whole has not stopped: its own standing and its integration's findings say why.

**A finding names its entries only to a holder of the atlas app.** The `advisor` question is an administrator's, and an administrator holds every app today. The rule is kept on the finding all the same, where a later reader of the list cannot skip it: with the app, a finding names at most five entries (id, kind, key, name, and the owner that left) and counts them all in its `entries` fact; without it, the same title and count, no entry, no id, and a fix of kind `none` that says who can see which ([ADR-0127](0127-reading-the-atlas-takes-the-atlas-permission.md)). The atlas is read as the asker, so a reader without Gitlet is advised about no repository.

**The fix is an atlas tool call the reader sends.** A new fix kind, `atlas_act`, names one of the atlas's own tools (`atlas_set_owner`, `atlas_relate`, `atlas_confirm`, `atlas_refresh`), the arguments the engine already knows, and what the reader adds (`owner`, `monitor`, `value`, or nothing). The dashboard completes one call per named entry and sends it from the reader's session through `POST /operator/act` ([ADR-0024](0024-the-dashboard-acts-as-the-principal-its-session-resolves-to.md)), where the tool's own gate decides it. The advisor still imports no write path and applies nothing.

**No atlas finding leaves the engine.** The insights sharer reads the advisor's findings without the atlas: they are added by the `advisor` question alone, for the principal who asked, and an insights record is made only of an evidence class of its closed catalogue.

## Why the obvious alternative is wrong

**A second list on the Atlas screen.** The atlas already has a review for what waits on a decision. What is missing is the administrator's view of what the setup costs, and that list exists: a second advisor would need its own severity rules, its own "what could not be read" and its own place in the go-live check.

**One finding per entry.** A company with forty unowned services would bury every other finding under forty rows saying the same thing. A class is one row with its count, and names the first five, the bound a finding's records already keep.

**A stored finding with a state.** A row that says "open" outlives the fix that closed it unless something else remembers to close it, and two nodes that both noticed would write two.

**Naming the entries to every administrator regardless.** It would be true today and silently wrong the day the list gains a reader who is not one, or an administrator's apps can be narrowed. The count carries what an administrator needs to know there is something to fix.

**Guessing that a service is public from a vendor in front of it.** A proxy or a tunnel also fronts what only the company reaches, and a finding that is wrong about half its entries is one nobody reads.

## What this does not decide

- **Waking an owning agent when its service drifts.** That is a wake, decided where the wake feed is.
- **What the Home screen shows of the atlas.** It reads the same answer.
- **Which roles may be named an owner.** The owner write decides that, as it does on the entry's own page.
