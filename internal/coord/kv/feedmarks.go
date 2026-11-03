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
// plain put landing out of order would move it back. Retried as every fleet
// compare-and-swap loop is, at most [fleetCASAttempts] times: its contenders
// are the nodes consuming the group, which the fleet's head count bounds.
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
	settled, err := casRetry(ctx, &f.positionTrips, "raise a feed mark", func() (bool, error) {
		held, revision, found, readErr := f.feedMarkAt(ctx, key)
		switch {
		case readErr != nil:
			return false, readErr
		case found && !m.Supersedes(held):
			return true, nil
		case !found:
			_, createErr := f.create(ctx, f.positions, key, body)
			switch {
			case createErr == nil:
				raised = true
				return true, nil
			case errors.Is(createErr, jetstream.ErrKeyExists):
				return false, nil
			case errors.Is(createErr, errCreateUnanswered):
				return false, writeUnknown("raise a feed mark", createErr)
			default:
				return false, unavailable("raise a feed mark", createErr)
			}
		}
		_, updateErr := f.positions.Update(ctx, key, body, revision)
		switch {
		case updateErr == nil:
			raised = true
			return true, nil
		case lostUpdateRace(updateErr):
			return false, nil
		default:
			return false, writeUnknown("raise a feed mark", updateErr)
		}
	})
	if err == nil && !settled {
		err = contended("RaiseFeedMark", key)
	}
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
