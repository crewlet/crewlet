package coord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"
)

// A person's chat read state: how far they have read in each room they have
// opened, which rooms they have muted, and whether they are in do-not-disturb.
//
// # Why it is here and not on the chat log
//
// It is the one piece of chat state that is NOT a fact about the company. A
// cursor is a fact about one person's attention, nobody replays it, and no
// other member of the fleet derives anything from its history — which is the
// whole of what the state log is for.
//
// The volume decides it. At twenty people reading through a working day and a
// flush no oftener than every fifteen seconds, cursors alone are tens of
// thousands of writes a day: comparable to the entire message census, on a
// strictly ordered identity-claiming log, entering the trim's terms, every
// snapshot, every rejoining node's transfer and the identity claim itself —
// for a number nobody will ever read back. Coordination is where "everything
// else a fleet has to agree on" lives, it already has compare-and-set, and a
// cursor written here costs one key.
//
// # Why the bucket has NO age
//
// A read cursor has no expiry that means anything. A person who has not
// opened a room in four months has still read it up to where they read it,
// and an aged-out cursor would silently mark a whole channel unread — the
// exact opposite of the fact it stores. So the bucket joins the ageless slots
// beside the activation pointer, the budget counters, the channel records and
// the sealed secrets, and removal is a DECISION rather than a clock:
// [ChatReads.ForgetChatRead], called by the membership reconcile when a handle
// leaves the org chart.
//
// # Why advancing is a merge rather than a write
//
// The caller never sees a revision. Two tabs belonging to one person read two
// different rooms at once, and a last-writer-wins PUT would lose whichever
// flush landed first — not a conflict worth reporting to anybody, just a
// channel that quietly re-unreads itself. So the operation is a forward MERGE:
// cursors only ever move forward, an absent mute list leaves the stored one
// alone, and the store resolves the race itself by retrying its own
// read-modify-write. That is safe precisely BECAUSE the merge is monotonic —
// re-applying it is a no-op, so a retry cannot lose anything.

// MaxReadCursors bounds how many channels one person's record remembers.
//
// 256, matching the bound the tracker puts on a person's own lists, and the
// number is the easy half. The hard half is what happens AT the cap, and it is
// genuinely different here: the tracker's cap is safe because entries below
// the seen-through position are PRUNED on every write, so the list bounds
// itself and the cap is a backstop nobody reaches. A read cursor is never
// prunable — every one of them is still true.
//
// So this IS the discarding cap, and the discard is stated rather than
// discovered: past 256 the LEAST RECENTLY ADVANCED cursor is dropped, and that
// channel reads as unread from its tail the next time the person opens it. The
// cost is a stale badge on the room somebody has ignored longest, which is the
// least wrong thing to lose; the alternative — refusing the write — would
// freeze every other cursor in the record to protect the one nobody looks at.
const MaxReadCursors = 256

// ChatReadState is one person's whole chat read state.
type ChatReadState struct {
	// Cursors maps a channel id to the PACKED log position that person
	// has read through. Packed rather than a triple because it is only
	// ever compared, never decomposed: a cursor that cannot be compared
	// with a message's own version is not a cursor.
	Cursors map[string]int64 `json:"cursors,omitempty"`

	// Muted are the channels that raise no badge and no push. A mute
	// suppresses NOTICE, never delivery: the messages are still there and
	// still searchable, and a seat's wake is not affected by anybody's
	// mute at all.
	Muted []string `json:"muted,omitempty"`

	// DNDUntil is when do-not-disturb ends. The ZERO INSTANT means not in
	// do-not-disturb, and an instant in the past means it is already over
	// — which is why it is an instant rather than a bool: a bool would
	// need a second gesture to clear it and would strand a person silenced
	// by a toggle they forgot.
	DNDUntil time.Time `json:"dnd_until,omitempty"`

	// UpdatedAt is diagnostic. Nothing branches on it — the bucket has no
	// age — but a record with no stamp makes "since when" unanswerable
	// without reading the broker's own metadata.
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// ChatReadDelta is what one flush changes.
//
// EVERY FIELD IS OPTIONAL AND ABSENT MEANS UNCHANGED, which is what lets two
// tabs flush different facts about one person without either erasing the
// other's. A caller that wants to clear the mute list sends an empty
// non-nil slice; nil leaves it alone.
type ChatReadDelta struct {
	// Cursors are advanced, never set: a value at or below what is stored
	// is ignored. A cursor that could move backwards would make "unread"
	// flicker as two tabs disagree about which is further along.
	Cursors map[string]int64

	// Muted replaces the whole list when non-nil. It is a set a person
	// edits deliberately, so the last deliberate edit is the right answer
	// and a per-entry merge would make un-muting unreliable.
	Muted []string

	// DNDUntil replaces when non-nil, including with the zero instant,
	// which is how do-not-disturb is cleared.
	DNDUntil *time.Time

	// At stamps the record. Passed in rather than read from a clock here,
	// because every other write in this package takes its instant from the
	// caller and a package that reads the clock in one place is a package
	// whose tests are serial.
	At time.Time
}

// ChatReads is the company's per-person chat read state.
type ChatReads interface {
	// ChatRead reports one person's read state.
	//
	// A person who has read nothing is a ZERO STATE and no error: the
	// absence of a record is the ordinary condition of somebody who has
	// not opened chat yet, and making the caller distinguish it from a
	// failure would push a three-valued answer onto a question that has
	// two honest ones.
	ChatRead(ctx context.Context, handle string) (ChatReadState, error)

	// AdvanceChatRead merges a flush into one person's record and reports
	// the state that resulted.
	//
	// FORWARD-ONLY on the cursors, so it is idempotent and order-free: two
	// tabs racing produce the same record whichever lands first, and the
	// store's own retry cannot lose a flush. Past [MaxReadCursors] the
	// least recently advanced cursor is dropped — see the constant.
	AdvanceChatRead(ctx context.Context, handle string, delta ChatReadDelta) (ChatReadState, error)

	// ForgetChatRead removes a person's record, reporting whether one was
	// there.
	//
	// The bucket has no age, so this is the ONLY way a record leaves it.
	// Its caller is the membership reconcile: a handle that is no longer
	// in the org chart has no rooms to have read.
	ForgetChatRead(ctx context.Context, handle string) (bool, error)

	// ChatReaders lists the handles holding read state.
	//
	// What makes the reconcile possible at all: without it, finding the
	// records of people who left would mean knowing who used to be here,
	// which is the question the org chart no longer answers.
	ChatReaders(ctx context.Context) ([]string, error)
}

// MergeChatRead applies a flush to a stored state and reports the result.
//
// THE ONE IMPLEMENTATION OF THE MERGE RULE, shared by both backends, because a
// merge written twice is two answers to "did this cursor move" and the two
// would differ first on exactly the case nobody tests: a delta that arrives
// out of order.
//
// It is PURE and MONOTONIC. Pure, so both backends and the suite exercise the
// same arithmetic without a store. Monotonic, so re-applying it is a no-op —
// which is what makes a lost compare-and-set safe to retry, and is the whole
// reason the caller never sees a revision.
func MergeChatRead(state ChatReadState, delta ChatReadDelta) (ChatReadState, error) {
	out := ChatReadState{
		Cursors:   make(map[string]int64, len(state.Cursors)+len(delta.Cursors)),
		Muted:     slices.Clone(state.Muted),
		DNDUntil:  state.DNDUntil,
		UpdatedAt: state.UpdatedAt,
	}
	maps.Copy(out.Cursors, state.Cursors)

	for channel, at := range delta.Cursors {
		if channel == "" {
			return ChatReadState{}, errors.New(
				"coord: a read cursor needs a channel; the flush named one with " +
					"no id, which is a client that lost the value on the way here")
		}
		// FORWARD ONLY. A cursor that could move backwards would make an
		// unread badge flicker as two tabs disagree about which is
		// further along — and the loser would be whichever flush the
		// network happened to deliver second.
		if at > out.Cursors[channel] {
			out.Cursors[channel] = at
		}
	}
	evictStaleCursors(out.Cursors)

	if delta.Muted != nil {
		if len(delta.Muted) > MaxReadCursors {
			// REFUSED RATHER THAN CUT, which is the opposite of what the
			// cursors do one line up, and deliberately: a mute list is
			// ONE deliberate edit by one person, so a silent truncation
			// is a room they muted that keeps notifying them and no way
			// to find out. A cursor is machinery and losing the stalest
			// one costs a stale badge.
			return ChatReadState{}, fmt.Errorf(
				"coord: muted carries %d channels and the cap is %d; unmute "+
					"something, or mute a container rather than its rooms",
				len(delta.Muted), MaxReadCursors)
		}
		out.Muted = slices.Clone(delta.Muted)
		slices.Sort(out.Muted)
		out.Muted = slices.Compact(out.Muted)
	}
	if delta.DNDUntil != nil {
		out.DNDUntil = delta.DNDUntil.UTC()
	}
	if !delta.At.IsZero() {
		out.UpdatedAt = delta.At.UTC()
	}
	if len(out.Cursors) == 0 {
		// An empty map and a nil one encode differently, and the record
		// is compared by the suite: one shape, so a person who has muted
		// a room and read nothing round-trips identically on both
		// backends.
		out.Cursors = nil
	}
	return out, nil
}

// evictStaleCursors bounds the map at [MaxReadCursors].
//
// THE STALEST CURSOR GOES, and "stalest" is the LOWEST POSITION rather than a
// timestamp nobody stores: a position is a place on a totally ordered log, so
// the cursor furthest back in it is by construction the room this person has
// read least recently. That is the fact a second `updated_at` per channel
// would have carried, derived from the value already there.
func evictStaleCursors(cursors map[string]int64) {
	if len(cursors) <= MaxReadCursors {
		return
	}
	type entry struct {
		channel string
		at      int64
	}
	all := make([]entry, 0, len(cursors))
	for channel, at := range cursors {
		all = append(all, entry{channel, at})
	}
	// Ordered by position and then by channel id, so two nodes evicting
	// the same over-full record drop the same cursor: the tie-break is
	// what stops "identical inputs, identical output" from depending on
	// Go's map iteration order.
	slices.SortFunc(all, func(a, b entry) int {
		if a.at != b.at {
			return cmp.Compare(a.at, b.at)
		}
		return cmp.Compare(a.channel, b.channel)
	})
	for _, drop := range all[:len(all)-MaxReadCursors] {
		delete(cursors, drop.channel)
	}
}
