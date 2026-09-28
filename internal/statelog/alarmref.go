package statelog

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore/disk"
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
	KindApplyLag: "This node is more than a minute behind the log. Being " +
		"behind does not move its seats; a position that stops moving does.",
	KindReadRefusals: "Reads are being refused for something other than " +
		"ordinary lag, and have been for longer than a heartbeat.",
	KindBarrierSlow: "The read barrier — the append every linearizable read " +
		"waits on — is spending a quarter of the whole read budget.",
	KindLogHeadroom: "The log is within a tenth of the byte ceiling its " +
		"ordinary writes are held to. A full log refuses writes rather than " +
		"dropping records; on the tracker and pages logs an eviction still " +
		"lands in the gate reserve above that ceiling.",
	KindBackupAge: "No verified backup has been recorded, or the newest is " +
		"older than the policy asks for. The trim does not advance either way.",
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
	KindIVFRecallBelowFloor: "The latest measurement of the partition's " +
		"semantic index found recall against the exact scan below the floor, " +
		"in a query shape a search is issued in, even probing every list — " +
		"which is the full scan's own candidate pool, so the first stage is " +
		"below the floor on this corpus with or without the index.",
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
	KindCensusDrift: "A log is taking more than twice the linearizable reads " +
		"its share of the census allows — 125 a day per agent seat, over its " +
		"domain's logs — so every sizing decision under it is stale.",
	KindObjectsMissing: "Parts of the company's files that the placement map " +
		"puts on this node are held by no member of the fleet — every member " +
		"asked answered that it has no copy — so those files cannot be read " +
		"in full.",
	KindObjectsDegraded: "Copies the placement map puts on this node are not " +
		"here: its last completed repair at the map's current epoch left some " +
		"behind, or none has completed for more than twice the repair " +
		"interval — counted from the last one that did at that epoch, or, if " +
		"none has, from when this node first placed by it. Those files have " +
		"fewer copies than the company asked for.",
	KindObjectsUnhealthy: "This node's object store has failed, or its volume " +
		"is full. A failed store is taken out of the placement map after the " +
		"absence grace; a full one keeps serving while writes go elsewhere.",
	// The two marks are the store's own, named once in internal/objstore/disk.
	KindObjectsNearFull: fmt.Sprintf("This node's object store volume is past "+
		"%.0f%% used. At %.0f%% it refuses every new chunk.",
		disk.NearFullRatio*100, disk.FullRatio*100),
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
