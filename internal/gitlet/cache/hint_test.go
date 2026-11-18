package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// hashLog records every hash of a hint's pack (cache.SetHashHook): the pack
// and the bound each was given, in order.
type hashLog struct {
	mu     sync.Mutex
	packs  []string
	bounds map[string]time.Duration
	// during, when set, runs inside each hash's first read slot.
	during func()
}

func (l *hashLog) hook(pack string, bound time.Duration) {
	l.mu.Lock()
	l.packs = append(l.packs, pack)
	if l.bounds == nil {
		l.bounds = map[string]time.Duration{}
	}
	l.bounds[pack] = bound
	during := l.during
	l.mu.Unlock()
	if during != nil {
		during()
	}
}

func (l *hashLog) hashed() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.packs)
}

func (l *hashLog) bound(pack string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.bounds[pack]
}

// A HINT IS CONFIRMED AGAINST THE PACK'S ROW, and its verdict is kept FOR THE
// FILES IT WAS REACHED ON. ConfirmHint hashes the hinted pack's files against
// its row, bounded by HashBudget (never CheckBudget: a hint runs no fsck): a
// flipped byte is RebuildCorrupt, a sound pack and a pack no row names (at the
// log's head) no repair. A second hint hashes nothing, nor does one after an
// adoption or a sync that leaves these packs' files alone; only a pack's own
// files replaced make the next hint hash it again.
//
// Mutations: answer RebuildCorrupt for every hint (the sound arm repairs);
// forget a verdict, or drop every verdict at a write into the copy (a later
// hint hashes again); keep a verdict over files replaced (the hint after the
// reinstall hashes nothing); bound the hash by CheckBudget.
func TestAHintIsConfirmedAgainstThePacksRow(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, damagedRow := w.push(t, repo, "refs/heads/main", nil)
	_, soundRow := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	var log hashLog
	cache.SetHashHook(c, log.hook)
	s := w.sync(t, c, repo)
	pack := filepath.Join(s.Dir, "objects", "pack", "pack-"+damagedRow.GitChecksum+".pack")
	b, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	overwriteBytes(t, pack, 20, []byte{b[20] ^ 0xff})
	object := strings.Repeat("ab", 20)
	noRows := strings.Repeat("cd", 20)
	asks := func(s cache.Synced, round string) {
		t.Helper()
		if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: damagedRow.GitChecksum}); !ok ||
			f.Reason != cache.RebuildCorrupt || !slices.Equal(f.Packs, []string{damagedRow.GitChecksum}) {
			t.Errorf("%s: a hint naming a pack not its row's: ConfirmHint = %+v, %v; want corrupt naming the pack",
				round, f, ok)
		}
		if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: soundRow.GitChecksum}); ok {
			t.Errorf("%s: a hint naming a sound pack: ConfirmHint = %+v, want no repair", round, f)
		}
		if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: noRows}); ok {
			t.Errorf("%s: a hint naming no row's pack: ConfirmHint = %+v, want no repair", round, f)
		}
	}
	asks(s, "first")
	asks(s, "second")
	if got, want := log.hashed(), []string{damagedRow.GitChecksum, soundRow.GitChecksum}; !slices.Equal(got, want) {
		t.Errorf("hashed %v, want %v: the damaged pack once, then the sound one once", got, want)
	}
	for _, row := range []gitlet.PackRow{damagedRow, soundRow} {
		ref := row.Ref()
		bytes := ref.PackBytes() + ref.IndexBytes + ref.ReverseBytes
		want := cache.HashBudget(bytes)
		if got := log.bound(row.GitChecksum); got != want {
			t.Errorf("pack %s's hash was bounded by %v, want HashBudget's %v (CheckBudget's would be %v)",
				row.GitChecksum, got, want, cache.CheckBudget(bytes))
		}
	}
	for outcome, want := range map[string]uint64{"confirmed": 2, "sound": 2, "unknown": 2} {
		if got := counter(w.rec, metrics.CodeCacheHints, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheHints, outcome, got, want)
		}
	}
	for outcome, want := range map[string]uint64{"confirmed": 1, "sound": 1, "unknown": 1} {
		if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheHintHashes, outcome, got, want)
		}
	}

	// Writes into the copy that leave both packs' files alone: an adoption
	// of a push's pack, and a sync that installs another.
	a := w.receiveOver(t, c, s, repo)
	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	w.push(t, repo, "refs/heads/main", nil)
	s2 := w.sync(t, c, repo)
	if cache.Incarnation(s2) != cache.Incarnation(s) {
		t.Fatalf("the push moved the cache to incarnation %d", cache.Incarnation(s2))
	}
	asks(s2, "after an adoption and a sync")
	if got := log.hashed(); len(got) != 2 {
		t.Errorf("hashed %v after writes that left the packs' files alone, want the first two hashes alone", got)
	}
	if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", "unknown"); got != 1 {
		t.Errorf("%s{outcome=unknown} = %d after the writes, want the one verdict kept", metrics.CodeCacheHintHashes, got)
	}

	// The sound pack's index replaced by a copy of itself: other files, so
	// its next hint hashes it again.
	idx := filepath.Join(s2.Dir, "objects", "pack", "pack-"+soundRow.GitChecksum+".idx")
	body, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx+".tmp", body, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(idx+".tmp", idx); err != nil {
		t.Fatal(err)
	}
	if f, ok := c.ConfirmHint(t.Context(), s2, gitproc.Hint{Object: object, Pack: soundRow.GitChecksum}); ok {
		t.Errorf("a hint naming a sound pack whose index was replaced: ConfirmHint = %+v, want no repair", f)
	}
	if got := log.hashed(); len(got) != 3 || got[2] != soundRow.GitChecksum {
		t.Errorf("hashed %v, want the sound pack hashed again once its files were replaced", got)
	}
}

// ONE HASH A PACK HOWEVER MANY HINTS ASK, AND IT OUTLIVES THEM. Confirmations
// asked while a hash of the pack runs wait for it and are answered its
// verdict; the pack is hashed once. The hash is the cache's, not its first
// asker's: a caller that leaves mid-hash is answered gone, and the hash goes
// on to answer those asked after.
//
// The case runs in a synctest bubble and lets the hash go only after
// synctest.Wait, so all eight askers are parked on the verdict while the hash
// is held; one arriving after the hash ended would hash nothing anyway.
//
// Mutations: hash in every confirmation (the pack is hashed nine times); hash
// on the first asker's context (the askers after it get no verdict).
func TestOneHashAPackHowManyHintsAskAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		_, row := w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		s := w.sync(t, c, repo)
		pack := filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum+".pack")
		b, err := os.ReadFile(pack)
		if err != nil {
			t.Fatal(err)
		}
		overwriteBytes(t, pack, 20, []byte{b[20] ^ 0xff})
		hint := gitproc.Hint{Object: strings.Repeat("ab", 20), Pack: row.GitChecksum}

		started, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		log := hashLog{during: func() {
			once.Do(func() { close(started) })
			<-release
		}}
		cache.SetHashHook(c, log.hook)

		// The first asker leaves while its hash runs.
		first, leave := context.WithCancel(t.Context())
		firstAnswer := make(chan bool, 1)
		go func() {
			_, ok := c.ConfirmHint(first, s, hint)
			firstAnswer <- ok
		}()
		select {
		case <-started:
		case <-time.After(hangDetector):
			t.Fatal("the first confirmation's hash never started")
		}
		leave()
		if ok := <-firstAnswer; ok {
			t.Error("a confirmation whose caller left mid-hash answered a repair")
		}

		// Eight more ask while that same hash still runs.
		const asks = 8
		var wg sync.WaitGroup
		answers := make(chan bool, asks)
		for range asks {
			wg.Go(func() {
				ask, done := context.WithTimeout(t.Context(), hangDetector)
				defer done()
				f, ok := c.ConfirmHint(ask, s, hint)
				answers <- ok && f.Reason == cache.RebuildCorrupt
			})
		}
		// Every confirmation parked on its verdict before the hash is let go.
		synctest.Wait()
		close(release)
		wg.Wait()
		close(answers)
		for ok := range answers {
			if !ok {
				t.Errorf("a confirmation asked while the first asker's hash ran was not answered its verdict, corrupt")
			}
		}
		if got := log.hashed(); len(got) != 1 {
			t.Errorf("hashed %d times, want once for all %d confirmations, the one that left included", len(got),
				asks+1)
		}
		for outcome, want := range map[string]uint64{"gone": 1, "confirmed": asks} {
			if got := counter(w.rec, metrics.CodeCacheHints, "outcome", outcome); got != want {
				t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheHints, outcome, got, want)
			}
		}
		if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", "confirmed"); got != 1 {
			t.Errorf("%s{outcome=confirmed} = %d, want the one hash", metrics.CodeCacheHintHashes, got)
		}
	})
}

// A HINT'S HASH RUNS IN A READ SLOT, a piece at a time (gitproc.Pool.HoldRead):
// hashing is core work requests wait on, so many repositories damaged at once
// hash on the read lane's cores, not beside them. Inside the hash the lane has
// a slot fewer free.
//
// Mutation: hash in no slot (hashBundle given no hold): the lane reads all free.
func TestAHintsHashRunsInAReadSlot(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	slots := w.pool.Slots(gitproc.ClassRead)
	var free []int
	log := hashLog{during: func() { free = append(free, w.pool.Free(gitproc.ClassRead)) }}
	cache.SetHashHook(c, log.hook)
	if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: strings.Repeat("ab", 20), Pack: row.GitChecksum}); ok {
		t.Fatalf("a hint naming a sound pack: ConfirmHint = %+v, want no repair", f)
	}
	if len(free) != 1 || free[0] != slots-1 {
		t.Errorf("inside the hash the read lane had %v of %d slots free, want %d", free, slots, slots-1)
	}
}

// A HINT READS ONLY THE PACKS OF ITS ROWS. Without a full read of the rows, a
// confirmation looks its pack up in a full snapshot's pack pages (a full
// snapshot serves every pack before any ref), never every ref. A pack those
// pages do not name is asked once of the log's head, linearizable, and that
// verdict is kept: the next hint about it reads nothing.
//
// Mutation: read the whole snapshot (the refs' pages are read too).
func TestAHintReadsOnlyThePacksOfItsRows(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.log.set(func(f *fakeLog) { f.pageRows = 4 })
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	head, row := w.push(t, repo, "refs/heads/main", nil)
	refs := map[string]string{}
	for i := range 40 {
		refs["refs/heads/b"+strconv.Itoa(i)] = head
	}
	w.log.write(repo, nil, refs)
	c := w.open(t)
	s := w.sync(t, c, repo)
	object := strings.Repeat("ab", 20)

	before := len(w.log.callsOf(repo))
	if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: row.GitChecksum}); ok {
		t.Fatalf("a hint naming a sound pack: ConfirmHint = %+v, want no repair", f)
	}
	calls := w.log.callsOf(repo)[before:]
	if len(calls) != 1 || calls[0].Level != statelog.ReadSession || calls[0].Page != 1 {
		t.Errorf("the confirmation read %+v, want the one page of the snapshot's packs, at session", calls)
	}

	before = len(w.log.callsOf(repo))
	noRows := strings.Repeat("cd", 20)
	if f, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: noRows}); ok {
		t.Fatalf("a hint naming no row's pack: ConfirmHint = %+v, want no repair", f)
	}
	calls = w.log.callsOf(repo)[before:]
	if len(calls) != 2 || calls[0].Level != statelog.ReadSession || calls[1].Level != statelog.ReadLinearizable {
		t.Errorf("a pack no row names read %+v, want the packs' page at session and then at the log's head", calls)
	}
	before = len(w.log.callsOf(repo))
	if _, ok := c.ConfirmHint(t.Context(), s, gitproc.Hint{Object: object, Pack: noRows}); ok {
		t.Fatal("a second hint naming no row's pack answered a repair")
	}
	if calls := w.log.callsOf(repo)[before:]; len(calls) != 0 {
		t.Errorf("a second hint naming no row's pack read %+v, want the verdict kept", calls)
	}
}

// A ROUND HASHES ITS HINT WITHOUT THE WRITE LOCK. A round's own git (the
// peel's cat-file) can name damage only in words; the hash of the pack it
// nominates is made once the round has let the entry's write lock go
// (TestARoundEndsBeforeItsHintIsHashed covers the rest of the repository). A
// sound pack answers the round's callers git's own failure and keeps that
// verdict when git freshens it; a pack not its row's answers them with a
// request's repair: THAT pack fetched again in place, one GET, `stored`.
//
// Mutations: confirm the hint under the round's lock; answer every round's
// hint with git's failure; ask the whole rebuild (damagedRound's askRebuild
// with nothing seeded: a new incarnation); read a .pack's modification time
// into its identity (check.go's fileID: the freshened pack is hashed again).
func TestARoundHashesItsHintWithoutTheWriteLock(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	flag := filepath.Join(t.TempDir(), "hint")
	w.gitPath = gittest.Wrap(t, `if [ "$1" = cat-file ] && [ -e '`+flag+`' ]; then cat '`+flag+`' >&3; exit 128; fi`)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	w.tag(t, repo, "v1", true)
	c := w.open(t)
	held := map[bool]int{}
	var mu sync.Mutex
	// during removes the stand-in's flag, once set: the repair's own peel,
	// in the rebuilt copy, then answers.
	var unflag bool
	log := hashLog{during: func() {
		mu.Lock()
		defer mu.Unlock()
		held[cache.WriteLockHeld(c, repo)]++
		if unflag {
			if err := os.Remove(flag); err != nil {
				t.Error(err)
			}
		}
	}}
	cache.SetHashHook(c, log.hook)
	first := w.sync(t, c, repo)

	// The next round's peel names the sound pack corrupt in words.
	tag := w.tag(t, repo, "v2", true)
	event := `{"event":"error","sid":"stand-in","thread":"main","msg":"packed object ` + tag +
		` (stored in ./objects/pack/pack-` + row.GitChecksum + `.pack) is corrupt","fmt":"%s"}` + "\n"
	if err := os.WriteFile(flag, []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) || len(exit.Result.Trace.Hints) != 1 {
		t.Fatalf("a sync whose peel names a sound pack in words: %v, want git's own failure carrying its hint", err)
	}
	mu.Lock()
	if held[true] != 0 || held[false] != 1 {
		t.Errorf("the round's hint was hashed %d times with the write lock held and %d without, want once without",
			held[true], held[false])
	}
	mu.Unlock()
	if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", "sound"); got != 1 {
		t.Errorf("%s{outcome=sound} = %d, want the round's one hash", metrics.CodeCacheHintHashes, got)
	}

	// A FRESHENED pack (git moves its time, not its bytes, when a process
	// writes an object it holds) keeps its verdict: the next hint hashes nothing.
	pack := filepath.Join(first.Dir, "objects", "pack", "pack-"+row.GitChecksum+".pack")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(pack, later, later); err != nil {
		t.Fatal(err)
	}
	_, err = c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	if !errors.As(err, &exit) || len(exit.Result.Trace.Hints) != 1 {
		t.Fatalf("a sync whose peel names a freshened sound pack in words: %v, want git's own failure carrying "+
			"its hint", err)
	}
	if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", "sound"); got != 1 {
		t.Errorf("%s{outcome=sound} = %d once the pack was freshened, want its verdict kept: one hash",
			metrics.CodeCacheHintHashes, got)
	}
	if got := counter(w.rec, metrics.CodeCacheHints, "outcome", "sound"); got != 2 {
		t.Errorf("%s{outcome=sound} = %d, want both namings answered sound", metrics.CodeCacheHints, got)
	}
	mu.Lock()
	unflag = true
	mu.Unlock()

	// The pack's name now holds another file whose bytes are not its row's,
	// renamed in as every writer of a copy places one (a write through the
	// inode keeps its identity and is the scrub's to find): the next hint is
	// confirmed, and the round's callers answered by THAT pack's repair in place.
	b, err := os.ReadFile(pack)
	if err != nil {
		t.Fatal(err)
	}
	b[20] ^= 0xff
	staged := pack + ".staged"
	if err := os.WriteFile(staged, b, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, pack); err != nil {
		t.Fatal(err)
	}
	gets := w.objects.total()
	repaired, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("a sync whose peel names a damaged pack in words: %v, want it answered by the repair", err)
	}
	defer repaired.Release()
	if cache.Incarnation(repaired) != cache.Incarnation(first) {
		t.Errorf("the repair answered incarnation %d, want %d repaired in place", cache.Incarnation(repaired),
			cache.Incarnation(first))
	}
	// The tag's pack is the copy's since the sync before: the hinted pack,
	// by the repair, is the one download.
	if got := w.objects.total() - gets; got != 1 {
		t.Errorf("the sync and its repair downloaded %d bundles, want the hinted pack alone", got)
	}
	lines := w.logs.lines(t, "code_cache_pack_reinstalled")
	if len(lines) != 1 || lines[0]["found"] != "hint" || lines[0]["pack_id"] != row.PackID {
		t.Errorf("code_cache_pack_reinstalled = %v, want the hinted pack %s fetched again", lines, row.PackID)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
		t.Errorf("%s{reason=stored} = %d, want the one in-place repair the confirmed hint asked",
			metrics.CodeCacheRebuilds, got)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildCorrupt)); got != 0 {
		t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild", metrics.CodeCacheRebuilds, got)
	}
}

// A ROUND'S OWN PACK DAMAGE IS REPAIRED IN PLACE, as a request's is: one GET,
// never a whole rebuild. A new process's first round peels every tag, so a tag
// object damaged on disk (G: compressed bytes; S1: header size) before any
// maintenance (no bitmap or graph: git inflates it and names its pack) meets
// its own cat-file, on the node pin and floor, named by a bare "%s" (the pin's
// S1) or a format-table row with a hint (the pin's G; both on the floor). The
// hint, confirmed, names the pack to a forced round of the same incarnation:
// one GET of seven packs, `stored`, no scrub (reports{request}).
//
// Mutations: damagedRound's askRebuild unseeded (all seven downloaded);
// converge's hinted only for unclassified failures (checks{trigger=request});
// no reader on the peel (refs.go), or settleReader skipped (reports{queued}).
func TestARoundsOwnPackDamageIsRepairedInPlace(t *testing.T) {
	t.Parallel()
	const pushes = 6
	for _, git := range []struct {
		name string
		path func(testing.TB) string
	}{{"pin", gittest.RequireNode}, {"floor", gittest.NodeFloor}} {
		for _, damage := range []gittest.DamageCase{gittest.EntryBytes, gittest.EntrySize} {
			t.Run(git.name+"/"+string(damage), func(t *testing.T) {
				t.Parallel()
				w := newSyncWorld(t, onMemory)
				w.gitPath = git.path(t)
				repo := w.repo(t, 1)
				for range pushes {
					w.push(t, repo, "refs/heads/main", nil)
				}
				tag := w.tag(t, repo, "v1", true)
				c := w.open(t)
				s, err := c.Sync(t.Context(), repo, linearizable)
				if err != nil {
					t.Fatalf("the first sync: %v", err)
				}
				dir, n := s.Dir, cache.Incarnation(s)
				s.Release()
				gittest.DamageInPlace(t, dir, damage, tag)
				if _, err := runGit(t, w.pool, dir, "", "cat-file", "-p", tag); err == nil {
					t.Fatalf("the tag reads whole after damage %s: the case damaged nothing", damage)
				}

				c = w.reopen(t, c) // its first round is full, and peels every tag
				gets := w.objects.total()
				// The case's own read of the damaged tag reported it too.
				queued := counterOf(w.rec, metrics.CodeCacheReports, map[string]string{"outcome": "queued"})
				ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
				defer cancel()
				r, err := c.Sync(ctx, repo, linearizable)
				if err != nil {
					t.Fatalf("a sync whose own peel met a damaged tag object: %v, want its repair's answer", err)
				}
				defer r.Release()
				if got := cache.Incarnation(r); got != n {
					t.Errorf("the repair answered incarnation %d, want %d repaired in place", got, n)
				}
				if got := w.objects.total() - gets; got != 1 {
					t.Errorf("the repair downloaded %d bundles, want the tag's pack alone of %d", got, pushes+1)
				}
				lines := w.logs.lines(t, "code_cache_pack_reinstalled")
				if len(lines) != 1 || lines[0]["found"] != "hint" {
					t.Errorf("code_cache_pack_reinstalled = %v, want the tag's pack fetched again for its hint", lines)
				}
				if got := counter(w.rec, metrics.CodeCacheHints, "outcome", "confirmed"); got != 1 {
					t.Errorf("%s{outcome=confirmed} = %d, want the peel's one hint", metrics.CodeCacheHints, got)
				}
				for _, reason := range cache.RebuildReasons {
					want := uint64(0)
					if reason == cache.RebuildStored {
						want = 1
					}
					if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(reason)); got != want {
						t.Errorf("%s{reason=%s} = %d, want %d", metrics.CodeCacheRebuilds, reason, got, want)
					}
				}
				if got := checks(w, "pack", "damaged", "request"); got != 0 {
					t.Errorf("the repair's scrub found %d packs damaged, want none run: the hint named the pack", got)
				}
				got := counterOf(w.rec, metrics.CodeCacheReports, map[string]string{"outcome": "queued"}) - queued
				if got != 0 {
					t.Errorf("%s{outcome=queued} rose by %d, want the peel's report held for the round's repair",
						metrics.CodeCacheReports, got)
				}
				if got := counterOf(w.rec, metrics.CodeCacheReports, map[string]string{"outcome": "request"}); got == 0 {
					t.Errorf("%s{outcome=request} = 0, want the peel's report consumed by the round's repair",
						metrics.CodeCacheReports)
				}
				if _, err := runGit(t, w.pool, r.Dir, "", "cat-file", "-p", tag); err != nil {
					t.Errorf("the repaired copy still fails on the tag: %v", err)
				}
			})
		}
	}
}

// A DAMAGED PACK THE STORE WILL NOT HAND OVER IS ASKED FOR ONCE A WINDOW, and
// nothing else is downloaded. A round's repair of a hinted pack (G, seven
// packs) asks its one GET, which the store refuses, holding the pack off
// (repairMark.fetchFailed); the whole rebuild would need the same pack, so
// none is asked and the sync answers the refusal. Within a window no round
// asks the store again or is forced to recheck index trailers
// (checks{trigger=forced}); once the store answers, the next window installs
// the pack in place: same incarnation, one GET, `stored`.
//
// Mutations: ask the step-4 rebuild while a needed pack is held off (two asks
// a window), or unseeded (every pack each window); count a held-off pack for
// step 2's skip, or leave a failed forced round unsettled (forced checks).
func TestADamagedPackTheStoreRefusesIsAskedForOnceAWindow(t *testing.T) {
	t.Parallel()
	const pushes = 6
	for _, git := range []struct {
		name string
		path func(testing.TB) string
	}{{"pin", gittest.RequireNode}, {"floor", gittest.NodeFloor}} {
		t.Run(git.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			w.gitPath = git.path(t)
			clk := newClock()
			repo := w.repo(t, 1)
			for range pushes {
				w.push(t, repo, "refs/heads/main", nil)
			}
			tag, row := w.tagged(t, repo, "v1")
			c := w.open(t, clk.on)
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Fatalf("the first sync: %v", err)
			}
			dir, n := s.Dir, cache.Incarnation(s)
			s.Release()
			gittest.DamageInPlace(t, dir, gittest.EntryBytes, tag)
			refused := errors.New("the object store refuses the tag's pack")
			w.objects.mu.Lock()
			w.objects.fail = func(_ context.Context, k objstore.Key) error {
				if k == row.Object {
					return refused
				}
				return nil
			}
			w.objects.mu.Unlock()

			c = w.reopen(t, c, clk.on) // its first round is full, and peels every tag
			start, _, _ := w.objects.counts()
			total := w.objects.total()
			asked := func() (tagPack, all int) {
				got, _, _ := w.objects.counts()
				return got[row.Object] - start[row.Object], w.objects.total() - total
			}
			forced := func() uint64 {
				return counterOf(w.rec, metrics.CodeCacheChecks, map[string]string{"trigger": "forced"})
			}
			sync := func(what string) error {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
				defer cancel()
				s, err := c.Sync(ctx, repo, linearizable)
				if err == nil {
					s.Release()
					t.Fatalf("%s: answered, want the store's refusal of the tag's pack", what)
				}
				return err
			}
			for window := 1; window <= 3; window++ {
				if err := sync("window " + strconv.Itoa(window)); !errors.Is(err, refused) ||
					!errors.Is(err, cache.ErrStoreRead) {
					t.Errorf("window %d: %v, want the store's refusal of the tag's pack", window, err)
				}
				if tagPack, all := asked(); tagPack != window || all != window {
					t.Errorf("window %d: the store was asked for the tag's pack %d times and for %d bundles in "+
						"all, want once a window and nothing else", window, tagPack, all)
				}
				before := forced()
				for range 2 {
					if err := sync("within window " + strconv.Itoa(window)); !errors.Is(err, refused) {
						t.Errorf("within window %d: %v, want the store's refusal of the tag's pack", window, err)
					}
				}
				if tagPack, all := asked(); tagPack != window || all != window {
					t.Errorf("within window %d: the store was asked for the tag's pack %d times and for %d "+
						"bundles in all, want no download until the window passes", window, tagPack, all)
				}
				if got := forced() - before; got != 0 {
					t.Errorf("within window %d: %d files checked by forced rounds, want none: no forced round "+
						"can fetch the held-off pack", window, got)
				}
				if window == 1 {
					// A REQUEST whose confirmed hint names the held-off pack
					// takes the same path: no forced round, no download, the
					// store's refusal its answer.
					p, ok := cache.Pin(c, repo)
					if !ok {
						t.Fatal("no incarnation to pin")
					}
					before := forced()
					ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
					r, err := c.Rebuild(ctx, p, cache.Failure{Reason: cache.RebuildCorrupt,
						Packs: []string{row.GitChecksum}, Began: time.Now()})
					cancel()
					p.Release()
					if err == nil {
						r.Release()
					}
					if !errors.Is(err, refused) {
						t.Errorf("a request's repair naming the held-off pack: %v, want the store's refusal", err)
					}
					if tagPack, all := asked(); tagPack != window || all != window {
						t.Errorf("a request's repair asked the store for the tag's pack %d times and for %d "+
							"bundles in all, want no download until the window passes", tagPack, all)
					}
					if got := forced() - before; got != 0 {
						t.Errorf("a request's repair naming the held-off pack: %d files checked by forced rounds, "+
							"want none", got)
					}
				}
				clk.Add(cache.RebuildWindow(window) + time.Second)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildCorrupt)); got != 0 {
				t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild: it needs the held-off pack",
					metrics.CodeCacheRebuilds, got)
			}

			// The store answers again: the next window's repair installs the
			// pack in place.
			w.objects.mu.Lock()
			w.objects.fail = nil
			w.objects.mu.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
			defer cancel()
			r, err := c.Sync(ctx, repo, linearizable)
			if err != nil {
				t.Fatalf("once the store answers again: %v, want the repaired copy", err)
			}
			defer r.Release()
			if got := cache.Incarnation(r); got != n {
				t.Errorf("the repair answered incarnation %d, want %d repaired in place", got, n)
			}
			if tagPack, all := asked(); tagPack != 4 || all != 4 {
				t.Errorf("in all the store was asked for the tag's pack %d times and for %d bundles, want "+
					"once a window and nothing else", tagPack, all)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(cache.RebuildStored)); got != 1 {
				t.Errorf("%s{reason=stored} = %d, want the one pack installed again", metrics.CodeCacheRebuilds, got)
			}
			if _, err := runGit(t, w.pool, r.Dir, "", "cat-file", "-p", tag); err != nil {
				t.Errorf("the repaired copy still fails on the tag: %v", err)
			}
		})
	}
}

// heldHint is a round whose own peel named a SOUND pack in words, its hash
// held in the hash hook until letGo. The stand-in fails no peel once the hash
// has begun, so every later round answers.
type heldHint struct {
	w     *syncWorld
	c     *cache.Cache
	repo  string
	row   gitlet.PackRow // the pack the hint names
	first cache.Synced   // the copy the hinted round was a delta onto
	tag   string         // the tag the hinted round could not peel
	log   *hashLog
	// hinted is the hinted round's caller's answer: its incarnation (the
	// Synced released at once) and its error.
	hinted chan heldAnswer
	letGo  func()
}

type heldAnswer struct {
	incarnation int
	err         error
}

func holdAHint(t *testing.T) *heldHint {
	t.Helper()
	w := newSyncWorld(t, onMemory)
	flag := filepath.Join(t.TempDir(), "hint")
	w.gitPath = gittest.Wrap(t, `if [ "$1" = cat-file ] && [ -e '`+flag+`' ]; then cat '`+flag+`' >&3; exit 128; fi`)
	h := &heldHint{w: w, repo: w.repo(t, 1), hinted: make(chan heldAnswer, 1)}
	_, h.row = w.push(t, h.repo, "refs/heads/main", nil)
	w.tag(t, h.repo, "v1", true)
	h.c = w.open(t)
	// Let go on every path. Registered after the cache's cleanup, this runs
	// before Stop, which waits for the hash.
	hashing, release := make(chan struct{}), make(chan struct{})
	var began, let sync.Once
	h.letGo = func() { let.Do(func() { close(release) }) }
	t.Cleanup(h.letGo)
	h.log = &hashLog{during: func() {
		began.Do(func() { close(hashing) })
		<-release
	}}
	cache.SetHashHook(h.c, h.log.hook)
	h.first = w.sync(t, h.c, h.repo)

	h.tag = w.tag(t, h.repo, "v2", true)
	event := `{"event":"error","sid":"stand-in","thread":"main","msg":"packed object ` + h.tag +
		` (stored in ./objects/pack/pack-` + h.row.GitChecksum + `.pack) is corrupt","fmt":"%s"}` + "\n"
	if err := os.WriteFile(flag, []byte(event), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		s, err := h.c.Sync(t.Context(), h.repo, statelog.Freshness{Level: statelog.ReadLinearizable})
		var n int
		if err == nil {
			n = cache.Incarnation(s)
		}
		s.Release()
		h.hinted <- heldAnswer{incarnation: n, err: err}
	}()
	select {
	case <-hashing:
	case a := <-h.hinted:
		t.Fatalf("the sync whose peel names a pack in words was answered (%v) before the pack was hashed", a.err)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	return h
}

// unanswered fails the case when the hinted round's caller has its answer.
func (h *heldHint) unanswered(t *testing.T) {
	t.Helper()
	select {
	case a := <-h.hinted:
		t.Errorf("the hinted round's own caller was answered (%v) before the hash it waits on ended", a.err)
	default:
	}
}

// A ROUND ENDS BEFORE ITS HINT IS HASHED. A linearizable caller (every answer
// naming a ref) never rides a round that began before it asked, so a round
// whose git named a pack in words stops being the running round, its
// incarnation pinned, before it hashes: the round queued behind it answers
// during the hash, and only the hinted round's callers wait for the verdict,
// here git's own failure (the pack is sound).
//
// Mutation: confirm the hint while the round still runs (the linearizable
// sync waits for the hash).
func TestARoundEndsBeforeItsHintIsHashed(t *testing.T) {
	t.Parallel()
	h := holdAHint(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	s, err := h.c.Sync(ctx, h.repo, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Errorf("a linearizable sync asked while the round's hint was hashed: %v, want it answered by a round "+
			"of its own before the hash ends", err)
	} else {
		if got := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(got, h.tag+" refs/tags/v2") {
			t.Errorf("the sync answered during the hash holds packed-refs\n%s\nwant the tag the hinted round "+
				"could not peel, %s", got, h.tag)
		}
		s.Release()
	}
	h.unanswered(t)

	h.letGo()
	a := <-h.hinted
	var exit *gitproc.ExitError
	if !errors.As(a.err, &exit) || len(exit.Result.Trace.Hints) != 1 {
		t.Errorf("the hinted round's caller, once the hash of a sound pack ended: %v, want git's own failure "+
			"carrying its hint", a.err)
	}
	// One hash, whatever it decided: held past its bound by a loaded
	// machine it decides nothing, which answers the caller the same way.
	if got, want := h.log.hashed(), []string{h.row.GitChecksum}; !slices.Equal(got, want) {
		t.Errorf("hashed %v, want %v: the hinted round's pack, once", got, want)
	}
}

// A HINTED ROUND'S CALLERS FOLLOW THE COPY THAT REPLACED ITS OWN. A rebuild
// may run during the hash and replace the hinted round's incarnation; the
// round's callers are then queued again for a round on the current copy (as a
// Rebuild of a replaced copy is answered by a sync), never told git's failure
// in a directory they would no longer be handed.
//
// Mutation: answer them with the round's own answer (git's failure).
func TestAHintedRoundsCallersFollowTheCopyThatReplacedItsOwn(t *testing.T) {
	t.Parallel()
	h := holdAHint(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	rebuilt, err := h.c.Rebuild(ctx, h.first, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("a rebuild asked while the round's hint was hashed: %v, want it run before the hash ends", err)
	}
	defer rebuilt.Release()
	if cache.Incarnation(rebuilt) == cache.Incarnation(h.first) {
		t.Fatalf("the rebuild answered incarnation %d, the one it was asked to replace", cache.Incarnation(rebuilt))
	}
	h.unanswered(t)

	h.letGo()
	a := <-h.hinted
	switch {
	case a.err != nil:
		t.Errorf("the hinted round's caller, its copy replaced while the pack was hashed: %v, want it answered "+
			"by a round on the copy that replaced it", a.err)
	case a.incarnation != cache.Incarnation(rebuilt):
		t.Errorf("the hinted round's caller was handed incarnation %d, want the rebuilt %d", a.incarnation,
			cache.Incarnation(rebuilt))
	}
}

// A HINTED ROUND'S CALLERS ARE SYNCED AGAIN PAST A REPAIR. A request's repair
// may refetch the hinted pack during the hash, which then decides nothing; the
// round's git failed on what the repair may have replaced, so its callers are
// synced again on the copy as it stands, never told a failure it no longer has.
//
// Mutation: answer them git's failure whenever the hints confirm nothing (no
// predates clause in Cache.run's replaced).
func TestAHintedRoundsCallersAreSyncedAgainPastARepair(t *testing.T) {
	t.Parallel()
	h := holdAHint(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	repaired, err := h.c.Rebuild(ctx, h.first, cache.Failure{Reason: cache.RebuildCorrupt,
		Packs: []string{h.row.GitChecksum}, Began: time.Now()})
	if err != nil {
		t.Fatalf("a request's repair asked while the round's hint was hashed: %v, want it run before the hash ends",
			err)
	}
	defer repaired.Release()
	if cache.Incarnation(repaired) != cache.Incarnation(h.first) {
		t.Fatalf("the request's repair answered incarnation %d, want %d repaired in place",
			cache.Incarnation(repaired), cache.Incarnation(h.first))
	}
	if lines := h.w.logs.lines(t, "code_cache_pack_reinstalled"); len(lines) != 1 {
		t.Fatalf("code_cache_pack_reinstalled = %v, want the request's one reinstall of the hinted pack", lines)
	}
	h.unanswered(t)

	h.letGo()
	a := <-h.hinted
	switch {
	case a.err != nil:
		t.Errorf("the hinted round's caller, a repair written into its copy while the pack was hashed: %v, "+
			"want it synced again and answered", a.err)
	case a.incarnation != cache.Incarnation(h.first):
		t.Errorf("the hinted round's caller was handed incarnation %d, want %d", a.incarnation,
			cache.Incarnation(h.first))
	}
}
