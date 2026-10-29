package code

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// quiet keeps a benchmark's output to its figures: every estate a benchmark
// opens logs at info, and an info line in the middle of benchmark output is a
// line benchstat cannot read (`make bench` runs this package).
func quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}

// widestPushRow is the commands and introduced ids of the widest row a push
// stores: MaxCommandsPerPush commands at MaxRefNameBytes refs, each refused
// with the longest code a command records, and MaxIntroduced ids — about
// 470 KB, which is what makes code_pushes the domain's widest table, some
// fourteen times a change's body at MaxChangeBodyBytes.
func widestPushRow(b *testing.B) (commands, introduced string) {
	b.Helper()
	longest := Refusal("")
	for _, r := range Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	cmds := make([]Command, MaxCommandsPerPush)
	for c := range cmds {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		cmds[c] = Command{Ref: ref + strings.Repeat("x", MaxRefNameBytes-len(ref)),
			Old: oid(fmt.Sprint("old", c)), New: oid(fmt.Sprint("new", c)), Forced: true,
			Result: ResultNG, Reason: longest}
	}
	commands, err := recordedCommands(cmds)
	if err != nil {
		b.Fatal(err)
	}
	ids := make([]string, MaxIntroduced)
	for i := range ids {
		ids[i] = oid(fmt.Sprint("introduced", i))
	}
	return commands, strings.Join(ids, " ")
}

// seedWide writes n push rows of a repository at the widest a row can be,
// stamped at the epoch — expired by decades, so the history horizon may take
// them, and oldest first, so a purge takes them first.
func (e *estate) seedWide(b *testing.B, id string, n int) {
	b.Helper()
	commands, introduced := widestPushRow(b)
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := store.InsertRows(ctx, tx, e.db.Caps().MaxVariables,
			`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
				introduced, introduced_count, received_at, row_bytes) VALUES `,
			`(?, ?, ?, ?, 'seed', 'operator', 'git', ?, ?, ?, 0, ?)`, ``, n, func(i int) []any {
				return []any{fmt.Sprintf("%s-wide-%04d", id, i), id, i + 1, i, commands, introduced,
					MaxIntroduced, len(commands) + len(introduced)}
			})
		return err
	}); err != nil {
		b.Fatalf("seed %s's wide pushes: %v", id, err)
	}
}

// seedNarrow writes n push rows of a repository of about a kilobyte each,
// stamped at the epoch — the history of a repository whose pushes move a few
// refs each.
func (e *estate) seedNarrow(id string, n int) {
	e.t.Helper()
	commands := `[` + strings.Repeat(`{"ref":"refs/heads/main","result":"ok"},`, 25) +
		`{"ref":"refs/heads/main","result":"ok"}]`
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, via, commands,
			received_at, row_bytes)
		SELECT printf('%s-narrow-%06d', ?, i), ?, i + 1, i, 'seed', 'operator', 'git', ?, 0, ? FROM k`,
		n, id, id, commands, len(commands))
}

// seedChangeRows writes changes of a repository, numbered from 1, each with
// revisions revisions, authors authors and, where body is not 0, a body of
// that many bytes, which its change weighs — the rows a purge part walks.
func (e *estate) seedChangeRows(id string, changes, revisions, authors, body int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref, target_ref,
			head_oid, revision, owner, owner_kind, opened_at, opened_position, version, position)
		SELECT printf('%s-change-%06d', ?, i), ?, i + 1, 'a change', ?, 'open',
			printf('refs/heads/change-%06d', i), 'refs/heads/main', printf('%040d', i), ?, 'seat', 'agent',
			0, i + 1, 1, i + 1 FROM k`, changes, id, id, body, revisions)
	if body > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_change_bodies (change_id, body)
			SELECT printf('%s-change-%06d', ?, i), ? FROM k`, changes, id, strings.Repeat("b", body))
	}
	if revisions > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_revisions (change_id, n, head_oid, pusher, pusher_kind, position, at)
			SELECT printf('%s-change-%06d', ?, i / ?), i % ? + 1, printf('%040d', i), 'seat', 'agent', 1, 0
			FROM k`, changes*revisions, id, revisions, revisions)
	}
	if authors > 0 {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
			SELECT printf('%s-change-%06d', ?, i / ?), printf('seat-%05d', i % ?), 'agent', 1, 1 FROM k`,
			changes*authors, id, authors, authors)
	}
}

// seedLiveChanges writes n live changes of a repository at one revision each,
// as seedChangeRows does, and what a live change stands on: its source branch
// (liveSource), live at the change's head (liveHead) and counted, as a push's
// apply counts a branch; and the operator every pushEntry and benchProposal
// names ("op") as one of its authors, as a bot that pushed the changes it now
// pushes again is — so a push moving one costs its record the change's row and
// its revision, and no author: the most changes one record can move.
func (e *estate) seedLiveChanges(id string, n int) {
	e.t.Helper()
	e.seedChangeRows(id, n, 1, 0, 0)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/change-%06d', i), printf('%040d', i), 1, 0 FROM k`, n, id)
	e.exec(`UPDATE code_repos SET refs = refs + ?, branches = branches + ? WHERE id = ?`, n, n, id)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_change_authors (change_id, author, author_kind, first_revision, position)
		SELECT printf('%s-change-%06d', ?, i), 'op', 'person', 1, 1 FROM k`, n, id)
}

// liveSource is the source branch of the i-th change seedLiveChanges seeds,
// counted from 0, and liveHead its head, the commit that branch is at.
func liveSource(i int) string { return fmt.Sprintf("refs/heads/change-%06d", i) }
func liveHead(i int) string   { return fmt.Sprintf("%040d", i) }

// rebasingChanges is the shape of the record that moves the most live
// changes: heads, a push of MaxCommandsPerPush fast-forwards of changes'
// sources by a pusher already their author — a ref, and the change's row and
// its revision, each — and closes, a push deleting the sources of as many
// more changes as the record's rows leave beside it — a ref, the sweep's room
// for its tombstone and the object it left ([commandRows]), and the change's
// row, each. Counted as the push decide counts a record's rows, which
// [estate.decidedRecord] holds the shape to.
func rebasingChanges() (heads, closes int) {
	heads = MaxCommandsPerPush
	moving := PushEntry{Pack: &PackRef{}, Commands: make([]Command, heads)}
	for c := range moving.Commands {
		moving.Commands[c] = move(liveSource(c), liveHead(c), oid(fmt.Sprint("rebased", c)))
	}
	rows := entryRows(moving) + 2*heads
	deleting := PushEntry{Pack: &PackRef{}}
	for {
		deleting.Commands = append(deleting.Commands, move(liveSource(heads+closes), liveHead(heads+closes),
			ZeroOID))
		if recordRows(rows+entryRows(deleting)+len(deleting.Commands)) > MaxRecordRows {
			return heads, closes
		}
		closes++
	}
}

// decidedRecord is the record the push decide forms over e's rows of repo
// from pushes — each proposed as its commands, undecided, with a pack minted
// at the decide's instant — failing b unless it admits every one whole into
// one record within MaxRecordRows, as a committer appends it: so a record a
// benchmark applies is one a committer forms, its change rows counted by the
// decide that holds the record to its bound ([estate.admitted]'s rule, for a
// push). It answers the record's entries and those change rows ([changeBook]).
func (e *estate) decidedRecord(b *testing.B, repo string, pushes []PushEntry) ([]PushEntry, int) {
	b.Helper()
	batch := make([]Proposal, len(pushes))
	for i, p := range pushes {
		cmds := make([]Command, len(p.Commands))
		for c, cmd := range p.Commands {
			cmds[c] = Command{Ref: cmd.Ref, Old: cmd.Old, New: cmd.New, Forced: cmd.Forced}
		}
		batch[i] = withPack(Proposal{PushID: p.PushID, RepoID: repo, Principal: p.Principal, Via: p.Via,
			Node: p.Node, Commands: cmds, ReceivedAt: p.ReceivedAt}, p.Pack.Size, applyClock)
	}
	var (
		entries    []PushEntry
		answers    []decided
		changeRows int
	)
	ctx := context.Background()
	if err := e.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		entries, answers, changeRows, _, err = decideBatch(ctx, tx, repo, batch, DefaultLimits(),
			DefaultDeletedRetention, SeatRules{}, applyClock, &verdicts{})
		return err
	}); err != nil {
		b.Fatalf("decide the record: %v", err)
	}
	for i, a := range answers {
		if !a.inRecord || slices.ContainsFunc(a.commands, func(c Command) bool { return c.Result != ResultOK }) {
			b.Fatalf("push %d of %d is not in the record whole (carried %t, refused %q: %s)", i, len(batch),
				a.carried, a.refused, a.detail)
		}
	}
	rows := changeRows
	for _, entry := range entries {
		rows += entryRows(entry)
	}
	if recordRows(rows) > MaxRecordRows {
		b.Fatalf("the decide formed a record of %d rows, past MaxRecordRows (%d)", recordRows(rows), MaxRecordRows)
	}
	return entries, changeRows
}

// changeRowsOf is every row of a repository's changes seedChangeRows wrote
// that the estate still holds, revisions, authors and bodies found by their
// ids.
func (e *estate) changeRowsOf(id string) int64 {
	e.t.Helper()
	prefix := id + "-change-%"
	return e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, id) +
		e.num(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix) +
		e.num(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix) +
		e.num(`SELECT COUNT(*) FROM code_change_bodies WHERE change_id LIKE ?`, prefix)
}

// seedTombstones writes n ref tombstones into a repository, deleted at, and
// counts them, as a sweep finds them.
func (e *estate) seedTombstones(id string, n int, at time.Time) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/gone-%06d', i), '', 1, ? FROM k`, n, id, store.EncodeTime(at))
	e.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, n, id)
}

// applyTimed applies one record into the estate inside the benchmark's timer
// — Apply and the commit, which is the time it holds the write lane — and
// holds the rows it reported to at least rows.
func (e *estate) applyTimed(b *testing.B, a *Applier, r built, rows int) {
	b.Helper()
	ctx := context.Background()
	env, err := Domain{}.Envelope(r.payload)
	if err != nil {
		b.Fatal(err)
	}
	record := statelog.Record{Envelope: env, Payload: r.payload, StoredAt: applyClock,
		Position: statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: e.next}}
	e.next++
	opts := statelog.ApplyOptions{StoredAt: applyClock, MaxVariables: e.db.Caps().MaxVariables}
	b.StartTimer()
	err = e.db.Tx(ctx, func(tx *sql.Tx) error {
		n, err := a.Apply(ctx, tx, record, opts)
		if err == nil && n < rows {
			err = fmt.Errorf("the record wrote %d rows, want at least %d", n, rows)
		}
		return err
	})
	b.StopTimer()
	if err != nil {
		b.Fatal(err)
	}
}

// fullest is the pushes of the record the committer forms with the most
// commands of one kind: pushes of MaxCommandsPerPush commands, each with its
// pack, and a last push of whatever room the record's row bound leaves —
// held, by [rowBound] and [recordRows] themselves, to fill the bound exactly,
// so a shape priced or applied here is one the committer admits and the
// largest of its kind. command is the c-th command of push p.
func fullest(tb testing.TB, prefix string, i int, command func(p, c int) Command) []PushEntry {
	tb.Helper()
	var entries []PushEntry
	var rows int
	for p := 0; ; p++ {
		e := pushEntry(fmt.Sprint(prefix, "-", i, "-", p))
		for c := 0; c < MaxCommandsPerPush; c++ {
			e.Commands = append(e.Commands, command(p, c))
			if recordRows(rows+rowBound(Proposal{Commands: e.Commands, Pack: e.Pack})) > MaxRecordRows {
				e.Commands = e.Commands[:c]
				break
			}
		}
		if len(e.Commands) == 0 {
			break
		}
		entries = append(entries, e)
		rows += rowBound(Proposal{Commands: e.Commands, Pack: e.Pack})
	}
	if recordRows(rows) != MaxRecordRows {
		tb.Fatalf("the fullest record of %s bounds %d rows, not MaxRecordRows (%d)", prefix,
			recordRows(rows), MaxRecordRows)
	}
	return entries
}

// BenchmarkApplyAPushRecord is a push record APPLIED AND COMMITTED — the time
// it holds the replicated estate's one write lane, which every other domain's
// applier on the node waits behind — into a repository of its own each
// iteration, set up outside the timer: one holding 10 000 refs, or, for the
// sweeps, one holding expired tombstones. Its shapes are the records the
// committer forms and the ones the horizons make longest:
//
//   - maximal: the most ref creations a record the committer forms carries —
//     three pushes of MaxCommandsPerPush and a fourth of the 926 the row
//     bound leaves, 3 935 rows — with nothing left for the sweep, since it
//     makes no tombstone;
//   - maximal+history: that, with HorizonDeletesPerRecord expired push rows
//     waiting, each at the widest a row can be — the history's removal is
//     bounded by HorizonBytesPerRecord and takes three of them;
//   - group: MaxPushesPerRecord pushes of one command each, what group commit
//     turns a CI's burst of tag pushes into;
//   - one: one push of one command, the common case;
//   - sweep: one push of one command whose record sweeps every expired
//     tombstone its row budget leaves room for — 3 995;
//   - deletions: the most ref deletions a record the committer forms carries
//     — a push of MaxCommandsPerPush and one of the 310 left, each deletion
//     counted three times in the bound (rowBound: its tombstone, the sweep's
//     room for one and the object it left) — at a repository holding
//     MaxTombstonesPerRepo expired tombstones, so the record writes 1 310
//     tombstones and as many ref drops and sweeps the 1 374 its rows leave:
//     the most rows a record can both write and delete;
//   - deletions+history: that, with the widest expired push rows waiting too
//     — the longest shape of a record that moves no live change;
//   - removal: three pushes of MaxCommandsPerPush ref deletions at a
//     repository already holding MaxTombstonesPerRepo fresh tombstones, so
//     the record writes its deletions as removals and the objects they left
//     as 3 000 ref drops — a record no committer of this build forms (it
//     counts each deletion three times, and two such pushes never share a
//     record), whose cost the applier still bounds;
//   - expiry: one push of one command at a repository holding MaxRecordRows
//     ref drops past their grace, whose record forgets every one its row
//     budget leaves room for — 3 996, the drops' counterpart of sweep;
//   - forced: one push of MaxCommandsPerPush forced updates at a repository
//     holding MaxRecordRows ref drops past their grace, so the record writes
//     a thousand refs and as many drops and forgets the 1 997 its rows leave
//     — the most drops a record both writes and forgets;
//   - changes: the most live changes one record moves — a push moving the
//     heads of MaxCommandsPerPush of them by a pusher already their author,
//     which costs each change its row and its revision beside its ref, then
//     one deleting the sources of as many more as the record's rows leave,
//     which closes them (rebasingChanges) — 1 232 changes, each row
//     rewritten, the bulk rebase a bot's force-push of every change it
//     rebased makes;
//   - group+changes: MaxPushesPerRecord pushes of one command each, each
//     moving the head of a live change of its own — group commit, where
//     every push is a change's next revision.
//
// The two change arms are DECIDED over the rows before they are applied
// ([estate.decidedRecord]), since what a record writes about changes is what
// the repository holds: so each is a record the committer forms, its change
// rows counted by the decide that holds it to MaxRecordRows.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which a record
// is checked against only at its boundary; doc.go's measured section records
// the figures.
func BenchmarkApplyAPushRecord(b *testing.B) {
	quiet(b)
	// A PUSH ID IS THE FLEET'S, not the repository's, so every iteration's
	// pushes carry their own: a repeated one is a redelivery and applies as
	// nothing.
	maximal := func(i int) []PushEntry {
		return fullest(b, "max", i, func(p, c int) Command {
			return move(fmt.Sprintf("refs/heads/new/%d/%04d", p, c), ZeroOID, oid(fmt.Sprint(p, c)))
		})
	}
	// THE DELETIONS OF e.seed's refs, as many as the committer puts in one
	// record.
	deleting := func(i int) []PushEntry {
		return fullest(b, "deleting", i, func(p, c int) Command {
			return move(fmt.Sprintf("refs/heads/r%06d", p*MaxCommandsPerPush+c), oid("seeded"), ZeroOID)
		})
	}
	group := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range MaxPushesPerRecord {
			entries = append(entries, pushEntry(fmt.Sprint("tag-", i, "-", p),
				move(fmt.Sprintf("refs/tags/build-%02d", p), ZeroOID, oid(fmt.Sprint(p)))))
		}
		return entries
	}
	one := func(i int) []PushEntry {
		return []PushEntry{pushEntry(fmt.Sprint("one-", i), move("refs/heads/main", ZeroOID, oid("main")))}
	}
	// THE FORCED UPDATES OF e.seed's refs, one maximal push of them.
	forcing := func(i int) []PushEntry {
		var cmds []Command
		for c := range MaxCommandsPerPush {
			cmds = append(cmds, forced(fmt.Sprintf("refs/heads/r%06d", c), oid("seeded"),
				oid(fmt.Sprint("forced", c))))
		}
		return []PushEntry{pushEntry(fmt.Sprint("forced-", i), cmds...)}
	}
	// THE DELETIONS OF e.seed's refs, three maximal pushes of them, which no
	// committer of this build puts in one record.
	removing := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range 3 {
			var cmds []Command
			for c := range MaxCommandsPerPush {
				cmds = append(cmds, move(fmt.Sprintf("refs/heads/r%06d", p*MaxCommandsPerPush+c),
					oid("seeded"), ZeroOID))
			}
			entries = append(entries, pushEntry(fmt.Sprint("del-", i, "-", p), cmds...))
		}
		return entries
	}
	// A RECORD'S WORTH OF LIVE CHANGES, rebasingChanges' shape, and every
	// change it moves seeded live; and a group of pushes each moving one.
	heads, closes := rebasingChanges()
	rebasing := func(i int) []PushEntry {
		moves := make([]Command, heads)
		for c := range moves {
			moves[c] = move(liveSource(c), liveHead(c), oid(fmt.Sprint("rebased", c)))
		}
		deletions := make([]Command, closes)
		for c := range deletions {
			deletions[c] = move(liveSource(heads+c), liveHead(heads+c), ZeroOID)
		}
		return []PushEntry{pushEntry(fmt.Sprint("rebase-", i, "-heads"), moves...),
			pushEntry(fmt.Sprint("rebase-", i, "-closes"), deletions...)}
	}
	revising := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range MaxPushesPerRecord {
			entries = append(entries, pushEntry(fmt.Sprint("revise-", i, "-", p),
				move(liveSource(p), liveHead(p), oid(fmt.Sprint("revised", p)))))
		}
		return entries
	}
	liveAt := func(changes int) func(e *estate, repo string) {
		return func(e *estate, repo string) {
			e.seed(repo, 0, 10_000, 0)
			e.seedLiveChanges(repo, changes)
		}
	}
	expired := time.Unix(0, 0)
	// THE ROWS EACH SHAPE MUST TOUCH, from the entries themselves: a ref,
	// a pack and a push row an entry, and the counters.
	written := func(entries []PushEntry) int {
		n := 1
		for _, e := range entries {
			n += len(e.Commands) + 2
		}
		return n
	}
	deletionsAt := func(e *estate, repo string) {
		e.seed(repo, 0, 2*MaxCommandsPerPush, 0)
		e.seedTombstones(repo, MaxTombstonesPerRepo, expired)
	}
	for _, shape := range []struct {
		name    string
		entries func(int) []PushEntry
		seed    func(e *estate, repo string)
		history bool
		rows    int
		// decide is a shape decided over the rows before it is applied
		// ([estate.decidedRecord]), whose rows are its entries' and the
		// change rows the decide counted; revised and closed are the
		// changes its record must leave at their second revision and
		// closed.
		decide          bool
		revised, closed int
	}{
		{name: "maximal", entries: maximal, rows: written(maximal(0))},
		{name: "maximal+history", entries: maximal, history: true, rows: written(maximal(0))},
		{name: "group", entries: group, rows: 2 * MaxPushesPerRecord},
		{name: "one", entries: one, rows: 3},
		{name: "sweep", entries: one, seed: func(e *estate, repo string) {
			e.seedTombstones(repo, MaxRecordRows, expired)
		}, rows: MaxRecordRows},
		// EVERY ROW OF THE BUDGET TOUCHED: the deletions' tombstones and
		// their record's own rows, and the sweep taking what they leave.
		{name: "deletions", entries: deleting, seed: deletionsAt, rows: MaxRecordRows},
		{name: "deletions+history", entries: deleting, seed: deletionsAt, history: true, rows: MaxRecordRows},
		// A RECORD THAT REMOVES: every ref it deletes at a repository already
		// holding MaxTombstonesPerRepo fresh tombstones, which no sweep can
		// take, so its 3 000 deletions are removals rather than rows — each
		// still writing the object it left.
		{name: "removal", entries: removing, seed: func(e *estate, repo string) {
			e.seed(repo, 0, 3*MaxCommandsPerPush, 0)
			e.seedTombstones(repo, MaxTombstonesPerRepo, applyClock.Add(-time.Hour))
		}, rows: 3 * (1 + 2*MaxCommandsPerPush + 1)},
		// EVERY ROW OF THE BUDGET TOUCHED BY THE DROPS: the record's own rows
		// and the expiry forgetting what they leave.
		{name: "expiry", entries: one, seed: func(e *estate, repo string) {
			e.seedDrops(repo, 1, expired, MaxRecordRows)
		}, rows: MaxRecordRows},
		{name: "forced", entries: forcing, seed: func(e *estate, repo string) {
			e.seed(repo, 0, MaxCommandsPerPush, 0)
			e.seedDrops(repo, 1, expired, MaxRecordRows)
		}, rows: MaxRecordRows},
		// EVERY CHANGE ROW A RECORD CAN REWRITE: the heads and the closes
		// of rebasingChanges, and a group of revisions.
		{name: "changes", entries: rebasing, seed: liveAt(heads + closes), decide: true, revised: heads,
			closed: closes},
		{name: "group+changes", entries: revising, seed: liveAt(MaxPushesPerRecord), decide: true,
			revised: MaxPushesPerRecord},
	} {
		removal := shape.name == "removal"
		b.Run(shape.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier()
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				if shape.history {
					e.seedWide(b, repo, HorizonDeletesPerRecord)
				}
				if shape.seed != nil {
					shape.seed(e, repo)
				} else {
					e.seed(repo, 0, 10_000, 0)
				}
				entries, rows := shape.entries(i), shape.rows
				if shape.decide {
					var changeRows int
					entries, changeRows = e.decidedRecord(b, repo, entries)
					rows = written(entries) + changeRows
				}
				e.applyTimed(b, a, pushOf(b, repo, entries...), rows)
				// THE CHANGES IT NAMES, each moved as its command says.
				if shape.decide && (e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ? AND revision = 2
					AND state = 'open'`, repo) != int64(shape.revised) || e.num(`SELECT COUNT(*) FROM code_changes
					WHERE repo_id = ? AND state = 'closed'`, repo) != int64(shape.closed)) {
					b.Fatalf("the %s shape's record did not move its %d changes' heads and close %d", shape.name,
						shape.revised, shape.closed)
				}
				// THE SHAPE IT NAMES: every deletion a removal, the count held.
				if removal && (e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''`,
					repo) != MaxTombstonesPerRepo || e.num(`SELECT refs FROM code_repos WHERE id = ?`, repo) != 0) {
					b.Fatal("the removal shape's record did not write its deletions as removals")
				}
				// AND A RECORD THE COMMITTER FORMS KEEPS EVERY DELETION A ROW.
				if strings.HasPrefix(shape.name, "deletions") {
					made := 0
					for _, p := range shape.entries(i) {
						made += len(p.Commands)
					}
					if kept := e.num(`SELECT COUNT(*) FROM code_refs WHERE repo_id = ? AND target = ''
						AND ref LIKE 'refs/heads/r%'`, repo); kept != int64(made) {
						b.Fatalf("the deletions shape kept %d of its %d deletions as tombstones", kept, made)
					}
				}
			}
		})
	}
}

// BenchmarkApplyAPurgePart is a purge record APPLIED AND COMMITTED, in each of
// the shapes its bounds allow, into a repository of its own each iteration,
// seeded outside the timer:
//
//   - narrow: PurgePartRows push rows of about a kilobyte, which the row
//     bound stops at;
//   - wide: a hundred push rows at the widest a row can be, which the byte
//     bound stops at — about seventy-two of them, PurgePartBytes and at most
//     one row past it;
//   - wide+refs: the widest rows short of PurgePartBytes and as many refs as
//     PurgePartRows leaves, which neither bound cuts — the most one part can
//     take of push rows and narrow ones;
//   - final: the purge itself over what wide+refs seeds, with
//     MaxProtectionsPerRepo rules: the most the gate record's unbounded
//     statements can meet, since the decide forms it only once what is left
//     fits a part;
//   - changes: a quarter of PurgePartRows changes of a revision, an author
//     and a body at MaxChangeBodyBytes, which the part takes whole — the most
//     changes one part's walk counts and its four deletes take, with 24 MiB
//     of bodies;
//   - wide+changes: the widest rows short of PurgePartBytes, then a thousand
//     such changes: the bodies share the pushes' bytes, so the part takes only
//     the few whose bodies fill what the pushes left — weighed by their rows
//     alone it would take them to the row bound, 732 of them and 24 MB more;
//   - wide+bodies: both bounds at once — as many such changes as the rows
//     leave room for beside the widest rows that fill what their bodies leave
//     of PurgePartBytes: the most one part can take of push rows and changes;
//   - final+bodies: the purge itself over what wide+bodies seeds, with
//     MaxProtectionsPerRepo rules;
//   - cut: one change of PurgePartRows revisions, an author and a body at the
//     bound, which the part cuts, its oldest revisions first, leaving the
//     author, the body and the row.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot
// split a record; doc.go's measured section records the figures.
func BenchmarkApplyAPurgePart(b *testing.B) {
	quiet(b)
	commands, introduced := widestPushRow(b)
	width := len(commands) + len(introduced)
	under := (PurgePartBytes - 1) / width
	// A CHANGE AT ITS WIDEST is four rows: its own, a revision, an author and
	// a body at MaxChangeBodyBytes.
	const perChange = 4
	bodied := func(e *estate, repo string, n int) { e.seedChangeRows(repo, n, 1, 1, MaxChangeBodyBytes) }
	// after is how many of those a part takes behind the widest rows short of
	// PurgePartBytes: the bodies that fill what the pushes left, the last
	// reaching the bound.
	after := (PurgePartBytes - under*width + MaxChangeBodyBytes - 1) / MaxChangeBodyBytes
	// mixed widest rows and beside changes fill both bounds: the changes the
	// rows leave room for, and the pushes their bodies leave room for.
	mixed := (PurgePartBytes - PurgePartRows/perChange*MaxChangeBodyBytes) / (width - MaxChangeBodyBytes/perChange)
	beside := (PurgePartRows - mixed) / perChange
	ruled := func(e *estate, repo string) {
		e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_protections (repo_id, pattern, direct_push)
			SELECT ?, printf('refs/heads/rule-%03d', i), 'persons' FROM k`,
			MaxProtectionsPerRepo, repo)
	}
	for _, shape := range []struct {
		name  string
		seed  func(e *estate, repo string)
		final bool
		rows  int
		// pushes and changes are what the record left of the repository's
		// push rows and of the rows of its changes.
		pushes, changes int
	}{
		{"narrow", func(e *estate, repo string) { e.seedNarrow(repo, PurgePartRows+500) },
			false, PurgePartRows, 500, 0},
		{"wide", func(e *estate, repo string) { e.seedWide(b, repo, 100) },
			false, (PurgePartBytes + width - 1) / width, 100 - (PurgePartBytes+width-1)/width, 0},
		{"wide+refs", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
		}, false, PurgePartRows, 0, 0},
		{"final", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
			ruled(e, repo)
		}, true, PurgePartRows + MaxProtectionsPerRepo, 0, 0},
		{"changes", func(e *estate, repo string) { bodied(e, repo, PurgePartRows/perChange) },
			false, PurgePartRows, 0, 0},
		{"wide+changes", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			bodied(e, repo, 1000)
		}, false, under + after*perChange, 0, (1000 - after) * perChange},
		{"wide+bodies", func(e *estate, repo string) {
			e.seedWide(b, repo, mixed)
			bodied(e, repo, beside)
		}, false, mixed + beside*perChange, 0, 0},
		{"final+bodies", func(e *estate, repo string) {
			e.seedWide(b, repo, mixed)
			bodied(e, repo, beside)
			ruled(e, repo)
		}, true, mixed + beside*perChange + MaxProtectionsPerRepo, 0, 0},
		{"cut", func(e *estate, repo string) { e.seedChangeRows(repo, 1, PurgePartRows, 1, MaxChangeBodyBytes) },
			false, PurgePartRows, 0, 3},
	} {
		b.Run(shape.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier()
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo, name := fmt.Sprintf("repo-%06d", i), fmt.Sprintf("r%06d", i)
				e.apply(create(b, repo, name))
				shape.seed(e, repo)
				r := partOf(b, repo, 1)
				if shape.final {
					r = rec(b, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken(name)), RepoPath(repo)},
						Purge{RepoID: repo, Name: name, Reason: "past retention"})
				}
				e.applyTimed(b, a, r, shape.rows)
				if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); left != int64(shape.pushes) {
					b.Fatalf("the record left %d push rows, want %d", left, shape.pushes)
				}
				if left := e.changeRowsOf(repo); left != int64(shape.changes) {
					b.Fatalf("the record left %d rows of the changes, want %d", left, shape.changes)
				}
			}
		})
	}
}

// BenchmarkApplyARetireSweep is a retire_sweep record APPLIED AND COMMITTED —
// the time it holds the replicated estate's one write lane, on every data node
// — into a repository of its own each iteration, its retired rows seeded
// outside the timer as one earlier record's retirements:
//
//   - rows=8: a sweep of a few rows, what the pass takes of a repository one
//     small compaction retired from, whose cost is its commit's;
//   - rows=3999: RetireSweepRows, the most rows one record removes.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot split
// a record; doc.go's measured section records the figures, and limits.go's
// RetentionLaneShare and RetentionPassBudget reason from them beside
// BenchmarkRetireSweepStep's whole step.
func BenchmarkApplyARetireSweep(b *testing.B) {
	quiet(b)
	old := applyClock.Add(-RetireGrace - time.Hour)
	for _, rows := range []int{8, RetireSweepRows} {
		b.Run(fmt.Sprint("rows=", rows), func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier()
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("repo-%06d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				// RETIRED BY THE CREATE'S OWN POSITION: any record below the
				// sweep's is one a sweep may name.
				retiredBy := e.next - 1
				e.seedRetired(repo, retiredBy, old, rows)
				// THE REPOSITORY'S OWN ROW beside the rows it removes.
				e.applyTimed(b, a, sweepOf(b, repo, retiredBy), rows+1)
				if left := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); left != 0 {
					b.Fatalf("the sweep left %d of %s's %d retired rows", left, repo, rows)
				}
			}
		})
	}
}

// widestCheck is the widest queued attempt and the widest ending of it a
// writer forms, key's: every field at its bound, of text JSON escapes to six
// bytes a character, as TestTheLargestCheckRecordsFitARecord builds them — so
// the row they join to is the widest a check row can be, its definition
// stored as the record carries it and its failures as JSON beside its
// summary, tail and link.
func widestCheck(tb testing.TB, key CheckKey, at time.Time) (CheckQueued, CheckFinished) {
	tb.Helper()
	wide := func(n int) string { return strings.Repeat("&", n) }
	// A REPORT'S PATH AT ITS BOUND: sixteen components of the 255 bytes a
	// file name takes, under MaxTreePathBytes whole.
	report := func(lead string) string { return lead + strings.Repeat("/"+wide(254), 15) + "/x" }
	def := CheckDef{Name: key.Name, Run: wide(MaxCheckRunBytes),
		TimeoutSeconds: int(CheckTimeoutCeiling / time.Second),
		Report:         CheckReport{JUnit: report(wide(254)), SARIF: report("s" + wide(253))}, RunIn: CellE2B}
	for i := range MaxCheckPaths {
		head := fmt.Sprintf("%02d/", i)
		def.Paths = append(def.Paths, head+wide(128-len(head)))
	}
	for i := range coneList.max {
		def.Checkout.Cone = append(def.Checkout.Cone, fmt.Sprintf("%02d", i)+wide(253))
	}
	for i := range MaxCheckEnv {
		def.Env = append(def.Env, fmt.Sprintf("V%02d_", i)+strings.Repeat("A", MaxCheckEnvNameBytes-4))
	}
	queued := CheckQueued{CheckKey: key, Purpose: PurposeTip, Target: "refs/heads/main", Reason: QueueDispatch,
		Def: &def, QueuedAt: at}
	var failures []CheckFailure
	for i := 0; ; i++ {
		f := CheckFailure{Path: "p", Line: i, TestID: "t", Message: wide(100)}
		encoded, err := json.Marshal(append(failures, f))
		if err != nil {
			tb.Fatal(err)
		}
		if len(failures) == MaxCheckFailures || len(encoded) > MaxCheckFailureBytes {
			break
		}
		failures = append(failures, f)
	}
	exit := 1
	ended := CheckFinished{CheckKey: key, State: CheckStateFailed, Reason: CheckReasonExit, ExitCode: &exit,
		DurationMs: 1 << 40, Summary: wide(CheckSummaryBytes), Tail: wide(CheckTailBytes), Failures: failures,
		FailuresOmitted: 1 << 30, Log: logOf(key.Name + "-log"),
		URL: "https://ci.example.com/" + wide(MaxCheckURLBytes-len("https://ci.example.com/")), FinishedAt: at}
	for _, p := range []interface{ Validate() error }{queued, ended} {
		if err := p.Validate(); err != nil {
			tb.Fatalf("the widest check record is not one a writer forms: %v", err)
		}
	}
	return queued, ended
}

// seedPastTheHorizon writes one more failed check row of a repository than an
// ending's horizon removes (CheckHorizonRows), each ended at the instant at,
// each with its log and few bytes of its own — what a repository running a
// narrow check on every push holds.
func (e *estate) seedPastTheHorizon(repo string, at time.Time) {
	e.t.Helper()
	n := CheckHorizonRows + 1
	ended := store.EncodeTime(at)
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_checks (repo_id, commit_oid, name, attempt, state, purpose, change_id,
			revision, candidate_id, entry_id, target_ref, base_oid, defs_oid, required, run_in, timeout_seconds,
			reason, runner_node, placement, box_handle, borrowed, reporter, queued_at, started_at, finished_at,
			duration_ms, exit_code, failures_omitted, queued_position, finished_position, row_bytes, url,
			summary, def, failures, tail)
		SELECT ?, printf('%040d', i), 'unit', 1, 'failed', 'tip', '', 0, '', '', 'refs/heads/main', '', '', 0,
			'container', 600, 'exit', 'node-a', 'container', 'box', 0, '', ?, ?, ?, 1000, 1, 0, 1, 2, 76, '',
			'1 test failed', '{"name":"unit","run":"make test","timeout_seconds":600}', '', '--- FAIL' FROM k`,
		n, repo, ended, ended, ended)
	e.exec(`INSERT INTO code_check_logs (repo_id, commit_oid, name, attempt, object, hash, size)
		SELECT repo_id, commit_oid, name, attempt, 'logs/' || commit_oid, 'sha256:' || commit_oid, 4096
		FROM code_checks WHERE repo_id = ?`, repo)
}

// BenchmarkApplyACheckRecord is a record on a repository's checks subject
// APPLIED AND COMMITTED — the time it holds the replicated estate's one write
// lane, on every data node — into a repository of its own each iteration, its
// attempt's earlier records applied and its old rows seeded outside the timer.
// Every data node applies every one of these records, four an attempt (queued,
// claimed, its box named, ended), and every ending runs its repository's
// horizon:
//
//   - claim: a runner's check_started of the widest queued attempt — a join
//     reading the row whole and writing it back;
//   - ending: the widest ending a writer forms (a failed run's, every field at
//     its bound, with its log) of a claimed attempt, nothing of its
//     repository past the history — an ending's own cost;
//   - ending+narrow: that, with CheckHorizonRows+1 narrow rows past the
//     history, each with its log, so its horizon removes CheckHorizonRows of
//     them and their logs: the most rows an ending's apply touches;
//   - ending+wide: that, with rows of the widest a check row can be past the
//     history, each with its log, so its horizon removes them until their
//     bytes reach CheckHorizonBytes — about a mebibyte of overflow pages, which
//     a check row carries five secondary indexes beside.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot
// split a record; doc.go's measured section records the figures, and
// CheckHorizonRows' and CheckHorizonBytes' docs reason from them.
func BenchmarkApplyACheckRecord(b *testing.B) {
	quiet(b)
	old := applyClock.Add(-CheckHistory - time.Hour)
	// width is what a row of the widest attempt weighs, and through how many
	// of them a horizon's bytes reach CheckHorizonBytes.
	probe := CheckKey{RepoID: checkRepo, OID: oid("probe"), Name: "probe", Attempt: 1}
	pq, pf := widestCheck(b, probe, applyClock)
	pe, err := finishedPart(pf, applyContext{})
	if err != nil {
		b.Fatal(err)
	}
	width := checkRow{queue: *queuedPart(pq, applyContext{}).queue, ending: *pe.ending}.rowBytes()
	through := int((CheckHorizonBytes + width - 1) / width)
	for _, arm := range []struct {
		name  string
		claim bool
		// past seeds the repository's rows past the history, and answers
		// how many the timed ending's horizon removes.
		past func(b *testing.B, e *estate, repo string) int
	}{
		{"claim", true, func(*testing.B, *estate, string) int { return 0 }},
		{"ending", false, func(*testing.B, *estate, string) int { return 0 }},
		{"ending+narrow", false, func(_ *testing.B, e *estate, repo string) int {
			e.seedPastTheHorizon(repo, old)
			return CheckHorizonRows
		}},
		{"ending+wide", false, func(b *testing.B, e *estate, repo string) int {
			subject, scope := ChecksSubject(repo), Scope{ChecksPath(repo)}
			for i := range through + 1 {
				key := CheckKey{RepoID: repo, OID: oid(fmt.Sprint("old", i)), Name: "wide", Attempt: 1}
				q, f := widestCheck(b, key, old)
				e.apply(rec(b, subject, OpCheckQueued, scope, q).at(old),
					rec(b, subject, OpCheckFinished, scope, f).at(old))
			}
			return through
		}},
	} {
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier()
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				repo := fmt.Sprintf("0190f3c4-0000-7000-8000-%012d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				removed := arm.past(b, e, repo)
				held := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo)
				key := CheckKey{RepoID: repo, OID: oid("checked"), Name: "unit", Attempt: 1}
				queued, ended := widestCheck(b, key, applyClock)
				subject, scope := ChecksSubject(repo), Scope{ChecksPath(repo)}
				e.apply(rec(b, subject, OpCheckQueued, scope, queued))
				claim := rec(b, subject, OpCheckStarted, scope, CheckStarted{CheckKey: key, Runner: "node-a",
					Placement: PlacementContainer, StartedAt: applyClock})
				if arm.claim {
					e.applyTimed(b, a, claim, 1)
					continue
				}
				e.apply(claim)
				// ITS ROW, ITS LOG, AND EACH ROW ITS HORIZON REMOVES WITH ITS
				// LOG.
				e.applyTimed(b, a, rec(b, subject, OpCheckFinished, scope, ended), 2+2*removed)
				if left := e.num(`SELECT COUNT(*) FROM code_checks WHERE repo_id = ?`, repo); left != held+1-int64(removed) {
					b.Fatalf("the ending left %d of the repository's %d rows and its own, want its horizon to "+
						"remove %d", left, held, removed)
				}
			}
		})
	}
}

// BenchmarkApplyACompaction is a compaction record APPLIED AND COMMITTED — the
// time it holds the replicated estate's one write lane, on every data node —
// into a repository of its own each iteration, holding MaxPacksPerRepo live
// packs seeded outside the timer:
//
//   - inputs=8, 64 and MaxCompactInputs: a compaction of that many inputs
//     into as many outputs as one of that many may write (at most
//     MaxCompactOutputs), so every arm is the most rows its input count can
//     touch — compactDecidedRows of them, every input retired and removed and
//     every output written;
//   - publication/inputs=0 and publication/inputs=1024: a PUBLICATION. The
//     repository's chain is MaxPublishedLayers layers naming every live pack,
//     MaxPacksPerRepo/MaxPublishedLayers of them a layer, each layer cut into
//     MaxLayerParts parts and published as its packs arrived — every one
//     decided and applied outside the timer. The publication's own layer, of
//     MaxLayerParts parts, replaces every layer but the root — their
//     MaxPublishedLayers−1 rows and their parts deleted — beside a repack of
//     none of the packs they covered (545 rows) or of MaxCompactInputs of
//     them into MaxCompactOutputs outputs: every bound at once, the 2 609
//     rows compactDecidedRows counts. A publication's apply also reads what
//     no other compaction's does — the chain whole before, 2 048 checksums
//     here, and the live packs again for the recount after — so the widest
//     record of the op is publication/inputs=1024, not inputs=1024;
//   - expunge/begin: an expunge's BEGIN, the other rewrite that takes a
//     compaction's bound on its inputs, at every bound at once: the chain the
//     publication arms seed, MaxExpungeInputs inputs — the first packs, which
//     the root names, so it drops every layer and its parts — into
//     MaxExpungeOutputs outputs, half kept live and half left dropping: the
//     2 625 rows expungeBeginRows counts, the widest record the code domain
//     forms, beside the chain read whole before and the live packs again for
//     the recount after;
//   - expunge/commit: the COMMIT of a purge's begin of MaxExpungeInputs inputs
//     that left MaxExpungeOutputs packs dropping, retiring them all into as
//     many rescue packs — expungeCommitRows' 97 rows — and judging the open
//     request by one seek of the packs' content index.
//
// Every shape is held to what a committer forms: its proposal is Validated and
// decided over the rows, outside the timer, before its record is applied.
//
// inputs=1024, publication/inputs=1024 and expunge/begin are held under a third
// of statelog.ApplyTxTimeBudget, which a record is checked against only at its
// boundary: MaxCompactInputs is halved until the first is, a publication's
// repack bounded below it until the second is, and MaxExpungeInputs halved
// below MaxCompactInputs until the third is (their docs, and doc.go's
// measured section).
func BenchmarkApplyACompaction(b *testing.B) {
	quiet(b)
	for _, arm := range []struct {
		name        string
		inputs      int
		publication bool
	}{
		{"inputs=8", 8, false},
		{"inputs=64", 64, false},
		{fmt.Sprint("inputs=", MaxCompactInputs), MaxCompactInputs, false},
		{"publication/inputs=0", 0, true},
		{fmt.Sprint("publication/inputs=", MaxCompactInputs), MaxCompactInputs, true},
	} {
		outputs := min(arm.inputs, MaxCompactOutputs)
		b.Run(arm.name, func(b *testing.B) {
			e := openApplyEstate(b)
			a := NewApplier()
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for i := range b.N {
				// A UUID, as the committer takes a repository's id.
				repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
				e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
				var c Compaction
				if arm.publication {
					c = e.widestPublication(b, repo, e.seedChain(b, repo), arm.inputs)
				} else {
					e.seed(repo, 0, 0, MaxPacksPerRepo)
					retire := make([]string, arm.inputs)
					for k := range retire {
						retire[k] = idOf(repo, k)
					}
					cid := statelog.NewOpID(applyClock, "")
					add := make([]PackRef, outputs)
					for k := range add {
						add[k] = outputOf(cid, k, 1, applyClock)
					}
					c = Compaction{RepoID: repo, CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
						Retire: retire, Add: add}
				}
				e.admitted(b, c)
				e.applyTimed(b, a, recordOf(b, c), compactDecidedRows(len(c.Retire), len(c.Add), c.Layer))
				if left := e.num(`SELECT live_packs FROM code_repos WHERE id = ?`, repo); left !=
					int64(MaxPacksPerRepo-arm.inputs+outputs) {
					b.Fatalf("the compaction left %d live packs, want %d", left, MaxPacksPerRepo-arm.inputs+outputs)
				}
				if !arm.publication {
					continue
				}
				// THE CHAIN IT LEAVES: the root and the layer, which covers
				// every live pack the root does not.
				if chain := e.chainOf(repo); len(chain) != 2 || chain[1] != c.Layer.LayerID {
					b.Fatalf("the publication left the chain %v, want the root and %s", chain, c.Layer.LayerID)
				}
				if tail := e.num(`SELECT live_packs - published_packs FROM code_repos WHERE id = ?`, repo); tail != 0 {
					b.Fatalf("the publication left %d live packs no layer covers, want none", tail)
				}
			}
		})
	}
	b.Run("expunge/begin", func(b *testing.B) {
		e := openApplyEstate(b)
		a := NewApplier()
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for i := range b.N {
			repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
			e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
			begin := e.widestBegin(b, repo)
			e.expungeAdmitted(b, ExpungeProposal{Begin: &begin})
			e.applyTimed(b, a, expungeRec(b, ExpungeProposal{Begin: &begin}), expungeBeginRows(len(begin.Retire),
				len(begin.Add)+len(begin.Dropping), len(begin.DropLayers)))
			if live, want := e.num(`SELECT live_packs FROM code_repos WHERE id = ?`, repo),
				int64(MaxPacksPerRepo-len(begin.Retire)+len(begin.Add)); live != want {
				b.Fatalf("the begin left %d live packs, want %d", live, want)
			}
			if chain := e.chainOf(repo); len(chain) != 0 {
				b.Fatalf("the begin left the chain %v, want none: its inputs are under the root", chain)
			}
		}
	})
	b.Run("expunge/commit", func(b *testing.B) {
		e := openApplyEstate(b)
		a := NewApplier()
		b.ReportAllocs()
		b.ResetTimer()
		b.StopTimer()
		for i := range b.N {
			repo := fmt.Sprintf("0190f3c4-5b6a-7c8d-9e0f-%012d", i)
			e.apply(create(b, repo, fmt.Sprintf("r%06d", i)))
			commit := e.widestCommit(b, repo)
			e.expungeAdmitted(b, ExpungeProposal{Commit: &commit})
			e.applyTimed(b, a, expungeRec(b, ExpungeProposal{Commit: &commit}),
				expungeCommitRows(len(commit.Retire), len(commit.Add)))
			if dropping := e.num(`SELECT COUNT(*) FROM code_packs WHERE repo_id = ? AND state = 'dropping'`,
				repo); dropping != 0 {
				b.Fatalf("the commit left %d packs dropping, want none", dropping)
			}
			if id, _ := e.openRequest(repo); id != commit.RequestID {
				b.Fatalf("the commit served request %s with packs from before it live (open %q)",
					commit.RequestID, id)
			}
		}
	})
}

// ones is n packs' objects, one each: the outputs of the widest expunge
// records, each holding an object of the inputs they replace.
func ones(n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = 1
	}
	return out
}

// widestBegin seeds repository id as the publication arms do — MaxPacksPerRepo
// live packs under a chain of MaxPublishedLayers layers of MaxLayerParts parts
// naming every one — and answers an expunge's begin at every bound at once:
// MaxExpungeInputs inputs, the first packs, which the chain's root names, so
// it drops every layer; MaxExpungeOutputs outputs, half kept and half left
// dropping — expungeBeginRows' 2 625 rows.
func (e *estate) widestBegin(b *testing.B, id string) ExpungeBegin {
	b.Helper()
	chain := e.seedChain(b, id)
	retire := make([]string, MaxExpungeInputs)
	for k := range retire {
		retire[k] = idOf(id, k)
	}
	layers := make([]string, len(chain))
	for k, l := range chain {
		layers[k] = l.LayerID
	}
	begin := beginOver(id, 1, retire, ones(MaxExpungeOutputs/2), ones(MaxExpungeOutputs-MaxExpungeOutputs/2),
		layers...)
	begin.Seen = codeAt(e.next - 1)
	return begin
}

// widestCommit seeds repository id with MaxPacksPerRepo live packs, asks for a
// purge of it, and applies — outside any timer — that purge's begin of
// MaxExpungeInputs inputs, leaving MaxExpungeOutputs packs dropping and
// keeping none; and answers the commit at every bound: it retires all of them
// and writes as many rescue packs — expungeCommitRows' 97 rows — and, a
// request being open, judges it with one seek of the packs' content index.
func (e *estate) widestCommit(b *testing.B, id string) ExpungeCommit {
	b.Helper()
	e.seed(id, 0, 0, MaxPacksPerRepo)
	request := requestOf(id, statelog.NewOpID(applyClock, "code_purge_unreachable"))
	e.apply(expungeRec(b, ExpungeProposal{Request: &request}))
	retire := make([]string, MaxExpungeInputs)
	for k := range retire {
		retire[k] = idOf(id, k)
	}
	begin := beginOver(id, 1, retire, nil, ones(MaxExpungeOutputs))
	begin.RequestID, begin.Grace, begin.Seen = request.RequestID, 0, codeAt(e.next-1)
	e.expungeAdmitted(b, ExpungeProposal{Begin: &begin})
	e.apply(expungeRec(b, ExpungeProposal{Begin: &begin}))
	commit := commitOver(begin, ones(MaxExpungeOutputs)...)
	commit.Seen = codeAt(e.next - 1)
	return commit
}

// expungeAdmitted fails b unless p is an expunge record the committer takes
// and whose decide over e's rows forms its record — [estate.admitted]'s rule
// for an expunge's.
func (e *estate) expungeAdmitted(b *testing.B, p ExpungeProposal) {
	b.Helper()
	if err := p.Validate(); err != nil {
		b.Fatalf("the committer refuses the %s's proposal: %v", opOf(p), err)
	}
	switch d, err := expungeDecidedIn(b, e, p, applyClock); {
	case err != nil:
		b.Fatalf("decide the %s: %v", opOf(p), err)
	case d.refusal != "":
		b.Fatalf("the %s is refused %s: %s", opOf(p), d.refusal, d.detail)
	}
}

// admitted fails b unless c is a compaction the committer takes and whose
// decide over e's rows forms its record — so a record a benchmark applies is
// one a committer appends, never one no build forms.
func (e *estate) admitted(b *testing.B, c Compaction) {
	b.Helper()
	if err := (CompactionProposal{Compaction: c}).Validate(); err != nil {
		b.Fatalf("the committer refuses the compaction's proposal: %v", err)
	}
	switch d, err := compactionDecidedIn(b, e, c, applyClock); {
	case err != nil:
		b.Fatalf("decide the compaction: %v", err)
	case d.refusal != "":
		b.Fatalf("the compaction is refused %s: %s", d.refusal, d.detail)
	}
}

// widestLayer is a layer named for name over packs (git checksums) on base,
// replacing replaces, its bytes cut into MaxLayerParts parts — the most rows
// one layer writes, and deletes once it is replaced.
func widestLayer(name, base string, replaces []string, packs ...string) LayerRef {
	l := layerOver(name, base, replaces, applyClock, packs...)
	l.Parts = make([]LayerPart, MaxLayerParts)
	for i := range l.Parts {
		l.Parts[i] = LayerPart{Object: objstore.KeyAt(applyClock),
			Hash: objstore.HashOf([]byte(fmt.Sprint(name, i))), Size: LayerPartBytes}
	}
	l.MidxBytes, l.BitmapBytes = MaxLayerParts*LayerPartBytes-1<<20, 1<<20
	return l
}

// seedChain publishes MaxPublishedLayers layers on repository id as its packs
// arrive — MaxPacksPerRepo/MaxPublishedLayers packs seeded, then a layer of
// MaxLayerParts parts over them on the layer before, decided over the rows
// and applied — so the chain names every live pack and is one its committer
// formed. It answers the layers, root first.
func (e *estate) seedChain(b *testing.B, id string) []LayerRef {
	b.Helper()
	per := MaxPacksPerRepo / MaxPublishedLayers
	chain := make([]LayerRef, 0, MaxPublishedLayers)
	base := ""
	for k := range MaxPublishedLayers {
		e.seedPacks(id, k*per, per)
		sums := make([]string, per)
		for n := range sums {
			sums[n] = sumOf(id, k*per+n)
		}
		c := publication(id, e.next-1, widestLayer(fmt.Sprint(id, "/", k), base, nil, sums...), nil)
		e.admitted(b, c)
		e.apply(recordOf(b, c))
		chain = append(chain, *c.Layer)
		base = c.Layer.LayerID
	}
	return chain
}

// widestPublication is the publication of repository id that replaces every
// layer of chain but its root with one layer of MaxLayerParts parts, beside a
// repack of inputs of the packs the replaced layers cover, the first after the
// root's, into as many outputs as that many inputs may write: the layer covers
// what the replaced layers did but those inputs, and the outputs.
func (e *estate) widestPublication(b *testing.B, id string, chain []LayerRef, inputs int) Compaction {
	b.Helper()
	per := MaxPacksPerRepo / MaxPublishedLayers
	retire := make([]string, inputs)
	for k := range retire {
		retire[k] = idOf(id, per+k)
	}
	cid := statelog.NewOpID(applyClock, "")
	add := make([]PackRef, min(inputs, MaxCompactOutputs))
	sums := make([]string, 0, MaxPacksPerRepo)
	for k := range add {
		add[k] = outputOf(cid, k, 1, applyClock)
		sums = append(sums, add[k].GitChecksum)
	}
	for n := per + inputs; n < MaxPacksPerRepo; n++ {
		sums = append(sums, sumOf(id, n))
	}
	replaces := make([]string, 0, len(chain)-1)
	for _, l := range chain[1:] {
		replaces = append(replaces, l.LayerID)
	}
	return publication(id, e.next-1, widestLayer(id+"/top", chain[0].LayerID, replaces, sums...), retire, add...)
}

// BenchmarkPushHistoryBytes is what ONE PUSH LEAVES ON EVERY DATA NODE'S DISK:
// the replicated estate's growth over records each landing one push, divided
// by the pushes — the whole file's (bytes/row), and each of the push's two
// rows' by the pages its own table's b-trees grew by (the estate's
// [pageTally]), since the two have two lifetimes:
//
//   - code_pushes-bytes/row is the push's code_pushes row and its index
//     entries, which the push-history horizon keeps usage.History (181
//     days) — the id and size of the push's pack among its columns, where it
//     carries one;
//   - code_packs-bytes/row is its pack's code_packs row and its index
//     entries, which live as long as the pack does — until a compaction
//     retires it, or the repository is purged.
//
// BY THE TABLES, never by the file's growth with a pack less its growth
// without one: that difference counts the push row's pack columns, kept 181
// days, as the pack row's, kept while the pack is live, and moves with how
// many rows of each shape fit a page, so one pack row weighed differently
// beside a person's push and beside a seat's. What a deployment's push
// history costs a data node is the first figure times its pushes a day times
// 181, plus the second times the live packs its repositories hold — about
// CompactTargetPacks a compacted repository, never past MaxPacksPerRepo, and
// a full pack (FullPackBytes) more for each that its history has filled —
// plus the retired arm's figure times the packs a compaction retired in the
// last RetireGrace and the code-retention pass's interval; every data node
// holds all three, since every one holds the whole estate.
//
// Two arms by the pack — pack=yes, a push carrying objects, and pack=no, one
// carrying none (a deletion, or a ref moved to a commit the repository
// holds), whose row names no pack and which leaves no pack row — and four by
// the push:
//
//   - push=typical: one command moving a ref named in 40 bytes, one
//     introduced commit id — a person's push of a commit through the token
//     bound to them, received by git;
//   - push=agent: the same commit pushed by a seat and built by the engine,
//     its row naming the seat as its actor and the run behind it at each
//     field's usual size — a turn id (a UUID), a tracker's work item
//     (`native`, `ENG-1234`) and a model id of a provider's usual length —
//     so the difference from typical is what a seat's provenance costs;
//   - push=box: the same commit pushed by the seat's coding box, received by
//     git, its run naming as well the coding run that pushed (its launch, a
//     UUID) and its coding agent (`claude-code`) — so the difference from
//     agent is what a box's two fields cost;
//   - push=widest: MaxCommandsPerPush commands on refs of MaxRefNameBytes and
//     MaxIntroduced introduced ids — the widest row a push stores.
//
// And one arm by what a pack's row becomes once a compaction merges it,
// retired/inputs=CompactMinMerge: typical pushes landed CompactMinMerge at a
// time, each group merged into one output by a compaction — held to what a
// committer forms, Validated and decided over the rows before its record is
// applied — so the code_packs_retired-bytes/row it reports is a retired
// pack's row and its three index entries, which code_packs_retired keeps
// RetireGrace until the code-retention duty's retire_sweep removes it. How
// many a data node holds is that figure times the packs retired a day — a
// push's pack, and the outputs a later compaction merges again
// (internal/code/compact's steady arm counts them, retired/push) — times
// RetireGrace and RetentionInterval over a day.
//
// EVERY ID IS THE SHAPE PRODUCTION WRITES, AND IN ITS ORDER, since an id is
// in a row and in the index entries beside it, and an index takes its
// entries at its end only while they come in order: the repository's and
// each push's a UUIDv7, each push's minted after the one before as a
// receiver mints one, its pack's id the push's own and its pack's object key
// minted at its put as the object store mints one (objstore.KeyAt), and a
// person's credential a token's name — `pat:` and its id, the push row's
// actor — beside the human seat they hold; and a compaction's id a UUIDv7
// minted as its home builds it, its output's object key at its put. Ids of a
// dozen bytes and object keys in no order once made every row these figures
// price lighter than the rows a data node holds.
//
// Every push moves refs that exist before the file is measured, so a ref's
// row is rewritten in place and never counted: a ref lives as long as it is
// kept, not as long as the history. Every other row a push writes is
// rewritten in place as well — its repository's counters — and the tables
// beside the two growing past [inPlaceShare] of what the push rows grow by
// fails the benchmark, as a third row each push leaves that nothing here
// prices. doc.go's measured section records the arms.
func BenchmarkPushHistoryBytes(b *testing.B) {
	quiet(b)
	const typicalRef = "refs/heads/feature/push-history-measured"
	if len(typicalRef) != 40 {
		b.Fatalf("the typical ref is %d bytes, not 40", len(typicalRef))
	}
	widestRef := func(c int) string {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		return ref + strings.Repeat("w", MaxRefNameBytes-len(ref))
	}
	// person is who makes a person's push as git hands it over: the token
	// bound to them, by its name, and the human seat they hold.
	person := Principal{Kind: PrincipalOperator, Seat: "ada",
		OperatorID: iam.MachineTokenName(uuid.NewSHA1(uuid.NameSpaceOID, []byte("ada's token")).String())}
	// pushed is one person's push of cmds, its id minted as a receiver mints
	// one, which its pack carries too, and its pack stored under a key the
	// object store mints at the put.
	pushed := func(cmds ...Command) PushEntry {
		entry := pushEntry(uuid.Must(uuid.NewV7()).String(), cmds...)
		entry.Principal, entry.Pack.Object = person, objstore.KeyAt(time.Now())
		return entry
	}
	// seatRun is the run the engine stamps on the r-th push of a seat, at each
	// field's usual size.
	seatRun := func(r int) RunProvenance {
		return RunProvenance{
			TurnID:      uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("turn-", r))).String(),
			WorkBackend: "native", WorkKey: "ENG-1234", Model: "claude-sonnet-4-5",
		}
	}
	for _, shape := range []struct {
		name string
		// records is how many pushes are measured: enough that the file
		// grows by thousands of pages, so a page's granularity is lost in
		// the figure.
		records   int
		ref       func(c int) string
		cmds, ids int
		// by stamps who made the r-th push, and how, on its entry: nil for
		// a person's, through git.
		by func(e *PushEntry, r int)
	}{
		{"typical", 4_000, func(int) string { return typicalRef }, 1, 1, nil},
		{"agent", 4_000, func(int) string { return typicalRef }, 1, 1, func(e *PushEntry, r int) {
			e.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: seatRun(r)}
			e.Via = ViaEngine
		}},
		{"box", 4_000, func(int) string { return typicalRef }, 1, 1, func(e *PushEntry, r int) {
			run := seatRun(r)
			run.RunID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprint("launch-", r))).String()
			run.CodingAgent = "claude-code"
			e.Principal = Principal{Kind: PrincipalAgent, Seat: "docs-writer", Run: run}
			e.Via = ViaGit
		}},
		{"widest", 100, widestRef, MaxCommandsPerPush, MaxIntroduced, nil},
	} {
		for _, pack := range []string{"no", "yes"} {
			b.Run("push="+shape.name+"/pack="+pack, func(b *testing.B) {
				var perRow, pushRow, packRow float64
				for b.Loop() {
					b.StopTimer()
					e := openApplyEstate(b)
					repo := uuid.Must(uuid.NewV7()).String()
					e.apply(create(b, repo, "history"))
					// THE REFS EXIST BEFORE THE FILE IS MEASURED: each record
					// moves them on from what the one before left.
					cmds := make([]Command, shape.cmds)
					for c := range cmds {
						cmds[c] = move(shape.ref(c), ZeroOID, oid(fmt.Sprint("tip-0-", c)))
					}
					e.apply(pushOf(b, repo, pushed(cmds...)))
					before := e.tally(b)
					b.StartTimer()
					for r := 1; r <= shape.records; r++ {
						for c := range cmds {
							cmds[c] = move(cmds[c].Ref, cmds[c].New, oid(fmt.Sprint("tip-", r, "-", c)))
						}
						entry := pushed(cmds...)
						entry.Introduced = make([]string, shape.ids)
						for i := range entry.Introduced {
							entry.Introduced[i] = oid(fmt.Sprint("introduced-", r, "-", i))
						}
						entry.IntroducedCount = shape.ids
						if pack == "no" {
							entry.Pack = nil
						}
						if shape.by != nil {
							shape.by(&entry, r)
						}
						e.apply(pushOf(b, repo, entry))
					}
					b.StopTimer()
					if got := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); got != int64(shape.records+1) {
						b.Fatalf("the estate holds %d push rows, want %d", got, shape.records+1)
					}
					after := e.tally(b)
					per := func(pages int64) float64 {
						return float64(pages*after.pageBytes) / float64(shape.records)
					}
					perRow = per(after.pages - before.pages)
					pushRow = per(after.tables["code_pushes"] - before.tables["code_pushes"])
					packRow = per(after.tables["code_packs"] - before.tables["code_packs"])
					var elsewhere int64
					var grown []string
					for table, pages := range after.tables {
						if grew := pages - before.tables[table]; grew > 0 && table != "code_pushes" &&
							table != "code_packs" {
							elsewhere += grew
							grown = append(grown, fmt.Sprintf("%s by %d", table, grew))
						}
					}
					pushes := after.tables["code_pushes"] - before.tables["code_pushes"]
					if elsewhere*inPlaceShare > pushes {
						slices.Sort(grown)
						b.Fatalf("over %d pushes the push rows grew by %d pages and the tables a push rewrites "+
							"in place by %d (%s), past a %dth of them: a row each push leaves beside its two, "+
							"whose lifetime nothing here prices — measure it with theirs", shape.records, pushes,
							elsewhere, strings.Join(grown, ", "), inPlaceShare)
					}
					b.StartTimer()
				}
				b.ReportMetric(perRow, "bytes/row")
				b.ReportMetric(pushRow, "code_pushes-bytes/row")
				b.ReportMetric(packRow, "code_packs-bytes/row")
			})
		}
	}
	b.Run(fmt.Sprintf("retired/inputs=%d", CompactMinMerge), func(b *testing.B) {
		// retiredRows is how many packs the arm's compactions retire: enough
		// that code_packs_retired grows by hundreds of pages, so a page's
		// granularity is lost in the figure.
		const retiredRows = 4_096
		var retiredRow float64
		for b.Loop() {
			b.StopTimer()
			e := openApplyEstate(b)
			repo := uuid.Must(uuid.NewV7()).String()
			e.apply(create(b, repo, "history"))
			tip := oid("tip-0")
			e.apply(pushOf(b, repo, pushed(move(typicalRef, ZeroOID, tip))))
			before := e.tally(b)
			b.StartTimer()
			for r := range retiredRows / CompactMinMerge {
				retire := make([]string, 0, CompactMinMerge)
				for k := range CompactMinMerge {
					next := oid(fmt.Sprint("tip-", r, "-", k))
					entry := pushed(move(typicalRef, tip, next))
					e.apply(pushOf(b, repo, entry))
					retire, tip = append(retire, entry.Pack.PackID), next
				}
				slices.Sort(retire)
				// AS ITS HOME BUILDS ONE: the compaction's id minted when it
				// is built, its output's key at its put — each after the
				// last, as the object index takes production's.
				cid := statelog.NewOpID(time.Now(), "")
				c := Compaction{RepoID: repo, CompactionID: cid, Node: "node-a", Seen: codeAt(e.next - 1),
					Retire: retire, Add: []PackRef{outputOf(cid, 0, int64(len(retire)), time.Now())}}
				if err := (CompactionProposal{Compaction: c}).Validate(); err != nil {
					b.Fatalf("the committer refuses the compaction's proposal: %v", err)
				}
				switch d, err := compactionDecidedIn(b, e, c, time.Now()); {
				case err != nil:
					b.Fatalf("decide the compaction: %v", err)
				case d.refusal != "":
					b.Fatalf("the compaction is refused %s: %s", d.refusal, d.detail)
				}
				e.apply(recordOf(b, c))
			}
			b.StopTimer()
			if got := e.num(`SELECT COUNT(*) FROM code_packs_retired WHERE repo_id = ?`, repo); got != retiredRows {
				b.Fatalf("the estate holds %d retired pack rows, want %d", got, retiredRows)
			}
			after := e.tally(b)
			retired := after.tables["code_packs_retired"] - before.tables["code_packs_retired"]
			retiredRow = float64(retired*after.pageBytes) / retiredRows
			// THE RETIREMENT LEAVES ONE ROW: beside the retired rows, a
			// compaction moves its inputs out of code_packs and its output
			// in, and the pushes the arm lands leave theirs — every other
			// table a record touches is rewritten in place.
			var elsewhere int64
			var grown []string
			for table, pages := range after.tables {
				if grew := pages - before.tables[table]; grew > 0 && table != "code_packs_retired" &&
					table != "code_packs" && table != "code_pushes" {
					elsewhere += grew
					grown = append(grown, fmt.Sprintf("%s by %d", table, grew))
				}
			}
			if elsewhere*inPlaceShare > retired {
				slices.Sort(grown)
				b.Fatalf("over %d retirements the retired rows grew by %d pages and the tables a "+
					"compaction rewrites in place by %d (%s), past a %dth of them: a row each retirement "+
					"leaves beside its own, whose lifetime nothing here prices — measure it with theirs",
					retiredRows, retired, elsewhere, strings.Join(grown, ", "), inPlaceShare)
			}
			b.StartTimer()
		}
		b.ReportMetric(retiredRow, "code_packs_retired-bytes/row")
	})
}

// inPlaceShare is how small, against what the push rows grow by over a
// history benchmark's records, the growth of every table a push only rewrites
// in place — its refs, its repository's counters — must stay: a HUNDREDTH.
// Such a table grows at all only as a rewrite splits a page or a rewritten
// row's numbers widen: code_refs by four pages over the widest arm's hundred
// records of a thousand refs each, against its push rows' 10 256, and nothing
// else at all (measured with the bound at zero). A third row each push left
// would be a few percent of a push row at the least (a 30-byte counter row is
// five percent of one), and under a hundredth no figure these rows forecast
// moves past its rounding.
const inPlaceShare = 100
