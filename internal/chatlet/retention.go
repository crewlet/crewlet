package chatlet

import (
	"cmp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// THE PRUNE'S SELECTION: which rooms a retention sweep covers, and at what
// horizon each is covered.
//
// THIS FILE NAMES THE ROOMS AND THE HORIZON, PURE over values (a company
// default, a room's override): no clock, no store, no transaction, so the rule
// is testable without a database. THE INSTANT AND EVERYTHING READ FROM THE ROWS
// ARE THE WRITER'S, formed inside [Store.Prune]'s decide, in the snapshot the
// record is built from. A cutoff is a decision that already happened: the
// record carries an INSTANT, not the day count ([Prune.Cutoff]), so a replay a
// month later deletes what the duty decided then. A count taken here would
// bound nothing: the apply's row budget ([statelog.ApplyTxRowBudget]) is
// checked at record boundaries, against rows from the record's own snapshot.

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
// The second return is "there is a horizon at all": a zero [time.Duration]
// cannot say "for ever", so a caller reading only the number would prune a room
// kept permanently. Same shape as
// [github.com/crewlet/crewlet/internal/config.ChatletConfig.MessageRetention].
//
// A NEGATIVE HORIZON IS NOT ONE. The write paths refuse it
// ([ChannelCreate.Validate], [ChannelPatch.Validate]), so one here is a row
// another build wrote, and its cutoff would lie in the FUTURE and delete
// everything the room ever said, irreversibly. It reads as "no horizon".
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

// HorizonConfirmation is how a room's own setting is repeated to confirm it:
// its whole days in decimal, [RetentionForever]'s `0` included, or, for none of
// its own, the word for the setting that then decides — [RetentionCompany].
//
// A shorter horizon is a prune on a delay, so whoever sets one types it twice:
// `?confirm=` on `PUT /chatlet/channels/{channel_id}/retention`, and `-confirm` on
// `crewlet chatlet retention`, which checks it before it sends anything and then
// sends the same word. ONE SPELLING FOR BOTH ENDS, because the command's check
// and the route's are the same comparison and two copies of a word agree only
// until somebody edits one of them.
func HorizonConfirmation(override *int) string {
	if override == nil {
		return string(RetentionCompany)
	}
	return strconv.Itoa(*override)
}

// Eligible names every room a prune covers, and the horizon each is covered
// at. PURE (see the division at the top of this file).
//
// Every eligible room is named, with no cap: room id is the only order, so a
// sweep of the first N would never reach the tail. The set is unbounded (no
// room delete exists), so the duty asks a PAGE at a time and resumes a lap.
//
// THE ORDER IS BY ROOM ID, not the input's: nodes reading in different orders
// publish the same sweep, and a page is a slice of the whole.
//
// A ROOM WITH NO ID IS DROPPED, not refused, so one malformed row cannot hold
// up the company's retention.
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
