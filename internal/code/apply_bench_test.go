package code

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

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

// seedTombstones writes n expired ref tombstones into a repository and counts
// them, as a sweep finds them.
func (e *estate) seedTombstones(id string, n int) {
	e.t.Helper()
	e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_refs (repo_id, ref, target, position, at)
		SELECT ?, printf('refs/heads/gone-%06d', i), '', 1, 0 FROM k`, n, id)
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

// BenchmarkApplyAPushRecord is a push record APPLIED AND COMMITTED — the time
// it holds the replicated estate's one write lane, which every other domain's
// applier on the node waits behind — into a repository of its own each
// iteration, set up outside the timer: one holding 10 000 refs, or, for the
// sweeps, one holding a whole record's budget of expired tombstones. Its
// shapes are the records the committer forms and the ones the horizons make
// longest:
//
//   - maximal: three pushes of MaxCommandsPerPush ref creations, the largest
//     record by rows (3 007, and the sweep's 992 more when tombstones wait);
//   - group: MaxPushesPerRecord pushes of one command each, what group commit
//     turns a CI's burst of tag pushes into;
//   - one: one push of one command, the common case;
//   - sweep: one push of one command whose record sweeps every expired
//     tombstone its row budget leaves room for — 3 995;
//   - maximal+sweep: the largest record with expired tombstones waiting,
//     which sweeps the 992 its own rows leave;
//   - maximal+sweep+history: that, with HorizonDeletesPerRecord expired push
//     rows waiting too, each at the widest a row can be — the longest a push
//     record can hold the lane, since the history's removal is bounded by
//     HorizonBytesPerRecord and takes three of them.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which a record
// is checked against only at its boundary; doc.go's measured section records
// the figures.
func BenchmarkApplyAPushRecord(b *testing.B) {
	// A PUSH ID IS THE FLEET'S, not the repository's, so every iteration's
	// pushes carry their own: a repeated one is a redelivery and applies as
	// nothing.
	maximal := func(i int) []PushEntry {
		var entries []PushEntry
		for p := range 3 {
			var cmds []Command
			for c := range MaxCommandsPerPush {
				cmds = append(cmds, move(fmt.Sprintf("refs/heads/new/%d/%04d", p, c), ZeroOID,
					oid(fmt.Sprint(p, c))))
			}
			entries = append(entries, pushEntry(fmt.Sprint("max-", i, "-", p), cmds...))
		}
		return entries
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
	for _, shape := range []struct {
		name    string
		entries func(int) []PushEntry
		stones  bool
		history bool
		rows    int
	}{
		{"maximal", maximal, false, false, 3 * (1 + MaxCommandsPerPush + 1)},
		{"group", group, false, false, 2 * MaxPushesPerRecord},
		{"one", one, false, false, 3},
		{"sweep", one, true, false, MaxRecordRows},
		{"maximal+sweep", maximal, true, false, MaxRecordRows},
		{"maximal+sweep+history", maximal, true, true, MaxRecordRows},
	} {
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
				if shape.stones {
					e.seedTombstones(repo, MaxRecordRows)
				} else {
					e.seed(repo, 0, 10_000, 0)
				}
				e.applyTimed(b, a, pushOf(b, repo, shape.entries(i)...), shape.rows)
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
//     fits a part.
//
// Each is held against statelog.ApplyTxTimeBudget (250 ms), which cannot
// split a record; doc.go's measured section records the figures.
func BenchmarkApplyAPurgePart(b *testing.B) {
	commands, introduced := widestPushRow(b)
	width := len(commands) + len(introduced)
	under := (PurgePartBytes - 1) / width
	for _, shape := range []struct {
		name       string
		seed       func(e *estate, repo string)
		final      bool
		rows, left int
	}{
		{"narrow", func(e *estate, repo string) { e.seedNarrow(repo, PurgePartRows+500) },
			false, PurgePartRows, 500},
		{"wide", func(e *estate, repo string) { e.seedWide(b, repo, 100) },
			false, (PurgePartBytes + width - 1) / width, 100 - (PurgePartBytes+width-1)/width},
		{"wide+refs", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
		}, false, PurgePartRows, 0},
		{"final", func(e *estate, repo string) {
			e.seedWide(b, repo, under)
			e.seed(repo, 0, PurgePartRows-under, 0)
			e.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
				INSERT INTO code_protections (repo_id, pattern, direct_push)
				SELECT ?, printf('refs/heads/rule-%03d', i), 'persons' FROM k`,
				MaxProtectionsPerRepo, repo)
		}, true, PurgePartRows + MaxProtectionsPerRepo, 0},
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
				if left := e.num(`SELECT COUNT(*) FROM code_pushes WHERE repo_id = ?`, repo); left != int64(shape.left) {
					b.Fatalf("the record left %d push rows, want %d", left, shape.left)
				}
			}
		})
	}
}
