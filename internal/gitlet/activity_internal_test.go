package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE ORACLE, spelled apart from the applies in SQL: each push row's UTC day and quarter-hour by
// integer arithmetic, and its pusher by a CASE of its own, every key with its count.
const (
	oracleDaysSQL = `SELECT repo_id, at - at % 86400000000, (at % 86400000000) / 900000000, COUNT(*)
		FROM code_pushes GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`
	oraclePushersSQL = `SELECT repo_id || '/' || day || '/' || kind || '/' || pusher || ':' || COUNT(*) FROM (
		SELECT repo_id, at - at % 86400000000 AS day,
			CASE WHEN via IN ('merge', 'queue', 'import', 'mirror') THEN 'gitlet'
				WHEN actor_kind = 'agent' THEN 'agent' ELSE 'person' END AS kind,
			CASE WHEN via IN ('merge', 'queue', 'import', 'mirror') THEN '' ELSE actor_seat END AS pusher
		FROM code_pushes WHERE via != 'expiry' AND (via IN ('merge', 'queue', 'import', 'mirror')
			OR (actor_kind IN ('agent', 'operator') AND actor_seat != '')))
		GROUP BY repo_id, day, kind, pusher ORDER BY repo_id, day, kind, pusher`
	heldDaysSQL    = `SELECT repo_id, day, pushes, ` + pushQuarterColumns + ` FROM code_push_days ORDER BY repo_id, day`
	heldPushersSQL = `SELECT repo_id || '/' || day || '/' || pusher_kind || '/' || pusher || ':' || pushes
		FROM code_pusher_days ORDER BY repo_id, day, pusher_kind, pusher`
)

// activityDrift names the first difference, "" for none, between the two tables and the oracle:
// each day row's quarter-hours, its total against their sum, and each pusher row, none at zero or below.
func activityDrift(ctx context.Context, tx *sql.Tx) (string, error) {
	held, err := heldDayLines(ctx, tx)
	if err != nil {
		return "", err
	}
	want, err := oracleDayLines(ctx, tx)
	if err != nil {
		return "", err
	}
	if drift := firstDifference(held, want); drift != "" {
		return "the push days " + drift, nil
	}
	if held, err = textRows(ctx, tx, heldPushersSQL); err != nil {
		return "", err
	}
	if want, err = textRows(ctx, tx, oraclePushersSQL); err != nil {
		return "", err
	}
	if drift := firstDifference(held, want); drift != "" {
		return "the pusher days " + drift, nil
	}
	return "", nil
}

// firstDifference names the first line held and want differ at, "" for none.
func firstDifference(held, want []string) string {
	for i := range max(len(held), len(want)) {
		var h, w string
		if i < len(held) {
			h = held[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if h != w {
			return fmt.Sprintf("hold %q where the push rows count %q", h, w)
		}
	}
	return ""
}

// heldDayLines is each day row as `repo/day:pushes:sum:k=n,…` over its non-zero quarter-hours.
func heldDayLines(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, heldDaysSQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var (
			repo string
			day  int64
			d    pushDay
		)
		dest := []any{&repo, &day, &d.pushes}
		for k := range d.quarters {
			dest = append(dest, &d.quarters[k])
		}
		if err = rows.Scan(dest...); err != nil {
			return nil, err
		}
		var sum int64
		var quarters []string
		for k, n := range d.quarters {
			sum += n
			if n != 0 {
				quarters = append(quarters, fmt.Sprintf("%d=%d", k, n))
			}
		}
		out = append(out, fmt.Sprintf("%s/%d:%d:%d:%s", repo, day, d.pushes, sum, strings.Join(quarters, ",")))
	}
	return out, rows.Err()
}

// oracleDayLines is the oracle's days in heldDayLines' shape.
func oracleDayLines(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, oracleDaysSQL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type key struct {
		repo string
		day  int64
	}
	var (
		order    []key
		sums     = map[key]int64{}
		quarters = map[key][]string{}
	)
	for rows.Next() {
		var (
			k    key
			q, n int64
		)
		if err = rows.Scan(&k.repo, &k.day, &q, &n); err != nil {
			return nil, err
		}
		if _, seen := sums[k]; !seen {
			order = append(order, k)
		}
		sums[k] += n
		quarters[k] = append(quarters[k], fmt.Sprintf("%d=%d", q, n))
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, len(order))
	for i, k := range order {
		out[i] = fmt.Sprintf("%s/%d:%d:%d:%s", k.repo, k.day, sums[k], sums[k], strings.Join(quarters[k], ","))
	}
	return out, nil
}

// textRows is every row of a one-column text query.
func textRows(ctx context.Context, tx *sql.Tx, query string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", query, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// activityMatchesRows fails the test where the push activity is not what the push rows count.
func (e *estate) activityMatchesRows() {
	e.t.Helper()
	if drift := e.activityDrift(); drift != "" {
		e.t.Fatal(drift)
	}
}

// activityDrift is [activityDrift] over the estate.
func (e *estate) activityDrift() string {
	e.t.Helper()
	var drift string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) (err error) {
		drift, err = activityDrift(context.Background(), tx)
		return err
	}); err != nil {
		e.t.Fatalf("read the push activity: %v", err)
	}
	return drift
}

// forgetActivity leaves the activity as an upgraded node holds it before its first boot: empty.
func (e *estate) forgetActivity() {
	e.t.Helper()
	e.exec(`DELETE FROM code_push_days`)
	e.exec(`DELETE FROM code_pusher_days`)
}

// recountActivity re-derives repository id's push activity in one writer transaction, for a case
// that seeds push rows by hand.
func (e *estate) recountActivity(id string) {
	e.t.Helper()
	if err := e.db.Tx(context.Background(), func(tx *sql.Tx) error {
		_, err := rederivePushActivityOf(context.Background(), tx, e.db.Caps().MaxVariables, id)
		return err
	}); err != nil {
		e.t.Fatalf("recount %s's push activity: %v", id, err)
	}
}

// activityTables is both tables' rows whole, as text.
func activityTables(e *estate) string {
	e.t.Helper()
	var days, pushers []string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) (err error) {
		if days, err = heldDayLines(context.Background(), tx); err != nil {
			return err
		}
		pushers, err = textRows(context.Background(), tx, heldPushersSQL)
		return err
	}); err != nil {
		e.t.Fatalf("read the push activity: %v", err)
	}
	return strings.Join(days, "\n") + "\n|\n" + strings.Join(pushers, "\n")
}

// activityText is repository repo's activity as text: each day row, `date total k=n …` over its
// non-zero quarter-hours, then each pusher row, `date kind/handle=n`.
func activityText(e *estate, repo string) string {
	e.t.Helper()
	var days, pushers []string
	for _, line := range strings.Split(activityTables(e), "\n") {
		key, rest, ok := strings.Cut(line, "/")
		if !ok || key != repo {
			continue
		}
		head, tail, _ := strings.Cut(rest, ":")
		if day, who, isPusher := strings.Cut(head, "/"); isPusher {
			pushers = append(pushers, dateOf(day)+" "+who+":"+tail)
			continue
		}
		parts := strings.SplitN(tail, ":", 3)
		days = append(days, dateOf(head)+" "+parts[0]+" "+strings.ReplaceAll(parts[2], ",", " "))
	}
	return strings.Join(days, "; ") + " | " + strings.Join(pushers, "; ")
}

// dateOf is an encoded UTC midnight, as text, as its date.
func dateOf(micros string) string {
	var n int64
	if _, err := fmt.Sscan(micros, &n); err != nil {
		return micros
	}
	return store.DecodeTime(n).Format(time.DateOnly)
}

// personNamed is a person pushing as their human seat handle, through the credential cred.
func personNamed(handle, cred string) Principal {
	return Principal{Kind: PrincipalOperator, OperatorID: cred, Seat: handle}
}

// seatNamed is a seat pushing as itself.
func seatNamed(handle string) Principal { return Principal{Kind: PrincipalAgent, Seat: handle} }

// pushedAs is pushEntry by who, setting branch ref.
func pushedAs(id string, who Principal, ref string) PushEntry {
	e := pushEntry(id, move(ref, ZeroOID, oid(id)))
	e.Principal = who
	return e
}

// EACH RECORD COUNTS ITS PUSHES WHERE IT SHOULD: every writer of a push row leaves its day's
// quarter-hour and its pusher's day as the case states, and the rows the oracle counts.
// Mutation: drop any arm of the push apply, the horizon's or a part's move, or the final purge's deletes.
func TestEachRecordCountsItsPushesWhereItShould(t *testing.T) {
	t.Parallel()
	const repo = "repo-c"
	// applyClock is 03:14Z: quarter-hour 12 of its day.
	created := func(t *testing.T, e *estate) {
		e.apply(create(t, repo, "c"), ccBranches(t, repo, "p0", 1, 2, 3))
	}
	opened := func(changes ...int) func(t *testing.T, e *estate) {
		return func(t *testing.T, e *estate) {
			created(t, e)
			for _, n := range changes {
				e.apply(ccOpen(t, repo, n, false))
			}
		}
	}
	enqueued := func(n int) built {
		return ccRecord(t, repo, OpEnqueue, Enqueue{RepoID: repo, EntryID: ccChange(100 + n), ChangeID: ccChange(n),
			TargetRef: ccMain, HeadOID: ccHead(n), Method: MergeMethodMerge, Principal: ccSeat})
	}
	mirrored := func(t *testing.T, e *estate) {
		created(t, e)
		s := settingsFor("")
		s.MirrorFrom = &MirrorSource{URL: "https://example.com/mirror.git"}
		e.apply(ccSettings(t, repo, s), ccRecord(t, repo, OpImportDone, ImportDone{RepoID: repo, Node: "git-a"}))
	}
	seatBranch := SeatNamespace("dev") + "idle"
	midnight := time.Date(2031, 4, 3, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		given   func(t *testing.T, e *estate)
		records func(t *testing.T) []built
		want    string
	}{
		{"a person's push", created, func(t *testing.T) []built {
			return []built{pushOf(t, repo, pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a"))}
		}, "2031-04-02 2 12=2 | 2031-04-02 person/ann:1; 2031-04-02 person/op:1"},
		{"a seat's engine push", created, func(t *testing.T) []built {
			e := pushedAs("p1", ccSeat, "refs/heads/a")
			e.Via = ViaEngine
			return []built{pushOf(t, repo, e)}
		}, "2031-04-02 2 12=2 | 2031-04-02 agent/dev:1; 2031-04-02 person/op:1"},
		{"a group of two seats and a person", created, func(t *testing.T) []built {
			return []built{pushOf(t, repo, pushedAs("p1", seatNamed("dev"), "refs/heads/a"),
				pushedAs("p2", seatNamed("qa"), "refs/heads/b"), pushedAs("p3", personNamed("ann", "pat:ann"), "refs/heads/c"))}
		}, "2031-04-02 4 12=4 | 2031-04-02 agent/dev:1; 2031-04-02 agent/qa:1; 2031-04-02 person/ann:1; " +
			"2031-04-02 person/op:1"},
		{"a person through two credentials", created, func(t *testing.T) []built {
			return []built{pushOf(t, repo, pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a"),
				pushedAs("p2", personNamed("ann", "token:ann-ci"), "refs/heads/b"))}
		}, "2031-04-02 3 12=3 | 2031-04-02 person/ann:2; 2031-04-02 person/op:1"},
		{"a refused command beside an ok one", created, func(t *testing.T) []built {
			e := pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a")
			e.Commands = append(e.Commands, Command{Ref: "refs/heads/b", Old: ZeroOID, New: oid("b"),
				Result: ResultNG, Reason: RefusalStale})
			return []built{pushOf(t, repo, e)}
		}, "2031-04-02 2 12=2 | 2031-04-02 person/ann:1; 2031-04-02 person/op:1"},
		{"a merge by a person", opened(1), func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpMerge, Merge{RepoID: repo, MergeID: ccChange(300), ChangeID: ccChange(1),
				Method: MergeMethodMerge, TargetRef: ccMain, BaseOID: oid("base"), HeadOID: ccHead(1),
				CommitOID: oid("merged"), Pack: packOf(ccChange(300)), Node: "git-a", ReceivedAt: applyClock,
				Principal: personNamed("ann", "pat:ann")})}
		}, "2031-04-02 2 12=2 | 2031-04-02 gitlet/:1; 2031-04-02 person/op:1"},
		{"a landing of two", opened(1, 2), func(t *testing.T) []built {
			l := Land{RepoID: repo, LandID: ccChange(400), TargetRef: ccMain, BaseOID: oid("base"), Node: "git-a",
				ReceivedAt: applyClock, Entries: []LandEntry{
					{EntryID: ccChange(101), ChangeID: ccChange(1), Commit: oid("landed-1")},
					{EntryID: ccChange(102), ChangeID: ccChange(2), Commit: oid("landed-2")}}}
			l.TipOID = l.Entries[1].Commit
			return []built{enqueued(1), enqueued(2), ccRecord(t, repo, OpLand, l)}
		}, "2031-04-02 3 12=3 | 2031-04-02 gitlet/:2; 2031-04-02 person/op:1"},
		{"an import chunk", func(t *testing.T, e *estate) {
			e.apply(rec(t, NameSubject("c"), OpCreate, Scope{NamePath(NameToken("c")), RepoPath(repo)},
				CreateRepo{RepoID: repo, Name: "c", Settings: settingsFor(""),
					Import: &ImportSource{URL: "https://example.com/c.git"}}))
		}, func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpImportChunk, ImportChunk{RepoID: repo, ChunkID: ccChange(500),
				Node: "git-a", Seq: 1, Commands: []Command{move(ccMain, ZeroOID, oid("imported"))}})}
		}, "2031-04-02 1 12=1 | 2031-04-02 gitlet/:1"},
		{"a mirror sync, and one that moves nothing", mirrored, func(t *testing.T) []built {
			return []built{
				ccRecord(t, repo, OpMirrorSync, MirrorSync{RepoID: repo, SyncID: ccChange(600), Node: "git-a",
					FetchedAt: applyClock, Commands: []Command{move(ccMain, oid("base"), oid("synced"))}}),
				ccRecord(t, repo, OpMirrorSync, MirrorSync{RepoID: repo, SyncID: ccChange(601), Node: "git-a",
					FetchedAt: applyClock})}
		}, "2031-04-02 2 12=2 | 2031-04-02 gitlet/:1; 2031-04-02 person/op:1"},
		{"an expiry", func(t *testing.T, e *estate) {
			created(t, e)
			e.apply(pushOf(t, repo, pushedAs("p1", ccSeat, seatBranch)))
		}, func(t *testing.T) []built {
			return []built{pushOf(t, repo, PushEntry{PushID: "x1", Principal: Principal{Kind: PrincipalSystem},
				Via: ViaExpiry, Node: "node-a", ReceivedAt: applyClock,
				Commands: []Command{move(seatBranch, oid("p1"), ZeroOID)}})}
		}, "2031-04-02 3 12=3 | 2031-04-02 agent/dev:1; 2031-04-02 person/op:1"},
		{"a redelivered push", created, func(t *testing.T) []built {
			p := pushOf(t, repo, pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a"))
			return []built{p, p}
		}, "2031-04-02 2 12=2 | 2031-04-02 person/ann:1; 2031-04-02 person/op:1"},
		{"a microsecond before midnight and midnight", func(t *testing.T, e *estate) {
			e.apply(create(t, repo, "c"))
		}, func(t *testing.T) []built {
			return []built{
				pushOf(t, repo, pushedAs("p1", seatNamed("dev"), "refs/heads/a")).at(midnight.Add(-time.Microsecond)),
				pushOf(t, repo, pushedAs("p2", seatNamed("dev"), "refs/heads/b")).at(midnight)}
		}, "2031-04-02 1 95=1; 2031-04-03 1 0=1 | 2031-04-02 agent/dev:1; 2031-04-03 agent/dev:1"},
		{"a horizon sharing a day and taking one alone, beside a push", func(t *testing.T, e *estate) {
			e.apply(create(t, repo, "c"))
			old := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
			e.apply(pushOf(t, repo, pushedAs("h1", seatNamed("dev"), "refs/heads/h1")).at(old.Add(time.Hour)),
				pushOf(t, repo, pushedAs("h2", seatNamed("dev"), "refs/heads/h2")).at(old.Add(24*time.Hour+time.Hour)),
				pushOf(t, repo, pushedAs("h3", seatNamed("dev"), "refs/heads/h3")).at(old.Add(24*time.Hour+2*time.Hour)))
		}, func(t *testing.T) []built {
			// The horizon falls between h2 and h3: h1 goes with its day, h2 leaves h3 in theirs.
			at := time.Date(2030, 6, 2, 1, 30, 0, 0, time.UTC).Add(PushHistory)
			return []built{pushOf(t, repo, pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a")).at(at)}
		}, "2030-06-02 1 8=1; 2030-11-30 1 6=1 | 2030-06-02 agent/dev:1; 2030-11-30 person/ann:1"},
		{"a purge part", func(t *testing.T, e *estate) {
			created(t, e)
			e.apply(pushOf(t, repo, pushedAs("p1", personNamed("ann", "pat:ann"), "refs/heads/a")),
				ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
		}, func(t *testing.T) []built { return []built{partOf(t, repo, 1)} }, " | "},
		{"a final purge", func(t *testing.T, e *estate) {
			created(t, e)
			e.apply(ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
		}, func(t *testing.T) []built {
			return []built{rec(t, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken("c")), RepoPath(repo)},
				Purge{RepoID: repo, Name: "c", Reason: "test"})}
		}, " | "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			tc.given(t, e)
			e.apply(tc.records(t)...)
			if got := activityText(e, repo); got != tc.want {
				t.Errorf("the activity holds\n%s\nwant\n%s", got, tc.want)
			}
			e.activityMatchesRows()
		})
	}
}

// A PUSH COUNTS AS ITS PARTY OR GITLET: the ways Gitlet lands count as Gitlet whoever pushed, the
// expiry as nobody, and git's and the engine's as the seat or the person's human seat.
// Mutation: count a merge as its merger, or grow PartyKinds without PusherKinds.
func TestAPushCountsAsItsPartyOrGitlet(t *testing.T) {
	t.Parallel()
	principals := map[string]Principal{"a seat": seatNamed("dev"), "a person": personNamed("ann", "pat:ann"),
		"the engine": {Kind: PrincipalSystem}, "a credential bound to nobody": {Kind: PrincipalOperator,
			OperatorID: "token:ci"}}
	for _, via := range PushVias() {
		for name, p := range principals {
			kind, handle, counted := pusherOf(via, p.Kind, p.Seat)
			var want string
			switch {
			case via == ViaExpiry:
			case via == ViaMerge || via == ViaQueue || via == ViaImport || via == ViaMirror:
				want = "gitlet/"
			case name == "a seat":
				want = "agent/dev"
			case name == "a person":
				want = "person/ann"
			}
			got := ""
			if counted {
				got = string(kind) + "/" + handle
			}
			if got != want {
				t.Errorf("a %s push by %s counts as %q, want %q", via, name, got, want)
			}
		}
	}
	for _, k := range PartyKinds() {
		if !PusherKind(k).Valid() {
			t.Errorf("the party kind %q is no pusher kind", k)
		}
	}
}

// A DAY AND A QUARTER-HOUR ARE UTC'S: an instant's day is its UTC midnight and its quarter-hour
// the one it falls in from there. Mutation: a floor by the hour.
func TestADayAndAQuarterHourAreUTCs(t *testing.T) {
	t.Parallel()
	midnight := time.Date(2031, 4, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		at      time.Time
		quarter int
	}{
		{midnight.Add(15*time.Minute - time.Microsecond), 0},
		{midnight.Add(15 * time.Minute), 1},
		{midnight.Add(24*time.Hour - time.Microsecond), 95},
	} {
		at := store.EncodeTime(tc.at)
		if day, quarter := dayOf(at), quarterOf(at); day != store.EncodeTime(midnight) || quarter != tc.quarter {
			t.Errorf("%s falls on %s, quarter-hour %d, want %s, %d", tc.at, store.DecodeTime(day), quarter,
				midnight, tc.quarter)
		}
	}
}

// THE PUSH DAY COLUMNS ARE THE QUARTER-HOURS: the column list, a row's places, the upsert's tail and
// the migrated table each spell q00 to q95 in order, and a row's text reads back whole. Mutation: drop or
// swap a quarter in any of them.
func TestThePushDayColumnsAreTheQuarterHours(t *testing.T) {
	t.Parallel()
	var want []string
	for k := range pushQuarters {
		want = append(want, fmt.Sprintf("q%02d", k))
	}
	quarter := regexp.MustCompile(`\bq\d\d\b`)
	if got := quarter.FindAllString(pushQuarterColumns, -1); !slices.Equal(got, want) {
		t.Errorf("pushQuarterColumns names %v", got)
	}
	if got := quarter.FindAllString(pushQuartersText, -1); !slices.Equal(got, want) ||
		!strings.HasPrefix(pushQuartersText, "concat_ws(',', q00,") {
		t.Errorf("pushQuartersText joins %v", got)
	}
	var read [pushQuarters]int64
	if err := parseQuarters(strings.Repeat("7,", pushQuarters-1)+"-2", &read); err != nil || read[0] != 7 ||
		read[pushQuarters-1] != -2 {
		t.Errorf("parseQuarters read %v, %v", read, err)
	}
	for _, short := range []string{strings.Repeat("1,", pushQuarters-2) + "1", strings.Repeat("1,", pushQuarters)} {
		if err := parseQuarters(short, &read); err == nil {
			t.Errorf("parseQuarters read %d counts", strings.Count(short, ",")+1)
		}
	}
	var tail []string
	for _, k := range want {
		tail = append(tail, k, k, "excluded."+k)
	}
	if got := regexp.MustCompile(`\b(?:excluded\.)?q\d\d\b`).FindAllString(pushDaysUpsertTailSQL, -1); !slices.Equal(got,
		tail) {
		t.Errorf("pushDaysUpsertTailSQL adds %v", got)
	}
	if places := strings.Count(pushDaysRowSQL, "?"); places != 3+pushQuarters {
		t.Errorf("pushDaysRowSQL holds %d places, want %d", places, 3+pushQuarters)
	}
	e := openApplyEstate(t)
	var columns []string
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `PRAGMA table_info(code_push_days)`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		names, err := rows.Columns()
		if err != nil {
			return err
		}
		for rows.Next() {
			values := make([]sql.NullString, len(names))
			into := make([]any, len(names))
			for i := range values {
				into[i] = &values[i]
			}
			if err = rows.Scan(into...); err != nil {
				return err
			}
			columns = append(columns, values[slices.Index(names, "name")].String)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(columns, append([]string{"repo_id", "day", "pushes"}, want...)) {
		t.Errorf("code_push_days' columns are %v", columns)
	}
}

// PUSHERS ARE MOST FIRST AND BOUNDED: ties go by kind and handle, the cut ones are counted, and the
// days are the UTC days that begin within the bounds. Mutation: drop the tie-break, or the cut's count.
func TestPushersAreMostFirstAndBounded(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	day := func(d int) int64 { return store.EncodeTime(time.Date(2031, 4, d, 0, 0, 0, 0, time.UTC)) }
	for _, row := range []struct {
		day          int64
		kind, pusher string
		n            int
	}{
		{day(2), "person", "ann", 2}, {day(3), "person", "ann", 1}, {day(2), "agent", "dev", 3},
		{day(3), "gitlet", "", 3}, {day(2), "agent", "zed", 3}, {day(2), "agent", "qa", 2},
		{day(3), "agent", "qa", 1}, {day(3), "agent", "bo", 3}, {day(4), "agent", "late", 9},
		{day(1), "agent", "early", 9},
	} {
		e.exec(`INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes) VALUES ('r', ?, ?, ?, ?)`,
			row.day, row.kind, row.pusher, row.n)
	}
	read := func(bounds []time.Time, limit int) *Pushers {
		var p *Pushers
		if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) (err error) {
			p, err = pushersIn(context.Background(), tx, "r", bounds, limit)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Bounds at +5:45's midnights: the UTC days that begin within them are April 2 and 3.
	at := func(d int) time.Time { return time.Date(2031, 4, d, 18, 15, 0, 0, time.UTC) }
	// Six pushers tie at 3: the agents by handle, then the person, then Gitlet.
	from, until := time.Date(2031, 4, 2, 0, 0, 0, 0, time.UTC), time.Date(2031, 4, 4, 0, 0, 0, 0, time.UTC)
	all := []PusherPushes{{Kind: PusherAgent, Handle: "bo", Pushes: 3}, {Kind: PusherAgent, Handle: "dev", Pushes: 3},
		{Kind: PusherAgent, Handle: "qa", Pushes: 3}, {Kind: PusherAgent, Handle: "zed", Pushes: 3},
		{Kind: PusherPerson, Handle: "ann", Pushes: 3}, {Kind: PusherGitlet, Pushes: 3}}
	for _, tc := range []struct {
		limit int
		want  *Pushers
	}{
		{10, &Pushers{From: from, Until: until, Top: all}},
		{3, &Pushers{From: from, Until: until, Top: all[:3], Omitted: 3, OmittedPushes: 9}},
	} {
		if got := read([]time.Time{at(1), at(2), at(3)}, tc.limit); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pushersIn at most %d answered %+v, want %+v", tc.limit, got, tc.want)
		}
	}
	if got := read([]time.Time{at(1), at(1).Add(time.Hour)}, 2); !got.From.Equal(got.Until) || got.Top != nil {
		t.Errorf("bounds inside one UTC day answered %+v, want no day and no pusher", got)
	}
}

// A PURGE PART TAKES THE ACTIVITY LEFT ONCE ITS PUSHES ARE GONE: rows no push row stands for are taken
// a part at a time, on top of what an older build's part takes, until the final purge forms.
// Mutation: drop the part's take, and the decide never forms the final purge.
func TestAPurgePartTakesTheActivityLeftOnceItsPushesAreGone(t *testing.T) {
	t.Parallel()
	const repo = "repo-c"
	e := openApplyEstate(t)
	e.apply(create(t, repo, "c"), ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1}))
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes)
		SELECT ?, 0, 'agent', printf('seat-%06d', i), 1 FROM k`, PurgePartRows+10, repo)
	e.seedRetired(repo, 1, applyClock, 3)
	remaining := func() int64 {
		var rows int64
		if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
			var bytes int64
			return tx.QueryRowContext(context.Background(), remainingSQL, repo, PurgePartRows+1).Scan(&rows, &bytes)
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	parts := 0
	for remaining() > PurgePartRows {
		parts++
		if parts > 3 {
			t.Fatalf("the purge formed %d parts and %d rows remain", parts, remaining())
		}
		before := e.num(`SELECT COUNT(*) FROM code_pusher_days WHERE repo_id = ?`, repo)
		e.apply(partOf(t, repo, parts))
		after := e.num(`SELECT COUNT(*) FROM code_pusher_days WHERE repo_id = ?`, repo)
		if before-after > PurgePartRows {
			t.Errorf("part %d took %d activity rows, past PurgePartRows", parts, before-after)
		}
		if retired := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); retired != 0 {
			t.Errorf("part %d left %d retired packs an older build's part takes", parts, retired)
		}
	}
	if parts != 1 {
		t.Errorf("the purge took %d parts, want 1", parts)
	}
	e.apply(rec(t, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken("c")), RepoPath(repo)},
		Purge{RepoID: repo, Name: "c", Reason: "test"}))
	if n := e.num(`SELECT COUNT(*) FROM code_pusher_days WHERE repo_id = ?`, repo); n != 0 {
		t.Errorf("the final purge left %d pusher days", n)
	}

	// A PART WITH PUSHES LEFT takes none of the activity beyond its pushes' own.
	const other = "repo-d"
	e.apply(create(t, other, "d"), pushOf(t, other, pushedAs("p1", seatNamed("dev"), "refs/heads/a")),
		ccRecord(t, other, OpDelete, LifecycleRecord{RepoID: other, ConfigEpoch: 1}))
	e.exec(`INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes) VALUES (?, 0, 'agent', 'x', 1)`,
		other)
	e.apply(partOf(t, other, 1))
	if n := e.num(`SELECT COUNT(*) FROM code_pusher_days WHERE repo_id = ? AND day = 0`, other); n != 1 {
		t.Errorf("a part with pushes left took the activity no push stands for: %d left", n)
	}
}

// THE PUSH ACTIVITY IS THE PUSH ROWS: random records, attempts among them, leave both tables what the
// oracle counts after each one, through a clock that jumps past the history and steps back, a
// re-derivation midway, a replay from zero, a re-derivation of the end, an older build's final purge
// and seeded drift. Mutation: drop a horizon's or a part's move, count an expiry or a merger, floor by
// the hour, skip the prune, count a redelivery, walk only code_repos in the re-derivation, or re-derive
// only the days whose total moved, and one differs.
func TestThePushActivityIsThePushRows(t *testing.T) {
	t.Parallel()
	applied := map[string]bool{}
	for seed := uint64(1); seed <= 4; seed++ {
		w := &activityWorld{countsWorld: newCountsWorld(t, seed), clock: applyClock}
		upgradeAt := 20 + w.rng.IntN(200)
		for step := range 250 {
			if step == upgradeAt {
				w.e.forgetActivity()
				w.e.rederive()
				w.e.activityMatchesRows()
			}
			op := w.step()
			if drift := w.e.activityDrift(); drift != "" {
				t.Fatalf("seed %d step %d, %s: %s", seed, step, op, drift)
			}
		}
		maps.Copy(applied, w.applied)

		// THE REPLAY FROM ZERO writes the same rows.
		replayed := openApplyEstate(t)
		replayed.apply(w.records...)
		if got, want := activityTables(replayed), activityTables(w.e); got != want {
			t.Errorf("seed %d: the replay from zero holds\n%s\nand the estate it replays\n%s", seed, got, want)
		}
		// THE RE-DERIVATION of rows kept writes nothing, and of the rows forgotten the same rows.
		kept := activityTables(w.e)
		if n := w.e.rederive(); n != 0 {
			t.Errorf("seed %d: re-deriving the rows the applies kept wrote %d rows", seed, n)
		}
		w.e.forgetActivity()
		w.e.rederive()
		if got := activityTables(w.e); got != kept {
			t.Errorf("seed %d: the re-derivation holds\n%s\nand the applies kept\n%s", seed, got, kept)
		}
		// AN OLDER BUILD'S FINAL PURGE leaves the activity of a repository whose rows it deleted, and
		// SEEDED DRIFT stands beside it; one re-derivation brings both to the rows.
		if r := w.pick(`SELECT DISTINCT repo_id FROM code_pushes`); r != nil {
			w.e.exec(`DELETE FROM code_pushes WHERE repo_id = ?`, r[0])
			w.e.exec(`DELETE FROM code_repos WHERE id = ?`, r[0])
		}
		if d := w.pick(`SELECT repo_id, day FROM code_push_days`); d != nil {
			w.e.exec(`UPDATE code_push_days SET pushes = -pushes WHERE repo_id = ? AND day = ?`, d[0], d[1])
		}
		if p := w.pick(`SELECT repo_id, day, pusher_kind, pusher FROM code_pusher_days`); p != nil {
			w.e.exec(`UPDATE code_pusher_days SET pushes = 0 WHERE repo_id = ? AND day = ? AND pusher_kind = ?
				AND pusher = ?`, p[0], p[1], p[2], p[3])
		}
		// A quarter-hour moved within a kept day: its total stays.
		if d := w.pick(`SELECT repo_id, day, ` + pushQuartersText + ` FROM code_push_days
			WHERE pushes > 0 AND repo_id IN (SELECT repo_id FROM code_pushes)`); d != nil {
			var q [pushQuarters]int64
			if err := parseQuarters(d[2], &q); err != nil {
				t.Fatal(err)
			}
			from := slices.IndexFunc(q[:], func(n int64) bool { return n > 0 })
			to := (from + 1) % pushQuarters
			w.e.exec(fmt.Sprintf(`UPDATE code_push_days SET q%02d = q%02d - 1, q%02d = q%02d + 1
				WHERE repo_id = ? AND day = ?`, from, from, to, to), d[0], d[1])
		}
		w.e.exec(`INSERT INTO code_pusher_days (repo_id, day, pusher_kind, pusher, pushes)
			VALUES ('never-pushed', 0, 'agent', 'dev', 2)`)
		w.e.exec(`INSERT INTO code_push_days (repo_id, day, pushes, ` + pushQuarterColumns + `)
			SELECT 'never-pushed', 0, 0, ` + strings.Repeat("0, ", pushQuarters-1) + `0`)
		if w.e.activityDrift() == "" {
			t.Fatalf("seed %d: the seeded drift reads as none", seed)
		}
		w.e.rederive()
		w.e.activityMatchesRows()
	}
	for _, op := range activityOps() {
		if !applied[op.name] {
			t.Errorf("no seed applied %s: the property holds nothing of it", op.name)
		}
	}
	for _, event := range []string{"a horizon that deleted rows", "a part that deleted pushes"} {
		if !applied[event] {
			t.Errorf("no seed applied %s: the property holds nothing of it", event)
		}
	}
}

// activityWorld is a countsWorld whose records are stored at a broker clock of its own.
type activityWorld struct {
	*countsWorld
	clock time.Time
}

// activityOp is one kind of record the activity property draws.
type activityOp struct {
	name string
	make func(w *activityWorld) []built
}

// activityPushers are who the activity property's pushes are by: two seats, and two persons, one
// through two credentials.
var activityPushers = []Principal{seatNamed("dev"), seatNamed("qa"), personNamed("ann", "pat:ann"),
	personNamed("ann", "token:ann-ci"), personNamed("bob", "pat:bob")}

// activityOps is countsOps and the activity's own: groups by several pushers, refused commands,
// expiries and a mirror sync that moves nothing.
func activityOps() []activityOp {
	var ops []activityOp
	for _, op := range countsOps {
		if op.name == "a redelivery" {
			ops = append(ops, activityOp{op.name, (*activityWorld).redelivery})
			continue
		}
		ops = append(ops, activityOp{op.name, func(w *activityWorld) []built { return op.make(w.countsWorld) }})
	}
	return append(ops,
		activityOp{"a group by several pushers", func(w *activityWorld) []built {
			r := w.pick(ccInUse)
			if r == nil {
				return nil
			}
			var entries []PushEntry
			for range 1 + w.rng.IntN(4) {
				who := activityPushers[w.rng.IntN(len(activityPushers))]
				ref := "refs/heads/b-" + w.id()
				if who.Kind == PrincipalAgent && w.rng.IntN(2) == 0 {
					ref = SeatNamespace(who.Seat) + w.id()
				}
				e := pushedAs(w.id(), who, ref)
				if w.rng.IntN(3) == 0 {
					e.Commands = append(e.Commands, Command{Ref: "refs/heads/r-" + w.id(), Old: ZeroOID,
						New: oid(w.id()), Result: ResultNG, Reason: RefusalStale})
				}
				if who.Kind == PrincipalAgent && w.rng.IntN(2) == 0 {
					e.Via = ViaEngine
				}
				entries = append(entries, e)
			}
			return []built{pushOf(w.t, r[0], entries...)}
		}},
		activityOp{"an attempt", func(w *activityWorld) []built {
			r := w.pick(ccInUse)
			if r == nil {
				return nil
			}
			who := activityPushers[w.rng.IntN(len(activityPushers))]
			e := pushedAs(w.id(), who, "refs/heads/locked-"+w.id())
			e.Commands[0].Result, e.Commands[0].Reason = ResultNG, RefusalProtectedDirect
			e.Pack, e.Offered = nil, w.rng.IntN(3)
			return []built{pushOf(w.t, r[0], e)}
		}},
		activityOp{"an expiry", func(w *activityWorld) []built {
			b := w.pick(`SELECT f.repo_id, f.ref, f.target FROM code_refs f JOIN code_repos r ON r.id = f.repo_id
				WHERE f.target != '' AND f.ref LIKE 'refs/heads/seats/%' AND r.status IN ('active', 'archived')`)
			if b == nil {
				return nil
			}
			return []built{pushOf(w.t, b[0], PushEntry{PushID: w.id(), Principal: Principal{Kind: PrincipalSystem},
				Via: ViaExpiry, Node: "node-a", ReceivedAt: applyClock,
				Commands: []Command{move(b[1], b[2], ZeroOID)}})}
		}},
		activityOp{"a mirror sync that moves nothing", func(w *activityWorld) []built {
			r := w.pick(`SELECT id FROM code_repos WHERE status = 'active' AND mirror_from != ''`)
			if r == nil {
				return nil
			}
			return []built{ccRecord(w.t, r[0], OpMirrorSync, MirrorSync{RepoID: r[0], SyncID: w.id(), Node: "git-a",
				FetchedAt: applyClock})}
		}},
	)
}

// tick moves the clock before a step: by minutes mostly, hours or days at times, past the push
// history about once in 25 steps, and back by up to an hour (a leader change) once in 30.
func (w *activityWorld) tick() {
	switch n := w.rng.IntN(150); {
	case n < 6:
		w.clock = w.clock.Add(PushHistory + time.Duration(w.rng.IntN(48))*time.Hour)
	case n < 11:
		w.clock = w.clock.Add(-time.Duration(1+w.rng.IntN(60)) * time.Minute)
	case n < 30:
		w.clock = w.clock.Add(time.Duration(1+w.rng.IntN(72)) * time.Hour)
	default:
		w.clock = w.clock.Add(time.Duration(w.rng.IntN(21)) * time.Minute)
	}
}

// redelivery is an earlier record stored after the newest record's clock less PushHistory: the log
// dedupes within the history, so no record whose pushes a horizon deleted is delivered again.
func (w *activityWorld) redelivery() []built {
	var newest time.Time
	for _, r := range w.records {
		if r.storedAt.After(newest) { //fixedwait:stated the world's own stamps, no clock's
			newest = r.storedAt
		}
	}
	var recent []built
	for _, r := range w.records {
		if r.storedAt.After(newest.Add(-PushHistory)) { //fixedwait:stated the world's own stamps, no clock's
			recent = append(recent, r)
		}
	}
	if len(recent) == 0 {
		return nil
	}
	return []built{recent[w.rng.IntN(len(recent))]}
}

// step applies one drawn op's records, each stored at the clock but a redelivery, and answers its name.
func (w *activityWorld) step() string {
	w.tick()
	ops := activityOps()
	for {
		op := ops[w.rng.IntN(len(ops))]
		records := op.make(w)
		if records == nil {
			continue
		}
		before := w.fingerprint()
		pushes := w.e.num(`SELECT COUNT(*) FROM code_pushes`)
		expired := w.e.num(`SELECT COUNT(*) FROM code_pushes WHERE at < ?`,
			store.EncodeTime(w.clock.Add(-PushHistory)))
		for _, r := range records {
			if op.name != "a redelivery" {
				r = r.at(w.clock)
			}
			w.e.apply(r)
			w.records = append(w.records, r)
		}
		if op.name == "a redelivery" || w.fingerprint() != before {
			w.applied[op.name] = true
		}
		if op.name == "a mirror sync that moves nothing" {
			w.applied[op.name] = true
		}
		after := w.e.num(`SELECT COUNT(*) FROM code_pushes WHERE at < ?`, store.EncodeTime(w.clock.Add(-PushHistory)))
		if op.name != "a purge part" && op.name != "a purge" && after < expired {
			w.applied["a horizon that deleted rows"] = true
		}
		if op.name == "a purge part" && w.e.num(`SELECT COUNT(*) FROM code_pushes`) < pushes {
			w.applied["a part that deleted pushes"] = true
		}
		return op.name
	}
}

// A PUSH RECORD WRITES NO MORE ACTIVITY THAN ITS DECIDE COUNTS: each kind of record's pushes write at
// most the day row recordRows counts and a pusher row each entry's bound counts, and the committer's
// fullest record, nothing due, reports every row its decide counted but the horizon's room and the mark.
// Mutation: drop pusherDayRows from entryRows, rowBound or mergePushRows, or pushDayRows from recordRows.
func TestAPushRecordWritesNoMoreActivityThanItsDecideCounts(t *testing.T) {
	t.Parallel()
	counted := func(entries []PushEntry) int {
		rows := pushDayRows
		for _, e := range entries {
			rows += entryRows(e) - entryRows(PushEntry{Commands: e.Commands, Pack: e.Pack, Via: ViaExpiry,
				Principal: e.Principal, Landings: e.Landings})
		}
		return rows
	}
	written := func(t *testing.T, entries []PushEntry) int {
		e := openApplyEstate(t)
		var n int
		if err := e.db.Tx(context.Background(), func(tx *sql.Tx) (err error) {
			at := applyContext{brokerAt: applyClock, maxVariables: e.db.Caps().MaxVariables}
			n, err = countPushActivity(context.Background(), tx, at, "repo-c", entries, pushActivity{})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	each := func(n int, of func(i int) PushEntry) []PushEntry {
		out := make([]PushEntry, n)
		for i := range out {
			out[i] = of(i)
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		entries []PushEntry
		exact   bool
	}{
		{"a group by as many seats", each(MaxPushesPerRecord, func(i int) PushEntry {
			return pushedAs(fmt.Sprint("p", i), seatNamed(fmt.Sprint("seat-", i)), "refs/heads/a")
		}), true},
		{"a group by one seat", each(MaxPushesPerRecord, func(i int) PushEntry {
			return pushedAs(fmt.Sprint("p", i), seatNamed("dev"), "refs/heads/a")
		}), false},
		{"a merge", []PushEntry{Merge{MergeID: "m", Principal: personNamed("ann", "pat:ann"), TargetRef: ccMain,
			BaseOID: oid("base"), CommitOID: oid("merged")}.push()}, true},
		{"a landing", each(MaxLandEntries, func(i int) PushEntry {
			return PushEntry{PushID: fmt.Sprint("l", i), Principal: ccSeat, Via: ViaQueue,
				Commands: []Command{move(ccMain, oid(fmt.Sprint(i)), oid(fmt.Sprint(i+1)))}}
		}), false},
		{"an import chunk", []PushEntry{{PushID: "i", Principal: Principal{Kind: PrincipalSystem}, Via: ViaImport,
			Commands: []Command{move(ccMain, ZeroOID, oid("i"))}}}, true},
		{"an expiry", []PushEntry{{PushID: "x", Principal: Principal{Kind: PrincipalSystem}, Via: ViaExpiry,
			Commands: []Command{move(SeatNamespace("dev")+"idle", oid("x"), ZeroOID)}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, bound := written(t, tc.entries), counted(tc.entries)
			if got > bound || tc.exact && got != bound {
				t.Errorf("the pushes wrote %d activity rows and their decide counted %d", got, bound)
			}
		})
	}
	t.Run("a landing within its share of the land's bound", func(t *testing.T) {
		t.Parallel()
		if share := landPushRows(MaxLandEntries) - landPushRows(0) - MaxLandEntries*(1+1+3); share !=
			MaxLandEntries*pusherDayRows {
			t.Errorf("landPushRows counts %d pusher rows for %d landings", share, MaxLandEntries)
		}
	})
	t.Run("a merge within its share of the merge's bound", func(t *testing.T) {
		t.Parallel()
		m := Merge{MergeID: "m", Principal: seatNamed("dev"), TargetRef: ccMain, BaseOID: oid("base"),
			CommitOID: oid("merged"), SourceRef: SeatNamespace("dev") + "b", HeadOID: oid("head"), DeleteSource: true,
			Pack: &PackRef{}}
		if got := entryRows(m.push()); got != mergePushRows {
			t.Errorf("a merge's fullest push counts %d rows as an entry, and mergePushRows %d", got, mergePushRows)
		}
	})
	t.Run("the committer's fullest record", func(t *testing.T) {
		t.Parallel()
		e := openApplyEstate(t)
		e.apply(create(t, "repo-c", "c"))
		entries := fullest(t, "fullest", 0, func(p, c int) Command {
			return move(fmt.Sprintf("refs/heads/f%d-%04d", p, c), ZeroOID, oid(fmt.Sprint(p, c)))
		})
		for i := range entries {
			entries[i].Principal = personNamed(fmt.Sprint("person-", i), fmt.Sprint("pat:", i))
		}
		if got, want := e.applyCounting(pushOf(t, "repo-c", entries...)),
			MaxRecordRows-HorizonDeletesPerRecord-1; got != want {
			t.Errorf("the fullest record reported %d rows, want %d: every row its decide counted but the "+
				"horizon's and the mark", got, want)
		}
		e.activityMatchesRows()
	})
}

// writesPushRows is a statement that inserts, deletes or updates a push row.
var writesPushRows = regexp.MustCompile(`(INSERT INTO|DELETE FROM|UPDATE) code_pushes([^_a-z]|$)`)

// EVERY WRITER OF A PUSH ROW MOVES ITS ACTIVITY: the functions spelling a write of code_pushes are
// exactly landPushes, which counts its pushes in and its horizon's out, deletePushes, whose callers
// are the horizon and a purge part, and applyPurge, which deletes the activity with the rows.
// Mutation: a new writer of push rows, a stale entry, or a dropped call.
func TestEveryWriterOfAPushRowMovesItsActivity(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		files = append(files, f)
	}
	info := &types.Info{Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	conf := types.Config{Importer: emptyImports{}, Error: func(error) {}}
	_, _ = conf.Check("github.com/crewlet/crewlet/internal/gitlet", fset, files, info)
	writing := func(lit *ast.BasicLit) bool {
		v, unquoteErr := strconv.Unquote(lit.Value)
		return lit.Kind == token.STRING && unquoteErr == nil && writesPushRows.MatchString(v)
	}
	statements := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if g, ok := decl.(*ast.GenDecl); ok && (g.Tok == token.CONST || g.Tok == token.VAR) {
				for _, spec := range g.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, value := range vs.Values {
						ast.Inspect(value, func(n ast.Node) bool {
							if lit, isLit := n.(*ast.BasicLit); isLit && writing(lit) && i < len(vs.Names) {
								statements[vs.Names[i].Name] = true
							}
							return true
						})
					}
				}
			}
		}
	}
	writers := map[string]bool{}
	calls, names := map[string]map[string]bool{}, map[string]map[string]bool{}
	callers := map[string][]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := declKey(fn)
			calls[key], names[key] = map[string]bool{}, map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.BasicLit:
					if writing(n) {
						writers[key] = true
					}
				case *ast.Ident:
					names[key][n.Name] = true
					if statements[n.Name] {
						writers[key] = true
					}
				case *ast.CallExpr:
					if callee := calleeKey(info, n); callee != "" && !calls[key][callee] {
						calls[key][callee] = true
						callers[callee] = append(callers[callee], key)
					}
				}
				return true
			})
		}
	}
	want := []string{"(*Applier).applyPurge", "(*Applier).landPushes", "deletePushes"}
	if got := slices.Sorted(maps.Keys(writers)); !slices.Equal(got, want) {
		t.Errorf("the writers of push rows are %v, want %v: a new one moves the activity with them", got, want)
	}
	for _, check := range []struct {
		ok   bool
		what string
	}{
		{calls["(*Applier).landPushes"]["countPushActivity"] && calls["(*Applier).landPushes"]["(*Applier).expirePushes"],
			"landPushes counts its pushes and its horizon's through countPushActivity"},
		{slices.Equal(slices.Sorted(slices.Values(callers["deletePushes"])),
			[]string{"(*Applier).applyPurgePart", "(*Applier).expirePushes"}),
			"deletePushes is called by the horizon and a purge part alone"},
		{slices.Equal(callers["(*Applier).expirePushes"], []string{"(*Applier).landPushes"}),
			"expirePushes is called by landPushes alone, which counts what it deleted out"},
		{calls["(*Applier).applyPurgePart"]["moveActivity"] && calls["(*Applier).applyPurgePart"]["purgeLeftActivity"],
			"a purge part moves what it deleted and takes what is left"},
		{names["(*Applier).applyPurge"]["purgeAllPushDaysSQL"] && names["(*Applier).applyPurge"]["purgeAllPusherDaysSQL"],
			"the final purge deletes the activity with the push rows"},
	} {
		if !check.ok {
			t.Errorf("want: %s", check.what)
		}
	}
}
