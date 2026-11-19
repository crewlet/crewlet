package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/store"
)

// A REPOSITORY'S PUSHES BY DAY AND BY PUSHER: the counts the applies keep (apply_activity.go),
// folded at the asker's own day bounds, so the engine takes no zone.

// THE ACTIVITY'S BOUNDS.
const (
	// PushSlot is the step a day's pushes are counted in: every UTC offset in use is a multiple of a
	// quarter-hour (+5:45 and +12:45 among them), so every zone's midnight starts one.
	PushSlot = 15 * time.Minute
	// MaxDayBounds is the most bounds a read names: a month of the asker's days and today.
	MaxDayBounds = 32
	// MaxDaySpan is the most a read's bounds span: 31 days and a clock change fit.
	MaxDaySpan = 32 * 24 * time.Hour
	// MaxListingDaySpan is the most a listing of repositories' bounds span: its rows draw a 14-day
	// spark, and a repository's fold then reads at most 16 day rows.
	MaxListingDaySpan = 15 * 24 * time.Hour
	// MaxSparkReposPerPage is the most repositories a listing that counts pushes by day reads a page:
	// a page of MaxReposPerPage folds 8 000 day rows, past a tenth of statelog.ReadBudget.
	MaxSparkReposPerPage = 100
	// PushDaysKept is how old a read's first bound may be: PushHistory, less the two hours a node's
	// clock may stand from the broker instant the history's horizon deletes by.
	PushDaysKept = PushHistory - 2*objstore.AssumedClockSkew
	// MaxPushersAnswered is the most pushers a detail names, the rest counted: about 10 KB at most.
	MaxPushersAnswered = 100
)

// PusherKind is whom a push counts as among a repository's pushers: a seat or a person, as a change's
// rows name them ([PartyKind]), or Gitlet for the pushes it makes itself.
type PusherKind string

const (
	// PusherAgent is a seat, by its handle.
	PusherAgent = PusherKind(PartyAgent)
	// PusherPerson is a person, by the human seat they hold, whichever credential they pushed with.
	PusherPerson = PusherKind(PartyPerson)
	// PusherGitlet is a merge's, a queue landing's, an import's or a pull mirror's push, never the
	// merger or enqueuer its row names.
	PusherGitlet PusherKind = "gitlet"
)

// PusherKinds is the closed set, in this order.
func PusherKinds() []PusherKind { return []PusherKind{PusherAgent, PusherPerson, PusherGitlet} }

// Valid reports a kind this build knows.
func (k PusherKind) Valid() bool { return slices.Contains(PusherKinds(), k) }

// Pushers is who pushed to a repository on whole UTC days, from From up to Until: each UTC day that
// begins within the asker's bounds. Top is at most MaxPushersAnswered, most pushes first.
type Pushers struct {
	From  time.Time      `json:"from"`
	Until time.Time      `json:"until"`
	Top   []PusherPushes `json:"top,omitempty"`
	// Omitted is the pushers past Top, and OmittedPushes their pushes together.
	Omitted       int   `json:"omitted,omitempty"`
	OmittedPushes int64 `json:"omitted_pushes,omitempty"`
}

// PusherPushes is one pusher's pushes over [Pushers]' days; Handle is empty for Gitlet.
type PusherPushes struct {
	Kind   PusherKind `json:"kind"`
	Handle string     `json:"handle,omitempty"`
	Pushes int64      `json:"pushes"`
}

// pusherOf is whom a push row of via, by a principal of kind holding seat, counts as; false for the ref
// lifecycle's expiry, which is housekeeping, and for a principal no row names.
func pusherOf(via PushVia, kind PrincipalKind, seat string) (PusherKind, string, bool) {
	switch via {
	case ViaExpiry:
		return "", "", false
	case ViaMerge, ViaQueue, ViaImport, ViaMirror:
		return PusherGitlet, "", true
	}
	handle, party := Principal{Kind: kind, Seat: seat}.party()
	return PusherKind(party), handle, party != ""
}

// validateDayBounds is [ReadQuery.validate] for the asker's day bounds.
func (q ReadQuery) validateDayBounds() error {
	b := q.DayBounds
	refused := func(why string) error { return refusedf("%w: day_bounds: %s", ErrInvalid, why) }
	switch {
	case (q.Kind == ReadRepos || q.Kind == ReadRepo) && !q.RepoFacts:
		return refused("a repository's pushes by day are among its facts: ask repo_facts with them")
	case q.Kind == ReadPushes && q.Push != "":
		return refused("one push is read whole, with no days")
	case len(b) < 2 || len(b) > MaxDayBounds:
		return refused(fmt.Sprintf("%d instants: a day is two of them, and a read names at most %d", len(b),
			MaxDayBounds))
	}
	for i, t := range b {
		switch {
		case !t.After(time.Unix(0, 0)):
			return refused(instantText(t) + " is not after 1970")
		case !t.Equal(t.Truncate(PushSlot)):
			return refused(instantText(t) + " is not on a quarter-hour: every zone's midnight is one")
		case i > 0 && !t.After(b[i-1]):
			return refused(fmt.Sprintf("%s is not after the bound before it, %s", instantText(t),
				instantText(b[i-1])))
		}
	}
	first, last := b[0], b[len(b)-1]
	switch span := last.Sub(first); {
	case span > MaxDaySpan:
		return refused(fmt.Sprintf("from %s to %s is past the %d days one read counts", instantText(first),
			instantText(last), MaxDaySpan/(24*time.Hour)))
	case q.Kind == ReadRepos && span > MaxListingDaySpan:
		return refused(fmt.Sprintf("from %s to %s is past the %d days a listing of repositories counts",
			instantText(first), instantText(last), MaxListingDaySpan/(24*time.Hour)))
	case q.Kind == ReadRepos && q.Limit > MaxSparkReposPerPage:
		return refusedf("%w: limit: a listing that counts pushes by day reads at most %d repositories a page",
			ErrInvalid, MaxSparkReposPerPage)
	}
	return nil
}

// daysKept refuses day bounds whose first is older than PushDaysKept by the reader's clock, so a day
// the history may no longer hold whole is never answered as a count.
func (q ReadQuery) daysKept(now time.Time) error {
	if len(q.DayBounds) == 0 || !q.DayBounds[0].Before(now.Add(-PushDaysKept)) {
		return nil
	}
	return refusedf("%w: day_bounds: %s is older than the push history keeps whole: %d days, less %d hours "+
		"for the clocks it is kept by", ErrInvalid, instantText(q.DayBounds[0]), PushHistory/(24*time.Hour),
		(PushHistory-PushDaysKept)/time.Hour)
}

// instantText is t as a refusal names it.
func instantText(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// pushesOnDays is each of ids' pushes on each of the asker's days, oldest first, zeros included:
// its day rows within the bounds folded at them, read in one statement.
func pushesOnDays(ctx context.Context, tx *sql.Tx, ids []string, bounds []time.Time) (map[string][]int64,
	error) {
	if len(ids) == 0 {
		return nil, nil
	}
	at := make([]int64, len(bounds))
	for i, b := range bounds {
		at[i] = store.EncodeTime(b)
	}
	out := make(map[string][]int64, len(ids))
	for _, id := range ids {
		out[id] = make([]int64, len(bounds)-1)
	}
	list, err := jsonList(ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, pushDaysOfSQL, list, dayOf(at[0]), at[len(at)-1])
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the pushes by day: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		id, text string
		day      int64
		quarters [pushQuarters]int64
	)
	for rows.Next() {
		if err = rows.Scan(&id, &day, &text); err != nil {
			return nil, fmt.Errorf("gitlet: read the pushes by day: %w", err)
		}
		if err = parseQuarters(text, &quarters); err != nil {
			return nil, err
		}
		counts := out[id]
		for k, n := range quarters {
			if n == 0 {
				continue
			}
			if i := dayIndex(at, day+int64(k)*quarterMicros); i >= 0 {
				counts[i] += n
			}
		}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the pushes by day: %w", err)
	}
	return out, nil
}

// dayIndex is the asker's day holding the instant at (encoded bounds, ascending), or -1 outside them.
func dayIndex(bounds []int64, at int64) int {
	i := sort.Search(len(bounds), func(i int) bool { return bounds[i] > at }) - 1
	if i < 0 || i >= len(bounds)-1 {
		return -1
	}
	return i
}

// pushersIn is who pushed to repoID on the UTC days that begin within bounds, most first, at most limit.
func pushersIn(ctx context.Context, tx *sql.Tx, repoID string, bounds []time.Time, limit int) (*Pushers,
	error) {
	first, last := store.EncodeTime(bounds[0]), store.EncodeTime(bounds[len(bounds)-1])
	from, until := dayOf(first+dayMicros-1), dayOf(last+dayMicros-1)
	out := &Pushers{From: store.DecodeTime(from), Until: store.DecodeTime(until)}
	if from == until {
		return out, nil
	}
	rows, err := tx.QueryContext(ctx, pusherDaysOfSQL, repoID, from, until)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read %s's pushers: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	type who struct {
		kind   PusherKind
		handle string
	}
	sums := map[who]int64{}
	for rows.Next() {
		var (
			w    who
			kind string
			n    int64
		)
		if err = rows.Scan(&kind, &w.handle, &n); err != nil {
			return nil, fmt.Errorf("gitlet: read %s's pushers: %w", repoID, err)
		}
		if w.kind = PusherKind(kind); !w.kind.Valid() {
			return nil, fmt.Errorf("gitlet: %s counts pushes by a pusher of kind %q, which this build does not "+
				"know", repoID, kind)
		}
		sums[w] += n
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read %s's pushers: %w", repoID, err)
	}
	top := make([]PusherPushes, 0, len(sums))
	for w, n := range sums {
		top = append(top, PusherPushes{Kind: w.kind, Handle: w.handle, Pushes: n})
	}
	slices.SortFunc(top, func(a, b PusherPushes) int {
		return cmp.Or(cmp.Compare(b.Pushes, a.Pushes),
			cmp.Compare(slices.Index(PusherKinds(), a.Kind), slices.Index(PusherKinds(), b.Kind)),
			cmp.Compare(a.Handle, b.Handle))
	})
	if len(top) > limit {
		for _, p := range top[limit:] {
			out.Omitted++
			out.OmittedPushes += p.Pushes
		}
		top = top[:limit]
	}
	if len(top) > 0 {
		out.Top = top
	}
	return out, nil
}

// The reads' statements, registered in plan_test.go.
const (
	// pushDaysOfSQL is the day rows from ?2 up to ?3 of the repositories a JSON list names, one
	// range of the key a repository.
	pushDaysOfSQL = `SELECT d.repo_id, d.day, ` + pushQuartersText + ` FROM json_each(?1) AS j
		CROSS JOIN code_push_days AS d WHERE d.repo_id = CAST(j.value AS TEXT) AND d.day >= ?2 AND d.day < ?3`
	// pusherDaysOfSQL is one repository's pushers on the UTC days from ?2 up to ?3, one range of the key.
	pusherDaysOfSQL = `SELECT pusher_kind, pusher, pushes FROM code_pusher_days
		WHERE repo_id = ? AND day >= ? AND day < ?`
)
