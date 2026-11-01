package cache_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A SNAPSHOT THAT MOVES IS READ AGAIN, from its first page and FROM THE SAME
// POSITION: page 1 decides full or delta afresh, so a restart converges — and
// a round that keeps finding its history moved past MaxSnapshotRestarts fails
// rather than read for ever.
func TestASnapshotThatMovesIsReadAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	w.log.set(func(f *fakeLog) { f.pageRows = 2 })

	moveRefs := func(prefix string) {
		refs := map[string]string{}
		for i := range 5 {
			refs[fmt.Sprintf("refs/heads/%s%d", prefix, i)] = tip
		}
		w.log.write(repo, nil, refs)
	}
	movedTimes := func(n int64) {
		var moved atomic.Int64
		w.log.set(func(f *fakeLog) {
			f.hook = func(call fakeCall) error {
				if call.Page == 2 && moved.Add(1) <= n {
					return fmt.Errorf("%w: a test moved it", code.ErrSnapshotMoved)
				}
				return nil
			}
		})
	}

	moveRefs("a")
	movedTimes(2)
	pages := len(w.log.firstPages(repo))
	s := w.sync(t, c, repo)
	firsts := w.log.firstPages(repo)[pages:]
	if len(firsts) != 3 {
		t.Fatalf("a snapshot moved twice cost %d page-1 reads, want 3", len(firsts))
	}
	for _, f := range firsts {
		if f.Since != firsts[0].Since || f.Since.IsZero() {
			t.Errorf("a restart asked from %s, want the round's own %s", f.Since, firsts[0].Since)
		}
	}
	if got := len(w.logs.lines(t, "code_sync_restarted")); got != 2 {
		t.Errorf("logged %d code_sync_restarted lines, want 2", got)
	}
	if refs := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(refs, "refs/heads/a4") {
		t.Errorf("the restarted round left packed-refs without the delta:\n%s", refs)
	}

	moveRefs("b")
	movedTimes(cache.MaxSnapshotRestarts + 1)
	pages = len(w.log.firstPages(repo))
	_, err := c.Sync(t.Context(), repo, linearizable)
	if !errors.Is(err, code.ErrSnapshotMoved) {
		t.Fatalf("a snapshot that kept moving: %v, want code.ErrSnapshotMoved", err)
	}
	if got := len(w.log.firstPages(repo)) - pages; got != cache.MaxSnapshotRestarts+1 {
		t.Errorf("a snapshot that kept moving cost %d page-1 reads, want %d", got, cache.MaxSnapshotRestarts+1)
	}
}

// A STREAM CHANGE IS A FULL SYNC: a log RECREATED under the cache — provisioned
// anew, its stamp not the one the cache read under (ADR-0076) — answers a
// delta from the cache's position with the rows of an earlier state, which a
// delta cannot carry; the cache sees the stream it read from is not this one,
// reads the whole answer, and converges to the recreated rows — packs they do
// not name swept, refs back where they were. And every other repository of
// the node is read whole at its next round, answered at once from nothing
// until then: one log is under every one of them.
func TestAStreamChangeIsAFullSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, other := w.repo(t, 1), w.repo(t, 2)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, other, "refs/heads/main", nil)
	earlier := w.log.state()
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	w.push(t, other, "refs/heads/lost", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	w.sync(t, c, repo).Release() // converged: the next round is a delta
	w.sync(t, c, other).Release()

	w.log.recreate(earlier)
	pages := len(w.log.firstPages(repo))
	s := w.sync(t, c, repo)
	firsts := w.log.firstPages(repo)[pages:]
	if len(firsts) != 2 || firsts[0].Since.IsZero() || !firsts[1].Since.IsZero() {
		t.Fatalf("page-1 reads %+v, want a delta then the whole answer", firsts)
	}
	if s.Kind != cache.SyncFull {
		t.Errorf("Kind = %s, want full", s.Kind)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("the image after a recreation:\n%s\nwant the recreated rows':\n%s", got, w.rebuilt(t, repo))
	}
	if got := len(w.logs.lines(t, "code_cache_stream_changed")); got != 1 {
		t.Errorf("logged %d code_cache_stream_changed lines, want 1", got)
	}

	if cache.Converged(c, other) {
		t.Fatal("another repository on the recreated log is still answered at once from the old one")
	}
	calls := len(w.log.callsOf(other))
	o, err := c.Sync(t.Context(), other, stale)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()
	if o.Kind != cache.SyncFull || len(w.log.callsOf(other)) == calls {
		t.Errorf("a stale caller of the other repository cost %s, want a full round", o.Kind)
	}
	if refs := read(t, filepath.Join(o.Dir, "packed-refs")); strings.Contains(refs, "refs/heads/lost") {
		t.Errorf("the other repository still serves a ref the recreated log never held:\n%s", refs)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "stream"); got != 2 {
		t.Errorf("%s{reason=stream} = %d, want 2: each repository read whole once", metrics.CodeCacheRebuilds, got)
	}
}

// AN ANSWER BEHIND THE CACHE CHANGES NOTHING — unless it was read at the log's
// head. A delta from a stale copy, on the stream the cache read from, below
// where the cache already is, changes nothing: the cache never moves backwards
// for one (a client that fetched from this node must not see its refs go
// back). A FULL answer below it — the first round after a restart — is asked
// again at linearizable before anything is concluded from it: there the copy
// was only behind, and the log's head answers at the cache's own position,
// which the cache converges to unchanged.
func TestAnAnswerBehindTheCacheChangesNothing(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	at := w.sync(t, c, repo).Position
	before := w.image(t, c, repo)

	w.log.set(func(f *fakeLog) { f.behindBy = 2 })
	beyond := at
	beyond.Seq++
	pages := len(w.log.firstPages(repo))
	s, err := c.Sync(t.Context(), repo, statelog.FloorAt(statelog.ReadStale, beyond))
	if err != nil {
		t.Fatal(err)
	}
	if s.Position != at {
		t.Errorf("an answer behind the cache moved it to %s, from %s", s.Position, at)
	}
	s.Release()
	if got := w.image(t, c, repo); got != before {
		t.Errorf("an answer behind the cache changed it:\n%s\nwas:\n%s", got, before)
	}
	if firsts := w.log.firstPages(repo)[pages:]; len(firsts) != 1 || firsts[0].Since.IsZero() {
		t.Errorf("page-1 reads %+v, want the one delta: a stale delta behind the cache is asked nothing more", firsts)
	}

	c = w.reopen(t, c)
	floor := at
	floor.Seq -= 2
	pages = len(w.log.firstPages(repo))
	s, err = c.Sync(t.Context(), repo, statelog.FloorAt(statelog.ReadSession, floor))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	firsts := w.log.firstPages(repo)[pages:]
	if len(firsts) != 2 || firsts[0].Level != statelog.ReadSession || firsts[1].Level != statelog.ReadLinearizable ||
		!firsts[0].Since.IsZero() || !firsts[1].Since.IsZero() {
		t.Fatalf("page-1 reads %+v, want the whole answer at session, then at linearizable", firsts)
	}
	if s.Position != at || s.Kind != cache.SyncFull {
		t.Errorf("the restarted cache was answered %s at %s, want a full round at %s", s.Kind, s.Position, at)
	}
	if got := w.image(t, c, repo); got != before {
		t.Errorf("a full answer behind the restarted cache changed it:\n%s\nwas:\n%s", got, before)
	}
	if !cache.Converged(c, repo) {
		t.Error("the round the log's head answered left the incarnation unconverged")
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "rewound"); got != 0 {
		t.Errorf("%s{reason=rewound} = %d for a copy that was only behind, want 0", metrics.CodeCacheRebuilds, got)
	}
}

// A LOG RESTORED BEHIND THE CACHE IS FOLLOWED BACK. A broker brought back from
// an older copy keeps its stream's identity (ADR-0076), so nothing but where it
// ends says it went back — and only a read at the log's head can say that: a
// linearizable answer below a position this cache read, whether its own
// repository's or another's. The cache then reads the repository whole and
// converges to the rows the log holds — the refs and packs written after the
// copy gone — and marks every other repository on the node to be read whole
// at its next round, answering nothing from them at once until then.
func TestALogRestoredBehindTheCacheIsFollowedBack(t *testing.T) {
	t.Parallel()
	restored := func(t *testing.T, w *syncWorld, c *cache.Cache, repo string) {
		t.Helper()
		if refs := read(t, filepath.Join(w.dir(t, c, repo), "packed-refs")); strings.Contains(refs, "refs/heads/lost") {
			t.Errorf("the cache still serves a ref the restored log never held:\n%s", refs)
		}
		if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
			t.Errorf("the image after the restore:\n%s\nwant the restored rows':\n%s", got, w.rebuilt(t, repo))
		}
		if len(w.logs.lines(t, "code_cache_log_rewound")) == 0 {
			t.Error("no code_cache_log_rewound line")
		}
	}

	t.Run("after a restart", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		earlier := w.log.state()
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/lost", nil)
		c := w.open(t)
		ahead := w.sync(t, c, repo).Position

		w.log.restore(earlier)
		c = w.reopen(t, c)
		// A stale caller of the restarted cache is not answered from the
		// state a previous process left, and its whole answer below the
		// cache is asked of the log's head, which says the log went back.
		s, err := c.Sync(t.Context(), repo, stale)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Release()
		if s.Kind == cache.SyncNone {
			t.Fatal("a stale caller after the restart was answered from the previous process's state")
		}
		if before, _ := s.Position.Before(ahead); !before {
			t.Errorf("Synced.Position %s, want the restored log's, below %s", s.Position, ahead)
		}
		restored(t, w, c, repo)
		if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "rewound"); got != 1 {
			t.Errorf("%s{reason=rewound} = %d, want 1", metrics.CodeCacheRebuilds, got)
		}
	})

	t.Run("in the same process", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		earlier := w.log.state()
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/lost", nil)
		c := w.open(t)
		w.sync(t, c, repo).Release()
		w.sync(t, c, repo).Release() // converged: the next round is a delta

		w.log.restore(earlier)
		pages := len(w.log.firstPages(repo))
		s := w.sync(t, c, repo)
		if firsts := w.log.firstPages(repo)[pages:]; len(firsts) != 2 || firsts[0].Since.IsZero() ||
			!firsts[1].Since.IsZero() {
			t.Errorf("page-1 reads %+v, want the delta then the whole answer", firsts)
		}
		if s.Kind != cache.SyncFull {
			t.Errorf("Kind = %s, want full", s.Kind)
		}
		restored(t, w, c, repo)
	})

	t.Run("seen through another repository", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		quiet, busy := w.repo(t, 1), w.repo(t, 2)
		w.push(t, quiet, "refs/heads/main", nil)
		w.push(t, busy, "refs/heads/main", nil)
		earlier := w.log.state()
		c := w.open(t)
		w.sync(t, c, quiet).Release()
		w.push(t, busy, "refs/heads/lost", nil)
		w.push(t, busy, "refs/heads/main", nil)
		w.sync(t, c, busy).Release()

		// The log goes back to before busy's pushes and is written again:
		// quiet's own position is still the log's, and only the mark of
		// what busy read says the log went back.
		w.log.restore(earlier)
		w.push(t, quiet, "refs/heads/main", nil)
		w.sync(t, c, quiet).Release()
		if got := w.image(t, c, quiet); got != w.rebuilt(t, quiet) {
			t.Errorf("quiet's image:\n%s\nwant:\n%s", got, w.rebuilt(t, quiet))
		}
		if cache.Converged(c, busy) {
			t.Fatal("a repository ahead of the restored log is still answered at once")
		}
		s, err := c.Sync(t.Context(), busy, stale)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Release()
		if s.Kind != cache.SyncFull {
			t.Errorf("busy's stale caller cost %s, want a full round", s.Kind)
		}
		restored(t, w, c, busy)
	})
}

// MANY PAGES INSTALL WHAT ONE PAGE WOULD: a snapshot read seven rows at a time
// is the same snapshot.
func TestManyPagesInstallWhatOnePageWould(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	var tips []string
	for range 40 {
		tip, _ := w.push(t, repo, "refs/heads/main", nil)
		tips = append(tips, tip)
	}
	refs := map[string]string{}
	for i := range 300 {
		refs[fmt.Sprintf("refs/heads/b%03d", i)] = tips[i%len(tips)]
	}
	w.log.write(repo, nil, refs)
	w.log.set(func(f *fakeLog) { f.pageRows = 7 })
	c := w.open(t)
	w.sync(t, c, repo).Release()
	if n := len(w.log.callsOf(repo)); n < 340/7 {
		t.Fatalf("the snapshot took %d calls, want one a page of seven", n)
	}
	w.log.set(func(f *fakeLog) { f.pageRows = 0 })
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("seven rows a page:\n%s\nwant one page's:\n%s", got, w.rebuilt(t, repo))
	}
}

// EVERY PAGE THE ESTATE ANSWERS IS TIMED BY WHAT IT SERVED: a full read's
// every page under `full`, a delta's under `delta` —
// crewlet.code.cache.read.page.duration is production's reading of the page
// MaxFullRounds was measured against, so a page it misses is a full read the
// cap's evidence never sees — and a page the estate refuses is no page
// served, timed under neither.
//
// Mutation: time page 1 alone, and a full read of many pages counts one;
// time a page by the read it was asked as, and a delta's page counts as full.
func TestEveryPageTheEstateAnswersIsTimedByWhatItServed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	refs := map[string]string{}
	for i := range 20 {
		refs[fmt.Sprintf("refs/heads/b%02d", i)] = tip
	}
	w.log.write(repo, nil, refs)
	w.log.set(func(f *fakeLog) { f.pageRows = 3 })
	c := w.open(t)
	timed := func(kind cache.SyncKind) uint64 {
		return histogramCount(w.rec, metrics.CodeCacheReadPageDuration, "kind", string(kind))
	}

	w.sync(t, c, repo).Release()
	full := uint64(len(w.log.callsOf(repo)))
	if full < 2 {
		t.Fatalf("the full read took %d pages, want several to time", full)
	}
	if got := timed(cache.SyncFull); got != full {
		t.Errorf("a full read of %d pages timed %d under full, want every one", full, got)
	}
	if got := timed(cache.SyncDelta); got != 0 {
		t.Errorf("a full read timed %d pages under delta, want none", got)
	}

	w.push(t, repo, "refs/heads/main", nil)
	before := len(w.log.callsOf(repo))
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatal(err)
	}
	s.Release()
	if s.Kind != cache.SyncDelta {
		t.Fatalf("the sync after a push was a %s round, want a delta", s.Kind)
	}
	delta := uint64(len(w.log.callsOf(repo)) - before)
	if got := timed(cache.SyncDelta); got != delta {
		t.Errorf("a delta of %d pages timed %d under delta, want every one", delta, got)
	}
	if got := timed(cache.SyncFull); got != full {
		t.Errorf("a delta moved the full pages timed from %d to %d", full, got)
	}

	w.push(t, repo, "refs/heads/main", nil)
	w.log.set(func(f *fakeLog) {
		f.hook = func(fakeCall) error { return errors.New("the data node did not answer") }
	})
	if _, err := c.Sync(t.Context(), repo, linearizable); !errors.Is(err, cache.ErrLogRead) {
		t.Fatalf("a sync whose page the estate refused: %v, want ErrLogRead", err)
	}
	if gotFull, gotDelta := timed(cache.SyncFull), timed(cache.SyncDelta); gotFull != full || gotDelta != delta {
		t.Errorf("a refused page was timed: %d full and %d delta, want %d and %d", gotFull, gotDelta, full, delta)
	}
}

// A TAIL PAGE SERVES THE PACKS ITS REFS NEED: a push that lands between two
// pages of a full snapshot moves a ref the later page serves, and adds a pack
// the packs phase has already passed (its id sorts below every other) — the
// tail is what installs it.
func TestATailPageServesThePacksItsRefsNeed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 5 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	tip, row := w.commit(t, repo, nil, "00000000-0000-7000-8000-000000000000")
	var landed atomic.Bool
	w.log.set(func(f *fakeLog) {
		f.pageRows = 3
		f.hook = func(call fakeCall) error {
			if call.Page == 2 && landed.CompareAndSwap(false, true) {
				w.log.write(repo, []code.PackRow{row}, map[string]string{"refs/heads/main": tip})
			}
			return nil
		}
	})
	c := w.open(t)
	s := w.sync(t, c, repo)
	if !statOK(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum+".idx")) {
		t.Fatal("the pack a push added between pages, below the packs phase's cursor, was not installed")
	}
	w.log.set(func(f *fakeLog) { f.hook = nil })
	if _, err := runGit(t, w.pool, s.Dir, "", "cat-file", "-e", tip); err != nil {
		t.Errorf("the new tip is not readable in the cache: %v", err)
	}
}

// A REF MOVED BETWEEN TWO PAGES OF A DELTA TAKES ITS NEWEST ROW: a delta's
// refs are served by position, so a ref that moves after a page served it is
// served again at its new position — and the second row is the one that is
// true.
func TestARefMovedBetweenPagesTakesItsNewestRow(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	first, _ := w.push(t, repo, "refs/heads/main", nil)
	second, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()

	w.log.write(repo, nil, map[string]string{"refs/heads/x": first, "refs/heads/y": first})
	var moved atomic.Bool
	w.log.set(func(f *fakeLog) {
		f.pageRows = 1
		f.hook = func(call fakeCall) error {
			if call.Page == 2 && moved.CompareAndSwap(false, true) {
				w.log.write(repo, nil, map[string]string{"refs/heads/x": second})
			}
			return nil
		}
	})
	s := w.sync(t, c, repo)
	if refs := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(refs, second+" refs/heads/x\n") {
		t.Errorf("a ref moved between pages kept its older row:\n%s", refs)
	}
}

// retiredAt is row's retirement at position at, as a retired phase serves one.
func retiredAt(row code.PackRow, at statelog.Position) code.RetiredRow {
	return code.RetiredRow{PackID: row.PackID, Part: row.Part, GitChecksum: row.GitChecksum,
		RetiredAt: time.Unix(1_800_000_000, 0).UTC(), Position: at}
}

// A ROW THE CACHE CANNOT INSTALL FAILS THE SYNC, before anything is written: a
// ref goes into a file git parses line by line, an object id is a path, a
// pack this build does not split is no pack it can install, and a retirement
// the snapshot cannot place is none it can take out.
func TestARowTheCacheCannotInstallFailsTheSync(t *testing.T) {
	t.Parallel()
	ref := func(name, oid string) func(*code.RepoSnapshot, code.PackRow, string) {
		return func(s *code.RepoSnapshot, _ code.PackRow, _ string) {
			s.Refs = append(s.Refs, code.RefRow{Ref: name, OID: oid, Position: s.Position})
		}
	}
	pack := func(alter func(*code.PackRow)) func(*code.RepoSnapshot, code.PackRow, string) {
		return func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			alter(&row)
			s.Packs = append(s.Packs, row)
		}
	}
	for _, c := range []struct {
		name  string
		full  bool
		pages bool
		alter func(s *code.RepoSnapshot, row code.PackRow, tip string)
	}{
		{name: "a ref with a space", alter: ref("refs/heads/a b", strings.Repeat("a", 40))},
		{name: "a ref with a newline", alter: ref("refs/heads/a\nb", strings.Repeat("a", 40))},
		{name: "a ref of one level", alter: ref("refs/x", strings.Repeat("a", 40))},
		{name: "an oid of 39 digits", alter: ref("refs/heads/x", strings.Repeat("a", 39))},
		{name: "the zero oid", alter: ref("refs/heads/x", gitref.ZeroOID)},
		{name: "a full answer's empty oid", full: true, alter: ref("refs/heads/x", "")},
		{name: "a part that is not a bundle", alter: pack(func(p *code.PackRow) { p.Part = "idx" })},
		{name: "a zero checksum", alter: pack(func(p *code.PackRow) { p.GitChecksum = gitref.ZeroOID })},
		{name: "an empty object", alter: pack(func(p *code.PackRow) { p.Size = 0 })},
		{name: "past the largest pack", alter: pack(func(p *code.PackRow) { p.Size = code.MaxPackBytes + 1 })},
		{name: "an index a byte long", alter: pack(func(p *code.PackRow) { p.IndexBytes++ })},
		{name: "a reverse index four short", alter: pack(func(p *code.PackRow) { p.ReverseBytes -= 4 })},
		{name: "a pack section of 31 bytes", alter: pack(func(p *code.PackRow) {
			p.Size = 31 + p.IndexBytes + p.ReverseBytes
		})},
		{name: "a default branch that is an option", alter: func(s *code.RepoSnapshot, _ code.PackRow, _ string) {
			s.Repo.DefaultBranch = "-a"
		}},
		{name: "a later page at another position", pages: true, alter: func(s *code.RepoSnapshot, _ code.PackRow, _ string) {
			if s.Next == nil {
				s.Position.Seq++
			}
		}},
		{name: "one pack twice with two objects", alter: func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			other := row
			other.Object[0]++
			s.Packs = append(s.Packs, row, other)
		}},
		{name: "a ref twice in a full answer", full: true, alter: func(s *code.RepoSnapshot, _ code.PackRow, tip string) {
			s.Refs = append(s.Refs, code.RefRow{Ref: "refs/heads/main", OID: tip, Position: s.Position})
		}},
		// THE RETIRED PHASE: what a round takes out of the copy (swap.go), so
		// a retirement it cannot place — a floor that is not the one asked, a
		// row outside the phase's bounds or out of its order — fails the sync
		// as any other row does.
		{name: "a delta serving the retirements above another floor", alter: func(s *code.RepoSnapshot, _ code.PackRow,
			_ string) {
			s.RetiredFrom.Seq--
		}},
		{name: "a full answer serving the retirements above another floor than asked", full: true,
			alter: func(s *code.RepoSnapshot, _ code.PackRow, _ string) { s.RetiredFrom.Seq-- }},
		{name: "a later page serving another floor", pages: true, alter: func(s *code.RepoSnapshot, _ code.PackRow,
			_ string) {
			if s.Next == nil {
				s.RetiredFrom.Seq--
			}
		}},
		{name: "a retirement in a full answer that serves none", full: true, alter: func(s *code.RepoSnapshot,
			row code.PackRow, _ string) {
			s.RetiredFrom = statelog.Position{}
			s.Retired = append(s.Retired, retiredAt(row, s.Position))
		}},
		{name: "a retirement of no checksum", alter: func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			r := retiredAt(row, s.Position)
			r.GitChecksum = "pack-" + row.GitChecksum
			s.Retired = append(s.Retired, r)
		}},
		{name: "a retirement at the delta's floor", alter: func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			s.Retired = append(s.Retired, retiredAt(row, s.RetiredFrom))
		}},
		{name: "a retirement past the answer's position", alter: func(s *code.RepoSnapshot, row code.PackRow,
			_ string) {
			past := s.Position
			past.Seq++
			s.Retired = append(s.Retired, retiredAt(row, past))
		}},
		{name: "a retirement on another log", alter: func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			r := retiredAt(row, s.Position)
			r.Position.Stream = "another-log"
			s.Retired = append(s.Retired, r)
		}},
		{name: "two retirements out of their order", alter: func(s *code.RepoSnapshot, row code.PackRow, _ string) {
			s.Retired = append(s.Retired, retiredAt(row, s.Position), retiredAt(row, s.Position))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			tip, row := w.push(t, repo, "refs/heads/main", nil)
			cc := w.open(t)
			s := w.sync(t, cc, repo)
			refsBefore, packsBefore := read(t, filepath.Join(s.Dir, "packed-refs")), packFiles(t, s.Dir)
			s.Release()
			if c.full {
				cc = w.reopen(t, cc)
			}
			w.log.write(repo, nil, map[string]string{"refs/heads/p1": tip, "refs/heads/p2": tip})
			w.log.set(func(f *fakeLog) {
				if c.pages {
					f.pageRows = 1
				}
				f.after = func(_ fakeCall, snap *code.RepoSnapshot) { c.alter(snap, row, tip) }
			})
			_, err := cc.Sync(t.Context(), repo, linearizable)
			if !errors.Is(err, cache.ErrBadSnapshot) {
				t.Fatalf("Sync: %v, want ErrBadSnapshot", err)
			}
			if !strings.Contains(err.Error(), "page") {
				t.Errorf("the refusal %q names no page", err)
			}
			dir := w.dir(t, cc, repo)
			if got := read(t, filepath.Join(dir, "packed-refs")); got != refsBefore {
				t.Errorf("a refused snapshot rewrote packed-refs:\n%s", got)
			}
			if got := packFiles(t, dir); strings.Join(got, ",") != strings.Join(packsBefore, ",") {
				t.Errorf("a refused snapshot changed the packs: %v, was %v", got, packsBefore)
			}
		})
	}
}
