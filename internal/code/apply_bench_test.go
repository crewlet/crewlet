package code

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
// 470 KB, which is what makes code_pushes the estate's one wide table.
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
			`INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands,
				introduced, introduced_count, received_at, row_bytes) VALUES `,
			`(?, ?, ?, ?, 'seed', 'operator', ?, ?, ?, 0, ?)`, ``, n, func(i int) []any {
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
		INSERT INTO code_pushes (push_id, repo_id, position, at, actor, actor_kind, commands,
			received_at, row_bytes)
		SELECT printf('%s-narrow-%06d', ?, i), ?, i + 1, i, 'seed', 'operator', ?, 0, ? FROM k`,
		n, id, id, commands, len(commands))
}

// seedChangeRows writes changes of a repository, numbered from 1, each with
// revisions revisions and authors authors — the rows a purge part walks.
func (e *estate) seedChangeRows(id string, changes, revisions, authors int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_changes (id, repo_id, number, title, state, source_ref, target_ref, head_oid,
			revision, owner, owner_kind, opened_at, opened_position, version, position)
		SELECT printf('%s-change-%06d', ?, i), ?, i + 1, 'a change', 'open',
			printf('refs/heads/change-%06d', i), 'refs/heads/main', printf('%040d', i), ?, 'seat', 'agent',
			0, i + 1, 1, i + 1 FROM k`, changes, id, id, revisions)
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

// changeRowsOf is every row of a repository's changes seedChangeRows wrote
// that the estate still holds, revisions and authors found by their ids.
func (e *estate) changeRowsOf(id string) int64 {
	e.t.Helper()
	prefix := id + "-change-%"
	return e.num(`SELECT COUNT(*) FROM code_changes WHERE repo_id = ?`, id) +
		e.num(`SELECT COUNT(*) FROM code_revisions WHERE change_id LIKE ?`, prefix) +
		e.num(`SELECT COUNT(*) FROM code_change_authors WHERE change_id LIKE ?`, prefix)
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
// so a shape priced here is one the committer admits and the largest of its
// kind. command is the c-th command of push p.
func fullest(b *testing.B, prefix string, i int, command func(p, c int) Command) []PushEntry {
	b.Helper()
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
		b.Fatalf("the fullest record of %s bounds %d rows, not MaxRecordRows (%d)", prefix,
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
//     — the longest shape a record the committer forms has;
//   - removal: three pushes of MaxCommandsPerPush ref deletions at a
//     repository already holding MaxTombstonesPerRepo fresh tombstones, so
//     the record writes its deletions as removals and the objects they left
//     as 3 000 ref drops — a record no committer of this build forms (it
//     counts each deletion three times, and two such pushes never share a
//     record), whose cost the applier still bounds.
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
	}{
		{"maximal", maximal, nil, false, written(maximal(0))},
		{"maximal+history", maximal, nil, true, written(maximal(0))},
		{"group", group, nil, false, 2 * MaxPushesPerRecord},
		{"one", one, nil, false, 3},
		{"sweep", one, func(e *estate, repo string) { e.seedTombstones(repo, MaxRecordRows, expired) },
			false, MaxRecordRows},
		// EVERY ROW OF THE BUDGET TOUCHED: the deletions' tombstones and
		// their record's own rows, and the sweep taking what they leave.
		{"deletions", deleting, deletionsAt, false, MaxRecordRows},
		{"deletions+history", deleting, deletionsAt, true, MaxRecordRows},
		// A RECORD THAT REMOVES: every ref it deletes at a repository already
		// holding MaxTombstonesPerRepo fresh tombstones, which no sweep can
		// take, so its 3 000 deletions are removals rather than rows — each
		// still writing the object it left.
		{"removal", removing, func(e *estate, repo string) {
			e.seed(repo, 0, 3*MaxCommandsPerPush, 0)
			e.seedTombstones(repo, MaxTombstonesPerRepo, applyClock.Add(-time.Hour))
		}, false, 3 * (1 + 2*MaxCommandsPerPush + 1)},
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
				e.applyTimed(b, a, pushOf(b, repo, shape.entries(i)...), shape.rows)
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
//     take;
//   - final: the purge itself over what wide+refs seeds, with
//     MaxProtectionsPerRepo rules: the most the gate record's unbounded
//     statements can meet, since the decide forms it only once what is left
//     fits a part;
//   - changes: a thousand changes of one revision and one author each,
//     PurgePartRows rows the part takes whole — the most changes one part's
//     walk counts and its three deletes take;
//   - cut: one change of PurgePartRows revisions and an author, which the
//     part cuts, its oldest revisions first, leaving the author and the row.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot
// split a record; doc.go's measured section records the figures.
func BenchmarkApplyAPurgePart(b *testing.B) {
	quiet(b)
	commands, introduced := widestPushRow(b)
	width := len(commands) + len(introduced)
	under := (PurgePartBytes - 1) / width
	for _, shape := range []struct {
		name       string
		seed       func(e *estate, repo string)
		final      bool
		rows, left int
		// changes counts what the record left of the repository's changes
		// rather than of its pushes.
		changes bool
	}{
		{"narrow", func(e *estate, repo string) { e.seedNarrow(repo, PurgePartRows+500) },
			false, PurgePartRows, 500, false},
		{"wide", func(e *estate, repo string) { e.seedWide(b, repo, 100) },
			false, (PurgePartBytes + width - 1) / width, 100 - (PurgePartBytes+width-1)/width, false},
		{"wide+refs", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
		}, false, PurgePartRows, 0, false},
		{"final", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
			e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
				INSERT INTO code_protections (repo_id, pattern, direct_push)
				SELECT ?, printf('refs/heads/rule-%03d', i), 'persons' FROM k`,
				MaxProtectionsPerRepo, repo)
		}, true, PurgePartRows + MaxProtectionsPerRepo, 0, false},
		{"changes", func(e *estate, repo string) { e.seedChangeRows(repo, PurgePartRows/3, 1, 1) },
			false, PurgePartRows, 0, true},
		{"cut", func(e *estate, repo string) { e.seedChangeRows(repo, 1, PurgePartRows, 1) },
			false, PurgePartRows, 2, true},
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
				left, of := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo), "push rows"
				if shape.changes {
					left, of = e.changeRowsOf(repo), "rows of its changes"
				}
				if left != int64(shape.left) {
					b.Fatalf("the record left %d %s, want %d", left, of, shape.left)
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

// BenchmarkPushHistoryBytes is what ONE PUSH LEAVES ON EVERY DATA NODE'S DISK:
// the growth of the replicated estate's file over records each landing one
// push, divided by the pushes. Two rows with two lifetimes, so two arms:
//
//   - pack=no is the push's code_pushes row and its index entries alone,
//     which the push-history horizon keeps usage.History (181 days);
//   - pack=yes is that and its pack's code_packs row with its index entries,
//     which live as long as the pack does — until the repository is purged
//     (until compaction retires it, once there is a compaction) — so the
//     difference between the two arms is what each push adds for good.
//
// What a deployment's push history costs a data node is the first figure
// times its pushes a day times 181, plus the second's share times every push
// its repositories' packs still hold; every data node holds both, since every
// one holds the whole estate.
//
//   - push=typical: one command moving a ref named in 40 bytes, one
//     introduced commit id — a person's push of a commit;
//   - push=widest: MaxCommandsPerPush commands on refs of MaxRefNameBytes and
//     MaxIntroduced introduced ids — the widest row a push stores.
//
// Every push moves refs that exist before the file is measured, so a ref's
// row is rewritten in place and never counted: a ref lives as long as it is
// kept, not as long as the history. The file is measured after a truncating
// checkpoint, so what is counted is the database and not a sidecar's frames.
// bytes/row is the growth over the records' pushes; doc.go's measured section
// records both arms.
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
	for _, shape := range []struct {
		name string
		// records is how many pushes are measured: enough that the file
		// grows by thousands of pages, so a page's granularity is lost in
		// the figure.
		records   int
		ref       func(c int) string
		cmds, ids int
	}{
		{"typical", 4_000, func(int) string { return typicalRef }, 1, 1},
		{"widest", 100, widestRef, MaxCommandsPerPush, MaxIntroduced},
	} {
		for _, pack := range []string{"no", "yes"} {
			b.Run("push="+shape.name+"/pack="+pack, func(b *testing.B) {
				var perRow float64
				for b.Loop() {
					b.StopTimer()
					e := openApplyEstate(b)
					const repo = "repo-history"
					e.apply(create(b, repo, "history"))
					// THE REFS EXIST BEFORE THE FILE IS MEASURED: each record
					// moves them on from what the one before left.
					cmds := make([]Command, shape.cmds)
					for c := range cmds {
						cmds[c] = move(shape.ref(c), ZeroOID, oid(fmt.Sprint("tip-0-", c)))
					}
					e.apply(pushOf(b, repo, pushEntry("history-0", cmds...)))
					before := e.fileBytes(b)
					b.StartTimer()
					for r := 1; r <= shape.records; r++ {
						for c := range cmds {
							cmds[c] = move(cmds[c].Ref, cmds[c].New, oid(fmt.Sprint("tip-", r, "-", c)))
						}
						entry := pushEntry(fmt.Sprint("history-", r), cmds...)
						entry.Introduced = make([]string, shape.ids)
						for i := range entry.Introduced {
							entry.Introduced[i] = oid(fmt.Sprint("introduced-", r, "-", i))
						}
						entry.IntroducedCount = shape.ids
						if pack == "no" {
							entry.Pack = nil
						}
						e.apply(pushOf(b, repo, entry))
					}
					b.StopTimer()
					if got := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); got != int64(shape.records+1) {
						b.Fatalf("the estate holds %d push rows, want %d", got, shape.records+1)
					}
					perRow = float64(e.fileBytes(b)-before) / float64(shape.records)
					b.StartTimer()
				}
				b.ReportMetric(perRow, "bytes/row")
			})
		}
	}
}

// fileBytes is the size of the estate's file once a truncating checkpoint has
// folded every committed page into it — the database itself, never a
// sidecar's frames.
func (e *estate) fileBytes(b *testing.B) int64 {
	b.Helper()
	db := storetest.ReplicatedDB(b, e.db)
	var busy, frames, folded sql.NullInt64
	if err := db.SQL().QueryRowContext(b.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &frames, &folded); err != nil {
		b.Fatalf("checkpoint the estate: %v", err)
	}
	if busy.Int64 != 0 {
		b.Fatalf("the estate's checkpoint was busy: %d of %d frames folded", folded.Int64, frames.Int64)
	}
	info, err := os.Stat(db.Path())
	if err != nil {
		b.Fatal(err)
	}
	return info.Size()
}
