package statelog

import (
	"errors"
	"log/slog"
	"strings"
	"time"
)

// THREE REFUSALS AS A PERSON READS THEM.
//
// Three of this framework's refusals mean one thing whatever the domain and
// whatever the object, and say what to do next on their own: a write that lost
// its compare-and-set rounds ([ErrConflict]), a create of what is already there
// ([ErrExists]) and a write about what a deletion marker keeps deleted
// ([ReasonDeleted]). What the error itself says about each is the subject it
// met, a broker path nobody can act on, so every surface that tells one of
// them tells it in the words below: the agent's tools, to a model, and every
// surface that answers a person in words of its own, the identity directory
// first among them. A person's act route and the human write surface answer
// each in its code's own sentence instead (ADR-0060).
//
// HERE, BESIDE THE SENTINELS, because this is the one package every surface
// that meets them already imports. The words lived in internal/agent/builtin,
// which the API's surfaces outside the operator's do not import, so the
// identity directory answered a lost race with the framework's own text, the
// subject it met included. A second copy there would have been the same
// sentence kept in step by nothing.
//
// Every other refusal of this package is a CONDITION of the node that met it,
// told in [ConditionSentence]'s words; this function answers none of them.

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
// Every other refusal this package makes is a condition of the node that met
// it: a write's [Reason] or a read's [ReadRefusal]. Each carries a detail
// written for whoever runs the node ([Unavailable.Detail], [Refused.Detail]),
// which names positions, settings, record versions and the broker's own
// words, and that detail is the node's log's, never a screen's: a person
// reading "statelog: unavailable (behind): the write needs this node to have
// applied 3:4182" learns nothing they can act on, and the framework's own
// words in front of it are a Go error's. So a surface that tells a person
// about a condition tells it in the words below, one sentence per code, and
// logs the detail.
//
// EACH SENTENCE SAYS WHAT HAPPENED AND WHAT TO DO, and the second half agrees
// with whether the answer carries a Retry-After ([RetryAfter]): a code that
// clears on its own asks the person to try again in a moment ([TryAgainSoon]),
// or in a few minutes where what clears it is the log's duplicate window
// ([TryAgainLater]), and one that does not never does, because a person told
// to try again in a moment where no wait changes the answer tries again in a
// moment, for ever.
// [TestEveryConditionIsToldInWordsForAPerson] walks every [Reason] and every
// [ReadRefusal] this build names and holds each to that, so a code added
// without a sentence fails the build rather than reaching a screen as the
// framework's text.

// TryAgainSoon is what a person is told to do about a condition that clears
// on its own: every one whose answer carries a Retry-After.
const TryAgainSoon = "Try again in a moment."

// TryAgainLater is what a person is told to do about a condition that clears
// on its own but not soon, whose hint is a minute or more: here, another
// node's copy of their change under a gate that blames its writer
// ([Unavailable.CopyWriter]), which the node that refused takes itself once
// the log's duplicate window lets go of the operation — minutes, not a moment
// ([Unavailable.RetryAfter]); beyond the state log, a coding sandbox's account
// limit.
const TryAgainLater = "Try again in a few minutes."

// WHO A CONDITION IS ABOUT is never "this node". A person's request reaches one
// node and the state log that refuses it may be another's: the estate router
// hands a data node's refusal back as its answer whenever the asking node's
// own copy does not serve — always, on a node without `data`. So each sentence
// names the node by what it is doing — handling the change, answering the
// question — which is true of either, and an administrator is sent to "the one
// that refused", which the asking node's log names where it was a data node it
// asked (`estate_peer_refused`).
//
// AND "AN ADMINISTRATOR NEEDS TO", never "ask an administrator": the identity
// directory's surfaces answer these too, and their reader is the
// administrator.
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
// NONE SAYS WHETHER ANYTHING WAS WRITTEN, a deletion's excepted
// ([Deleted]): a refusal is of one record, and the gesture it stopped may
// have written others before it — a direct message's open, the steps of an
// identity edit — which the surface answering it says (`landed`, a tool's own
// sentence). "So nothing was written" here would contradict them.
//
// A FULL LOG IS ONE AREA'S: every domain's log has a ceiling of its own, so
// changes of every other kind still save, and "the company's records are
// full" told a person nothing could be.
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
// that blames that copy's writer ([Unavailable.CopyWriter]): the node that
// refused passed its own fences and takes the change itself once the log's
// duplicate window has let go of the operation, so its advice is to send it
// again then — never to readmit a node the fleet evicted on purpose, nor to
// remake a change that will land as it is.
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

// ConditionLevel is the level a surface logs a condition's error at, where the
// detail a person is never shown goes: WARN where no wait clears it
// ([RetryAfter] is zero), since an administrator is the only one who can act
// on it, and cleared where one does, which the caller names because it turns
// on who asks again — INFO for a gesture somebody made once, DEBUG for a
// question a screen asks again after every hint for as long as a node is
// catching up, where a line per ask is noise beside the alarm that names it.
//
// ONE RULE FOR EVERY SURFACE, the estate router's line about a data node's
// refusal among them, because a level chosen at each put a log at its ceiling
// at INFO on one surface and WARN on the next. Each surface logs under its own
// component and with its own fields; only the level is decided here.
func ConditionLevel(err error, cleared slog.Level) slog.Level {
	if RetryAfter(err, time.Second) == 0 {
		return slog.LevelWarn
	}
	return cleared
}
