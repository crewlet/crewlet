// Package pickup decides the WORK PICKUP (ADR-0112): the one wake derived from
// state rather than a change. An assignment wakes its assignee once, and a
// turn that did not act on it was the last word on the item.
//
// # Once per item version
//
// A pickup is identified by [Key]: the seat, the item, the last change
// somebody else made to it, its status and, for started work, the seat's own
// last progress on it. Its event id is derived from the key, so the fleet's
// completion ledger refuses it again on any node and across restarts, and a
// fleet claim on the key keeps two nodes from both sending it during a move.
// A seat's own comment moves no part of the key, so declining with one is
// final until somebody else changes the item.
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

// Key identifies one pickup: the seat, the item, the last change somebody
// other than the seat made to it, its status, and the seat's own progress on
// it when started.
type Key struct {
	Seat       uuid.UUID
	Item       string
	Moved      uint64
	Status     string
	Progressed uint64
}

// KeyOf is the pickup an item would be for seat.
func KeyOf(seat uuid.UUID, item worklet.ReadyItem) Key {
	return Key{Seat: seat, Item: item.ID, Moved: item.Moved, Status: string(item.Status),
		Progressed: item.Progressed}
}

// pickupNamespace scopes derived pickup ids. Fixed for the life of the
// deployment: a new one would hand every seat every item again.
var pickupNamespace = uuid.MustParse("7d0c6f2e-4b1a-5e39-8c27-91f3a6b2d4e8")

func (k Key) text() string {
	return k.Seat.String() + "\x00" + k.Item + "\x00" +
		strconv.FormatUint(k.Moved, 10) + "\x00" + k.Status + "\x00" +
		strconv.FormatUint(k.Progressed, 10)
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

	ev := events.New(types.WorkPickup{
		Agent: seat.String(), RoleName: role,
		// THE ADDRESS: the id where another task claimed the key first.
		Item: item.ID, Key: worklet.ItemAddress(item.ID, item.Key, item.KeyCollision),
		Status: string(item.Status), Moved: item.Moved, Progressed: item.Progressed,
		Why: item.Why, Ready: ready, ReadyCapped: capped, WorkingFor: item.For,
	}, trace)
	ev.ID = key.WakeID()
	if !item.MovedAt.IsZero() {
		ev.Timestamp = item.MovedAt.UTC()
	}
	ev.Source = Source
	conversation := worklet.ItemConversation(item.ID)
	notify.Stamp(ev, conversation, conversation)
	return ev
}
