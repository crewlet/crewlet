package code_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// introducing is entries of a push each: one before the group, sixty-four in
// ONE record — sharing its position, so a page boundary falls inside it — and
// one after; and every push id in log order.
func introducing(t *testing.T, n *node, id string) (want []string, positions []statelog.Position) {
	t.Helper()
	lone := func(push string) code.PushEntry {
		e := entry(push, set("refs/heads/"+push, code.ZeroOID, oidOf(push)))
		e.Introduced, e.IntroducedCount = []string{oidOf(push)}, 3
		return e
	}
	first := lone("a-first")
	// AN ENTRY'S REFUSED COMMAND IS NO MOVE: the index is built from what a
	// push landed.
	first.Commands = append(first.Commands, code.Command{Ref: "refs/heads/main", Old: oidOf("x"),
		New: oidOf("y"), Result: code.ResultNG, Reason: code.RefusalStale})
	first.Principal = code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:42", Seat: "founder"}
	positions = append(positions, n.push(id, first).Position)
	want = append(want, first.PushID)
	var group []code.PushEntry
	for i := range code.MaxPushesPerRecord {
		e := lone(fmt.Sprintf("b-group-%02d", i))
		group = append(group, e)
		want = append(want, e.PushID)
	}
	positions = append(positions, n.push(id, group...).Position)
	positions = append(positions, n.push(id, lone("c-last")).Position)
	return append(want, "c-last"), positions
}

// INTRODUCED PAGES IN POSITION ORDER: a repository's pushes in log order,
// ascending over (After, Through] or descending below Before, a limit at a
// time — sixty-four pushes in one record and a page boundary inside it, none
// repeated and none skipped — each row the push as a git node's index is
// built from it: who, where, when, the refs it LANDED and the commits it
// introduced. A gap's range reads its pushes alone, and a read at session
// waits for the position it names.
//
// Mutations: the keyset after a cursor without its push id (position > ?
// alone) → a page boundary inside the record skips the rest of it; the
// refused command kept as a move → the first push moves main.
func TestIntroducedPagesInPositionOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	want, positions := introducing(t, n, id)

	read := func(in code.IntroducedQuery, fresh statelog.Freshness) []code.IntroducedRow {
		t.Helper()
		var rows []code.IntroducedRow
		q := code.ReadQuery{Kind: code.ReadIntroduced, Introduced: &in, Limit: 10, Fresh: fresh}
		for range 20 {
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(introduced %+v): %v", in, err)
			}
			rows = append(rows, answer.Introduced...)
			if answer.Next == "" {
				return rows
			}
			q.Cursor = answer.Next
		}
		t.Fatalf("the introduced read %+v did not end in twenty pages", in)
		return nil
	}
	ids := func(rows []code.IntroducedRow) []string {
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.PushID
		}
		return out
	}

	up := read(code.IntroducedQuery{RepoID: id}, stale)
	if fmt.Sprint(ids(up)) != fmt.Sprint(want) {
		t.Fatalf("ascending reads %v, want %v", ids(up), want)
	}
	first := up[0]
	if first.Position != positions[0] || first.Via != code.ViaGit || first.At.IsZero() ||
		first.Principal != (code.Principal{Kind: code.PrincipalOperator, OperatorID: "pat:42", Seat: "founder"}) ||
		fmt.Sprint(first.Moves) != fmt.Sprint([]code.RefMove{{Ref: "refs/heads/a-first", Old: code.ZeroOID,
			New: oidOf("a-first")}}) ||
		fmt.Sprint(first.Introduced) != fmt.Sprint([]string{oidOf("a-first")}) || first.Count != 3 {
		t.Fatalf("the first push reads %+v: want its position, its principal, the one ref it landed, "+
			"and its introduced commit of three", first)
	}

	down := read(code.IntroducedQuery{RepoID: id, Descending: true,
		Before: statelog.Position{Stream: positions[2].Stream, Generation: positions[2].Generation,
			Seq: positions[2].Seq + 1}}, stale)
	reversed := slices.Clone(want)
	slices.Reverse(reversed)
	if fmt.Sprint(ids(down)) != fmt.Sprint(reversed) {
		t.Fatalf("descending reads %v, want %v", ids(down), reversed)
	}

	// A GAP: the range (first record, group record] at session, floored at
	// the group's own position.
	gap := read(code.IntroducedQuery{RepoID: id, After: positions[0], Through: positions[1]},
		statelog.FloorAt(statelog.ReadSession, positions[1]))
	if fmt.Sprint(ids(gap)) != fmt.Sprint(want[1:1+code.MaxPushesPerRecord]) {
		t.Fatalf("the gap reads %v, want the group alone", ids(gap))
	}
}

// AN INTRODUCED READ IS THE ENGINE'S OWN: it names no asker — one there would
// be judged by nothing — and one range, running one way, by the repository's
// id; it states its level, a session read the position it waits for; and a
// repository with no row is not found, which drops the index that asked.
func TestAnIntroducedReadIsTheEnginesOwn(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	at := n.waiter.Committed()
	above := statelog.Position{Stream: at.Stream, Generation: at.Generation, Seq: at.Seq + 1}
	for name, q := range map[string]code.ReadQuery{
		"an asker":                 {Asker: asker, Introduced: &code.IntroducedQuery{RepoID: id}},
		"no range":                 {},
		"no repository":            {Introduced: &code.IntroducedQuery{}},
		"a name beside the id":     {Repo: "app", Introduced: &code.IntroducedQuery{RepoID: id}},
		"descending with no floor": {Introduced: &code.IntroducedQuery{RepoID: id, Descending: true}},
		"descending with a range": {Introduced: &code.IntroducedQuery{RepoID: id, Descending: true,
			Before: above, After: at}},
		"ascending below":       {Introduced: &code.IntroducedQuery{RepoID: id, Before: above}},
		"a range ending early":  {Introduced: &code.IntroducedQuery{RepoID: id, After: above, Through: at}},
		"a position elsewhere":  {Introduced: &code.IntroducedQuery{RepoID: id, After: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 1}}},
		"a limit past its page": {Introduced: &code.IntroducedQuery{RepoID: id}, Limit: code.MaxSnapshotItems + 1},
	} {
		q.Kind, q.Fresh = code.ReadIntroduced, stale
		if _, err := n.reader.Read(t.Context(), q); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: Read(introduced) = %v, want ErrInvalid", name, err)
		}
	}
	for name, fresh := range map[string]statelog.Freshness{
		"no level":                {},
		"a session with no floor": {Level: statelog.ReadSession},
	} {
		_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadIntroduced,
			Introduced: &code.IntroducedQuery{RepoID: id}, Fresh: fresh})
		if !errors.Is(err, code.ErrBadLevel) {
			t.Errorf("%s: Read(introduced) = %v, want ErrBadLevel", name, err)
		}
	}
	_, err := n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadIntroduced,
		Introduced: &code.IntroducedQuery{RepoID: "no-such-repository"}, Fresh: stale})
	if !errors.Is(err, code.ErrRepoNotFound) {
		t.Errorf("an introduced read of a repository with no row = %v, want ErrRepoNotFound", err)
	}
	_, err = n.reader.Read(t.Context(), code.ReadQuery{Kind: code.ReadIntroduced,
		Introduced: &code.IntroducedQuery{RepoID: id}, Cursor: "not a cursor", Fresh: stale})
	if !errors.Is(err, code.ErrBadCursor) {
		t.Errorf("an introduced read after a cursor it never issued = %v, want ErrBadCursor", err)
	}
	// AND THE OTHER WAY: a principal's read at session is still refused.
	_, err = n.reader.Read(t.Context(), code.ReadQuery{Asker: asker, Kind: code.ReadPushes, Repo: "app",
		Fresh: statelog.FloorAt(statelog.ReadSession, at)})
	if !errors.Is(err, code.ErrBadLevel) || !strings.Contains(err.Error(), "linearizable, stale and consistent_prefix") {
		t.Errorf("a principal's read at session = %v, want ErrBadLevel naming the levels it serves", err)
	}
}

// A DELTA SERVES THE PUSHES IT COVERS: asked for its pushes phase, a delta from
// Since serves every push applied in (Since, its position] — here three pushes
// in two records — each ONCE across pages of one row, in log order; never the
// push AT Since, which the round before folded, and never one applied after
// page 1's position, even while the snapshot is read, which the next delta
// serves. A delta not asking serves none, and neither does a full answer,
// whose pushes the backfill reads.
//
// Mutation: the pushes phase reading position >= Since → p1, the push at
// Since, is served again; bounded by each page's own checkpoint rather than
// page 1's → p5 is served.
func TestADeltaServesThePushesItCovers(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	since := n.push(id, entry("p1", set("refs/heads/a", code.ZeroOID, oidOf("a")))).Position
	n.push(id, entry("p2", set("refs/heads/b", code.ZeroOID, oidOf("b"))),
		entry("p3", set("refs/heads/c", code.ZeroOID, oidOf("c"))))
	n.push(id, entry("p4", set("refs/heads/d", code.ZeroOID, oidOf("d"))))
	code.SetPageLimits(n.reader, 1, code.MaxPageBytes)

	q := code.StateQuery{RepoID: id, Since: since, Fresh: stale, Pushes: true}
	var pushed []string
	for pages := 0; ; pages++ {
		if pages > 30 {
			t.Fatal("the delta did not end in thirty pages")
		}
		page, err := n.reader.Snapshot(t.Context(), q)
		if err != nil {
			t.Fatalf("Snapshot page %d: %v", pages+1, err)
		}
		if page.Full {
			t.Fatalf("the premise: the snapshot from %v is a delta", since)
		}
		for _, p := range page.Pushes {
			pushed = append(pushed, p.PushID)
		}
		if pages == 0 {
			// A PUSH AFTER PAGE 1, while the snapshot is read: the next
			// delta's.
			n.push(id, entry("p5", set("refs/heads/e", code.ZeroOID, oidOf("e"))))
		}
		if page.Next == nil {
			break
		}
		q.Cursor = page.Next
	}
	if fmt.Sprint(pushed) != "[p2 p3 p4]" {
		t.Fatalf("the delta's pushes phase served %v, want [p2 p3 p4]: each once, in log order, "+
			"above Since and at or below page 1's position", pushed)
	}

	for name, q := range map[string]code.StateQuery{
		"a delta not asking":      {RepoID: id, Since: since, Fresh: stale},
		"a full answer that asks": {RepoID: id, Fresh: stale, Pushes: true},
	} {
		code.SetPageLimits(n.reader, code.MaxSnapshotItems, code.MaxPageBytes)
		page, err := n.reader.Snapshot(t.Context(), q)
		if err != nil || len(page.Pushes) != 0 || page.Next != nil {
			t.Errorf("%s: Snapshot = %d pushes, next %+v, %v; want one page and no pushes", name,
				len(page.Pushes), page.Next, err)
		}
	}
}

// A PAGE OF PUSHES ENDS AT WHAT IT READS: a push row stores every command its
// client sent, a refused one too, and the `introduced` read and a delta's
// pushes phase send each push condensed to the refs it LANDED — so a push of
// one branch beside sixty refused refs of the longest a push records is a few
// hundred bytes as sent and some 24 KB as read. Each page ends at its byte
// budget counting the commands it read as well as the rows it sends, about
// three such pushes a page here, and paging still serves every push once and
// in log order: ascending, descending and in a delta.
//
// Mutation: either read counting a push by the row it sends alone → its first
// page serves every push, reading all of their commands for one page's budget.
func TestAPageOfPushesEndsAtWhatItReads(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	since := n.push(id, entry("p00", set("refs/heads/p00", code.ZeroOID, oidOf("p00")))).Position
	long := strings.Repeat("x", code.MaxRefNameBytes-len("refs/heads/")-len("-00-00"))
	want := []string{"p00"}
	for p := 1; p <= 12; p++ {
		push := fmt.Sprintf("p%02d", p)
		cmds := []code.Command{set("refs/heads/"+push, code.ZeroOID, oidOf(push))}
		for c := range 60 {
			cmds = append(cmds, code.Command{Ref: fmt.Sprintf("refs/heads/%s-%02d-%02d", long, p, c),
				Old: code.ZeroOID, New: oidOf(fmt.Sprint(push, c)), Result: code.ResultNG,
				Reason: code.RefusalNotPermitted})
		}
		n.push(id, entry(push, cmds...))
		want = append(want, push)
	}

	// WHAT EACH PUSH READS: the commands its row stores, every one.
	read := map[string]int{}
	if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT push_id, commands FROM code_pushes WHERE repo_id = ?`, id)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var push, commands string
			if err := rows.Scan(&push, &commands); err != nil {
				return err
			}
			read[push] = len(commands)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the pushes' stored commands: %v", err)
	}
	// A BUDGET OF THREE WIDE PUSHES AS READ, and of every push as sent.
	budget := 3 * read["p01"]
	code.SetPageLimits(n.reader, code.MaxSnapshotItems, budget)

	// held holds one page to its budget: what it READ — a push's stored
	// commands, any other row what it sends — and what it SENT, each at most
	// the budget and one row.
	type served struct{ sent, read int }
	held := func(what string, page []served) {
		t.Helper()
		var sent, read, row int
		for _, s := range page {
			sent, read, row = sent+s.sent, read+s.read, max(row, s.sent, s.read)
		}
		if read > budget+row || sent > budget+row {
			t.Fatalf("%s read %d bytes and sent %d, past its %d-byte budget and one row (%d)", what, read,
				sent, budget, row)
		}
	}
	sizeOf := func(v any) int {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}

	// introduced reads every page of in, holding each, and answers the pushes
	// in the order they were served and the bytes they were sent in.
	introduced := func(in code.IntroducedQuery) ([]string, int) {
		t.Helper()
		var got []string
		var sent int
		q := code.ReadQuery{Kind: code.ReadIntroduced, Introduced: &in, Fresh: stale}
		for pages := 1; ; pages++ {
			if pages > 30 {
				t.Fatalf("the introduced read %+v did not end in thirty pages", in)
			}
			answer, err := n.reader.Read(t.Context(), q)
			if err != nil {
				t.Fatalf("Read(introduced %+v) page %d: %v", in, pages, err)
			}
			var page []served
			for _, row := range answer.Introduced {
				page = append(page, served{sent: sizeOf(row), read: read[row.PushID]})
				sent += sizeOf(row)
				got = append(got, row.PushID)
			}
			held(fmt.Sprintf("introduced page %d", pages), page)
			if answer.Next == "" {
				return got, sent
			}
			q.Cursor = answer.Next
		}
	}
	up, sent := introduced(code.IntroducedQuery{RepoID: id})
	if fmt.Sprint(up) != fmt.Sprint(want) {
		t.Fatalf("ascending reads %v, want %v: each push once, in log order", up, want)
	}
	if sent >= budget {
		t.Fatalf("the premise: every push as sent (%d bytes) fits one %d-byte page, so only what a page "+
			"reads can end one before the last push", sent, budget)
	}
	at := n.waiter.Committed()
	down, _ := introduced(code.IntroducedQuery{RepoID: id, Descending: true,
		Before: statelog.Position{Stream: at.Stream, Generation: at.Generation, Seq: at.Seq + 1}})
	reversed := slices.Clone(want)
	slices.Reverse(reversed)
	if fmt.Sprint(down) != fmt.Sprint(reversed) {
		t.Fatalf("descending reads %v, want %v: each push once, newest first", down, reversed)
	}

	// A DELTA'S PUSHES PHASE shares its page with the packs and refs before it.
	q := code.StateQuery{RepoID: id, Since: since, Fresh: stale, Pushes: true}
	var pushed []string
	for pages := 1; ; pages++ {
		if pages > 30 {
			t.Fatal("the delta did not end in thirty pages")
		}
		page, err := n.reader.Snapshot(t.Context(), q)
		if err != nil {
			t.Fatalf("Snapshot page %d: %v", pages, err)
		}
		if page.Full {
			t.Fatalf("the premise: the snapshot from %v is a delta", since)
		}
		var rows []served
		for _, k := range page.Packs {
			rows = append(rows, served{sent: sizeOf(k), read: sizeOf(k)})
		}
		for _, r := range page.Refs {
			rows = append(rows, served{sent: sizeOf(r), read: sizeOf(r)})
		}
		for _, p := range page.Pushes {
			rows = append(rows, served{sent: sizeOf(p), read: read[p.PushID]})
			pushed = append(pushed, p.PushID)
		}
		held(fmt.Sprintf("snapshot page %d", pages), rows)
		if page.Next == nil {
			break
		}
		q.Cursor = page.Next
	}
	if fmt.Sprint(pushed) != fmt.Sprint(want[1:]) {
		t.Fatalf("the delta's pushes phase served %v, want %v: each once, in log order, above Since",
			pushed, want[1:])
	}
}
