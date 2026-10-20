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
		"(Worklet's, Doclet's, the identity estate's, the artifacts " +
		"log's, the artifact datastore's, the Dashboard Assistant's and " +
		"Chatlet's) an eviction still lands in the gate reserve above that " +
		"ceiling.",
	KindBackupAge: "No verified backup of a log has been recorded, or the " +
		"newest one covering it is older than the policy asks for. The trim " +
		"does not advance that log either way. One alarm names every such " +
		"log, leading with the one whose backup is missing or oldest, and a " +
		"log it names whose trim the backup term is holding past its replay " +
		"window raises no `trim_blocked` of its own.",
	KindTrimBlocked: "A log's trim has been blocked for longer than its " +
		"`min_age` replay window plus one trim tick, and the log is keeping " +
		"records older than that window, so it is holding what a working trim " +
		"would have removed and growing toward its ceiling. One alarm names " +
		"every such log and the term holding each, leading with the one held " +
		"longest; a log the backup term holds while `backup_age` names it is " +
		"named there instead. A young fleet blocked on its first backup or " +
		"snapshot donors does not raise it: nothing in its log is past the " +
		"window yet.",
	KindDeferredOld: "This node has been holding a record it cannot apply on " +
		"a log for longer than the thirty-minute deferral grace. One alarm " +
		"names every log that is, leading with the one held longest. Where a " +
		"log judges this node's copy of the estate (Worklet's, Doclet's and " +
		"Chatlet's do), the node no longer serves its copy, and its " +
		"seats read the estate from the other data nodes.",
	KindFloorUnknown: "This node has been unable to read a log's trim floor " +
		"for four heartbeats, so every read of that log here refuses. One " +
		"alarm names every such log, which is every log at once when " +
		"coordination is what cannot be reached.",
	KindPrefetchSlow: "Turn-start context assembly is over its budget. Every " +
		"turn on this node pays it before its first token.",
	KindSearchSlow: "Interactive search is over its target. The corpus has " +
		"outgrown what one node's share of it can scan in the budget.",
	KindSearchDegraded: "Searches are being answered without their semantic " +
		"half: the embeddings provider or the vector domain is failing.",
	KindSearchScoped: "Searches are being answered over part of the corpus " +
		"because a node did not answer its bucket range.",
	KindHistoryPartial: "Fleet history reads (turns, traces, the event log) " +
		"are being answered without every data node, because one did not " +
		"answer inside the fleet read budget.",
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
		"than the next restore, vacuum or snapshot needs for a second copy, or " +
		"could not be measured at all. The alarm names the volume.",
	KindWALLarge: "The write-ahead log has grown past a gibibyte, which means " +
		"a checkpoint is not happening.",
	KindPoolStarved: "Callers are queuing for a database connection before " +
		"their query starts.",
	KindCensusDrift: "A log is taking more than twice the linearizable reads " +
		"its census allows, so every sizing decision under it is stale. The " +
		"census is the rate its domain declares for each agent seat, counting " +
		"at least one seat, plus what the object store's collector reads on " +
		"its own schedule. That rate is 125 a day for the tracker, the " +
		"knowledge base, the artifacts and their datastore, 120 for chat, and " +
		"none for the identity estate and the Dashboard Assistant.",
	KindLogCeilingShort: "At the rate this log took in over the last day, " +
		"its byte ceiling holds less than the `min_age` window the trim " +
		"keeps, so it will fill and refuse writes with every trim term " +
		"satisfied.",
	KindObjectsMissing: "Some of the company's files cannot be read: the " +
		"collector's last audit asked the object store for every file's bytes, " +
		"and some were not there, or were there at the wrong size or under the " +
		"wrong digest. Those files cannot be downloaded or read until they are " +
		"restored.",
	KindObjectsBucketNearCeiling: "The fleet's own object bucket on the broker holds " +
		"more than four fifths of the bytes it reserves. A full bucket refuses every " +
		"upload, though nothing else on the broker is affected.",
	KindObjectiveBurn: "A service objective this node measures (publishing, serving a " +
		"document, a frame becoming ready in a viewer's browser, a datastore verdict) " +
		"is failing fast enough to spend its month's error budget early: in both the " +
		"long and the short window of one of the two burn-rate pairs.",
}

const alarmHeader = `# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you three ways, and they are the same table evaluated once: a
` + "`crewlet.alarm.active{kind}`" + ` gauge your collector scrapes, a named ` + "`WARN`" + `
line when it starts and another when it clears, carrying how long it was up,
and a count on the node's health envelope (` + "`GET /health`" + ` and the
dashboard's health push carry ` + "`alarms: {count, worst}`" + `, where ` + "`worst`" + `
is the alarm that has been firing longest). Nothing has to be polled for any
of them: every node running a state log, which is every data node, evaluates
the whole table on its own position heartbeat, every %s, and again the moment
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

Each of those names every log it stands on in its ` + "`domains`" + ` list, each
entry a measurement and the logs it was taken on — one log where a trim term
holds it, and every log measured in the same words otherwise, so a fleet with
no backup yet is one entry naming every log — and leads with the most pressing
log's, so the health count and the gauge (one series per kind, reading 1 while
any log's stands) agree about how many problems there are. Their log lines are
written per log:
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
