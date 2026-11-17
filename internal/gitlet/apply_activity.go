package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE PUSHES COUNTED AS THEY LAND AND GO: code_push_days by UTC day and quarter-hour and
// code_pusher_days by pusher and UTC day, moved by every insert and delete of a code_pushes row.

// pushDayRows is the day row a record's pushes take, and pusherDayRows the row one entry's pusher
// takes: counted once an entry, as seatDayRows is, so a decide is never short.
const (
	pushDayRows   = 1
	pusherDayRows = 1
)

// pushQuarters is the quarter-hours of a UTC day: code_push_days' q00 to q95.
const pushQuarters = int(24 * time.Hour / PushSlot)

// dayMicros and quarterMicros are a UTC day and a quarter-hour in store.EncodeTime's microseconds.
const (
	dayMicros     = int64(24 * time.Hour / time.Microsecond)
	quarterMicros = int64(PushSlot / time.Microsecond)
)

// dayOf is the UTC midnight an encoded instant after 1970 falls on, and quarterOf its quarter-hour.
func dayOf(at int64) int64   { return at - at%dayMicros }
func quarterOf(at int64) int { return int(at % dayMicros / quarterMicros) }

// parseQuarters reads one row's pushQuartersText into quarters.
func parseQuarters(text string, quarters *[pushQuarters]int64) error {
	for k := range quarters {
		field, rest, found := strings.Cut(text, ",")
		if found == (k == pushQuarters-1) {
			return fmt.Errorf("gitlet: a day's quarter-hours read %q, not %d counts", text, pushQuarters)
		}
		n, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			return fmt.Errorf("gitlet: a day's quarter-hours read %q: %w", field, err)
		}
		quarters[k], text = n, rest
	}
	return nil
}

// pushDay is one code_push_days row's counts: the day's pushes and each quarter-hour's.
type pushDay struct {
	pushes   int64
	quarters [pushQuarters]int64
}

// pusherDay is one key of code_pusher_days.
type pusherDay struct {
	day    int64
	kind   PusherKind
	handle string
}

// compare orders keys as the table's primary key does.
func (k pusherDay) compare(o pusherDay) int {
	return cmp.Or(cmp.Compare(k.day, o.day), cmp.Compare(k.kind, o.kind), cmp.Compare(k.handle, o.handle))
}

// pushActivity is push rows counted as the two tables count them, by key.
type pushActivity struct {
	days    map[int64]*pushDay
	pushers map[pusherDay]int64
}

// add counts n more push rows landed at at, of via, by a principal of kind holding seat: in its day
// and quarter-hour, and where it is somebody's push ([pusherOf]) in its pusher's day.
func (a *pushActivity) add(at int64, via PushVia, kind PrincipalKind, seat string, n int64) {
	day := dayOf(at)
	if a.days == nil {
		a.days = map[int64]*pushDay{}
	}
	d := a.days[day]
	if d == nil {
		d = &pushDay{}
		a.days[day] = d
	}
	d.pushes += n
	d.quarters[quarterOf(at)] += n
	if pusher, handle, ok := pusherOf(via, kind, seat); ok {
		if a.pushers == nil {
			a.pushers = map[pusherDay]int64{}
		}
		a.pushers[pusherDay{day: day, kind: pusher, handle: handle}] += n
	}
}

// minus is a less b key by key, a key whose every count is equal left out; negated is the empty
// activity less a.
func (a pushActivity) minus(b pushActivity) pushActivity {
	out := pushActivity{days: map[int64]*pushDay{}, pushers: map[pusherDay]int64{}}
	for day := range keysOf(a.days, b.days) {
		var d pushDay
		if x := a.days[day]; x != nil {
			d = *x
		}
		if y := b.days[day]; y != nil {
			d.pushes -= y.pushes
			for k, n := range y.quarters {
				d.quarters[k] -= n
			}
		}
		if d != (pushDay{}) {
			out.days[day] = &d
		}
	}
	for key := range keysOf(a.pushers, b.pushers) {
		if n := a.pushers[key] - b.pushers[key]; n != 0 {
			out.pushers[key] = n
		}
	}
	return out
}

func (a pushActivity) negated() pushActivity { return pushActivity{}.minus(a) }

// keysOf is the keys of a and of b together.
func keysOf[K comparable, V any](a, b map[K]V) map[K]bool {
	keys := make(map[K]bool, len(a)+len(b))
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	return keys
}

// countPushActivity counts the push rows a record inserts into repoID's activity and the rows its
// horizon deleted (gone) out of it, in one move; it answers the rows it wrote.
func countPushActivity(ctx context.Context, tx *sql.Tx, at applyContext, repoID string, pushes []PushEntry,
	gone pushActivity) (int, error) {
	var added pushActivity
	for _, e := range pushes {
		added.add(at.at(), e.Via, e.Principal.Kind, e.Principal.Seat, 1)
	}
	return moveActivity(ctx, tx, at.maxVariables, repoID, added.minus(gone))
}

// deletePushes deletes the push rows ids name and answers how many, and what they counted in the
// activity, read off the rows the delete took ([deletePushesSQL]'s RETURNING).
func deletePushes(ctx context.Context, tx *sql.Tx, ids []string) (deleted int, gone pushActivity, err error) {
	list, err := jsonList(ids)
	if err != nil {
		return 0, pushActivity{}, err
	}
	rows, err := tx.QueryContext(ctx, deletePushesSQL, list)
	if err != nil {
		return 0, pushActivity{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			at              int64
			via, kind, seat string
		)
		if err = rows.Scan(&at, &via, &kind, &seat); err != nil {
			return 0, pushActivity{}, err
		}
		gone.add(at, PushVia(via), PrincipalKind(kind), seat, 1)
		deleted++
	}
	if err = rows.Err(); err != nil {
		return 0, pushActivity{}, err
	}
	return deleted, gone, nil
}

// moveActivity adds d to repoID's rows, each table's keys in key order, then prunes the keys it
// moved down; it answers the keys it wrote.
func moveActivity(ctx context.Context, tx *sql.Tx, maxVariables int, repoID string, d pushActivity) (int, error) {
	days := slices.Sorted(maps.Keys(d.days))
	pushers := slices.SortedFunc(maps.Keys(d.pushers), pusherDay.compare)
	if _, err := store.InsertRows(ctx, tx, maxVariables, pushDaysUpsertSQL, pushDaysRowSQL, pushDaysUpsertTailSQL,
		len(days), func(i int) []any {
			row := d.days[days[i]]
			args := make([]any, 0, 3+pushQuarters)
			args = append(args, repoID, days[i], row.pushes)
			for _, n := range row.quarters {
				args = append(args, n)
			}
			return args
		}); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's pushes by day: %w", repoID, err)
	}
	if _, err := store.InsertRows(ctx, tx, maxVariables, pusherDaysUpsertSQL, `(?, ?, ?, ?, ?)`,
		pusherDaysUpsertTailSQL, len(pushers), func(i int) []any {
			k := pushers[i]
			return []any{repoID, k.day, string(k.kind), k.handle, d.pushers[k]}
		}); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's pushes by pusher: %w", repoID, err)
	}
	var down pushActivity
	for _, day := range days {
		if d.days[day].pushes <= 0 {
			if down.days == nil {
				down.days = map[int64]*pushDay{}
			}
			down.days[day] = d.days[day]
		}
	}
	for _, k := range pushers {
		if d.pushers[k] < 0 {
			if down.pushers == nil {
				down.pushers = map[pusherDay]int64{}
			}
			down.pushers[k] = d.pushers[k]
		}
	}
	if _, err := pruneActivity(ctx, tx, repoID, down); err != nil {
		return 0, err
	}
	return len(days) + len(pushers), nil
}

// pruneActivity deletes those of repoID's keys in a that hold no push; it answers how many.
func pruneActivity(ctx context.Context, tx *sql.Tx, repoID string, a pushActivity) (int, error) {
	pruned := 0
	if len(a.days) > 0 {
		list, err := json.Marshal(slices.Sorted(maps.Keys(a.days)))
		if err != nil {
			return 0, fmt.Errorf("gitlet: encode %s's days to prune: %w", repoID, err)
		}
		res, err := tx.ExecContext(ctx, prunePushDaysSQL, repoID, string(list))
		if err != nil {
			return 0, fmt.Errorf("gitlet: prune %s's days of no push: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		pruned += int(n)
	}
	if len(a.pushers) > 0 {
		keys := slices.SortedFunc(maps.Keys(a.pushers), pusherDay.compare)
		named := make([][]any, len(keys))
		for i, k := range keys {
			named[i] = []any{k.day, string(k.kind), k.handle}
		}
		list, err := json.Marshal(named)
		if err != nil {
			return 0, fmt.Errorf("gitlet: encode %s's pusher days to prune: %w", repoID, err)
		}
		res, err := tx.ExecContext(ctx, prunePusherDaysSQL, repoID, string(list))
		if err != nil {
			return 0, fmt.Errorf("gitlet: prune %s's pusher days of no push: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		pruned += int(n)
	}
	return pruned, nil
}

// purgeLeftActivity deletes up to PurgePartRows of repoID's activity rows, its days first: what a
// purge part takes once its pushes are gone, which the decide counts (purge.go).
func purgeLeftActivity(ctx context.Context, tx *sql.Tx, repoID string) (int, error) {
	taken := 0
	for _, stmt := range []string{purgeLeftPushDaysSQL, purgeLeftPusherDaysSQL} {
		if taken >= PurgePartRows {
			break
		}
		res, err := tx.ExecContext(ctx, stmt, repoID, PurgePartRows-taken)
		if err != nil {
			return 0, fmt.Errorf("gitlet: purge %s's push counts: %w", repoID, err)
		}
		n, _ := res.RowsAffected()
		taken += int(n)
	}
	return taken, nil
}

// rederivePushActivity recounts the activity of every repository code_pushes or either table names
// from its push rows and writes what differs; it answers the rows it wrote.
func rederivePushActivity(ctx context.Context, tx *sql.Tx, maxVariables int) (int, error) {
	repos, err := activityRepos(ctx, tx)
	if err != nil {
		return 0, err
	}
	written := 0
	for _, id := range repos {
		n, ofErr := rederivePushActivityOf(ctx, tx, maxVariables, id)
		if ofErr != nil {
			return 0, ofErr
		}
		written += n
	}
	return written, nil
}

// activityRepos is every repository code_pushes, code_push_days or code_pusher_days names, in order:
// one seek of each table's key a repository, all read before anything is written.
func activityRepos(ctx context.Context, tx *sql.Tx) ([]string, error) {
	named := map[string]bool{}
	for _, next := range []string{nextPushedRepoSQL, nextPushDaysRepoSQL, nextPusherDaysRepoSQL} {
		for after := ""; ; {
			var id string
			walkErr := tx.QueryRowContext(ctx, next, after).Scan(&id)
			if errors.Is(walkErr, sql.ErrNoRows) {
				break
			}
			if walkErr != nil {
				return nil, fmt.Errorf("gitlet: read the repositories whose pushes are counted: %w", walkErr)
			}
			named[id], after = true, id
		}
	}
	return slices.Sorted(maps.Keys(named)), nil
}

// rederivePushActivityOf is one repository's: its push rows counted and its rows held, every read
// closed before the move and the prune of the held keys its rows do not want.
func rederivePushActivityOf(ctx context.Context, tx *sql.Tx, maxVariables int, repoID string) (int, error) {
	want, err := pushRowsCounted(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	held, err := activityHeld(ctx, tx, repoID)
	if err != nil {
		return 0, err
	}
	moved, err := moveActivity(ctx, tx, maxVariables, repoID, want.minus(held))
	if err != nil {
		return 0, err
	}
	var unwanted pushActivity
	for day, d := range held.days {
		if want.days[day] == nil {
			if unwanted.days == nil {
				unwanted.days = map[int64]*pushDay{}
			}
			unwanted.days[day] = d
		}
	}
	for k, n := range held.pushers {
		if _, ok := want.pushers[k]; !ok {
			if unwanted.pushers == nil {
				unwanted.pushers = map[pusherDay]int64{}
			}
			unwanted.pushers[k] = n
		}
	}
	pruned, err := pruneActivity(ctx, tx, repoID, unwanted)
	if err != nil {
		return 0, err
	}
	return moved + pruned, nil
}

// pushRowsCounted is repoID's push rows counted as the activity counts them.
func pushRowsCounted(ctx context.Context, tx *sql.Tx, repoID string) (pushActivity, error) {
	var a pushActivity
	rows, err := tx.QueryContext(ctx, pushRowsCountedSQL, repoID)
	if err != nil {
		return a, fmt.Errorf("gitlet: read %s's push rows to count: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			at              int64
			via, kind, seat string
		)
		if err = rows.Scan(&at, &via, &kind, &seat); err != nil {
			return a, fmt.Errorf("gitlet: read %s's push rows to count: %w", repoID, err)
		}
		a.add(at, PushVia(via), PrincipalKind(kind), seat, 1)
	}
	if err = rows.Err(); err != nil {
		return a, fmt.Errorf("gitlet: read %s's push rows to count: %w", repoID, err)
	}
	return a, nil
}

// activityHeld is repoID's activity rows as they stand.
func activityHeld(ctx context.Context, tx *sql.Tx, repoID string) (pushActivity, error) {
	a := pushActivity{days: map[int64]*pushDay{}, pushers: map[pusherDay]int64{}}
	rows, err := tx.QueryContext(ctx, heldPushDaysSQL, repoID)
	if err != nil {
		return a, fmt.Errorf("gitlet: read %s's pushes by day: %w", repoID, err)
	}
	if err = eachRow(rows, func(row scanner) error {
		var (
			day      int64
			d        pushDay
			quarters string
		)
		if scanErr := row.Scan(&day, &d.pushes, &quarters); scanErr != nil {
			return scanErr
		}
		if parseErr := parseQuarters(quarters, &d.quarters); parseErr != nil {
			return parseErr
		}
		a.days[day] = &d
		return nil
	}); err != nil {
		return a, fmt.Errorf("gitlet: read %s's pushes by day: %w", repoID, err)
	}
	if rows, err = tx.QueryContext(ctx, heldPusherDaysSQL, repoID); err != nil {
		return a, fmt.Errorf("gitlet: read %s's pushes by pusher: %w", repoID, err)
	}
	if err = eachRow(rows, func(row scanner) error {
		var (
			k    pusherDay
			kind string
			n    int64
		)
		if scanErr := row.Scan(&k.day, &kind, &k.handle, &n); scanErr != nil {
			return scanErr
		}
		k.kind = PusherKind(kind)
		a.pushers[k] = n
		return nil
	}); err != nil {
		return a, fmt.Errorf("gitlet: read %s's pushes by pusher: %w", repoID, err)
	}
	return a, nil
}

// The activity's statements, each with a WHERE registered in plan_test.go but the upserts, which
// read no row.
const (
	// pushQuarterColumns are code_push_days' quarter-hours in order, q00 from midnight.
	pushQuarterColumns = `q00, q01, q02, q03, q04, q05, q06, q07, q08, q09, q10, q11,
		q12, q13, q14, q15, q16, q17, q18, q19, q20, q21, q22, q23,
		q24, q25, q26, q27, q28, q29, q30, q31, q32, q33, q34, q35,
		q36, q37, q38, q39, q40, q41, q42, q43, q44, q45, q46, q47,
		q48, q49, q50, q51, q52, q53, q54, q55, q56, q57, q58, q59,
		q60, q61, q62, q63, q64, q65, q66, q67, q68, q69, q70, q71,
		q72, q73, q74, q75, q76, q77, q78, q79, q80, q81, q82, q83,
		q84, q85, q86, q87, q88, q89, q90, q91, q92, q93, q94, q95`
	// pushQuartersText is code_push_days' quarter-hours as one text, comma-separated in order: the
	// driver fetches each column through a call of its own, and a listing's fold reads thousands of rows.
	pushQuartersText = `concat_ws(',', q00, q01, q02, q03, q04, q05, q06, q07, q08, q09, q10, q11,
		q12, q13, q14, q15, q16, q17, q18, q19, q20, q21, q22, q23,
		q24, q25, q26, q27, q28, q29, q30, q31, q32, q33, q34, q35,
		q36, q37, q38, q39, q40, q41, q42, q43, q44, q45, q46, q47,
		q48, q49, q50, q51, q52, q53, q54, q55, q56, q57, q58, q59,
		q60, q61, q62, q63, q64, q65, q66, q67, q68, q69, q70, q71,
		q72, q73, q74, q75, q76, q77, q78, q79, q80, q81, q82, q83,
		q84, q85, q86, q87, q88, q89, q90, q91, q92, q93, q94, q95)`
	pushDaysUpsertSQL = `INSERT INTO code_push_days (repo_id, day, pushes, ` + pushQuarterColumns + `) VALUES `
	// pushDaysRowSQL is one row's 99 places: the key, the total and the 96 quarter-hours.
	pushDaysRowSQL = `(?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	pushDaysUpsertTailSQL = `ON CONFLICT (repo_id, day) DO UPDATE SET pushes = pushes + excluded.pushes,
		q00 = q00 + excluded.q00, q01 = q01 + excluded.q01, q02 = q02 + excluded.q02, q03 = q03 + excluded.q03,
		q04 = q04 + excluded.q04, q05 = q05 + excluded.q05, q06 = q06 + excluded.q06, q07 = q07 + excluded.q07,
		q08 = q08 + excluded.q08, q09 = q09 + excluded.q09, q10 = q10 + excluded.q10, q11 = q11 + excluded.q11,
		q12 = q12 + excluded.q12, q13 = q13 + excluded.q13, q14 = q14 + excluded.q14, q15 = q15 + excluded.q15,
		q16 = q16 + excluded.q16, q17 = q17 + excluded.q17, q18 = q18 + excluded.q18, q19 = q19 + excluded.q19,
		q20 = q20 + excluded.q20, q21 = q21 + excluded.q21, q22 = q22 + excluded.q22, q23 = q23 + excluded.q23,
		q24 = q24 + excluded.q24, q25 = q25 + excluded.q25, q26 = q26 + excluded.q26, q27 = q27 + excluded.q27,
		q28 = q28 + excluded.q28, q29 = q29 + excluded.q29, q30 = q30 + excluded.q30, q31 = q31 + excluded.q31,
		q32 = q32 + excluded.q32, q33 = q33 + excluded.q33, q34 = q34 + excluded.q34, q35 = q35 + excluded.q35,
		q36 = q36 + excluded.q36, q37 = q37 + excluded.q37, q38 = q38 + excluded.q38, q39 = q39 + excluded.q39,
		q40 = q40 + excluded.q40, q41 = q41 + excluded.q41, q42 = q42 + excluded.q42, q43 = q43 + excluded.q43,
		q44 = q44 + excluded.q44, q45 = q45 + excluded.q45, q46 = q46 + excluded.q46, q47 = q47 + excluded.q47,
		q48 = q48 + excluded.q48, q49 = q49 + excluded.q49, q50 = q50 + excluded.q50, q51 = q51 + excluded.q51,
		q52 = q52 + excluded.q52, q53 = q53 + excluded.q53, q54 = q54 + excluded.q54, q55 = q55 + excluded.q55,
		q56 = q56 + excluded.q56, q57 = q57 + excluded.q57, q58 = q58 + excluded.q58, q59 = q59 + excluded.q59,
		q60 = q60 + excluded.q60, q61 = q61 + excluded.q61, q62 = q62 + excluded.q62, q63 = q63 + excluded.q63,
		q64 = q64 + excluded.q64, q65 = q65 + excluded.q65, q66 = q66 + excluded.q66, q67 = q67 + excluded.q67,
		q68 = q68 + excluded.q68, q69 = q69 + excluded.q69, q70 = q70 + excluded.q70, q71 = q71 + excluded.q71,
		q72 = q72 + excluded.q72, q73 = q73 + excluded.q73, q74 = q74 + excluded.q74, q75 = q75 + excluded.q75,
		q76 = q76 + excluded.q76, q77 = q77 + excluded.q77, q78 = q78 + excluded.q78, q79 = q79 + excluded.q79,
		q80 = q80 + excluded.q80, q81 = q81 + excluded.q81, q82 = q82 + excluded.q82, q83 = q83 + excluded.q83,
		q84 = q84 + excluded.q84, q85 = q85 + excluded.q85, q86 = q86 + excluded.q86, q87 = q87 + excluded.q87,
		q88 = q88 + excluded.q88, q89 = q89 + excluded.q89, q90 = q90 + excluded.q90, q91 = q91 + excluded.q91,
		q92 = q92 + excluded.q92, q93 = q93 + excluded.q93, q94 = q94 + excluded.q94, q95 = q95 + excluded.q95`
	pusherDaysUpsertSQL     = `INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes) VALUES `
	pusherDaysUpsertTailSQL = `ON CONFLICT (repo_id, day, pusher_kind, pusher) DO UPDATE SET pushes = pushes + excluded.pushes`
	// prunePushDaysSQL deletes the days a JSON list names that count no push, one seek each.
	prunePushDaysSQL = `DELETE FROM code_push_days WHERE rowid IN (SELECT d.rowid FROM json_each(?2) AS j
		CROSS JOIN code_push_days AS d WHERE d.repo_id = ?1 AND d.day = CAST(j.value AS INTEGER) AND d.pushes = 0)`
	// prunePusherDaysSQL deletes the pusher days a JSON list of [day, kind, handle] names that count none.
	prunePusherDaysSQL = `DELETE FROM code_pusher_days WHERE rowid IN (SELECT d.rowid FROM json_each(?2) AS j
		CROSS JOIN code_pusher_days AS d WHERE d.repo_id = ?1 AND d.day = CAST(j.value ->> 0 AS INTEGER)
		AND d.pusher_kind = CAST(j.value ->> 1 AS TEXT) AND d.pusher = CAST(j.value ->> 2 AS TEXT) AND d.pushes = 0)`
	// The re-derivation's: a repository's push rows as the activity counts them, its rows held,
	// and the repository after one in each table's key.
	pushRowsCountedSQL    = `SELECT at, via, actor_kind, actor_seat FROM code_pushes WHERE repo_id = ?`
	heldPushDaysSQL       = `SELECT day, pushes, ` + pushQuartersText + ` FROM code_push_days WHERE repo_id = ?`
	heldPusherDaysSQL     = `SELECT day, pusher_kind, pusher, pushes FROM code_pusher_days WHERE repo_id = ?`
	nextPushedRepoSQL     = `SELECT repo_id FROM code_pushes WHERE repo_id > ? ORDER BY repo_id LIMIT 1`
	nextPushDaysRepoSQL   = `SELECT repo_id FROM code_push_days WHERE repo_id > ? ORDER BY repo_id LIMIT 1`
	nextPusherDaysRepoSQL = `SELECT repo_id FROM code_pusher_days WHERE repo_id > ? ORDER BY repo_id LIMIT 1`
	// A purge's: what a part takes once its pushes are gone, and the final purge's, beside purgeAllPushesSQL.
	purgeLeftPushDaysSQL = `DELETE FROM code_push_days WHERE rowid IN (SELECT rowid FROM code_push_days
		WHERE repo_id = ? LIMIT ?)`
	purgeLeftPusherDaysSQL = `DELETE FROM code_pusher_days WHERE rowid IN (SELECT rowid FROM code_pusher_days
		WHERE repo_id = ? LIMIT ?)`
	purgeAllPushDaysSQL   = `DELETE FROM code_push_days WHERE repo_id = ?`
	purgeAllPusherDaysSQL = `DELETE FROM code_pusher_days WHERE repo_id = ?`
)
