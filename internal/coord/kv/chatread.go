package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// A person's chat read state: one record per seat handle.
//
// One key, not a cursor per channel: a person's state stays one read rather
// than a filtered bucket walk on every chat paint, and the mute list shares the
// record, so "what does this person see" cannot disagree with itself. A flush
// is a read-modify-write, safe to retry because the merge is forward-only.
//
// Reads are the leader's ([FleetStore.get]: a stale replica would lose every
// compare-and-set), a first write is [createKey], to write over a forgotten
// record's purge marker (markers.go), and a refusal reaching this file is one
// the leader DECIDED ([settled]). The bucket has no age ([coord.ChatReads]):
// a record leaves only by [FleetStore.ForgetChatRead]'s purge.

// chatReadClass is the key class, so a read record cannot collide with
// anything else that might one day share this bucket.
const chatReadClass = "chatread"

// chatReadKey composes one seat's key.
func chatReadKey(handle string) string {
	return coord.DocumentKey(chatReadClass, handle)
}

// chatReadHandle recovers the seat's handle from a key of the read class,
// reporting false for a key of any other shape — one this build does not
// write, which names no seat it should judge or push.
func chatReadHandle(key string) (string, bool) {
	segments, ok := coord.DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != chatReadClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}

// decodeChatRead reads a stored record back at the revision it was read at.
func decodeChatRead(handle string, raw []byte, revision uint64) (coord.ChatReadState, error) {
	var state coord.ChatReadState
	if err := json.Unmarshal(raw, &state); err != nil {
		return coord.ChatReadState{}, fmt.Errorf(
			"coord/kv: decode the chat read state for %s: %w", handle, err)
	}
	// The entry's own revision: a stream sequence, never reused, so it
	// outlives a purge ([coord.ChatReadState.Revision]).
	state.Revision = revision
	return state, nil
}

// ChatRead reports one person's read state.
//
// A missing key is the zero state and no error: never having opened chat is
// the ordinary case, not a failure. An unreadable store still raises.
func (f *FleetStore) ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error) {
	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/kv: read a person's chat state needs a handle")
	}
	state, _, err := f.chatReadAt(ctx, handle)
	return state, err
}

// chatReadAt reads the record and its revision, for the merge's
// compare-and-set. A missing key (never written, or forgotten) reads as
// revision zero, which [FleetStore.putChatReadAt] takes to mean "create".
func (f *FleetStore) chatReadAt(ctx context.Context, handle string) (coord.ChatReadState, uint64, error) {
	entry, err := f.get(ctx, f.chatReads, chatReadKey(handle))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.ChatReadState{}, 0, nil
	}
	if err != nil {
		return coord.ChatReadState{}, 0, unavailable(
			fmt.Sprintf("read the chat read state for %s", handle), err)
	}
	state, err := decodeChatRead(handle, entry.Value(), entry.Revision())
	if err != nil {
		return coord.ChatReadState{}, 0, err
	}
	return state, entry.Revision(), nil
}

// AdvanceChatRead merges a flush into one person's record.
//
// The merge is forward-only, so re-applying it is a no-op: a lost
// compare-and-set costs a round trip, never a flush, and the caller never
// writes against a revision.
func (f *FleetStore) AdvanceChatRead(ctx context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/kv: advance a person's chat state needs a handle")
	}
	state, _, err := f.mutateChatRead(ctx, "advance", handle, true,
		func(stored coord.ChatReadState) (coord.ChatReadState, bool, error) {
			merged, err := coord.MergeChatRead(stored, delta)
			return merged, true, err
		})
	return state, err
}

// SetChatReadAbsence stamps or clears when a record's seat was first found
// missing from the company the fleet is pointed at, reporting whether the
// seat has a record.
//
// Through the flush's loop, so the two cannot lose each other: the stamp is a
// field of the record, which [coord.MergeChatRead] carries through untouched.
func (f *FleetStore) SetChatReadAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: stamp a person's chat absence needs a handle")
	}
	_, exists, err := f.mutateChatRead(ctx, "stamp the absence on", handle, false,
		func(stored coord.ChatReadState) (coord.ChatReadState, bool, error) {
			next, changed := stored.WithAbsence(since)
			return next, changed, nil
		})
	return exists, err
}

// mutateChatRead applies change to one person's record under a
// compare-and-set, reporting the resulting state and whether a record exists.
// create says whether a missing record gets one (a flush does, an absence
// stamp does not); a change reporting nothing changed writes nothing; what
// names a failure.
//
// Rounds ([casRetry]) are bounded in writers, not calls: twice [fleetWriters]
// outlasts every tab a person holds open. Each round re-reads and
// re-applies, safe because a flush merges and a stamp keeps the first; lost at
// every round is [contended], a definite "did not land". Only a put never
// answered or decided may have landed ([writeUnknown]): a caller cannot tell
// it from a failed read by which call failed. Every other failure wrote nothing.
func (f *FleetStore) mutateChatRead(ctx context.Context, what, handle string, create bool,
	change func(coord.ChatReadState) (coord.ChatReadState, bool, error),
) (coord.ChatReadState, bool, error) {
	key := chatReadKey(handle)
	var (
		result coord.ChatReadState
		exists bool
	)
	settled, err := casRetry(ctx, &f.chatReadTrips, what+" the chat read state for "+handle, func() (bool, error) {
		stored, revision, err := f.chatReadAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 && !create {
			result, exists = coord.ChatReadState{}, false
			return true, nil
		}
		next, changed, err := change(stored)
		if err != nil {
			return false, err
		}
		if !changed {
			next.Revision = revision
			result, exists = next, true
			return true, nil
		}
		// The revision is not stored: only the write's answer says where it lands.
		persisted := next
		persisted.Revision = 0
		raw, err := json.Marshal(persisted)
		if err != nil {
			return false, fmt.Errorf("coord/kv: encode the chat read state for %s: %w", handle, err)
		}
		landed, written, err := f.putChatReadAt(ctx,
			fmt.Sprintf("%s the chat read state for %s", what, handle), key, raw, revision)
		switch {
		case err != nil:
			// Already classified where the write was made ([FleetStore.putChatReadAt]).
			return false, err
		case !written:
			// Another tab or the reconcile won: read and apply again.
			return false, nil
		}
		next.Revision = landed
		result, exists = next, true
		return true, nil
	})
	switch {
	case err != nil:
		return coord.ChatReadState{}, false, err
	case !settled:
		return coord.ChatReadState{}, false, contended(what+" the chat read state", handle)
	}
	return result, exists, nil
}

// putChatReadAt writes one person's record over the revision the caller read
// (zero: no live record), reporting where it landed, or false and no error
// when another writer got there first ([jetstream.ErrKeyExists] on a create,
// [lostUpdateRace] on an update). what names a failure.
//
// It classifies every other failure, since only here is it known whether a
// write went out. An update's may have landed ([writeUnknown]); so may a
// create ([createKey]) one of whose writes went unanswered. A create refused
// and then unable to re-read, or refused past [createRounds], wrote nothing:
// unavailable alone, as [coord.ChatReads] promises an unmarked error is, and a
// failure rather than a lost race, since more rounds of the same race would
// not outwait it.
func (f *FleetStore) putChatReadAt(ctx context.Context, what, key string, raw []byte,
	revision uint64) (uint64, bool, error) {

	if revision == 0 {
		landed, err := f.create(ctx, f.chatReads, key, raw)
		switch {
		case err == nil:
			return landed, true, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			return 0, false, nil
		case errors.Is(err, errCreateUnanswered):
			return 0, false, writeUnknown(what, err)
		default:
			return 0, false, unavailable(what, err)
		}
	}
	landed, err := f.chatReads.Update(ctx, key, raw, revision)
	switch {
	case err == nil:
		return landed, true, nil
	case lostUpdateRace(err):
		return 0, false, nil
	default:
		return 0, false, writeUnknown(what, err)
	}
}

// ForgetChatRead removes a person's record if it has been stamped absent at
// or before absentBy, reporting whether it did.
//
// A purge, like every removal in a bucket no clock ages (markers.go): older
// revisions go with it, and the marker left is the maintenance duty's to sweep.
// It purges at the revision it judged ([FleetStore.purgeChatReadAt]), so a seat
// a peer's sweep just found back, or a flush landing in between, refuses the
// purge and the next round ([casRetry]) judges the record as it now stands. A
// purge the leader never decided ([settled]) may have happened, and is marked so.
func (f *FleetStore) ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: forget a person's chat state needs a handle")
	}
	key := chatReadKey(handle)
	var removed bool
	settled, err := casRetry(ctx, &f.chatReadTrips, "forget the chat read state for "+handle, func() (bool, error) {
		stored, revision, err := f.chatReadAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 || !stored.AbsentBy(absentBy) {
			removed = false
			return true, nil
		}
		purged, err := f.purgeChatReadAt(ctx, key, revision)
		switch {
		case err != nil:
			return false, writeUnknown(
				fmt.Sprintf("forget the chat read state for %s", handle), err)
		case !purged:
			// Moved or gone under the read: judge it again.
			return false, nil
		}
		removed = true
		return true, nil
	})
	switch {
	case err != nil:
		return false, err
	case !settled:
		return false, contended("forget the chat read state", handle)
	}
	return removed, nil
}

// purgeChatReadAt purges one person's record only if it is still at the
// revision the caller judged, reporting whether it did; false with no error is
// a lost race ([lostUpdateRace]), not a failure.
//
// A unit of its own, for [FleetStore.writeFollowAt]'s reason: the interleaving
// it guards opens between [FleetStore.ForgetChatRead]'s read and write, which
// the memory twin can never stage. It alone keeps a seat just found back in
// the company from losing every cursor. Revision zero removes nothing:
// [jetstream.LastRevision] of zero sets no expectation, so the purge would be
// unconditional.
func (f *FleetStore) purgeChatReadAt(ctx context.Context, key string, revision uint64) (bool, error) {
	if revision == 0 {
		return false, nil
	}
	err := f.chatReads.Purge(ctx, key, jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, err
	}
}

// ChatReaders lists every seat holding read state, each with its absence
// stamp, in one pass under the class's subject filter.
//
// A record it cannot decode is raised: the reconcile decides from this list,
// and a reader dropped from it is never judged and so never forgotten.
func (f *FleetStore) ChatReaders(ctx context.Context) ([]coord.ChatReader, error) {
	var readers []coord.ChatReader
	err := f.eachUnder(ctx, f.chatReads, coord.DocumentFilter(chatReadClass),
		"the chat read state",
		func(entry jetstream.KeyValueEntry) error {
			handle, ok := chatReadHandle(entry.Key())
			if !ok {
				// A shape this build does not write: skipped, since an
				// invented handle would have the reconcile forget a seat
				// nobody named.
				return nil
			}
			stored, err := decodeChatRead(handle, entry.Value(), entry.Revision())
			if err != nil {
				return err
			}
			readers = append(readers, coord.ChatReader{
				Handle: handle, AbsentSince: stored.AbsentSince,
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(readers, func(a, b coord.ChatReader) int {
		return cmp.Compare(a.Handle, b.Handle)
	})
	return readers, nil
}

// WatchChatReads streams every seat's read state, the marker, then every
// change, through [watchRecords]: a certified first answer, and late changes
// filtered by revision. A purge is delivered as a forget.
//
// A replica consumer's lag is carried rather than closed with a leader read
// per change: it costs a badge a moment stale, which the tab's next read
// replaces, where a read per change is a leader read per flush per node on the
// fleet's busiest record.
func (f *FleetStore) WatchChatReads(ctx context.Context) (<-chan coord.ChatReadUpdate, error) {
	return watchRecords(ctx, f, f.chatReads, coord.DocumentFilter(chatReadClass),
		recordWatch[coord.ChatReadUpdate]{
			what: "the chat read state",
			ours: func(key string) bool {
				_, ok := chatReadHandle(key)
				return ok
			},
			update: func(kve jetstream.KeyValueEntry) (coord.ChatReadUpdate, error) {
				handle, _ := chatReadHandle(kve.Key())
				u := coord.ChatReadUpdate{Handle: handle}
				if removed(kve) {
					return u, nil
				}
				state, err := decodeChatRead(handle, kve.Value(), kve.Revision())
				if err != nil {
					return coord.ChatReadUpdate{}, err
				}
				u.State = &state
				return u, nil
			},
			undecodable: func(ctx context.Context, kve jetstream.KeyValueEntry, err error) {
				handle, _ := chatReadHandle(kve.Key())
				log.WarnContext(ctx, "chat_read_undecodable", "handle", handle,
					"error", err.Error())
			},
			current: coord.ChatReadUpdate{Current: true},
		})
}
