package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// A person's chat read state, as one record per handle.
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
// # Why there is no purge
//
// The bucket has NO age, for the reason [coord.ChatReads] states: an expired
// cursor silently marks a channel unread, which is the opposite of the fact it
// stores. Records leave only through [FleetStore.ForgetChatRead].

// chatReadClass is the key class, so a read record cannot collide with
// anything else that might one day share this bucket.
const chatReadClass = "chatread"

// casRounds bounds the read-modify-write retry on one person's record.
//
// EIGHT, and the number barely matters: the contention is one person's own
// tabs, so two is the realistic worst case and eight is there for the pause a
// scheduler can put between a read and its update. It is bounded at all
// because an unbounded retry against a bucket that is genuinely refusing
// writes is a screen that hangs rather than a badge that is briefly stale.
const casRounds = 8

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
// merge's compare-and-set expects. A missing key reads as revision zero, which
// is what [jetstream.KeyValue.Create] takes.
func (f *FleetStore) chatReadAt(ctx context.Context, handle string) (coord.ChatReadState, uint64, error) {
	entry, err := f.chatReads.Get(ctx, chatReadKey(handle))
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
	return state, entry.Revision(), nil
}

// AdvanceChatRead merges a flush into one person's record.
//
// THE MERGE IS FORWARD-ONLY AND THEREFORE RETRYABLE. Re-applying it is a
// no-op, so losing a compare-and-set costs one more round trip and never a
// flush — which is why the caller never sees a revision and never has to
// decide what a conflict means. Two tabs reading two rooms converge on the
// same record whichever lands first.
func (f *FleetStore) AdvanceChatRead(ctx context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	if handle == "" {
		return coord.ChatReadState{}, errors.New(
			"coord/kv: advance a person's chat state needs a handle")
	}
	for round := range casRounds {
		state, revision, err := f.chatReadAt(ctx, handle)
		if err != nil {
			return coord.ChatReadState{}, err
		}
		merged, err := coord.MergeChatRead(state, delta)
		if err != nil {
			return coord.ChatReadState{}, err
		}
		raw, err := json.Marshal(merged)
		if err != nil {
			return coord.ChatReadState{}, fmt.Errorf(
				"coord/kv: encode the chat read state for %s: %w", handle, err)
		}
		key := chatReadKey(handle)
		if revision == 0 {
			_, err = f.chatReads.Create(ctx, key, raw)
		} else {
			_, err = f.chatReads.Update(ctx, key, raw, revision)
		}
		switch {
		case err == nil:
			return merged, nil
		case errors.Is(err, jetstream.ErrKeyExists), isWrongRevision(err):
			// SOMEBODY ELSE'S TAB GOT THERE FIRST. Re-read and merge
			// again: the merge is monotonic, so this round's delta is
			// still exactly as true against their record as it was
			// against ours.
			_ = round
			continue
		default:
			return coord.ChatReadState{}, unavailable(
				fmt.Sprintf("advance the chat read state for %s", handle), err)
		}
	}
	return coord.ChatReadState{}, fmt.Errorf(
		"coord/kv: the chat read state for %s was rewritten under every one of "+
			"%d attempts; retry, or look for a client flushing far oftener than "+
			"the read cursor's own cadence", handle, casRounds)
}

// isWrongRevision reports the compare-and-set loss.
//
// MATCHED ON THE CLIENT'S OWN ERROR rather than on a string: an update at a
// stale revision is the one failure this loop retries, and every other error
// from the same call is a store that could not be reached — which must reach
// the caller rather than be spent as a retry.
func isWrongRevision(err error) bool {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence
	}
	return false
}

// ForgetChatRead removes a person's record, reporting whether one was there.
func (f *FleetStore) ForgetChatRead(ctx context.Context, handle string) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: forget a person's chat state needs a handle")
	}
	key := chatReadKey(handle)
	if _, err := f.chatReads.Get(ctx, key); errors.Is(err, jetstream.ErrKeyNotFound) {
		return false, nil
	} else if err != nil {
		return false, unavailable(
			fmt.Sprintf("read the chat read state for %s", handle), err)
	}
	if err := f.chatReads.Delete(ctx, key); err != nil {
		return false, unavailable(
			fmt.Sprintf("forget the chat read state for %s", handle), err)
	}
	return true, nil
}

// ChatReaders lists the handles holding read state.
//
// ONE FILTERED PASS, like every other listing here: the class filter is a
// subject wildcard the broker matches, so a bucket that one day holds a second
// class does not turn this into a walk that decodes two shapes as one.
func (f *FleetStore) ChatReaders(ctx context.Context) ([]string, error) {
	var handles []string
	err := f.eachUnder(ctx, f.chatReads, coord.DocumentFilter(chatReadClass),
		"the chat read state",
		func(entry jetstream.KeyValueEntry) error {
			segments, ok := coord.DocumentSegments(entry.Key())
			if !ok || len(segments) != 2 || segments[0] != chatReadClass {
				// A KEY THIS CLASS DID NOT WRITE. Skipped rather than
				// raised: the filter already narrowed to the class, so
				// anything left is a shape from a build that is not this
				// one, and refusing the whole listing over it would make
				// one unreadable record hide everybody else's state.
				return nil
			}
			handles = append(handles, segments[1])
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.Sort(handles)
	return handles, nil
}
