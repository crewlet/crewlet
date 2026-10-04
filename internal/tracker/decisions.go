package tracker

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The open asks put to one person, newest first — the tracker's half of
// `decisions`, which the API merges with the coding runs parked on a question
// for the same person.
//
// The same rows as [MyWork]'s `asked_of_me`, read through the same function
// ([readAsks]) so the two can never disagree about what is waiting, at a page
// of [MaxDecisions] rather than the twenty a turn-start block affords — and
// with the OLDEST ask's instant beside the count, because "how long has the
// longest one been waiting" is the question a landing screen asks and the
// page's own last row answers it only when the page holds them all.

// MaxDecisions bounds one page of decisions.
//
// FIFTY, the ceiling every listing page in this API shares: a person with more
// open questions than that has a triage problem a longer list will not solve,
// and the total beside the page says how many there are.
const MaxDecisions = 50

// DecisionsQuery asks what is waiting on one person.
type DecisionsQuery struct {
	// Handle is the person — their record name (iam.RecordOwner), which
	// for a person bound to a seat is that seat whatever credential they
	// hold, so an ask put to them under any credential is one row here.
	Handle string

	Level       statelog.ReadLevel
	Session     statelog.Position
	MinPosition statelog.Position
	MaxLag      time.Duration
	MaxLagSeq   uint64
}

// DecisionsAnswer is the page of open asks, the count of all of them and the
// oldest one's instant.
type DecisionsAnswer struct {
	Asks []AskRow `json:"asks"`

	// Total counts every open ask, capped at [TotalHintCeiling] and saying
	// so — see [ClaimTotal].
	Total ClaimTotal `json:"total"`

	// OldestAt is when the longest-waiting open ask was put, absent when
	// none is.
	OldestAt *time.Time `json:"oldest_at,omitempty"`

	Level          statelog.ReadLevel `json:"read_level,omitempty"`
	LogSeq         uint64             `json:"log_seq,omitempty"`
	AppliedThrough uint64             `json:"applied_through,omitempty"`
	LogLag         *uint64            `json:"log_lag,omitempty"`
	Complete       bool               `json:"complete"`
	Incomplete     *Incomplete        `json:"incomplete,omitempty"`
}

// Decisions answers the open asks put to one person.
func (r *Reader) Decisions(ctx context.Context, q DecisionsQuery, now time.Time,
	loc *time.Location) (DecisionsAnswer, error) {

	switch {
	case q.Level == "":
		return DecisionsAnswer{}, fmt.Errorf("tracker: this decisions read names " +
			"no level — a surface resolves an absent read_level to its own " +
			"default before it reads")
	case q.Handle == "":
		return DecisionsAnswer{}, invalid("a decisions read names nobody")
	}
	var out DecisionsAnswer
	served, err := r.log.Read(ctx, statelog.Query{
		Level: q.Level,
		// THE DOMAIN, for [Reader.MyWork]'s reason: an ask can be on any
		// task in any container.
		Scope:       statelog.ScopeSet{Paths: []string{pathDomain}}.Normalised(),
		Session:     q.Session,
		MinPosition: q.MinPosition,
		MaxLag:      q.MaxLag,
		MaxLagSeq:   q.MaxLagSeq,
		Set:         true,
	}, func(tx *sql.Tx) error {
		anchor, err := ResolveDate("today", now, loc)
		if err != nil {
			return err
		}
		if out.Asks, out.Total, err = readAsks(ctx, tx, q.Handle, anchor.At,
			MaxDecisions); err != nil {
			return err
		}
		if out.OldestAt, err = oldestAsk(ctx, tx, q.Handle); err != nil {
			return err
		}
		out.LogSeq, out.AppliedThrough, err = readCheckpoint(ctx, tx)
		return err
	})
	if err != nil {
		return DecisionsAnswer{}, err
	}
	out.Level = served.Level
	out.Complete = served.Complete
	out.LogLag = served.Lag
	if served.Incomplete != nil {
		out.Incomplete = incompleteFrom(served.Incomplete)
	}
	return out, nil
}

// oldestAsk is when the longest-waiting open ask was put to this person, over
// the same predicate [readAsks] pages.
func oldestAsk(ctx context.Context, tx *sql.Tx, handle string) (*time.Time, error) {
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT MIN(c.created_at) FROM `+openAsksFrom+`
		WHERE `+openAsksWhere, handle).Scan(&oldest); err != nil {
		return nil, fmt.Errorf("tracker: read the oldest ask waiting on %s: %w", handle, err)
	}
	if !oldest.Valid {
		return nil, nil
	}
	at := store.DecodeTime(oldest.Int64)
	return &at, nil
}
