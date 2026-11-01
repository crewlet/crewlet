package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// RaiseFeedMark writes a wake feed's mark unless the one held is at the same
// generation and at or above it ([coord.FeedMarkRegister]).
//
// IN THE POSITIONS BUCKET, under its own key class, for the reason the trim
// floors are there: a mark has no age, and one that expired would read as a
// group nobody ever consumed. A COMPARE-AND-SET on the revision it read from
// the leader, because every node consuming the group writes the mark, and a
// plain put landing out of order would move it back.
func (f *FleetStore) RaiseFeedMark(ctx context.Context, m coord.FeedMark) (bool, error) {
	if err := m.Validate(); err != nil {
		return false, err
	}
	if m.At.IsZero() {
		m.At = time.Now().UTC()
	}
	body, err := json.Marshal(m)
	if err != nil {
		return false, fmt.Errorf("coord: encode the feed mark for %s on %s: %w", m.Group, m.Stream, err)
	}
	key := coord.FeedMarkKey(m.Stream, m.Group)
	raised := false
	err = f.retryRaces(ctx, "raise a feed mark", key, func() (bool, error) {
		held, revision, found, err := f.feedMarkAt(ctx, key)
		switch {
		case err != nil:
			return false, err
		case found && !m.Supersedes(held):
			return false, nil
		case !found:
			_, err = f.create(ctx, f.positions, key, body)
			switch {
			case err == nil:
				raised = true
				return false, nil
			case errors.Is(err, jetstream.ErrKeyExists):
				return true, nil
			case errors.Is(err, errCreateUnanswered):
				return false, writeUnknown("raise a feed mark", err)
			default:
				return false, unavailable("raise a feed mark", err)
			}
		}
		_, err = f.positions.Update(ctx, key, body, revision)
		switch {
		case err == nil:
			raised = true
			return false, nil
		case lostUpdateRace(err):
			return true, nil
		default:
			return false, writeUnknown("raise a feed mark", err)
		}
	})
	return raised, err
}

// FeedMark reads one log's mark for one group, from the leader: a replica
// behind the latest raise would answer an older floor, and a group created
// from it replays what the newer one had already passed.
func (f *FleetStore) FeedMark(ctx context.Context, stream, group string) (coord.FeedMark, bool, error) {
	held, _, found, err := f.feedMarkAt(ctx, coord.FeedMarkKey(stream, group))
	return held, found, err
}

// feedMarkAt is the mark at key and its revision, read from the leader.
//
// A MARK THAT DOES NOT DECODE IS RAISED, not read as absent: absent is the
// answer that starts a group at its log's first record, which is the replay
// the mark exists to prevent.
func (f *FleetStore) feedMarkAt(ctx context.Context, key string) (coord.FeedMark, uint64, bool, error) {
	entry, err := f.get(ctx, f.positions, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.FeedMark{}, 0, false, nil
	case err != nil:
		return coord.FeedMark{}, 0, false, unavailable("read a feed mark", err)
	}
	var held coord.FeedMark
	if err := json.Unmarshal(entry.Value(), &held); err != nil {
		return coord.FeedMark{}, 0, false, fmt.Errorf("coord: the feed mark at %s does not decode, "+
			"and read as absent it would start its group at the log's first record: %w", key, err)
	}
	return held, entry.Revision(), true, nil
}
