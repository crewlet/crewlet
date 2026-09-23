package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Counting the log rather than folding a page of it.
//
// A caller asking "how many of these happened" and answering from a page of
// [EventLog.List] reports the PAGE: the count stops at the page size however
// many there were, and two such counts beside each other cover two different
// spans of time whenever the two kinds of event arrive at different rates, with
// nothing on either saying so. A GROUP BY over the listing's own predicate
// answers the question over the whole window asked for, in one statement, and
// cannot disagree with the rows a listing of the same filters returns.

// ErrTallyRelated is returned for a tally asked to filter by a related agent,
// refused for the reason [ErrHistogramRelated] gives: that filter adds every
// event sharing a trace with a direct match, so a count over the predicate
// alone is a smaller set than a listing of the same filters shows.
var ErrTallyRelated = errors.New("store: a tally cannot filter by related agent")

// ErrTallyTag is returned for a [TallyQuery.Tag] that is not a plain tag key.
var ErrTallyTag = errors.New("store: a tally tag must be lower-case letters, digits and underscores")

// TallyQuery asks how many events match, grouped.
//
// The filters are [ListQuery]'s, by embedding, so a filter added to the listing
// is one the tally already has. `Limit` and `Before` are ignored: a page size
// and a cursor are about where a page stops and resumes, and a count has no
// page. The WINDOW is `Since` and `Until`, under the same history floor every
// read of this log applies.
type TallyQuery struct {
	ListQuery

	// Tag names a tag whose value each count is grouped by as well, beside
	// the event type and the source. Empty groups by those two alone.
	//
	// A plain key only — lower-case letters, digits and underscores — because
	// it becomes a JSON path, where `.`, `[` and `"` are syntax rather than
	// part of a name.
	Tag string
}

// EventTally is one group's count.
type EventTally struct {
	Type   string
	Source string

	// Tag is the group's value of [TallyQuery.Tag]: empty when no tag was
	// asked for, and for the rows that carry none.
	Tag string

	Count int

	// Oldest and Newest are the group's first and last event times, so a
	// caller can say what span its count covers inside the window it asked
	// for.
	Oldest time.Time
	Newest time.Time
}

// Tally counts the events a listing of the same filters would return, grouped
// by event type, source and optionally one tag's value. Groups come back in
// that order, and an empty answer is an allocated empty slice, like every list
// read on [EventLog].
func (l *EventLog) Tally(ctx context.Context, q TallyQuery) ([]EventTally, error) {
	if q.RelatedAgent != "" {
		return nil, ErrTallyRelated
	}
	if q.Tag != "" && !plainTagKey(q.Tag) {
		return nil, fmt.Errorf("%w: got %q", ErrTallyTag, q.Tag)
	}
	from, where, args, col := q.predicate()

	// THE PATH IS BOUND, never spliced: the key was checked above, but a
	// bound parameter is a value the statement cannot be made to parse as
	// anything else whatever the check lets through.
	tag := "''"
	if q.Tag != "" {
		tag = "COALESCE(json_extract(" + col("tags") + ", ?), '')"
		args = append([]any{"$." + q.Tag}, args...)
	}
	// Every fragment joined into the statement is a compile-time constant;
	// each value travels as a bound parameter in args.
	query := "SELECT " + col("event_type") + ", " + col("source") + ", " + tag + " AS tag_value, " +
		"COUNT(*), MIN(" + col("event_time") + "), MAX(" + col("event_time") + ")" +
		" FROM " + from + " WHERE " + strings.Join(where, " AND ") +
		" GROUP BY " + col("event_type") + ", " + col("source") + ", tag_value" +
		" ORDER BY " + col("event_type") + ", " + col("source") + ", tag_value"

	rows, err := l.db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: tally events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := []EventTally{}
	for rows.Next() {
		var (
			t              EventTally
			oldest, newest int64
		)
		if err := rows.Scan(&t.Type, &t.Source, &t.Tag, &t.Count, &oldest, &newest); err != nil {
			return nil, fmt.Errorf("store: tally events: %w", err)
		}
		t.Oldest, t.Newest = DecodeTime(oldest), DecodeTime(newest)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tally events: %w", err)
	}
	return out, nil
}

// plainTagKey reports whether key is lower-case letters, digits and
// underscores, and not empty.
func plainTagKey(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}
