package search_test

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// BenchmarkTheChangeIndexBuild walks changes from an empty index, as a first build does, and reports
// the time and bytes a change costs: short, typical (150 distinct words, 110 of them rare words at
// random places in the term key) at two corpus sizes, so the time's growth shows, and long.
func BenchmarkTheChangeIndexBuild(b *testing.B) {
	for _, arm := range []struct {
		name    string
		changes int
		text    func(rng *rand.Rand, i int) (string, string)
	}{
		{"short", 10_000, func(rng *rand.Rand, i int) (string, string) {
			return words(rng, i, 8, 8, 400), padTo(words(rng, i, 60, 40, 400), 300)
		}},
		{"typical", 10_000, typicalChange},
		{"typical at 30 000", 30_000, typicalChange},
		{"long", 1_000, func(rng *rand.Rand, i int) (string, string) {
			return words(rng, i, 8, 8, 400), padTo(words(rng, i, 6_000, 0, 1_000), 32<<10)
		}},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var grown int64
			for range b.N {
				b.StopTimer()
				f := newChangeFixture(b)
				f.addRepo("repo-a")
				rng := rand.New(rand.NewPCG(24, 1))
				rows := make([]changeFixtureRow, arm.changes)
				for i := range rows {
					title, body := arm.text(rng, i)
					rows[i] = changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
						title: title, body: body}
				}
				f.writeAll(rows)
				f.numberFrom(1 << 24)
				before := f.fileBytes()
				b.StartTimer()
				f.sweepAll()
				b.StopTimer()
				grown += f.fileBytes() - before
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*arm.changes), "ns/change")
			b.ReportMetric(float64(grown)/float64(b.N*arm.changes), "bytes/change")
		})
	}
}

// BenchmarkAChangeSearchAtItsScanBound is one search whose every word and scope holds more changes
// than one search reads: it reads the newest MaxChangeTextScan of what it drives on and judges the
// rest of its words on each, answering the newest MaxChangeTextCandidates-sized page of ids.
func BenchmarkAChangeSearchAtItsScanBound(b *testing.B) {
	f := newChangeFixture(b)
	f.addRepo("repo-a")
	rows := make([]changeFixtureRow, 2*search.MaxChangeTextScan)
	for i := range rows {
		rows[i] = changeFixtureRow{id: changeUUID(i + 1), repo: "repo-a", owner: "ada", state: "open",
			title: "fix the flaky login", body: "the cache drops a token under load"}
	}
	f.writeAll(rows)
	f.sweepAll()
	for _, arm := range []struct {
		name string
		q    search.ChangeQuery
	}{
		{"one word", search.ChangeQuery{Terms: []string{"login"}, Limit: 1_000}},
		{"eight words in a repository", search.ChangeQuery{Terms: []string{"fix", "flaky", "login", "cache",
			"drops", "token", "under", "load"}, RepoID: "repo-a", State: "open", Owner: "ada", Limit: 1_000}},
	} {
		b.Run(arm.name, func(b *testing.B) {
			for b.Loop() {
				if held := f.holding(arm.q); !held.Truncated || len(held.IDs) != 1_000 {
					b.Fatalf("the search answered %d ids, truncated %v: not at its bound", len(held.IDs),
						held.Truncated)
				}
			}
		})
	}
}

// words is n words, the first shared drawn from a vocabulary of that many every change shares and the
// rest from vocabulary words of change i's own neighbourhood.
func words(rng *rand.Rand, i, n, shared, vocabulary int) string {
	out := make([]string, n)
	for k := range out {
		if k < shared {
			out[k] = fmt.Sprintf("s%03d", rng.IntN(max(shared, 1)))
			continue
		}
		out[k] = fmt.Sprintf("w%05d", (i*7+rng.IntN(vocabulary))%100_000)
	}
	return strings.Join(out, " ")
}

// typicalChange is change i of the typical kind: an eight-word title, 110 rare words and 40 shared.
func typicalChange(rng *rand.Rand, i int) (string, string) {
	return words(rng, i, 8, 8, 40), rareWords(rng, 110) + " " + sharedWords(40)
}

// rareWords is n words of eleven random letters, as names and identifiers are: no other change holds
// them, and each lands at its own place in the term key rather than past every earlier change's.
func rareWords(rng *rand.Rand, n int) string {
	out := make([]string, n)
	for k := range out {
		word := make([]byte, 11)
		for c := range word {
			word[c] = byte('a' + rng.IntN(26))
		}
		out[k] = string(word)
	}
	return strings.Join(out, " ")
}

// sharedWords is the n words every change holds.
func sharedWords(n int) string {
	out := make([]string, n)
	for k := range out {
		out[k] = fmt.Sprintf("s%03d", k)
	}
	return strings.Join(out, " ")
}

// padTo is text repeated and cut to n bytes.
func padTo(text string, n int) string {
	return strings.Repeat(text+" ", n/(len(text)+1)+1)[:n]
}

// writeAll writes rows at positions 1 to len(rows) in one transaction and settles the log past them.
func (f *changeFixture) writeAll(rows []changeFixtureRow) {
	f.t.Helper()
	if err := f.db.Replicated().Tx(f.ctx, func(tx *sql.Tx) error {
		for i, row := range rows {
			if _, err := tx.ExecContext(f.ctx, `INSERT INTO code_changes (id, repo_id, number, title,
				body_bytes, state, source_ref, target_ref, head_oid, revision, owner, owner_kind, opened_at,
				opened_position, version, position) VALUES (?, ?, ?, ?, ?, ?, 'refs/heads/topic',
				'refs/heads/main', '', 1, ?, 'agent', 0, ?, 1, ?)`, row.id, row.repo, i+1, row.title,
				len(row.body), row.state, row.owner, i+1, i+1); err != nil {
				return err
			}
			if row.body == "" {
				continue
			}
			if _, err := tx.ExecContext(f.ctx, `INSERT INTO code_change_bodies (change_id, body) VALUES (?, ?)`,
				row.id, row.body); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		f.t.Fatalf("write %d changes: %v", len(rows), err)
	}
	f.settle(int64(len(rows)))
}

// numberFrom has the index number its next document after seq, as an index a year in does.
func (f *changeFixture) numberFrom(seq int64) {
	f.t.Helper()
	if err := f.db.Tx(f.ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(f.ctx, `UPDATE change_index_state SET last_seq = ?`, seq)
		return err
	}); err != nil {
		f.t.Fatalf("number the index's documents from %d: %v", seq, err)
	}
}

// fileBytes is this node's own file's size once a truncating checkpoint folded every committed page
// into it.
func (f *changeFixture) fileBytes() int64 {
	f.t.Helper()
	var busy, frames, folded sql.NullInt64
	if err := f.db.SQL().QueryRowContext(f.ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &frames, &folded); err != nil {
		f.t.Fatalf("checkpoint the node's file: %v", err)
	}
	info, err := os.Stat(f.db.Path())
	if err != nil {
		f.t.Fatalf("measure the node's file: %v", err)
	}
	return info.Size()
}
