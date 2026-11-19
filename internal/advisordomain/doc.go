// Package advisordomain is the state log's record of what the Advisor found and
// what became of it: each finding's episodes and their history, the Advisor's
// runs, its weekly reports and the alarm episodes its detectors read, on the
// `CREWLET_ADVISOR_LOG` stream and applied into every data node's replicated
// estate.
//
// # Who has to agree on it
//
// The whole company, with history, readable from any node: the Advisor's page
// is read from whichever node a person reaches, its duty writes from the one
// node holding its lease, people's decisions (apply, snooze, dismiss, reopen,
// Run now) are written from any node, and the history must outlive any one
// node's event store. That is the replicated estate, which only this domain's
// applier writes. Nothing here is a person's words (a finding holds the
// engine's words, numbers, handles and record ids), so nothing is sealed and
// nothing is purged.
//
// # Subjects
//
//   - `finding`, by [KeyID] of the finding key (class, `:`, subject key; the
//     key holds `.`, `:` and `/`, so it never goes on the wire itself): every
//     record about every episode of the key, arbitrated on the key's version,
//     so a key has at most one non-closed episode.
//   - `run`, by the run's uuid: a person's request and the pass's record.
//   - `report`, by ISO week: the week's report, created once.
//   - `alarm`, by [AlarmSeriesID] of a node, an alarm's kind and its instance:
//     its episodes, raised and cleared.
//   - `retention`, by UTC day: the day's expiry, created once.
//
// The eviction, the generation, the barrier and the trimmed record are the
// framework's; the eviction is this log's only gate.
//
// # Ops
//
// On a finding: `open` (a new episode, [Opening]), `revise` (verdict or fix
// moved: the revision bumps and the history says what moved), `refresh` (facts
// moved under the same verdict and fix: no bump and no history line), `move`
// (a state transition), `explain` (the model's words, bound to one verdict
// digest) and `release` (a person lifts the key's hold). On a run: `request`
// and `record`, which also moves each class it ran to done. `write` on a
// report, `raise` and `clear` on an alarm series, `expire` on a day.
//
// # Legal moves
//
// Enforced by the writer in its snapshot, and again by the applier, which
// writes nothing for a record the rows no longer admit:
//
//   - `open` only where the key has no non-closed episode ([ErrExists]), and
//     its hold has lapsed or the new severity ranks above the hold's
//     (`blocks` > `costs` > `note`; else [ErrHeld]). The hold's lapse is a
//     clock reading, so it is the writer's alone: the applier reads no clock.
//   - `revise`, `refresh` and `explain` on a non-closed episode.
//   - `move`: open to applied; applied to open (`ineffective`, which adds the
//     fix's digest to the key's ineffective list); open or applied to closed
//     (`resolved`, `retired`, `expired` or `dismissed`, each with its own
//     reasons, a dismissal optionally holding the key); open to open
//     (`snoozed`, `unsnoozed`, `useful`). Closed is terminal ([ErrIllegalMove]).
//   - `release` on a key holding a hold.
//
// A write names the key's version it decided from ([KeyRow.Version],
// [IfNoRecord], [NoIfMatch]); a moved key answers [ErrStaleVersion]. An
// explanation is bound to its verdict digest instead.
//
// # Retention
//
// One `expire` record a day ([ExpiryFor]), on every node identically: closed
// episodes and their history after [ClosedRetention] (a key's row stays while
// it has an episode, a standing hold or an ineffective fix), runs after
// [RunRetention], reports after [ReportRetention] and cleared alarm episodes
// after [AlarmRetention]. No cutoff may reach inside its retention, and a day
// is swept once.
//
// # The applier judges no text
//
// Every judgement of a value (its words, its bounds, its JSON) is the writer's,
// before it publishes. The applier stores what the record says, so two builds
// that know different words still write one row, and a reader of an older
// build reads a newer word as a value.
//
// # A new record kind joins without reshaping
//
// A kind is a subject kind, an op and a payload: its row in [versionedFields]
// at the next [RecordVersion], a case in [DecodeMutation] and the applier, a
// table in [ReproducibleTables], and a writer method. Builds below it retain
// its records.
//
// # Reads
//
// Every read is at the caller's level, `session` by default ([DefaultRead]):
// [Reader.Findings] a page of episodes newest first; [Reader.NonClosed] every
// open and applied one, for the duty; [Reader.Finding] one with its history;
// [Reader.Episodes] a key's kept episodes; [Reader.Key] a key's version, hold
// and ineffective fixes; [Reader.Counts] the open ones by severity; the runs
// ([Reader.LatestRun], [Reader.Run], [Reader.PendingRequests],
// [Reader.LastRunOfClasses]); the reports; and the alarm episodes since an
// instant.
package advisordomain
