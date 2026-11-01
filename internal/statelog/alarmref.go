package statelog

import (
	"fmt"
	"strings"
)

// AlarmReference renders the alarm table as the published markdown page.
//
// GENERATED, and a test regenerates and diffs — the same idiom schema/ and
// the metrics reference use, for the same reason: an alarm an operator meets
// for the first time in a log line needs somewhere to look it up, and a page
// maintained by hand is one that stops matching the table.
func AlarmReference() string {
	var b strings.Builder
	// THE CADENCES ARE THE CONSTANTS', spelled as an alarm's own sentence
	// spells a duration: the page once stated the heartbeat as "ten
	// seconds" in prose, which a retune of [PositionHeartbeat] would have
	// left describing a loop that no longer ran at it — with the test that
	// diffs the page against this function still green, because the page
	// would be regenerated from the same stale words.
	fmt.Fprintf(&b, alarmHeader,
		spoken(AlarmInterval), spoken(TrimInterval), spoken(StallGrace))
	if perLog := PerLogKinds(); len(perLog) > 0 {
		named := make([]string, len(perLog))
		for i, kind := range perLog {
			named[i] = "`" + string(kind) + "`"
		}
		fmt.Fprintf(&b, perLogNote, sentenceList(named))
	}
	b.WriteString("\n| Alarm | What it means | What to do |\n")
	b.WriteString("|---|---|---|\n")
	for _, rule := range table {
		// The DETAIL is per-reading and belongs in the log line; what the
		// page carries is the condition in words and the remedy, which
		// are the two things that do not change between firings.
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n",
			rule.kind, alarmMeaning[rule.kind], rule.remedy)
	}
	b.WriteString(alarmFooter)
	return b.String()
}

// alarmMeaning is the condition in an operator's words.
//
// Separate from the rule's own closure because the closure is arithmetic and
// this is prose: "apply.lag.seconds > StallGrace" is precise and says nothing
// to somebody who has just been paged.
var alarmMeaning = map[Kind]string{
	KindApplyLag: "This node is more than a minute behind the log. Being " +
		"behind does not take its copy out of service; a position that stops " +
		"moving does.",
	KindReadRefusals: "Reads are being refused for something other than " +
		"ordinary lag, and have been for longer than a heartbeat.",
	KindBarrierSlow: "The read barrier (the append every linearizable read " +
		"waits on) is spending a quarter of the whole read budget.",
	KindLogHeadroom: "A log is within a tenth of the byte ceiling its " +
		"ordinary writes are held to. One alarm names every such log, " +
		"leading with the one with the least room. A full log refuses writes " +
		"rather than dropping records; on every log that claims identity " +
		"(every log but the vectors' and the usage domain's) an eviction " +
		"still lands in the gate reserve above that ceiling.",
	KindBackupAge: "No verified backup has been recorded, or the newest is " +
		"older than the policy asks for. The trim does not advance either way, " +
		"and the alarm names every log whose trim the backup term is holding " +
		"past its replay window, so those logs raise no `trim_blocked` of " +
		"their own.",
	KindTrimBlocked: "A log's trim has been blocked for longer than its " +
		"`min_age` replay window plus one trim tick, and the log is keeping " +
		"records older than that window, so it is holding what a working trim " +
		"would have removed and growing toward its ceiling. One alarm names " +
		"every such log and the term holding each, leading with the one held " +
		"longest; a log the backup term holds while `backup_age` fires is " +
		"named there instead. A young fleet blocked on its first backup or " +
		"snapshot donors does not raise it: nothing in its log is past the " +
		"window yet.",
	KindDeferredOld: "This node has been holding a record it cannot apply on " +
		"a log for longer than the thirty-minute deferral grace. One alarm " +
		"names every log that is, leading with the one held longest. The " +
		"node no longer serves its copy of each log named: what the estate " +
		"routes to that log is answered by the other data nodes, and every " +
		"other log's copy keeps serving here. The identity log is the one " +
		"whose requests are not all routed: what this node answers from its " +
		"own identity rows — the sign-ins, sessions and tokens of anybody the " +
		"held record may be about, and every directory decision — has " +
		"answered 503 here since the record was held, and goes on doing so " +
		"until this node can read it.",
	KindFloorUnknown: "This node has been unable to read a log's trim floor " +
		"for four heartbeats, so every read of that log here refuses. One " +
		"alarm names every such log, which is every log at once when " +
		"coordination is what cannot be reached.",
	KindPrefetchSlow: "The knowledge scans this node runs for turns' prefetches " +
		"are over their budget. Every turn whose prefetch it scans waits for " +
		"the scan before its first token.",
	KindSearchSlow: "Interactive search is over its target. The corpus has " +
		"outgrown what one node's share of it can scan in the budget.",
	KindSearchDegraded: "Searches are being answered without their semantic " +
		"half: the embeddings provider or the vector domain is failing, or a " +
		"node's copy of the vectors is out of service.",
	KindSearchScoped: "Searches are being answered over part of the corpus " +
		"because a node did not answer its bucket range, or its copy of what it " +
		"scans is out of service.",
	KindHistoryPartial: "Fleet history reads (turns, traces, the event log) " +
		"are being answered without every node, because one did not answer " +
		"inside the fleet read budget.",
	KindRecallBelowFloor: "Less of the corpus has current vectors than " +
		"semantic recall claims to cover.",
	KindIVFRecallBelowFloor: "The latest measurement of the " +
		"semantic index found recall against the exact scan below the floor, " +
		"in a query shape a search is issued in, even probing every list, " +
		"which is the full scan's own candidate pool, so the first stage is " +
		"below the floor on this corpus with or without the index.",
	KindRecordsGated: "An apply gate dropped a record. A gated record is " +
		"recoverable by nothing.",
	KindFeedUnreadable: "A change record no build on this node can read. It " +
		"redelivers for ever, so every wake behind it is waiting too.",
	KindMaintenanceOpen: "A maintenance operation has been open for an hour. " +
		"Maintenance stops every publisher on every node.",
	KindVolumeLow: "A volume holding this node's databases has less free space " +
		"than the next restore, vacuum or snapshot needs for a second copy and for " +
		"the write-ahead log a copy holds back there while it runs, or could not " +
		"be measured at all. The alarm names the volume.",
	KindWALLarge: "The write-ahead log has grown past a gibibyte, which means " +
		"a checkpoint is not happening: a backup's or a snapshot's copy holds it " +
		"back for the copy's run, which the alarm names, and anything else " +
		"holding it is a reader holding a snapshot open.",
	KindPoolStarved: "Callers are queuing for a database connection before " +
		"their query starts.",
	KindCensusDrift: "A log is taking more than twice the linearizable reads " +
		"its census allows (125 a day per agent seat, at least one seat, " +
		"plus what the object store's collector reads on its own schedule, " +
		"plus, on the code log whether or not the company hosts code now, " +
		"the git reads its ceiling was sized for: a tenth of it in barriers " +
		"over the trim's min_age), so every sizing decision under it is stale.",
	KindLogCeilingShort: "At the rate this log took in over the last day, " +
		"its byte ceiling holds less than the `min_age` window the trim " +
		"keeps, so it will fill and refuse writes with every trim term " +
		"satisfied.",
	KindObjectsMissing: "Some of the company's files cannot be read: the " +
		"collector's last audit asked the object store for every file's bytes, " +
		"and some were not there, or were there at the wrong size, under the " +
		"wrong digest or as nothing the store can read, or the store failed to " +
		"answer for them twice while it answered for the rest. Those files " +
		"cannot be downloaded or read until they are restored, or the store " +
		"serves them again.",
	KindObjectsBucketNearCeiling: "The fleet's own object bucket on the broker holds " +
		"more than four fifths of the bytes it reserves. A full bucket refuses every " +
		"upload, though nothing else on the broker is affected.",
	KindObjectiveBurn: "A service objective this node measures (publishing, serving a " +
		"document, a frame becoming ready in a viewer's browser, a datastore verdict) " +
		"is failing fast enough to spend its month's error budget early: in both the " +
		"long and the short window of one of the two burn-rate pairs.",
	KindClockSkew: "A clock stands further from the fleet's than the hour the object " +
		"store's deletions assume: this node's own, or the clock of the broker member " +
		"leading the coordination store's position register, and the alarm says which. " +
		"The fleet's clock is the median of the data nodes' clocks, measured on this " +
		"node's position heartbeat off the register: each node's row carries the instant " +
		"the node stamped on it and the instant the broker stored it. A node whose own " +
		"clock moved raises it about itself; a member whose clock moved is raised about " +
		"by every node. The object collector's grace and the code host's retired-pack " +
		"sweep each judge an instant one machine stamped by another machine's clock, and " +
		"both hold only while every node and that member keep within that hour of the " +
		"fleet's clock.",
	KindCodeMaintenanceRefused: "This node's git cache cannot write the bitmap " +
		"of some repositories, because a bitmap over each (about 600 bytes " +
		"an object, with room for a clone of the repository beside it) needs " +
		"more memory than the node can give its background work: its memory, " +
		"or its cgroup's limit, less what it keeps for its git lanes, the " +
		"store, the broker and the engine. Those repositories are indexed " +
		"without a bitmap and served more slowly: a clone walks their history. " +
		"A repository this node maintains whose next published layer it cannot " +
		"write counts too — its layer's bitmap is 600 bytes an object of the " +
		"repository — since no git node then publishes it, and every git node " +
		"serves the packs pushed since its last publication outside every bitmap.",
	KindCodeCompactionBehind: "Repositories this node maintains — the git node " +
		"that merges their packs and publishes their layers, the first in each " +
		"repository's order whose background budget holds its publication — " +
		"hold the most live packs a repository may, so every push carrying " +
		"objects into them is refused `compaction_behind` until a compaction of " +
		"theirs lands. Raised by their maintainer alone, from the listing it " +
		"polls every minute, for as long as any repository it maintains stays " +
		"at the bound.",
	KindCodeCommitBusy: "This node's committer — the data node a repository's " +
		"pushes are decided on — refused proposals `busy` within the last trim " +
		"tick because it could not hold them or reach them in time: a " +
		"repository taking pushes faster than one committer commits them, its " +
		"queue full or its proposals waiting in it until their asker could wait " +
		"no longer, or this node committing for too many busy repositories at " +
		"once, its whole bound held. Each was decided by nobody and is sent " +
		"again. Raised by the committer's node alone, whichever node received " +
		"the pushes, and only for a backlog: a proposal that arrived with its " +
		"asker's attempt already spent, or one that lost every compare-and-set " +
		"round, is counted and logged and raises nothing.",
	KindCodeChecksBacklog: "Queued attempts of checks have waited for a runner " +
		"longer than their own timeout — each judged at the timeout_seconds of " +
		"the definition it was queued under, the time its author gave the check " +
		"to run — so a change whose head they check waits on them past anything " +
		"its checks file planned for. Measured from the code log's rows by every " +
		"data node, in the order the attempts fell overdue, so it stands even " +
		"where no git node runs checks at all; past a thousand it says there " +
		"are more rather than how many. A deleted repository's queued attempts " +
		"are not counted: no runner may claim them, and its purge removes them.",
	KindCodeMergeRateOver: "Merge queues this node is home to — " +
		"branches whose merges wait for checks — were asked to land more " +
		"changes in the last hour than they can land in it: their " +
		"enqueues within the hour, live or ended, past depth × batch " +
		"changes a round at the mean round their trains took, the round " +
		"timed by this node from a train's build to its landing (a train " +
		"still testing counts at its age once it outlasts the mean). " +
		"Raised by the repository's home git node alone, for as long as " +
		"the hour's enqueues stay past the capacity; a node that has timed " +
		"no round of a queue yet — a home new to it — judges it from its " +
		"first train.",
	KindCodeRefsNearLimit: "Active repositories set refs so fast that they reach " +
		"the most live refs a repository may hold (100 000) within the ref " +
		"lifecycle's window (14 days) unless refs are deleted: their live refs " +
		"and the refs of those set within the window together reach the bound. " +
		"The lifecycle deletes only a seat's branch nobody set for the whole " +
		"window, so none of the refs that make the rate, and once the bound is " +
		"reached every push creating a ref there is refused `repo_refs`. " +
		"Measured from the code log's rows by every data node on the trim's " +
		"quarter-hour tick, so every data node raises it for the same " +
		"repositories; an archived repository, which takes no push, and a " +
		"deleted ref are never counted.",
}

const alarmHeader = `# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you four ways, and they are the same table evaluated once: a
` + "`crewlet.alarm.active{kind}`" + ` gauge your collector scrapes, a named ` + "`WARN`" + `
line when it starts and another when it clears, carrying how long it was up,
a count on the node's health envelope (` + "`GET /health`" + ` and the
dashboard's health push carry ` + "`alarms: {count, worst}`" + `, where ` + "`worst`" + `
is the alarm that has been firing longest), and the node's row on the fleet
view: each node's presence heartbeat carries every alarm firing on it with when
it was raised, and ` + "`GET /fleet`" + ` lists them per node (` + "`alarms`" + `), so
one read covers every node. And it wakes the agents that may read the
deployment's health: each time an alarm fires or clears on a node, or a node's
first evaluation since it started finds it firing, every agent seat whose role
carries ` + "`health_tools`" + ` is notified once and pointed at ` + "`fleet_health`" + ` for
what is firing now (see [Alarm wakes](../concepts/event-system.md#alarm-wakes)).
Nothing has to be polled for any of them: every node running a state log,
which is every data node, evaluates the whole table on its own position
heartbeat, every %s, and again the moment
the measurements it takes every %s (each log's daily intake, the vector
coverage, the store's connection-pool waits) land, so an alarm whose
threshold is %s is raised within a beat of passing it.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.
`

// perLogNote says which alarms are evaluated once per log, rendered from the
// table's own flag so the sentence cannot name a different set from the one
// the report lists logs under and the tracker writes per-log lines for.
const perLogNote = `
Each kind is one alarm, however many logs it stands on. These are about a log
rather than the node:

%s.

Each of those names every log it stands on in its ` + "`domains`" + ` list, with that
log's own measurement, and leads with the most pressing log's, so the health
count and the gauge (one series per kind, reading 1 while any log's stands)
agree about how many problems there are. Their log lines are written per log:
each log the alarm comes to stand on writes its own ` + "`alarm_raised`" + `, and each
log it stops standing on its own ` + "`alarm_cleared`" + `, carrying the log as
` + "`domain`" + `, so a second log joining an alarm that is already up, and the first
one leaving it, are each said.
`

const alarmFooter = `
An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something: the grace that takes a copy out of service, the grace
that stops a node serving a copy it cannot decode, the budget a caller was
promised, the replay window a log's ceiling was sized to hold.

A seat whose token window is spent is NOT an alarm, because nothing is wrong
with the node: the ceiling is doing what it was set to do. Its mail is parked
on its inbox until the window turns over or a revision raises the ceiling.
The ` + "`seat_budget_parked`" + ` line names the window and when it resets, and
the budgets surfaces show the refusing window. See the budget park in
docs/concepts/agent-runtime.md.
`

// sentenceList joins names the way a sentence does: "a", "a and b", "a, b
// and c".
func sentenceList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}
