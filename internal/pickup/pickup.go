// Package pickup decides the WORK PICKUP (ADR-0112): the one wake derived from
// state rather than a change. An assignment wakes its assignee once, and a
// turn that did not act on it was the last word on the item.
//
// # Once per item version
//
// A pickup is identified by [Key]: the seat, the item, the last change to it
// that would have woken the seat, its status and, for started work, the seat's
// own last progress on it. Its event id is derived from the key, so the fleet's
// completion ledger refuses it again on any node and across restarts, and a
// fleet claim on the key keeps two nodes from both sending it during a move.
// A seat's own comment moves no part of the key, so a comment alone does not
// re-arm the item at once. A wait until a date is part of the key once it
// passes, so the item is handed once when its date comes.
//
// # What answers a pickup, and a recheck a day
//
// A pickup's turn is delivered only by a typed answer on its item (ADR-0122):
// a change to its fields or an ask, read off the seat's own rows on the item
// since the turn began, never a comment alone, or by the item leaving the seat
// meanwhile. An item still ready for the seat comes back once each
// [RecheckAfter] it stays quiet: the key gains the instant the recheck came due
// ([Key.Recheck]), so the ledger takes it as new work. An answer that took the
// item out of the seat's ready set holds it there; one that left it ready, as
// words and an edit of its content do, is the seat's last word, and a day on
// it comes back. A recheck is never handed within [RecheckAfter] of the end of
// the item's last pickup at its version, whatever instant it fell due at, so an
// item quiet for days before its first pickup is not handed twice in an hour.
//
// # A wait on a person
//
// An agent's wait whose reason names a person it has not asked is ready
// before its date ([worklet.PersonsNamed]). Its key is marked ([Key.OnPerson]),
// so the pickup that hands it when its date comes is a new one.
//
// # A dead wait
//
// An item whose every open blocker is a pull request nothing moves is ready
// for its holder (ADR-0128). Its key is the heads nothing moves and nothing
// else ([Key.Dead]), so it is handed once per head, whatever else changes.
//
// # When a seat is idle
//
// Decided by the node holding the seat ([Conditions], [Idle]): nothing of the
// seat running, waiting, paused, parked or refused, the node neither shedding
// nor draining, and all of it true on two passes in a row.
//
// # The cooldown
//
// A fleet claim on the seat ([CooldownKey]) spaces a pickup that passes over
// an item the seat was already handed and left unchanged: declines, not work.
package pickup

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/workkey"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Source is the event source a pickup is published under.
const Source = "pickup"

// Tick is how often the holder of a seat decides its pickup. A minute: the
// idle settle is one tick, so a seat whose turn ended is handed its next item
// within two, and a company of fifty idle seats costs fifty indexed reads a
// minute.
const Tick = time.Minute

// RecheckAfter is how long an item stays quiet without a typed answer before
// it comes back to its holder (ADR-0122). A day, the stand-up rhythm a team
// rechecks a stalled item at: long enough that a wait on a person's working
// day is not nagged, short enough that a condition met overnight is noticed
// the next morning. A constant: no company has a reason to leave a stall
// unchecked for longer, and a shorter one would spend a turn an hour on words.
const RecheckAfter = 24 * time.Hour

// Key identifies one pickup: the seat, the item, the last change to it that
// would have woken the seat, its status, the seat's own progress on it when
// started, the start it waited until, whether it is a wait on a person handed
// before its date, and the recheck it came back for.
type Key struct {
	Seat       uuid.UUID
	Item       string
	Moved      uint64
	Status     string
	Progressed uint64
	Waited     int64

	// OnPerson marks a wait on a person handed before its date.
	OnPerson bool

	// Recheck is the instant, in Unix seconds, the item came due for its
	// latest recheck: whole [RecheckAfter]s after it went quiet
	// ([worklet.ReadyItem.Quiet]). Zero before the first.
	Recheck int64

	// Dead is a dead wait's changes and the heads nothing moves them at, as
	// `ref@head` in reference order; empty on every other pickup.
	Dead string
}

// KeyOf is the pickup an item would be for seat at now.
func KeyOf(seat uuid.UUID, item worklet.ReadyItem, now time.Time) Key {
	if item.Why == types.PickupDeadWait {
		heads := make([]string, 0, len(item.DeadWaits))
		for _, d := range item.DeadWaits {
			heads = append(heads, d.Ref+"@"+d.Head)
		}
		return Key{Seat: seat, Item: item.ID, Dead: strings.Join(heads, " ")}
	}
	key := Key{Seat: seat, Item: item.ID, Moved: item.Moved, Status: string(item.Status),
		Progressed: item.Progressed, OnPerson: item.Why == types.PickupWaitsOnPerson}
	// The instant it waited until: passed, or a wait on a person ahead.
	if item.Start != nil {
		key.Waited = item.Start.Unix()
	}
	if due, ok := recheckDue(item, now); ok {
		key.Recheck = due.Unix()
	}
	return key
}

// recheckDue is the instant item came due for its latest recheck at now, and
// false before its first or for an item a typed answer holds.
func recheckDue(item worklet.ReadyItem, now time.Time) (time.Time, bool) {
	if item.Quiet.IsZero() {
		return time.Time{}, false
	}
	n := now.Sub(item.Quiet) / RecheckAfter
	if n < 1 {
		return time.Time{}, false
	}
	return item.Quiet.Add(n * RecheckAfter), true
}

// earlierKeys is the keys key's item had at its version before the recheck key
// names: none, and each recheck that came due since the item went quiet, as far
// back as the ledger remembers. Empty for a key that names no recheck.
func earlierKeys(key Key, item worklet.ReadyItem) []Key {
	if key.Recheck == 0 || item.Quiet.IsZero() {
		return nil
	}
	first := key
	first.Recheck = 0
	out := []Key{first}
	day := int64(RecheckAfter / time.Second)
	for n := (key.Recheck-item.Quiet.Unix())/day - 1; n >= 1 && len(out) <= ledgerDays; n-- {
		earlier := key
		earlier.Recheck = item.Quiet.Unix() + n*day
		out = append(out, earlier)
	}
	return out
}

// ledgerDays is how many rechecks back an item's earlier pickups are looked
// for: the completion ledger remembers a pickup for seven days, and one that
// ended longer ago than a day spaces nothing.
const ledgerDays = 8

// nextRecheck is when item's key next changes on time alone, and false for one
// it never will.
func nextRecheck(item worklet.ReadyItem, now time.Time) (time.Time, bool) {
	if item.Quiet.IsZero() {
		return time.Time{}, false
	}
	n := max(now.Sub(item.Quiet)/RecheckAfter, 0)
	return item.Quiet.Add((n + 1) * RecheckAfter), true
}

// pickupNamespace scopes derived pickup ids. Fixed for the life of the
// deployment: a new one would hand every seat every item again.
var pickupNamespace = uuid.MustParse("7d0c6f2e-4b1a-5e39-8c27-91f3a6b2d4e8")

// text keeps an item with no start and no recheck under the key older builds
// derive; a recheck is marked, so it never reads as a start.
func (k Key) text() string {
	text := k.Seat.String() + "\x00" + k.Item + "\x00" +
		strconv.FormatUint(k.Moved, 10) + "\x00" + k.Status + "\x00" +
		strconv.FormatUint(k.Progressed, 10)
	if k.Waited != 0 {
		text += "\x00" + strconv.FormatInt(k.Waited, 10)
	}
	if k.OnPerson {
		text += "\x00p"
	}
	if k.Recheck != 0 {
		text += "\x00r" + strconv.FormatInt(k.Recheck, 10)
	}
	if k.Dead != "" {
		text += "\x00d" + k.Dead
	}
	return text
}

// WakeID is the pickup's event id: the same on every node and every attempt.
func (k Key) WakeID() uuid.UUID { return uuid.NewSHA1(pickupNamespace, []byte(k.text())) }

// WorkKey is the completion ledger's key for the pickup: the one a turn on
// its event alone records ([workkey.Derive]).
func (k Key) WorkKey() string { return workkey.Derive([]string{k.WakeID().String()}) }

// ClaimKey is the fleet claim that keeps a pickup from being published twice
// while its first copy waits for its turn.
func (k Key) ClaimKey() string { return "pickup|item|" + k.WakeID().String() }

// ProgressKey is the fleet claim that spaces a pickup handed again for the
// seat's own progress alone: the key without the progress.
func (k Key) ProgressKey() string {
	base := k
	base.Progressed = 0
	return "pickup|progress|" + base.WakeID().String()
}

// CooldownKey is the fleet claim that spaces one seat's pickups.
func CooldownKey(seat uuid.UUID) string { return "pickup|seat|" + seat.String() }

// Event is the pickup's wake: a [types.WorkPickup] naming the item and whom
// its work is for, under the derived id and the instant of the change the key
// names, filed under the item's own conversation.
func Event(seat uuid.UUID, role string, item worklet.ReadyItem, key Key, ready int,
	capped bool, trace events.TraceContext) *events.Event {

	payload := types.WorkPickup{
		Agent: seat.String(), RoleName: role,
		// THE ADDRESS: the id where another task claimed the key first.
		Item: item.ID, Key: worklet.ItemAddress(item.ID, item.Key, item.KeyCollision),
		Status: string(item.Status), Moved: item.Moved, Progressed: item.Progressed,
		Why: item.Why, Ready: ready, ReadyCapped: capped, WorkingFor: item.For,
		SubmittedBy: item.SubmittedBy, WaitsOn: item.WaitsOn,
	}
	for _, d := range item.DeadWaits {
		payload.DeadWaits = append(payload.DeadWaits, types.DeadChange{Ref: d.Ref, Head: d.Head,
			Author: d.Author, Draft: d.Draft})
	}
	if key.Recheck != 0 {
		// A RECHECK SAYS SO, and since when the item has been quiet.
		quiet := item.Quiet.UTC()
		payload.Quiet = &quiet
	}
	ev := events.New(payload, trace)
	ev.ID = key.WakeID()
	if !item.MovedAt.IsZero() {
		ev.Timestamp = item.MovedAt.UTC()
	}
	ev.Source = Source
	conversation := worklet.ItemConversation(item.ID)
	notify.Stamp(ev, conversation, conversation)
	return ev
}
