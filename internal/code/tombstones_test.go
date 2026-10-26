package code_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// A REPOSITORY'S TOMBSTONES ARE BOUNDED APART FROM ITS LIVE REFS, by the
// applier alone: past MaxTombstonesPerRepo the oldest are forgotten before the
// horizon, and a record whose deletions its sweep cannot make room for writes
// them as removals — so the count holds after EVERY record, and no client is
// ever refused for a deletion. Each case reads the repository back through
// the reader, because what forgetting a deletion costs is a cache answered in
// full, and only the reader over a real checkpoint says whether it is.
//
// The cases run on the whole path's harness rather than horizon_test.go's
// estate, which writes no checkpoint for a snapshot to be read at.

// seedTombstones writes n fresh tombstones of repoID straight into the node's
// estate, the i-th named gone-<i> and deleted at first plus i microseconds —
// oldest first in the order a sweep takes them — at position at, except the
// one at index odd, which is at position oddAt; and counts them.
func (n *node) seedTombstones(repoID string, count int, first time.Time, at statelog.Position,
	odd int, oddAt statelog.Position) {

	n.t.Helper()
	n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/gone-%06d', i), '', CASE WHEN i = ? THEN ? ELSE ? END, ? + i FROM k`,
		count, repoID, odd, oddAt.Packed(), at.Packed(), store.EncodeTime(first))
	n.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, count, repoID)
}

// seedLive writes the live refs of names straight into the node's estate, each
// at the object oidOf(name) and at position at, and counts them.
func (n *node) seedLive(repoID string, names []string, at statelog.Position) {
	n.t.Helper()
	ctx := n.t.Context()
	if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, n.db.Caps().MaxVariables,
			`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES `, `(?, ?, ?, ?, 0)`, ``,
			len(names), func(i int) []any { return []any{repoID, names[i], oidOf(names[i]), at.Packed()} })
		return err
	}); err != nil {
		n.t.Fatalf("seed %s's live refs: %v", repoID, err)
	}
	n.exec(`UPDATE code_repos SET refs = refs + ?, branches = branches + ? WHERE id = ?`,
		len(names), len(names), repoID)
}

// tombstoneCountersMatchRows holds the counts code_repos keeps of a
// repository's refs to a COUNT of the rows themselves.
func (n *node) tombstoneCountersMatchRows(id string) {
	n.t.Helper()
	for counter, where := range map[string]string{
		"refs":       `target != ''`,
		"branches":   `target != '' AND substr(ref, 1, 11) = 'refs/heads/'`,
		"tombstones": `target = ''`,
	} {
		kept := n.count(`SELECT `+counter+` FROM code_repos WHERE id = ?`, id)
		rows := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND `+where, id)
		if kept != rows {
			n.t.Errorf("%s counts %d %s and its refs hold %d", id, kept, counter, rows)
		}
	}
}

// through is the repository's tombstone mark.
func (n *node) through(id string) int64 {
	n.t.Helper()
	return int64(n.count(`SELECT tombstones_through FROM code_repos WHERE id = ?`, id))
}

// TOMBSTONES PAST THEIR BOUND ARE SWEPT OLDEST FIRST, before the horizon: a
// repository holding k more fresh tombstones than MaxTombstonesPerRepo loses
// exactly its k oldest to the next record that touches it, and the mark rises
// to the highest position among them, so a cache synced below it is answered
// in full rather than told a delta missing their deletions.
//
// The cache asked about is synced at the creation, where every tombstone but
// one was written: its delta is a handful of rows beside a full answer of the
// repository's live refs, so a full answer to it is the MARK's doing, never
// the size rule's (deltaOutweighs).
//
// Mutation: sweep by age only, and nothing is swept — the count stays k past
// its bound and the delta from below the mark is still a delta.
func TestTombstonesPastTheirBoundAreSweptOldestFirst(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	created := n.waiter.Committed()
	n.push(id, entry("p0", set("refs/heads/main", code.ZeroOID, oidOf("c0"))))
	pushed := n.waiter.Committed()
	// EVERY RECORD IS STORED AT ONE INSTANT, so the seeded deletions are an
	// hour old and fresh whatever the machine's clock does meanwhile.
	now := time.Now().UTC().Truncate(time.Microsecond)
	n.storedAt = func(uint64, time.Time) time.Time { return now }

	// The k oldest are at the creation's position but one, at the push's:
	// the highest swept position is that one's, and every tombstone kept is
	// at the creation. The live refs beside main make the full answer larger
	// than any delta from the creation.
	const k = 300
	first := now.Add(-time.Hour)
	n.seedTombstones(id, code.MaxTombstonesPerRepo+k, first, created, k/2, pushed)
	var live []string
	for i := range 10 {
		live = append(live, fmt.Sprintf("refs/heads/live-%d", i))
	}
	n.seedLive(id, live, created)
	below := created
	if pages := snapshot(t, n.reader, id, below); pages[0].Full {
		t.Fatal("the premise: a delta from the creation is a delta before any tombstone is swept")
	}

	n.push(id, entry("p1", set("refs/heads/main", oidOf("c0"), oidOf("c1"))))

	if stones := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`, id); stones != code.MaxTombstonesPerRepo {
		t.Fatalf("the repository holds %d tombstones after the push, want MaxTombstonesPerRepo "+
			"(%d): the %d past it swept", stones, code.MaxTombstonesPerRepo, k)
	}
	oldest := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = '' AND at < ?`,
		id, store.EncodeTime(first.Add(k*time.Microsecond)))
	if oldest != 0 {
		t.Fatalf("%d of the %d oldest tombstones are kept — the sweep takes the oldest first", oldest, k)
	}
	if mark := n.through(id); mark != pushed.Packed() {
		t.Fatalf("the mark is %d, want %d — the highest position a swept tombstone held", mark,
			pushed.Packed())
	}
	n.tombstoneCountersMatchRows(id)
	if pages := snapshot(t, n.reader, id, below); !pages[0].Full {
		t.Fatal("a delta from below a swept tombstone was answered as a delta, missing its deletion")
	}
	if pages := snapshot(t, n.reader, id, pushed); pages[0].Full {
		t.Fatal("a delta from at the mark was answered in full")
	}
}

// THE TOMBSTONE COUNT NEVER PASSES ITS BOUND, after any record — one no
// committer of this build forms included, since the applier applies whatever
// the log holds, a newer build's records among it: records of three pushes
// inside the horizon, each push deleting the 500 refs the previous record's
// push made and making 500 more, make 1 500 tombstones a record against a
// sweep of no rows at all (three such pushes write 4 507 rows, the objects
// their deletions left among them, past MaxRecordRows — their deletions this
// build's committer counts three times, so it never puts two such pushes in
// one record). So every such record that would leave the count past its
// bound writes its deletions as REMOVALS — no row left — and raises the mark
// to its own position, which answers a cache synced below it in full.
//
// Mutation: drop the removal, and the count passes the bound at the first
// record by what the sweep could not take.
func TestTheTombstoneCountNeverPassesItsBound(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	id := seededRepo(t, n, "app")
	seeded := n.waiter.Committed()
	now := time.Now().UTC().Truncate(time.Microsecond)
	n.storedAt = func(uint64, time.Time) time.Time { return now }

	const pushes, each, records = 3, 500, 4
	name := func(record, push, i int) string {
		return fmt.Sprintf("refs/heads/gen-%d-%d-%03d", record, push, i)
	}
	var made []string
	for p := range pushes {
		for i := range each {
			made = append(made, name(0, p, i))
		}
	}
	n.seedTombstones(id, code.MaxTombstonesPerRepo-1_000, now.Add(-time.Hour), seeded, -1, seeded)
	n.seedLive(id, made, seeded)

	for record := 1; record <= records; record++ {
		var entries []code.PushEntry
		for p := range pushes {
			var cmds []code.Command
			for i := range each {
				gone := name(record-1, p, i)
				cmds = append(cmds, set(gone, oidOf(gone), code.ZeroOID))
				fresh := name(record, p, i)
				cmds = append(cmds, set(fresh, code.ZeroOID, oidOf(fresh)))
			}
			entries = append(entries, entry(fmt.Sprintf("r%d-p%d", record, p), cmds...))
		}
		below := n.waiter.Committed()
		res := n.push(id, entries...)

		stones := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`, id)
		if stones > code.MaxTombstonesPerRepo {
			t.Fatalf("after record %d the repository holds %d tombstones, past "+
				"MaxTombstonesPerRepo (%d)", record, stones, code.MaxTombstonesPerRepo)
		}
		if live := n.count(`SELECT refs FROM code_repos WHERE id = ?`, id); live != pushes*each {
			t.Fatalf("after record %d the repository counts %d live refs, want %d", record, live,
				pushes*each)
		}
		n.tombstoneCountersMatchRows(id)
		if record < 2 {
			continue
		}
		left := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND ref LIKE ?`, id,
			fmt.Sprintf("refs/heads/gen-%d-%%", record-1))
		if left != 0 {
			t.Fatalf("record %d deleted %d refs its sweep had no room for, and %d of them keep a "+
				"row — each is a removal", record, pushes*each, left)
		}
		if mark := n.through(id); mark != res.Position.Packed() {
			t.Fatalf("after record %d the mark is %d, want the record's own position %d",
				record, mark, res.Position.Packed())
		}
		if pages := snapshot(t, n.reader, id, below); !pages[0].Full {
			t.Fatalf("a delta from below record %d's removals was answered as a delta, "+
				"missing its deletions", record)
		}
	}
}

// A RECORD THE COMMITTER FORMS NEVER REMOVES ITS OWN DELETIONS: the committer
// counts every deletion a push carries three times in the record's rows — its
// ref row, the old tombstone its new one may need swept, and the object it
// left (code.RowBound) — so whatever the tombstones hold, a record's sweep has
// room for every tombstone it makes, and the removal is left to records no
// committer of this build forms. Removing is what raises the mark to the
// record's own position, which sends every cache synced before the record —
// every git node holding the repository — to read it whole.
//
// Three pushes of MaxCommandsPerPush deletions and two of 900 creations,
// queued behind one in flight at a repository holding MaxTombstonesPerRepo
// fresh tombstones: the last deletion push shares a record with the first
// creation push, whose sweep has 1 096 rows for its 1 000 new tombstones.
// Counted without the sweep's row, a deletion shares one with both creation
// pushes, whose sweep has 195; counted without its drop's, the same; and
// counted as its ref alone, the three deletion pushes and a creation push
// share one record of 6 905 rows whose sweep has none.
//
// Mutation: count a deletion without the sweep's row, without its drop's, or
// as its ref alone in rowBound, and a record's deletions are removals — no
// tombstone left for them, the mark at the record's own position.
func TestACommittedRecordNeverRemovesItsOwnDeletions(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	seeded := n.waiter.Committed()
	now := time.Now().UTC().Truncate(time.Microsecond)
	n.storedAt = func(uint64, time.Time) time.Time { return now }
	const pushes, creates, creating = 3, 900, 2
	name := func(p, i int) string { return fmt.Sprintf("refs/heads/stale-%d-%04d", p, i) }
	var stale []string
	for p := range pushes {
		for i := range code.MaxCommandsPerPush {
			stale = append(stale, name(p, i))
		}
	}
	n.seedLive(repo, stale, seeded)
	n.seedTombstones(repo, code.MaxTombstonesPerRepo, now.Add(-time.Hour), seeded, -1, seeded)

	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var queued []<-chan answer
	enqueue := func(cmds []code.Command) {
		queued = append(queued, proposeAsync(t.Context(), c, proposal(repo, cmds...)))
		want := len(queued)
		eventually(t, "the proposal queued", func() bool { return code.CommitterQueued(c, repo) == want })
	}
	for p := range pushes {
		var cmds []code.Command
		for i := range code.MaxCommandsPerPush {
			cmds = append(cmds, update(name(p, i), oidOf(name(p, i)), code.ZeroOID))
		}
		enqueue(cmds)
	}
	for p := range creating {
		var cmds []code.Command
		for i := range creates {
			ref := fmt.Sprintf("refs/heads/fresh-%d-%04d", p, i)
			cmds = append(cmds, update(ref, code.ZeroOID, oidOf(ref)))
		}
		enqueue(cmds)
	}
	gate.open()
	requireOK(t, await(t, lead))
	first := statelog.Position{}
	for _, ch := range queued {
		a := await(t, ch)
		requireOK(t, a)
		if first.IsZero() || a.res.Position.Packed() < first.Packed() {
			first = a.res.Position
		}
	}

	kept := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = '' AND ref LIKE 'refs/heads/stale-%'`, repo)
	if kept != len(stale) {
		t.Fatalf("%d of the %d deletions keep a tombstone — a record the committer formed wrote "+
			"the rest as removals", kept, len(stale))
	}
	if stones := n.count(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`, repo); stones != code.MaxTombstonesPerRepo {
		t.Fatalf("the repository holds %d tombstones, want MaxTombstonesPerRepo (%d)", stones,
			code.MaxTombstonesPerRepo)
	}
	if mark := n.through(repo); mark >= first.Packed() {
		t.Fatalf("the mark is %d, at or past the first deletion record's position %d — only a "+
			"removal raises it there", mark, first.Packed())
	}
	n.tombstoneCountersMatchRows(repo)
}
