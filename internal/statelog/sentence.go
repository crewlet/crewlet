package statelog

import (
	"errors"
	"log/slog"
	"strings"
	"time"
)

// THREE REFUSALS AS A PERSON READS THEM.
//
// A lost compare-and-set ([ErrConflict]), a create of what is already there
// ([ErrExists]) and a write about what a deletion marker keeps deleted
// ([ReasonDeleted]) mean one thing in every domain and say what to do on their
// own. The error names the subject it met, a broker path nobody can act on, so
// every surface answering in words of its own (the agent's tools to a model,
// the identity directory) tells them in the words below; a person's act route
// and the human write surface answer each in its code's own sentence
// (ADR-0060). They live here because every such surface imports this package.
// Every other refusal is a CONDITION of the node, told in [ConditionSentence]'s
// words.

// LostRace is a write that lost its compare-and-set rounds ([ErrConflict]): what
// happened, that nothing was written, and that the same change may be made
// again once its writer has looked at what the object says now. A seat pause
// that lost its compare-and-set on the coordination store is told in these
// words too, since to the person who asked it is the same thing.
const LostRace = "Someone else changed this at the same moment, so nothing " +
	"was written. Look at it again, then make the change again if it still " +
	"needs making."

// AlreadyThere is a create the framework found already made ([ErrExists]) —
// most often the same create sent again after its operation was forgotten.
const AlreadyThere = "What this creates is already there, so nothing was " +
	"written. Look at what's there rather than making it again."

// Deleted is a write about something a deletion marker keeps deleted
// ([ReasonDeleted]): nothing will ever write it again, on any node.
const Deleted = "This was deleted for good, so nothing was written."

// Sentence is one of the three refusals above as a person reads it, and false
// for any other error, a condition of the node among them ([ConditionSentence]
// tells those).
//
// A DOMAIN'S OWN WRAP IS THE SAME REFUSAL: a domain that wraps [ErrConflict]
// in a sentinel of its own ("the page kept changing under this write") lost
// the same race, and is told in the same words.
func Sentence(err error) (string, bool) {
	var refused *Unavailable
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrConflict):
		return LostRace, true
	case errors.Is(err, ErrExists):
		return AlreadyThere, true
	case errors.As(err, &refused) && refused.Reason == ReasonDeleted:
		return Deleted, true
	}
	return "", false
}

// A CONDITION AS A PERSON READS IT.
//
// Every other refusal is a condition of the node that met it (a write's
// [Reason], a read's [ReadRefusal]), whose detail ([Unavailable.Detail],
// [Refused.Detail]) names positions, settings and the broker's words: the
// node's log's, never a screen's. A surface tells a person the code's sentence
// below and logs the detail.
//
// EACH SAYS WHAT HAPPENED AND WHAT TO DO, agreeing with [RetryAfter]:
// [TryAgainSoon] where it clears on its own, [TryAgainLater] where the log's
// duplicate window clears it, neither where no wait does, or a person retries
// for ever ([TestEveryConditionIsToldInWordsForAPerson]).

// TryAgainSoon is what a person is told to do about a condition that clears
// on its own: every one whose answer carries a Retry-After.
const TryAgainSoon = "Try again in a moment."

// TryAgainLater is what a person is told to do about a condition that clears
// on its own but not soon, its hint a minute or more: another node's copy of
// their change under a gate that blames its writer ([Unavailable.CopyWriter]),
// which the refusing node takes once the log's duplicate window lets go of the
// operation ([Unavailable.RetryAfter]); beyond the state log, a coding
// sandbox's account limit.
const TryAgainLater = "Try again in a few minutes."

// WHO A CONDITION IS ABOUT is never "this node": the estate router hands a
// data node's refusal back as the asking node's answer, so each sentence names
// the node by what it is doing (handling the change, answering the question),
// and an administrator is sent to "the one that refused", which the asking
// node's log names (`estate_peer_refused`). And "an administrator needs to",
// never "ask an administrator": the identity directory's reader is one.
const (
	// handling and answering are who a write's and a read's condition is
	// about.
	handling  = "the node handling this change"
	answering = "the node answering this"

	// tryElsewhere is a condition of the node that met it which another
	// node does not share, and that an administrator clears there.
	tryElsewhere = "Try another node, or have an administrator check the " +
		"one that refused."

	// readmit is a node the fleet evicted, which an administrator takes
	// back.
	readmit = "Try another node, or have an administrator readmit the one " +
		"that refused."

	// checkNode is a condition only an administrator clears, on the node
	// that met it, wherever the person asks.
	checkNode = "An administrator needs to check the node that refused."

	// checkStorage is a condition of the store every node writes to — the
	// broker, which may be an external cluster or another member — rather
	// than of any one node.
	checkStorage = "An administrator needs to check the company's storage."

	// makeRoom is a log at its ceiling: an administrator raises it or lets
	// the trim advance.
	makeRoom = "An administrator needs to make room for it."

	// remake is a change that will never take effect as it was sent, which
	// the person makes again, as a new change, if they still want it.
	remake = "Look at it again, then make the change again if it still " +
		"needs making."
)

// conditionWords is one condition as a person is told it, in two halves: what
// happened, a clause with no capital and no full stop, so a sentence of
// somebody else's (a tool's) can carry it, and what to do, a sentence of its
// own, or empty where nothing does.
type conditionWords struct {
	what string
	next string
}

// sentence is the two halves as one sentence a person reads.
func (c conditionWords) sentence() string {
	what := strings.ToUpper(c.what[:1]) + c.what[1:] + "."
	if c.next == "" {
		return what
	}
	return what + " " + c.next
}

// writeConditions is every write [Reason] as a person is told it.
//
// NONE SAYS WHETHER ANYTHING WAS WRITTEN, a deletion's excepted ([Deleted]):
// a refusal is of one record, and the gesture may have written others before
// it, which the answering surface says (`landed`, a tool's own sentence).
//
// A FULL LOG IS ONE AREA'S: every domain's log has its own ceiling, so the
// words never say the company's records are full.
var writeConditions = map[Reason]conditionWords{
	ReasonEvicted: {handling + " has been evicted from the fleet", readmit},
	ReasonEvictionUnknown: {handling + " couldn't check whether it's been " +
		"evicted", TryAgainSoon},
	ReasonDeferred: {handling + " holds changes it can't read yet", tryElsewhere},
	ReasonBehind: {handling + " hasn't caught up with the latest changes yet",
		TryAgainSoon},
	ReasonBelowFloor: {handling + " fell too far behind and is catching up " +
		"from another node", TryAgainSoon},
	ReasonFloorUnknown: {handling + " couldn't confirm it has caught up",
		TryAgainSoon},
	ReasonUsageUnknown: {handling + " couldn't check whether there's room to save it",
		TryAgainSoon},
	ReasonDeleted: {strings.ToLower(Deleted[:1]) + strings.TrimSuffix(Deleted[1:], "."), ""},
	ReasonAbandoned: {"this change was made from records that are no longer " +
		"kept, so it didn't take effect", remake},
	ReasonOvertaken: {"this change was made from an older copy of the " +
		"records, so it didn't take effect", remake},
	ReasonLogFull: {"there's no room left to save this kind of change",
		makeRoom},
	ReasonRecordTooLarge: {"this change is too large to save in one go",
		"Split it into smaller changes."},
	ReasonBrokerRefused: {"the company's storage refused the change",
		checkStorage},
	ReasonSkew: {handling + " has records out of step with the fleet's",
		checkNode},
	ReasonOpReused: {"this request was already used for a different change",
		remake},
	ReasonLogTruncated: {"recent changes were lost from the company's storage",
		checkStorage},
	ReasonWrongStream: {handling + " has records that don't match the " +
		"fleet's", checkNode},
	ReasonSuperseded: {"a later change already undid this one, so it wasn't " +
		"made again", remake},
}

// copyConditions is a write refused for ANOTHER node's copy of it under a gate
// that blames that copy's writer ([Unavailable.CopyWriter]): the refusing node
// passed its own fences and takes the change itself once the log's duplicate
// window lets go of the operation, so the advice is to send it again then,
// never to readmit an evicted node nor to remake a change that will land.
var copyConditions = map[Reason]conditionWords{
	ReasonEvicted: {"another node took this change and has since been " +
		"evicted from the fleet, so it hasn't taken effect yet", TryAgainLater},
	ReasonAbandoned: {"another node took this change from records that are " +
		"no longer kept, so it hasn't taken effect yet", TryAgainLater},
	ReasonOvertaken: {"another node took this change from an older copy of " +
		"the records, so it hasn't taken effect yet", TryAgainLater},
}

// readConditions is every [ReadRefusal] as a person is told it.
var readConditions = map[ReadRefusal]conditionWords{
	RefuseBehind: {answering + " hasn't caught up with the latest changes yet",
		TryAgainSoon},
	RefuseDeferred: {answering + " holds changes it can't read yet, so it " +
		"can't answer", tryElsewhere},
	RefuseDeferredScopeUnknown: {answering + " holds changes it can't read " +
		"yet, so it can't answer", tryElsewhere},
	RefuseStalled: {answering + " has stopped catching up with the latest " +
		"changes, so its answer could be wrong", TryAgainSoon},
	RefuseBelowFloor: {answering + " fell too far behind and is catching up " +
		"from another node", TryAgainSoon},
	RefuseFloorUnknown: {answering + " couldn't confirm it has caught up",
		TryAgainSoon},
	RefuseEvicted: {answering + " has been evicted from the fleet, so it " +
		"can't answer", readmit},
	RefuseBrokerUnreachable: {answering + " lost touch with the rest of the " +
		"fleet", TryAgainSoon},
	RefuseNoQuorum: {"the fleet couldn't agree on the latest changes",
		TryAgainSoon},
	// A STALL OF THE ONE MEMBER that stores the log: its write of the check
	// missed a barrier budget, which the Retry-After waits out
	// ([StalledRetryHint]). It clears when that write completes, so it is
	// never sent to an administrator; a stall that persists is in the logged
	// detail.
	RefuseBrokerStalled: {"the company's storage took too long to confirm " +
		"a check this answer needs", TryAgainSoon},
	RefuseLogFull: {"there's no room left to save this kind of change, so " +
		answering + " can't confirm it has the latest ones", makeRoom},
	RefuseBrokerRefused: {"the company's storage refused a check this answer " +
		"needs", checkStorage},
	RefuseWrongStream: {answering + " has records that don't match the " +
		"fleet's, so it can't answer", checkNode},
	RefuseTooStale: {answering + " is further behind than this request " +
		"accepts", "Try another node."},
	RefuseMaintenance: {"the fleet is in maintenance, so " + answering +
		" can't confirm it has the latest changes", "Try again once " +
		"maintenance is over."},
}

// unknownWrite and unknownRead are a code this build does not name — one a
// newer peer's refusal carried over the wire — which no wait is known to
// clear ([Reason.Retryable] and [ReadRefusal.Retryable] answer false for it).
var (
	unknownWrite = conditionWords{handling + " couldn't make it", checkNode}
	unknownRead  = conditionWords{answering + " couldn't answer", checkNode}
)

// bareCondition is [ErrUnavailable] with no refusal behind it — the sentinel a
// caller wrapped on its own, which says nothing about which condition it was.
// [RetryAfter] answers it with the caller's own hint, so it asks for a try in
// a moment.
var bareCondition = conditionWords{"the node handling this couldn't do it " +
	"right now", TryAgainSoon}

// conditionOf is the condition err is, read out of the chain however a domain
// wrapped it, and false for an error that carries none.
func conditionOf(err error) (conditionWords, bool) {
	var refused *Unavailable
	if errors.As(err, &refused) {
		if words, ok := copyConditions[refused.Reason]; ok && refused.CopyWriter != "" {
			return words, true
		}
		if words, ok := writeConditions[refused.Reason]; ok {
			return words, true
		}
		return unknownWrite, true
	}
	var read *Refused
	if errors.As(err, &read) {
		if words, ok := readConditions[read.Code]; ok {
			return words, true
		}
		return unknownRead, true
	}
	if errors.Is(err, ErrUnavailable) {
		return bareCondition, true
	}
	return conditionWords{}, false
}

// ConditionSentence is the condition err carries — a write refused for a
// [Reason], a read refused with a [ReadRefusal], or a bare [ErrUnavailable] —
// as a person reads it: what happened and what to do, and nothing of its
// detail, which the surface that answers it logs. False for an error that
// carries none.
func ConditionSentence(err error) (string, bool) {
	words, ok := conditionOf(err)
	if !ok {
		return "", false
	}
	return words.sentence(), true
}

// ConditionWords is [ConditionSentence] in its two halves, for a sentence that
// carries what happened inside its own words — a tool's, which a model reads:
// what happened, a clause with no capital and no full stop; and what to do, a
// sentence of its own, [TryAgainSoon] or [TryAgainLater] exactly where the
// condition clears on its own, or empty where nothing does.
func ConditionWords(err error) (what, next string, ok bool) {
	words, ok := conditionOf(err)
	return words.what, words.next, ok
}

// ConditionLevel is the level a surface logs a condition's error (and its
// detail) at: WARN where no wait clears it ([RetryAfter] is zero), since only
// an administrator can act, and cleared where one does, which the caller names
// by who asks again: INFO for a gesture made once, DEBUG for a screen that
// re-asks after every hint while a node catches up, where a line per ask is
// noise beside the alarm.
//
// ONE RULE FOR EVERY SURFACE (the estate router's line included), so one
// condition is never INFO on one surface and WARN on the next; each surface
// keeps its own component and fields.
func ConditionLevel(err error, cleared slog.Level) slog.Level {
	if RetryAfter(err, time.Second) == 0 {
		return slog.LevelWarn
	}
	return cleared
}
