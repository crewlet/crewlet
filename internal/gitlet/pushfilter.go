package gitlet

// A PUSH HISTORY NARROWED to one pusher, as code_pusher_days counts them, and a window of instants:
// code_pushes_repo_idx scanned newest first, each page held to what it reads, the pusher's days steering it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

const (
	// PushInstantSkew is the most a push's instant is assumed to stand before that of a push an earlier
	// record landed: two broker members' clocks, each assumed within objstore.AssumedClockSkew of the
	// fleet's. Past it, a narrowed page can leave out the push stamped further out of order.
	PushInstantSkew = 2 * objstore.AssumedClockSkew
	// MaxPushesScanned is the most pushes a page of a narrowed push history reads, and MaxPageBytes of
	// their rows: no index carries a pusher, so a page looks up each push it steps over and answers what it
	// kept with a Next. 5 000 holds a page that jumps at every push it keeps under a tenth of the read budget.
	MaxPushesScanned = 5_000
)

// narrowWindowRows is the most pushes one step of a narrowed walk weighs before it reads their rows, and
// narrowFirstWindowRows the fewest: a walk's windows double from the first to the most, starting over at
// each jump, so one that stops at a pusher's push soon after a jump has weighed few past it.
const (
	narrowWindowRows      = 1_000
	narrowFirstWindowRows = 64
)

// PushFilter narrows a push history ([ReadQuery.PushFilter]). A page keeps at most [ReadQuery.Limit] pushes,
// reads at most [MaxPushesScanned] pushes and MaxPageBytes of their rows, and answers a cursor wherever it
// stopped short.
type PushFilter struct {
	// PusherKind and Pusher keep one pusher's pushes as its activity counts them ([pusherOf]): a seat or a
	// person by a handle, Gitlet by none. Both empty keep every pusher's.
	PusherKind PusherKind `json:"pusher_kind,omitempty"`
	Pusher     string     `json:"pusher,omitempty"`
	// Since and Until keep the pushes whose record landed at or after Since and before Until; zero is no bound.
	Since time.Time `json:"since,omitzero"`
	Until time.Time `json:"until,omitzero"`
}

// PushScan is how far a narrowed page read: To, on a page with a cursor, the instant of the push it ended at.
type PushScan struct {
	To time.Time `json:"to,omitzero"`
}

// validatePushFilter is [ReadQuery.validate] for a push history's narrowing. Day bounds ride it only
// within its window and never beside a pusher: code_push_days counts every pusher's pushes.
func (q ReadQuery) validatePushFilter() error {
	f := q.PushFilter
	refused := func(field, why string) error { return refusedf("%w: %s: %s", ErrInvalid, field, why) }
	switch {
	case q.Push != "":
		return refused("push_filter", "one push is read whole, never narrowed")
	case f.PusherKind == "" && f.Pusher == "" && f.Since.IsZero() && f.Until.IsZero():
		return refused("push_filter", "a narrowing names a pusher, a window or both")
	case f.PusherKind == "" && f.Pusher != "":
		return refused("push_filter.pusher", "a pusher is named beside its kind: "+words.Or(PusherKinds()))
	case f.PusherKind != "" && !f.PusherKind.Valid():
		return refused("push_filter.pusher_kind", fmt.Sprintf("%q is not one of %s", f.PusherKind,
			words.Or(PusherKinds())))
	case f.PusherKind == PusherGitlet && f.Pusher != "":
		return refused("push_filter.pusher", "Gitlet's pushes are named by no handle")
	case (f.PusherKind == PusherAgent || f.PusherKind == PusherPerson) && !org.ValidHandle(f.Pusher):
		return refused("push_filter.pusher", fmt.Sprintf("%q is not a seat's handle", f.Pusher))
	}
	for _, bound := range []struct {
		field string
		t     time.Time
	}{{"push_filter.since", f.Since}, {"push_filter.until", f.Until}} {
		switch t := bound.t; {
		case t.IsZero():
		case !t.After(time.Unix(0, 0)):
			return refused(bound.field, instantText(t)+" is not after 1970")
		case !t.Equal(t.Truncate(time.Microsecond)):
			return refused(bound.field, instantText(t)+" is finer than a microsecond, the finest a push's "+
				"instant is kept at")
		}
	}
	if !f.Since.IsZero() && !f.Until.IsZero() && !f.Until.After(f.Since) {
		return refused("push_filter.until", fmt.Sprintf("%s is not after since %s: the window holds no instant",
			instantText(f.Until), instantText(f.Since)))
	}
	if len(q.DayBounds) == 0 {
		return nil
	}
	first, last := q.DayBounds[0], q.DayBounds[len(q.DayBounds)-1]
	switch {
	case f.PusherKind != "":
		return refused("day_bounds", "the days count every pusher's pushes, so a history narrowed to one "+
			"pusher counts none")
	case !f.Since.IsZero() && first.Before(f.Since):
		return refused("day_bounds", fmt.Sprintf("%s is before since %s: a day's count would hold pushes "+
			"the window leaves out", instantText(first), instantText(f.Since)))
	case !f.Until.IsZero() && last.After(f.Until):
		return refused("day_bounds", fmt.Sprintf("%s is after until %s: a day's count would hold pushes "+
			"the window leaves out", instantText(last), instantText(f.Until)))
	}
	return nil
}

// pusherVias is the ways of the pushes kind's pushers are counted for, as pusherOf reads each way for a
// principal of the kind's own: Gitlet's merges, landings, imports and syncs, or a seat's or a person's own.
func pusherVias(kind PusherKind) []PushVia {
	principal := map[PusherKind]PrincipalKind{PusherAgent: PrincipalAgent, PusherPerson: PrincipalOperator,
		PusherGitlet: PrincipalSystem}[kind]
	var out []PushVia
	for _, v := range PushVias() {
		if k, _, counted := pusherOf(v, principal, "probe"); counted && k == kind {
			out = append(out, v)
		}
	}
	return out
}

// viaSlotCount is how many ways a narrowed statement's IN list holds: every way this build knows fits.
const viaSlotCount = 7

// viaSlots is kind's ways as a narrowed statement binds them, the last repeated to fill every slot: a list
// of parameters rather than a JSON list, which the statement would read again for every row it tests.
func viaSlots(kind PusherKind) []any {
	vias := pusherVias(kind)
	out := make([]any, viaSlotCount)
	for i := range out {
		out[i] = string(vias[min(i, len(vias)-1)])
	}
	return out
}

// The narrowing's statements, over code_pushes_repo_idx and the pusher days' key. The ranged ones take a
// place (exclusive) as ?2, ?3 and a window's lowest push (inclusive) as ?4, ?5, the >= beside each OR
// keeping the range seek.
const (
	// pusherGroupsSQL is one pusher's days of one repository from ?4 up to ?5, oldest first, each one's
	// pushes: driven from the repository's own days, one seek of the pusher days' key a day.
	pusherGroupsSQL = `SELECT d.day, d.pushes FROM code_push_days AS a CROSS JOIN code_pusher_days AS d
		WHERE a.repo_id = ?1 AND a.day >= ?4 AND a.day < ?5 AND d.repo_id = ?1 AND d.day = a.day
		AND d.pusher_kind = ?2 AND d.pusher = ?3 ORDER BY a.day`
	// jumpNearSQL is, of the next ?4 pushes below position ?2, the newest stamped before ?3, the newest and the
	// lowest, and how many: a jump that ends among them is one statement, from the index alone.
	jumpNearSQL = `SELECT MAX(CASE WHEN at < ?3 THEN position END), MAX(position), MIN(position), COUNT(*)
		FROM (SELECT position, at FROM code_pushes WHERE repo_id = ?1 AND position < ?2
			ORDER BY position DESC, push_id DESC LIMIT ?4)`
	// pushAtOrBelowSQL is the newest push at or below a position and its instant, from the index alone.
	pushAtOrBelowSQL = `SELECT position, at FROM code_pushes WHERE repo_id = ?1 AND position <= ?2
		ORDER BY position DESC LIMIT 1`
	// THE WINDOW A NARROWED STEP READS, newest first below a place, from the index alone: the next ?4
	// pushes' count, weight and highest position stamped before ?5; their weights; the place of the ?4th.
	narrowWeighSQL = `SELECT COUNT(*), COALESCE(SUM(row_bytes), 0), MAX(CASE WHEN at < ?5 THEN position END)
		FROM (SELECT position, at, row_bytes FROM code_pushes WHERE repo_id = ?1 AND position <= ?2
			AND (position < ?2 OR push_id < ?3) ORDER BY position DESC, push_id DESC LIMIT ?4)`
	narrowWeightsSQL = `SELECT row_bytes FROM code_pushes WHERE repo_id = ?1 AND position <= ?2
		AND (position < ?2 OR push_id < ?3) ORDER BY position DESC, push_id DESC LIMIT ?4`
	narrowLastSQL = `SELECT position, push_id, at FROM code_pushes WHERE repo_id = ?1 AND position <= ?2
		AND (position < ?2 OR push_id < ?3) ORDER BY position DESC, push_id DESC LIMIT 1 OFFSET ?4`
	// narrowSpanSQL is the count and weight of the pushes below one place and at or above another.
	narrowSpanSQL = `SELECT COUNT(*), COALESCE(SUM(row_bytes), 0) FROM code_pushes WHERE repo_id = ?1
		AND position <= ?2 AND (position < ?2 OR push_id < ?3) AND position >= ?4 AND (position > ?4 OR push_id >= ?5)`
	// THE ROWS OF A WINDOW, each looked up to test what the index does not carry: stamped in ?6 to ?7, and
	// one pusher's by its kind, its handle and the ways its pushes count for it, or Gitlet's.
	narrowedRange = ` FROM code_pushes WHERE repo_id = ?1 AND position <= ?2 AND (position < ?2 OR push_id < ?3)
		AND position >= ?4 AND (position > ?4 OR push_id >= ?5) AND at >= ?6 AND at < ?7`
	narrowedWindowSQL = `SELECT ` + pushColumns + narrowedRange + ` ORDER BY position DESC, push_id DESC`
	narrowedPartySQL  = `SELECT ` + pushColumns + narrowedRange + ` AND actor_kind = ?8 AND actor_seat = ?9
		AND via IN (?10, ?11, ?12, ?13, ?14, ?15, ?16) ORDER BY position DESC, push_id DESC`
	narrowedGitletSQL = `SELECT ` + pushColumns + narrowedRange + `
		AND via IN (?8, ?9, ?10, ?11, ?12, ?13, ?14) ORDER BY position DESC, push_id DESC`
)

// pushPlace is where a narrowed walk stands: it has read every push above it in log order and reads on
// below it. A push's place is its own; a jump's is (p, "", at), below every push at position p, whose
// record is stamped at. No push id is empty, so (p, "") reads as "below position p" in every statement.
type pushPlace struct {
	position int64
	pushID   string
	at       int64
}

// topPlace is above every push.
var topPlace = pushPlace{position: math.MaxInt64, at: math.MaxInt64}

// skewMicros is PushInstantSkew in store.EncodeTime's microseconds.
const skewMicros = int64(PushInstantSkew / time.Microsecond)

// narrowing is one page of a narrowed push history as its walk fills it.
type narrowing struct {
	repo   RepoRef
	filter *PushFilter
	brief  bool
	// lo and hi bound the instants kept, math.MinInt64 and math.MaxInt64 where unset; stop is lo less the
	// margin, below which no push is kept.
	lo, hi, stop int64
	// kept is the page the rows it keeps fill, and reads the one every push it steps over fills.
	kept, reads page
	// size is how many pushes the next window weighs at most.
	size  int
	out   *ReadAnswer
	named mergedNames
	// charges, places and pending are each kept row's charge, its place, and the pending its cursor carries
	// there, so a page the recut cuts resumes as one the walk ended there.
	charges []int
	places  []pushPlace
	pending []int64
}

// pushesNarrowed serves one page of a push history narrowed by q.PushFilter, newest first in log order,
// scanning code_pushes_repo_idx below its place: a pusher's steered by its days' counts ([narrowing.pusherWalk]),
// a window's alone by its instants ([narrowing.windowWalk]); every page holds PushScan.
func (r *Reader) pushesNarrowed(ctx context.Context, tx *sql.Tx, q ReadQuery, repo RepoRef, out *ReadAnswer) error {
	f := q.PushFilter
	out.PushScan = &PushScan{}
	n := &narrowing{repo: repo, filter: f, brief: q.Brief, lo: math.MinInt64,
		hi: math.MaxInt64, stop: math.MinInt64, out: out, size: narrowFirstWindowRows,
		kept:  page{maxRows: limitOr(q.Limit, defaultPushLimit), maxB: r.pageBytes},
		reads: page{maxRows: r.pushesScanned, maxB: r.pageBytes}}
	if !f.Since.IsZero() {
		n.lo = store.EncodeTime(f.Since)
		n.stop = n.lo - skewMicros
	}
	if !f.Until.IsZero() {
		n.hi = store.EncodeTime(f.Until)
	}
	var (
		end pushPlace
		err error
	)
	if f.PusherKind == "" {
		end, err = n.windowWalk(ctx, tx, q.Cursor)
	} else {
		end, err = n.pusherWalk(ctx, tx, q.Cursor)
	}
	if err != nil {
		return fmt.Errorf("gitlet: read %s's narrowed pushes: %w", repo.Name, err)
	}
	grew, err := n.named.name(ctx, tx, repo, out.Pushes)
	if err != nil {
		return err
	}
	if kept := recut(n.charges, grew, n.kept.maxRows, n.kept.maxB); kept < len(out.Pushes) {
		out.Pushes = out.Pushes[:kept]
		end = n.places[kept-1]
		out.Next = n.cursor(end, n.pending[kept-1])
	} else if out.Next == "" {
		return nil
	}
	out.PushScan.To = store.DecodeTime(end.at)
	return nil
}

// cursor is the place a page ended at as the next page resumes it: a pusher's with what it had still to
// meet, or the history's own.
func (n *narrowing) cursor(at pushPlace, left int64) string {
	if n.filter.PusherKind == "" {
		return logCursor(at.position, at.pushID)
	}
	return pusherCursor(at, n.filter, left)
}

// windowWalk is a narrowing to a window alone, from the cursor's place or the top: windows of the pushes
// below it read until the stop, the bottom, the kept page or the reads fill. It answers where the page
// ended, with out.Next set there, or with none where the walk is over.
func (n *narrowing) windowWalk(ctx context.Context, tx *sql.Tx, cursor string) (pushPlace, error) {
	place := topPlace
	if cursor != "" {
		position, pushID, ok := cutHistoryCursor(cursor)
		if !ok {
			return pushPlace{}, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
		place = pushPlace{position: position, pushID: pushID}
	} else if n.hi != math.MaxInt64 {
		var err error
		if place, err = jumpBelow(ctx, tx, n.repo.ID, place, n.hi); err != nil {
			return pushPlace{}, err
		}
	}
	for {
		if n.reads.full() {
			n.out.Next = n.cursor(place, 0)
			return place, nil
		}
		w, err := narrowWindowOf(ctx, tx, n.repo.ID, place, n.stop, n.reads, n.size)
		if err != nil || w.pushes == 0 {
			return pushPlace{}, err
		}
		var full bool
		stopped, at, err := n.windowRows(ctx, tx, place, w, func(pushPlace) bool {
			n.pending = append(n.pending, 0)
			full = n.kept.full()
			return full
		})
		if err != nil {
			return pushPlace{}, err
		}
		if stopped {
			n.out.Next = n.cursor(at, 0)
			return at, nil
		}
		if place = w.last; w.ended {
			return pushPlace{}, nil
		}
		n.size = min(2*n.size, narrowWindowRows)
	}
}

// cutHistoryCursor reads the cursor of a history unnarrowed or narrowed to a window alone: false where its
// push id holds a '/', which only a pusher's narrowing writes ([pusherCursor]).
func cutHistoryCursor(s string) (position int64, pushID string, ok bool) {
	position, pushID, ok = cutLogCursor(s)
	return position, pushID, ok && !strings.Contains(pushID, "/")
}

// pusherDays is a pusher's counted days, oldest first, each one's pushes and the running sum through it.
type pusherDays struct {
	days, sums []int64
}

// pusherDaysOf is the pusher's days of repository repoID from the day since falls on, up to (and not
// including) the day until falls on, read from code_pusher_days.
func pusherDaysOf(ctx context.Context, tx *sql.Tx, repoID string, f *PushFilter, lo, hi int64) (pusherDays,
	error) {
	from, to := int64(0), int64(math.MaxInt64)
	if lo != math.MinInt64 {
		from = dayOf(lo)
	}
	if hi != math.MaxInt64 {
		to = dayOf(hi)
	}
	rows, err := tx.QueryContext(ctx, pusherGroupsSQL, repoID, string(f.PusherKind), f.Pusher, from, to)
	if err != nil {
		return pusherDays{}, err
	}
	defer func() { _ = rows.Close() }()
	var (
		d   pusherDays
		sum int64
	)
	for rows.Next() {
		var day, pushes int64
		if err = rows.Scan(&day, &pushes); err != nil {
			return pusherDays{}, err
		}
		sum += pushes
		d.days, d.sums = append(d.days, day), append(d.sums, sum)
	}
	return d, rows.Err()
}

// counted reports a day the pusher's days hold.
func (d pusherDays) counted(day int64) bool {
	i := sort.Search(len(d.days), func(i int) bool { return d.days[i] >= day })
	return i < len(d.days) && d.days[i] == day
}

// clear is the pushes of the days CLEAR of p, every one of which stands below p under the clock bound (each
// ended the margin or more before p's instant), and the newest of those days: none and 0 where none is.
func (d pusherDays) clear(p pushPlace) (pushes, newest int64) {
	i := sort.Search(len(d.days), func(i int) bool { return d.days[i] > p.at-dayMicros-skewMicros })
	if i == 0 {
		return 0, 0
	}
	return d.sums[i-1], d.days[i-1]
}

// pusherWalk is a narrowing to one pusher (and a window), from the cursor's place or the top: it counts down
// the pusher's pushes on the counted days as it meets them, jumps over what holds none still to meet, and is
// over once it has met them all and read the partial day through. It answers where the page ended, or none.
func (n *narrowing) pusherWalk(ctx context.Context, tx *sql.Tx, cursor string) (pushPlace, error) {
	days, err := pusherDaysOf(ctx, tx, n.repo.ID, n.filter, n.lo, n.hi)
	if err != nil {
		return pushPlace{}, err
	}
	place, pending := topPlace, int64(0)
	if cursor != "" {
		var ok bool
		if place, pending, ok = cutPusherCursor(cursor, n.filter); !ok {
			return pushPlace{}, fmt.Errorf("%w: %q", ErrBadCursor, cursor)
		}
	}
	// left is the pusher's pushes on counted days below the place that the walk has still to meet.
	cleared, _ := days.clear(place)
	left := pending + cleared
	// THE PARTIAL DAY is until's own where until is off a UTC midnight: its pushes before until are counted
	// by no day the walk holds, so the walk reads it through before any day jump.
	partial := n.hi != math.MaxInt64 && n.hi != dayOf(n.hi)
	open := func(p pushPlace) bool { return partial && p.at >= dayOf(n.hi)-skewMicros }
	over := func(p pushPlace) bool { return left <= 0 && !open(p) }
	// pendingAt is what a cursor at p carries: the pushes still to meet on days not clear of p.
	pendingAt := func(p pushPlace) int64 {
		c, _ := days.clear(p)
		return max(0, left-c)
	}
	jumpable := func(p pushPlace) bool { return pendingAt(p) <= 0 && !open(p) }
	ended := func(p pushPlace) string { return pusherCursor(p, n.filter, pendingAt(p)) }
	if cursor == "" && n.hi != math.MaxInt64 {
		if place, err = jumpBelow(ctx, tx, n.repo.ID, place, n.hi); err != nil {
			return pushPlace{}, err
		}
	}
	for {
		switch {
		case over(place):
			return pushPlace{}, nil
		case n.reads.full():
			n.out.Next = ended(place)
			return place, nil
		case jumpable(place):
			_, newest := days.clear(place)
			if place, err = jumpBelow(ctx, tx, n.repo.ID, place, newest+dayMicros); err != nil {
				return pushPlace{}, err
			}
			n.size = narrowFirstWindowRows
		}
		var (
			w                     narrowWindow
			stopped, isOver, full bool
			at                    pushPlace
		)
		if w, err = narrowWindowOf(ctx, tx, n.repo.ID, place, n.stop, n.reads, n.size); err != nil || w.pushes == 0 {
			return pushPlace{}, err
		}
		stopped, at, err = n.windowRows(ctx, tx, place, w, func(x pushPlace) bool {
			if days.counted(dayOf(x.at)) {
				left--
			}
			n.pending = append(n.pending, pendingAt(x))
			isOver, full = over(x), n.kept.full()
			return isOver || full || jumpable(x)
		})
		switch {
		case err != nil:
			return pushPlace{}, err
		case stopped && isOver:
			return pushPlace{}, nil
		case stopped && full:
			n.out.Next = ended(at)
			return at, nil
		case stopped:
			place = at
		case w.ended:
			return pushPlace{}, nil
		default:
			place, n.size = w.last, min(2*n.size, narrowWindowRows)
		}
	}
}

// windowRows reads the narrowing's rows in w below place, newest first, keeping each and handing its place
// to stop, which answers whether the walk stops at it. Where it stops, the reads take the pushes from below
// place down to it and it answers that row's place; else they take the window whole.
func (n *narrowing) windowRows(ctx context.Context, tx *sql.Tx, place pushPlace, w narrowWindow,
	stop func(pushPlace) bool) (bool, pushPlace, error) {

	query, args := narrowedWindowSQL, []any{n.repo.ID, place.position, place.pushID, w.last.position,
		w.last.pushID, n.lo, n.hi}
	switch f := n.filter; f.PusherKind {
	case "":
	case PusherGitlet:
		query, args = narrowedGitletSQL, append(args, viaSlots(f.PusherKind)...)
	default:
		kind := PrincipalAgent
		if f.PusherKind == PusherPerson {
			kind = PrincipalOperator
		}
		query, args = narrowedPartySQL, append(append(args, string(kind), f.Pusher), viaSlots(f.PusherKind)...)
	}
	at, stopped, err := n.keepRows(ctx, tx, query, args, stop)
	if err != nil || !stopped {
		if err == nil {
			n.reads.rows, n.reads.bytes = n.reads.rows+w.pushes, n.reads.bytes+w.weight
		}
		return false, pushPlace{}, err
	}
	var pushes, weight int
	if err = tx.QueryRowContext(ctx, narrowSpanSQL, n.repo.ID, place.position, place.pushID, at.position,
		at.pushID).Scan(&pushes, &weight); err != nil {
		return false, pushPlace{}, err
	}
	n.reads.rows, n.reads.bytes = n.reads.rows+pushes, n.reads.bytes+weight
	return true, at, nil
}

// keepRows keeps each row of one window's statement, newest first, until stop answers true at one.
func (n *narrowing) keepRows(ctx context.Context, tx *sql.Tx, query string, args []any,
	stop func(pushPlace) bool) (at pushPlace, stopped bool, err error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return pushPlace{}, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			row      PushRow
			commands int
		)
		if row, commands, err = scanPush(rows); err != nil {
			return pushPlace{}, false, err
		}
		n.named.add(len(n.out.Pushes), row)
		charge := row.encodedBytes(commands)
		if n.brief {
			row = row.brief()
			charge = max(charge, row.encodedBytes(row.commandsBytes()))
		}
		at = pushPlace{position: row.Position.Packed(), pushID: row.PushID, at: store.EncodeTime(row.At)}
		n.out.Pushes = append(n.out.Pushes, row)
		n.charges, n.places = append(n.charges, charge), append(n.places, at)
		n.kept.add(charge)
		if stop(at) {
			return at, true, nil
		}
	}
	return pushPlace{}, false, rows.Err()
}

// jumpBelow is where a walk below start begins once it skips the pushes stamped at or after t: below the
// lowest push it finds stamped at or after t+PushInstantSkew, or start where the newest below it is stamped
// before that. The place's instant is a floor of its record's, which is all a walk asks of it.
func jumpBelow(ctx context.Context, tx *sql.Tx, repoID string, start pushPlace, t int64) (pushPlace, error) {
	bar := t + skewMicros
	// A SHORT JUMP, the common one, lands above the newest of the next pushes stamped before bar, looked
	// for among the first window's worth and then the most a window weighs; only one past them all searches.
	var lowest sql.NullInt64
	for _, ahead := range []int{narrowFirstWindowRows, narrowWindowRows} {
		var (
			before, newest sql.NullInt64
			count          int
		)
		if err := tx.QueryRowContext(ctx, jumpNearSQL, repoID, start.position, bar, ahead).Scan(&before, &newest,
			&lowest, &count); err != nil {
			return pushPlace{}, err
		}
		switch {
		case count == 0, before.Valid && before.Int64 == newest.Int64:
			return start, nil
		case before.Valid:
			return pushPlace{position: before.Int64 + 1, at: bar}, nil
		case count < ahead:
			return pushPlace{position: lowest.Int64, at: bar}, nil
		}
	}
	probe := func(below int64) (position, at int64, found bool, err error) {
		err = tx.QueryRowContext(ctx, pushAtOrBelowSQL, repoID, below).Scan(&position, &at)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, nil
		}
		return position, at, err == nil, err
	}
	hi, hiAt := lowest.Int64, bar
	var (
		found bool
		err   error
	)
	// EVERY PUSH ABOVE ONE STAMPED AT OR AFTER bar is a later record's, so stamped at or after t; the
	// search finds a low one, and one misled by stepped instants lands higher, never lower.
	for lo := int64(-1); hi-lo > 1; {
		mid := lo + (hi-lo)/2
		var position, at int64
		position, at, found, err = probe(mid)
		switch {
		case err != nil:
			return pushPlace{}, err
		case !found || at < bar:
			lo = mid
		default:
			hi, hiAt = position, at
		}
	}
	return pushPlace{position: hi, at: hiAt}, nil
}

// narrowWindow is the stretch of pushes one step of a narrowed walk reads below its place: how many, what
// their rows weigh, its lowest push, and whether no push below it is kept (the walk's stop, or the bottom).
type narrowWindow struct {
	pushes, weight int
	last           pushPlace
	ended          bool
}

// narrowWindowOf is the window below from that reads may still take, from code_pushes_repo_idx alone: at
// most size pushes, none past the one whose weight fills reads' bytes, ending above the highest push
// stamped before stop.
func narrowWindowOf(ctx context.Context, tx *sql.Tx, repoID string, from pushPlace, stop int64,
	reads page, size int) (narrowWindow, error) {

	limit := min(reads.left(), size)
	var (
		w       narrowWindow
		count   int
		stopped sql.NullInt64
	)
	if err := tx.QueryRowContext(ctx, narrowWeighSQL, repoID, from.position, from.pushID, limit,
		stop).Scan(&count, &w.weight, &stopped); err != nil {
		return narrowWindow{}, err
	}
	if count == 0 {
		return narrowWindow{ended: true}, nil
	}
	w.pushes = count
	if reads.bytes+w.weight >= reads.maxB {
		var err error
		if w.pushes, w.weight, err = narrowWeights(ctx, tx, repoID, from, limit, reads); err != nil {
			return narrowWindow{}, err
		}
	}
	if err := tx.QueryRowContext(ctx, narrowLastSQL, repoID, from.position, from.pushID,
		w.pushes-1).Scan(&w.last.position, &w.last.pushID, &w.last.at); err != nil {
		return narrowWindow{}, err
	}
	w.ended = count < limit && w.pushes == count
	// EVERY PUSH AT stopped IS STAMPED BEFORE stop, a record's pushes sharing its instant, so every push
	// below it is stamped before stop plus the margin, which is the window's since.
	if stopped.Valid && stopped.Int64 >= w.last.position {
		w.last, w.ended = pushPlace{position: stopped.Int64 + 1}, true
	}
	return w, nil
}

// narrowWeights walks the weights of the pushes below from, newest first, counting each into reads (the
// page's copy), to the one that fills it or limit of them.
func narrowWeights(ctx context.Context, tx *sql.Tx, repoID string, from pushPlace, limit int,
	reads page) (pushes, weight int, err error) {

	rows, err := tx.QueryContext(ctx, narrowWeightsSQL, repoID, from.position, from.pushID, limit)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var w int
		if err = rows.Scan(&w); err != nil {
			return 0, 0, err
		}
		pushes, weight = pushes+1, weight+w
		if reads.add(w) {
			break
		}
	}
	return pushes, weight, rows.Err()
}

// pusherCursor is a pusher's narrowed page's place, "<packed position>/<push id>/<instant>/<kind>/<pusher>/
// <since>/<until>/<pending>": the push it ended at and its instant, the narrowing it was written for, and the
// pusher's pushes below it, on days not wholly below it, that the walk has still to meet.
func pusherCursor(at pushPlace, f *PushFilter, pending int64) string {
	return strings.Join([]string{strconv.FormatInt(at.position, 10), at.pushID, strconv.FormatInt(at.at, 10),
		string(f.PusherKind), f.Pusher, cursorInstant(f.Since), cursorInstant(f.Until),
		strconv.FormatInt(pending, 10)}, "/")
}

// cursorInstant is a narrowing's bound as its cursor spells it: microseconds, or nothing where unset.
func cursorInstant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(store.EncodeTime(t), 10)
}

// cutPusherCursor reads one written for f: false where it is of another shape or another narrowing.
func cutPusherCursor(s string, f *PushFilter) (at pushPlace, pending int64, ok bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 8 || parts[1] == "" || parts[3] != string(f.PusherKind) || parts[4] != f.Pusher ||
		parts[5] != cursorInstant(f.Since) || parts[6] != cursorInstant(f.Until) {
		return pushPlace{}, 0, false
	}
	position, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return pushPlace{}, 0, false
	}
	stamped, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || position < 0 || stamped < 0 {
		return pushPlace{}, 0, false
	}
	if pending, err = strconv.ParseInt(parts[7], 10, 64); err != nil || pending < 0 {
		return pushPlace{}, 0, false
	}
	return pushPlace{position: position, pushID: parts[1], at: stamped}, pending, true
}
