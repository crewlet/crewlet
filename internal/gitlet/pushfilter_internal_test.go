package gitlet

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"
)

// THE NARROWING KEEPS WHAT pusherOf COUNTS: for every way, principal and seat, a pusher's narrowing keeps a row exactly
// where pusherOf counts it for that pusher, and its bound ways are its ways, so a walk may count by code_pusher_days.
// Mutation: have pusherOf count a person's engine push as Gitlet's (the derived lists and the rows disagree).
func TestTheNarrowingKeepsWhatPusherOfCounts(t *testing.T) {
	t.Parallel()
	principal := map[PusherKind]PrincipalKind{PusherAgent: PrincipalAgent, PusherPerson: PrincipalOperator}
	for _, v := range PushVias() {
		for _, k := range PrincipalKinds() {
			for _, seat := range []string{"", "dev"} {
				for _, pk := range PusherKinds() {
					for _, handle := range []string{"", "dev"} {
						kept := slices.Contains(pusherVias(pk), v)
						if pk == PusherGitlet {
							kept = kept && handle == ""
						} else {
							kept = kept && k == principal[pk] && seat == handle && handle != ""
						}
						gotKind, gotHandle, counted := pusherOf(v, k, seat)
						if counts := counted && gotKind == pk && gotHandle == handle; counts != kept {
							t.Errorf("a %s push by %s %q: the narrowing to %s %q keeps it %t, pusherOf counts it %t",
								v, k, seat, pk, handle, kept, counts)
						}
					}
				}
			}
		}
	}
	for _, pk := range PusherKinds() {
		var bound []PushVia
		for _, v := range viaSlots(pk) {
			if via := PushVia(v.(string)); !slices.Contains(bound, via) {
				bound = append(bound, via)
			}
		}
		if !slices.Equal(bound, pusherVias(pk)) {
			t.Errorf("a narrowing to %s binds the ways %v, want its ways %v", pk, bound, pusherVias(pk))
		}
	}
	if got := fmt.Sprint(pusherVias(PusherAgent), pusherVias(PusherPerson), pusherVias(PusherGitlet)); got !=
		"[git engine] [git engine] [merge queue import mirror]" {
		t.Errorf("the pushers' ways are %s", got)
	}
}

// A NARROWED WINDOW REACHES NO PUSH PAST ITS BOUND: it ends at the push whose weight fills the page, at the rows left,
// at its size, or above the highest push stamped before its stop; and it is ended at the bottom.
// Mutation: weigh the walked pushes as nothing, drop the window's own size, read the last push one further, or ignore the stop.
func TestANarrowedWindowReachesNoPushPastItsBound(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-1", "app"))
	e.apply(create(t, "repo-2", "wide"))
	// FIVE PUSHES at positions 1 to 5, stamped 1 to 5, weighing 100 to 500 bytes.
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < 5)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands, received_at,
			row_bytes)
		SELECT printf('p%d', i), 'repo-1', i, i, 'ada', 'operator', '[]', i, i * 100 FROM k`)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands, received_at,
			row_bytes)
		SELECT printf('w%05d', i), 'repo-2', i, i, 'ada', 'operator', '[]', i, 1 FROM k`, narrowWindowRows+5)
	all := page{maxRows: 10_000, maxB: MaxPageBytes}
	window := func(repo string, from pushPlace, stop int64, reads page) narrowWindow {
		t.Helper()
		var w narrowWindow
		if err := e.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
			var err error
			w, err = narrowWindowOf(t.Context(), tx, repo, from, stop, reads, narrowWindowRows)
			return err
		}); err != nil {
			t.Fatalf("narrowWindowOf: %v", err)
		}
		return w
	}
	for name, tc := range map[string]struct {
		from  pushPlace
		stop  int64
		reads page
		want  narrowWindow
	}{
		"its bytes": {topPlace, 0, page{maxRows: 10, maxB: 850},
			narrowWindow{pushes: 2, weight: 900, last: pushPlace{4, "p4", 4}}},
		"its rows": {topPlace, 0, page{maxRows: 2, maxB: MaxPageBytes},
			narrowWindow{pushes: 2, weight: 900, last: pushPlace{4, "p4", 4}}},
		"every push, to the bottom": {topPlace, 0, all,
			narrowWindow{pushes: 5, weight: 1500, last: pushPlace{1, "p1", 1}, ended: true}},
		"its bytes, below a place and what the page holds": {pushPlace{4, "p4", 4}, 0,
			page{rows: 1, bytes: 450, maxRows: 10, maxB: 550},
			narrowWindow{pushes: 1, weight: 300, last: pushPlace{3, "p3", 3}}},
		"its stop": {topPlace, 3, all, narrowWindow{pushes: 5, weight: 1500, last: pushPlace{position: 3},
			ended: true}},
		"nothing below the last push": {pushPlace{1, "p1", 1}, 0, all, narrowWindow{ended: true}},
	} {
		if got := window("repo-1", tc.from, tc.stop, tc.reads); got != tc.want {
			t.Errorf("%s: the window is %+v, want %+v", name, got, tc.want)
		}
	}
	first := window("repo-2", topPlace, 0, all)
	if first.pushes != narrowWindowRows || first.ended {
		t.Fatalf("the wide repository's first window is %+v, want %d pushes", first, narrowWindowRows)
	}
	if rest := window("repo-2", first.last, 0, all); rest.pushes != 5 || !rest.ended {
		t.Errorf("the wide repository's second window is %+v, want the five left, ended", rest)
	}
}

// A JUMP PAST EVERY WINDOW IS SEARCHED: 3 000 pushes a minute apart, jumps that skip more than the short probes reach
// land below the lowest push stamped at or after the bar, and one stamped ahead within the bound is landed below too,
// skipping only pushes stamped at or after the jump's instant.
// Mutation: answer any other place from the search, or search by the jump's instant rather than its bar (it lands lower).
func TestAJumpPastEveryWindowIsSearched(t *testing.T) {
	t.Parallel()
	const pushes = 3_000
	e := openApplyEstate(t)
	minute := int64(time.Minute / time.Microsecond)
	for _, repo := range []string{"even", "ahead"} {
		e.apply(create(t, repo, repo))
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ?2)
			INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands, received_at,
				row_bytes)
			SELECT printf('%s%05d', ?1, i), ?1, i, i * ?3, 'ada', 'operator', '[]', i, 1 FROM k`, repo, pushes, minute)
	}
	// AHEAD: push 1000, the search's first probe, stamped at the bar of a jump to 980 minutes, 99 minutes past
	// push 1001's instant: within PushInstantSkew.
	e.exec(`UPDATE code_pushes SET at = ? WHERE repo_id = 'ahead' AND position = 1000`, 1100*minute)
	at := func(m int64) int64 { return m*minute - skewMicros }
	for name, tc := range map[string]struct {
		repo  string
		start pushPlace
		t     int64
		want  pushPlace
	}{
		"from the top": {"even", topPlace, at(1000), pushPlace{position: 1000, at: 1000 * minute}},
		"from a push": {"even", pushPlace{2500, "even02500", 2500 * minute}, at(1000),
			pushPlace{position: 1000, at: 1000 * minute}},
		"below every push": {"even", topPlace, at(1), pushPlace{position: 1, at: minute}},
		"a short jump":     {"even", topPlace, at(2990), pushPlace{position: 2990, at: 2990 * minute}},
		"nothing to skip": {"even", pushPlace{500, "even00500", 500 * minute}, at(2000),
			pushPlace{500, "even00500", 500 * minute}},
		"a push stamped ahead": {"ahead", topPlace, at(1100), pushPlace{position: 1000, at: 1100 * minute}},
	} {
		var got pushPlace
		var below int
		if err := e.db.Reader().Read(t.Context(), func(tx *sql.Tx) (err error) {
			if got, err = jumpBelow(t.Context(), tx, tc.repo, tc.start, tc.t); err != nil {
				return err
			}
			// NOTHING STAMPED BEFORE THE JUMP'S INSTANT is skipped.
			return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM code_pushes WHERE repo_id = ? AND
				position >= ? AND position < ? AND at < ?`, tc.repo, got.position, tc.start.position,
				tc.t).Scan(&below)
		}); err != nil {
			t.Fatalf("%s: jumpBelow: %v", name, err)
		}
		if got != tc.want || below != 0 {
			t.Errorf("%s: the jump landed at %+v, skipping %d pushes stamped before its instant; want %+v", name,
				got, below, tc.want)
		}
	}
}
