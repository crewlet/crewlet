package chat

import (
	"cmp"
	"slices"
	"strings"
	"time"
)

// THE PRUNE'S SELECTION: which rooms a retention sweep covers, and at what
// horizon each is covered.
//
// # The division of labour, stated here because it is the one place two
// packages could each assume the other did it
//
// THIS FILE NAMES THE ROOMS AND THE HORIZON. It is PURE over values — a
// company default, a room's own override — with no clock, no store and no
// transaction in it, so the rule that decides how long a conversation is kept
// can be exercised, and re-measured, without a database.
//
// THE INSTANT AND ANYTHING READ OUT OF THE ROWS ARE THE WRITER'S, formed
// inside [Store.Prune]'s own decide, in the snapshot the record is built from.
// That is not a tidiness preference:
//
//   - A CUTOFF IS A DECISION THAT ALREADY HAPPENED. The record carries an
//     INSTANT rather than the day count it came from ([Prune.Cutoff]), because
//     a node replaying it a month later must delete what the duty decided
//     then, not what the same arithmetic would decide now. The instant
//     therefore has to be taken where the record is formed and refused if it
//     is in the future, which only the writer can do.
//   - A COUNT TAKEN OUT HERE WOULD BOUND NOTHING. The apply's row budget
//     ([statelog.ApplyTxRowBudget]) is checked at RECORD boundaries and a
//     record is never split, so "how much is below this cutoff" is a question
//     about the rows the record will actually be decided against. Asked in
//     another transaction it is a number from another instant, and a selection
//     that answered it would be promising something it cannot see.
//
// So a duty composes the two: it reads the company's rooms, asks [Eligible]
// which of them have a horizon at all, and publishes one prune per room
// through the write path — which forms the cutoff, checks it against its own
// clock and decides inside its own snapshot.
//
// # Why every eligible room is named, with no cap on the sweep
//
// Because a cap needs an order, and the only order available here is the
// rooms' own ids: a sweep that took the first N alphabetically would prune the
// same rooms every tick and never reach the tail, which is a company whose
// last rooms are kept for ever while a screen says they are not. The set is
// already bounded — [MaxChannels] rooms per company — and a prune for a room
// with nothing below its cutoff is one record that deletes nothing, which is
// cheaper than the bookkeeping that would remember where a capped sweep had
// got to.

// RetentionSource says whose setting decided a room's horizon.
//
// A NAMED STRING TYPE with a closed set, so a value that reached a screen or
// an audit row is one this build wrote rather than an empty string that reads
// as "nobody decided this".
type RetentionSource string

const (
	// RetentionCompany is the company default, which is what a room that
	// says nothing about retention is kept for.
	RetentionCompany RetentionSource = "company"

	// RetentionChannel is the room's OWN override, which beats the
	// company default in both directions: a room may be kept longer than
	// the company keeps everything else, and — the case this matters for
	// — a room may be kept for ever when the company is not.
	RetentionChannel RetentionSource = "channel"
)

// RetentionSources are the two.
var RetentionSources = []RetentionSource{RetentionCompany, RetentionChannel}

// Valid reports whether a source is one this build writes.
func (s RetentionSource) Valid() bool { return slices.Contains(RetentionSources, s) }

// ChannelRetention is one room as the selection reads it.
//
// TWO FIELDS, AND THE POINTER IS THE WHOLE OF IT. `retention_days` is NULL on
// the row for a room that inherits, and a present 0 is [RetentionForever] —
// three settings in one column, which is exactly why the document, the record
// and this carry a pointer rather than an int. Collapsing the absent case into
// zero deletes a company's whole chat history a year after somebody asked for
// it to be kept for ever; collapsing zero into the absent case silently
// re-applies the default to the one room that opted out.
type ChannelRetention struct {
	ChannelID string

	// RetentionDays is the room's own override, nil when it inherits.
	RetentionDays *int
}

// Horizon is how long a room keeps what was said in it, and whose setting says
// so.
//
// THE THREE TRAVEL TOGETHER because they are one answer: the number a person
// set, the quantity a cutoff is subtracted by, and who decided it. Returned
// separately they would be three values a caller could pair wrongly, and the
// pairing is the whole of what [HorizonFor] does.
type Horizon struct {
	// Days is the horizon as a person set it and reads it back, which is
	// also what an audit line renders. It is always positive: a room with
	// no horizon is not a target at all.
	Days int

	// Duration is the same number as a duration, converted ONCE here so
	// every caller subtracts the same quantity. A DAY IS 24 HOURS FLAT:
	// the cutoff is compared against the broker's own stored instants on
	// every node, so a horizon that moved with somebody's daylight saving
	// would be a different set of rows deleted depending on which node
	// formed it.
	Duration time.Duration

	// Source says whose setting decided it, so an operator asking why a
	// room was pruned at ninety days is told where the ninety came from
	// rather than left to compare two config values by hand.
	Source RetentionSource
}

// PruneTarget is one room a sweep covers, and the horizon it is covered at.
type PruneTarget struct {
	ChannelID string

	Horizon
}

// Cutoff is the instant this target's prune deletes below, given the caller's
// own `now`.
//
// THE INSTANT IS THE CALLER'S, taken inside the snapshot the record is formed
// in — see the division at the top of this file. This method only subtracts,
// which is what keeps the arithmetic exercisable without a clock while leaving
// the decision of WHEN with the writer that has to defend it.
func (t PruneTarget) Cutoff(now time.Time) time.Time {
	return now.UTC().Add(-t.Duration)
}

// HorizonFor is the ONE place a room's own retention beats the company's.
//
// The second return is "there is a horizon at all", and it exists for
// [time.Duration]'s own blind spot: a zero duration is the one value that
// cannot say "for ever", so a caller reading only the number would prune a
// room that asked to be kept permanently the moment it forgot which way round
// the zero meant. It is the same shape
// [github.com/crewlet/crewlet/internal/config.ChatNativeConfig.MessageRetention]
// answers with, and for the same reason.
//
// A NEGATIVE HORIZON IS NOT ONE. Both write-path validations refuse a negative
// override naming the field ([ChannelCreate.Validate], [ChannelPatch.Validate]),
// so a negative reaching here is a row some other build wrote — and the one
// reading it could have is a cutoff in the FUTURE, which deletes everything
// the room ever said. There is no inverse for that, so it is read as "no
// horizon" rather than acted on.
func HorizonFor(companyDays int, override *int) (Horizon, bool) {
	days, source := companyDays, RetentionCompany
	if override != nil {
		days, source = *override, RetentionChannel
	}
	if days <= RetentionForever {
		return Horizon{Source: source}, false
	}
	return Horizon{
		Days: days, Duration: time.Duration(days) * 24 * time.Hour,
		Source: source,
	}, true
}

// Eligible names every room a prune covers, and the horizon each is covered
// at.
//
// PURE, AND THAT IS WHAT IT IS FOR. Given the same company default and the
// same rooms it answers the same thing, in the same order, for ever: there is
// no clock in it, no store behind it and nothing read from the rows. What it
// cannot know — whether a room has anything below its cutoff — is deliberately
// not its question; see the division at the top of this file.
//
// THE ORDER IS BY ROOM ID and not by the input's, so two nodes that read the
// company's rooms in different orders — a listing is not an ordering unless
// somebody says so — publish the same sweep in the same sequence, and a log
// read afterwards shows one duty's tick rather than an interleaving nobody can
// follow.
//
// A ROOM WITH NO ID IS NOT A ROOM. It is dropped rather than refused, because
// this is a selection over rows somebody else read and the alternative is a
// sweep that stops the first time a malformed row appears — which is the whole
// company's retention held up by one row nothing else is bothered by.
func Eligible(companyDays int, rooms []ChannelRetention) []PruneTarget {
	out := make([]PruneTarget, 0, len(rooms))
	for _, room := range rooms {
		id := strings.TrimSpace(room.ChannelID)
		if id == "" {
			continue
		}
		horizon, keeps := HorizonFor(companyDays, room.RetentionDays)
		if !keeps {
			continue
		}
		out = append(out, PruneTarget{ChannelID: id, Horizon: horizon})
	}
	slices.SortFunc(out, func(a, b PruneTarget) int {
		return cmp.Compare(a.ChannelID, b.ChannelID)
	})
	return out
}
