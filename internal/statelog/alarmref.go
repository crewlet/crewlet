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
	b.WriteString(alarmHeader)
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
	KindApplyLag: "This node is more than a minute behind the log. Its seats " +
		"move if it stays behind for thirty.",
	KindReadRefusals: "Reads are being refused for something other than " +
		"ordinary lag, and have been for longer than a heartbeat.",
	KindBarrierSlow: "The read barrier — the append every linearizable read " +
		"waits on — is spending a quarter of the whole read budget.",
	KindLogHeadroom: "The log is within a tenth of its byte ceiling. A full " +
		"log refuses writes rather than dropping records.",
	KindBackupAge: "The newest verified backup is older than the policy asks " +
		"for. The trim will not advance past it.",
	KindTrimBlocked: "The trim has a term it cannot satisfy, so the log is " +
		"growing toward its ceiling.",
	KindDeferredOld: "This node has been holding records it cannot apply for " +
		"longer than the deferral grace. Its seats have moved.",
	KindFloorUnknown: "The trim floor has been unreadable for four " +
		"heartbeats, so every read on this node refuses.",
	KindPrefetchSlow: "Turn-start context assembly is over its budget. Every " +
		"turn on this node pays it before its first token.",
	KindSearchSlow: "Interactive search is over its target. The corpus has " +
		"outgrown what one node's share of it can scan in the budget.",
	KindSearchDegraded: "Searches are being answered without their semantic " +
		"half — the embeddings provider or the vector domain is failing.",
	KindSearchScoped: "Searches are being answered over part of the corpus " +
		"because a node did not answer its bucket range.",
	KindRecallBelowFloor: "Less of the corpus has current vectors than " +
		"semantic recall claims to cover.",
	KindRecordsGated: "An apply gate dropped a record. A gated record is " +
		"recoverable by nothing.",
	KindFeedUnreadable: "A change record no build on this node can read. It " +
		"redelivers for ever, so every wake behind it is waiting too.",
	KindMaintenanceOpen: "A maintenance operation has been open for an hour. " +
		"Maintenance stops every publisher on every node.",
	KindVolumeLow: "The volume has less free space than the next restore, " +
		"vacuum or snapshot needs for a second copy.",
	KindWALLarge: "The write-ahead log has grown past a gibibyte, which means " +
		"a checkpoint is not happening.",
	KindPoolStarved: "Callers are queuing for a database connection before " +
		"their query starts.",
	KindCensusDrift: "This company is doing more than twice the reads its " +
		"log was sized for, so every sizing decision under it is stale.",
}

const alarmHeader = `# Alarms

Every condition the engine raises about itself, what it means, and what to do
about it.

An alarm reaches you two ways, and they are the same table evaluated once: a
` + "`crewlet.alarm.active{kind}`" + ` gauge your collector scrapes, and a named
` + "`WARN`" + ` line when it starts and another when it clears, carrying how long it
was up. Nothing has to be polled for either — alarms are evaluated on ticks
the engine already runs.

The log line is the one to read first. It carries the measurement that raised
the alarm, in the units of the thing measured, and the remedy from the table
below.
`

const alarmFooter = `
An alarm that fires on a healthy node is a defect in this table, not a
threshold for an operator to tune: each one fires at the number that already
decides something — the grace that sheds a node, the grace that moves its
seats, the budget a caller was promised.
`
