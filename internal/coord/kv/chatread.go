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

// A person's chat read state, as one record per seat handle.
//
// # Why the whole state is one key
//
// A cursor per channel would be the obvious shape and it is wrong twice. It
// would make reading one person's state a bucket walk filtered by handle —
// the listing this package spends a file explaining the cost of — on the path
// of every chat screen paint. And it would put the mute list somewhere else
// again, so "what does this person see" would be two reads that can disagree.
//
// One key costs a read-modify-write on every flush, which is what the forward
// merge below is for: the write is idempotent, so the retry that a lost
// compare-and-set forces cannot lose anything.
//
// # Every read and write is this package's own
//
// The read is the stream LEADER's ([FleetStore.get]), never the bucket
// handle's direct get: a replica a write behind would hand the merge a record
// that is already stale, and the compare-and-set built on it would lose round
// after round to a write nobody else is making. The first write is [createKey]
// rather than the handle's Create, because a record that was forgotten leaves
// a purge marker the next flush has to write over (markers.go). And a lost
// compare-and-set is told by the package's own answers — [jetstream.ErrKeyExists]
// from that create, [lostUpdateRace] for an update or a purge — on a bucket
// handle that is [settled]: the leader's in-flight answer, which a replicated
// bucket gives two tabs flushing one record at once, is waited out inside the
// write (settle.go), so a refusal that reaches this file is one the leader
// DECIDED.
//
// # Why nothing ages a record out
//
// The bucket has NO age, for the reason [coord.ChatReads] states: an expired
// cursor silently marks a channel unread, which is the opposite of the fact it
// stores. Records leave only through [FleetStore.ForgetChatRead], which PURGES
// — every removal in a bucket no clock ages is a purge (markers.go) — and the
// marker that leaves is removed in its turn by [FleetStore.SweepMarkers] under
// the maintenance duty, so a forgotten record costs the bucket nothing for
// good.

// chatReadClass is the key class, so a read record cannot collide with
// anything else that might one day share this bucket.
const chatReadClass = "chatread"

// chatReadKey composes one person's key.
func chatReadKey(handle string) string {
	return coord.DocumentKey(chatReadClass, handle)
}

// ChatRead reports one person's read state.
//
// A missing key is the ZERO STATE and no error: somebody who has not opened
// chat is the ordinary case, not a failure, and making every caller tell that
// from an unreachable store would push a three-valued answer onto a question
// with two honest ones. An unreadable store still raises — that IS the third
// fact, and it is the caller's to see.
func (f *FleetStore) ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error) {
	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/kv: read a person's chat state needs a handle")
	}
	state, _, err := f.chatReadAt(ctx, handle)
	return state, err
}

// chatReadAt reads the record and the revision it was at, which is what the
// merge's compare-and-set expects. A missing key — never written, forgotten,
// or forgotten and swept — reads as revision zero, which is what
// [FleetStore.putChatReadAt] takes to mean "create".
func (f *FleetStore) chatReadAt(ctx context.Context, handle string) (coord.ChatReadState, uint64, error) {
	entry, err := f.get(ctx, f.chatReads, chatReadKey(handle))
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return coord.ChatReadState{}, 0, nil
	}
	if err != nil {
		return coord.ChatReadState{}, 0, unavailable(
			fmt.Sprintf("read the chat read state for %s", handle), err)
	}
	var state coord.ChatReadState
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return coord.ChatReadState{}, 0, fmt.Errorf(
			"coord/kv: decode the chat read state for %s: %w", handle, err)
	}
	// THE ENTRY'S OWN REVISION, which is the stream sequence the record was
	// written at: strictly increasing across every write to the bucket and
	// never reused, so it outlives a purge — see [coord.ChatReadState.Revision].
	state.Revision = entry.Revision()
	return state, entry.Revision(), nil
}

// AdvanceChatRead merges a flush into one person's record.
//
// THE MERGE IS FORWARD-ONLY AND THEREFORE RETRYABLE. Re-applying it is a
// no-op, so losing a compare-and-set costs one more round trip and never a
// flush — which is why the caller never writes against a revision and never
// has to decide what a conflict means. Two tabs reading two rooms converge on the
// same record whichever lands first.
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
// Through the same loop as a flush, so the two cannot lose each other: the
// stamp is a field of the record a flush rewrites, and [coord.MergeChatRead]
// carries it through untouched.
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
// compare-and-set, reporting the state that resulted and whether the person
// has a record.
//
// create says whether a person with no record gets one — a flush does, an
// absence stamp does not — and a change that reports nothing changed writes
// nothing. what is the verb a failure names.
//
// # How long it races
//
// For the fleet store's patience ([FleetStore.retryRaces]), never for a count
// of rounds. A person's record has writers nothing bounds — every tab and
// device they read in, and the reconcile — and each flushes again as soon as
// it has landed, so a count measured how many of them were flushing rather
// than whether the store answered: sixteen rounds held sixteen writers with
// one to spare at three replicas, and a seventeenth tab was an outage on a
// store answering every write. Each round reads the record again and applies
// the change again, which is safe because a flush is an idempotent merge and
// a stamp keeps the first one: the change is exactly as true against the
// winner's record as it was against the one it lost to.
//
// A round is lost only to a write the leader DECIDED — the bucket handle
// waits out its in-flight answer inside the write ([settled]) — so every round
// lost is another writer's write that landed, and the jittered wait between
// rounds spreads the writers that lost to one winner rather than sending them
// back together. Raced out, the change is [outwaited]: unavailable, and a
// DEFINITE "did not land", since every write it made had been answered and
// refused.
//
// # Which failures may have written
//
// ONE: a put whose answer never came, or that the leader still had not decided
// when the write's budget ran out ([writeUnknown], marked
// [coord.ErrWriteUnknown]). A read that failed, a merge that refused, a first
// write's create that was refused and then could not re-read or was refused
// on every round ([FleetStore.putChatReadAt]), a patience raced out and a
// caller that hung up between rounds all return before any put landed —
// every put made was refused — so they are this store's condition and nothing
// more, and a caller told so may say nothing was recorded. A caller told the
// first may not, which is the whole reason the mark exists: the method reads
// before it writes, so its caller cannot tell the two apart by which call
// failed.
func (f *FleetStore) mutateChatRead(ctx context.Context, what, handle string, create bool,
	change func(coord.ChatReadState) (coord.ChatReadState, bool, error),
) (coord.ChatReadState, bool, error) {
	key := chatReadKey(handle)
	var (
		result coord.ChatReadState
		exists bool
	)
	err := f.retryRaces(ctx, what+" the chat read state", handle, func() (bool, error) {
		stored, revision, err := f.chatReadAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 && !create {
			result, exists = coord.ChatReadState{}, false
			return false, nil
		}
		next, changed, err := change(stored)
		if err != nil {
			return false, err
		}
		if !changed {
			next.Revision = revision
			result, exists = next, true
			return false, nil
		}
		// THE REVISION IS NOT PART OF THE RECORD: it is where the write
		// lands, which only the answer to the write can say.
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
			// CLASSIFIED WHERE THE WRITE WAS MADE: marked
			// [coord.ErrWriteUnknown] when the put may have landed, and
			// unavailable alone when a first write's create was refused
			// and could go no further — see [FleetStore.putChatReadAt].
			return false, err
		case !written:
			// SOMEBODY ELSE GOT THERE FIRST — another tab, or the
			// reconcile. Read again and apply again.
			return true, nil
		}
		next.Revision = landed
		result, exists = next, true
		return false, nil
	})
	if err != nil {
		return coord.ChatReadState{}, false, err
	}
	return result, exists, nil
}

// putChatReadAt writes one person's record over the revision the caller read
// — zero meaning it read no live record — reporting the revision it landed at,
// or false with no error when another writer got there first: a create that
// found the key held ([jetstream.ErrKeyExists]), or an update the leader
// refused at a revision a peer had moved past ([lostUpdateRace]). what is the
// verb and object a failure names.
//
// IT CLASSIFIES EVERY OTHER FAILURE, because only here is it known whether a
// write went out. Over a record, the one write is the Update, and anything
// but a refusal from it may have landed ([writeUnknown]) — an update the leader
// never decided ([settled]) among them. A FIRST WRITE IS A
// CREATE, which is a loop that reads between its writes ([createKey]), so its
// failures are two: one of its own writes going unanswered, which may have
// landed and is marked the same way, and a refusal followed by a re-read that
// failed or by a bound of refusals — after which every write it made had been
// refused, so it is unavailable and nothing more. Marking those too would
// have a person told that a flush which definitely did not land "may have
// been recorded", which the [coord.ChatReads] contract promises every
// unmarked error is not.
//
// AN EXHAUSTED CREATE IS A FAILURE, not a lost race for the caller's loop to
// retry. Its bound is set well past anything the measured contention needs
// ([createRounds]), so a create refused that often is behind something more
// rounds of the same race would not outwait either, and the honest answer is
// the one it gives: nothing was written, and the same flush may be sent again.
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
// A PURGE, as every removal in a bucket no clock ages is (markers.go), and as
// [FleetStore.DeleteSecret] is: the record's older revisions go with it
// whatever history the bucket was created with, and the one marker left is the
// maintenance duty's to sweep.
//
// AT THE REVISION IT JUDGED ([FleetStore.purgeChatReadAt]), so the stamp it
// read is the stamp it acts on: a person a peer's sweep has just found back,
// or a flush landing between the read and the purge, moves the record on and
// the purge is refused — and the next round ([FleetStore.retryRaces], for
// [FleetStore.mutateChatRead]'s reason) judges the record as it now stands
// rather than removing one this call never read. A purge the leader never
// decided ([settled]) is a removal that may have happened, and is marked so.
func (f *FleetStore) ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: forget a person's chat state needs a handle")
	}
	key := chatReadKey(handle)
	var removed bool
	err := f.retryRaces(ctx, "forget the chat read state", handle, func() (bool, error) {
		stored, revision, err := f.chatReadAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 || !stored.AbsentBy(absentBy) {
			removed = false
			return false, nil
		}
		purged, err := f.purgeChatReadAt(ctx, key, revision)
		switch {
		case err != nil:
			return false, writeUnknown(
				fmt.Sprintf("forget the chat read state for %s", handle), err)
		case !purged:
			// Moved or gone under the read: judge it again.
			return true, nil
		}
		removed = true
		return false, nil
	})
	if err != nil {
		return false, err
	}
	return removed, nil
}

// purgeChatReadAt purges one person's record only if it is still at the
// revision the caller judged, reporting whether it did.
//
// THE RULE [FleetStore.ForgetChatRead] IS BUILT FROM, in a unit that can be
// driven directly, for [FleetStore.writeFollowAt]'s reason: the interleaving
// it exists for opens INSIDE that method, between its read and its write, and
// the memory twin — which checks and removes under one mutex — can never stage
// it at all. It is the one thing standing between a seat a peer's sweep has
// just found back in the company and the loss of every cursor it holds.
//
// `false` with no error is a LOST RACE, not a failure — the record moved or
// went away under the read, which [lostUpdateRace] tells for a purge exactly
// as it does for an update.
//
// REVISION ZERO REMOVES NOTHING. It is no revision at all to the client —
// [jetstream.LastRevision] of zero sets no expectation, and the purge would
// be unconditional — and a caller that judged no record has nothing to act
// on.
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
// stamp.
//
// ONE FILTERED PASS, like every other listing here: the class filter is a
// subject wildcard the broker matches, so a bucket that one day holds a second
// class does not turn this into a walk that decodes two shapes as one.
//
// A record it cannot decode is RAISED, as the mailbox registry's is: the
// reconcile decides from this list, and a reader dropped from it is one it
// never judges and so never forgets.
func (f *FleetStore) ChatReaders(ctx context.Context) ([]coord.ChatReader, error) {
	var readers []coord.ChatReader
	err := f.eachUnder(ctx, f.chatReads, coord.DocumentFilter(chatReadClass),
		"the chat read state",
		func(entry jetstream.KeyValueEntry) error {
			segments, ok := coord.DocumentSegments(entry.Key())
			if !ok || len(segments) != 2 || segments[0] != chatReadClass {
				// A KEY THIS CLASS DID NOT WRITE. Skipped rather than
				// raised: the filter already narrowed to the class, so
				// anything left is a shape this build does not write, and
				// an invented handle would send the reconcile to forget a
				// seat nobody named.
				return nil
			}
			var stored coord.ChatReadState
			if err := json.Unmarshal(entry.Value(), &stored); err != nil {
				return fmt.Errorf("coord/kv: decode the chat read state for %s: %w",
					segments[1], err)
			}
			readers = append(readers, coord.ChatReader{
				Handle: segments[1], AbsentSince: stored.AbsentSince,
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
