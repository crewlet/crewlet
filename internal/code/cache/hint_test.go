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
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
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
// FILES IT WAS REACHED ON. git 2.56 names a corrupt packed object only
// through a bare "%s", which the pool records as a hint naming a pack
// (gitproc.Hint); ConfirmHint hashes that pack's three files, as the push
// stored them, against the row naming it — bounded by HashBudget of their
// bytes, never CheckBudget, whose fsck term a hint runs none of: a pack one
// byte of which was flipped is RebuildCorrupt (confirmed), a sound pack is no
// repair (sound), and a pack no row names, asked of the log's head, is no
// repair (unknown). A second hint about any of them hashes nothing. An
// adoption of another pack and a sync that installs another pack — writes
// into the copy that leave these packs' files alone — leave every verdict
// standing, so a copy whose rebuild a cooldown refuses is not hashed again at
// every push; and only a pack's own files replaced make the next hint about
// it hash again.
//
// Mutations: answer RebuildCorrupt for every hint (the sound arm repairs);
// forget a verdict (a second hint hashes again); drop every verdict at a
// write into the copy (the hints after the adoption hash again); keep a
// verdict over files replaced (the hint after the reinstall hashes nothing);
// bound the hash by CheckBudget (the bound it is given is not HashBudget's).
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
	for _, row := range []code.PackRow{damagedRow, soundRow} {
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

// ONE HASH A PACK HOWEVER MANY HINTS ASK, AND IT OUTLIVES THEM. Damage that
// fails a hot repository fails every request in flight on it together, each
// with the same hint: confirmations asked while a hash of the pack runs wait
// for it, and the pack is hashed once, every one answered its verdict. The
// hash is the CACHE'S, never its first asker's: a confirmation whose caller
// leaves mid-hash is answered no repair (gone), the hash goes on, and the
// confirmations asked after it left are answered from that same hash — a
// request a balancer cuts no longer takes the work of a slow disk's hash with
// it for the next to start again from byte zero.
//
// Mutations: hash in every confirmation (no hash in flight is joined) — the
// pack is hashed nine times; hash on the first asker's context — its leaving
// ends the hash, and the confirmations after it are answered no verdict.
func TestOneHashAPackHowManyHintsAskAtOnce(t *testing.T) {
	t.Parallel()
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
	case <-time.After(30 * time.Second):
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
			ask, done := context.WithTimeout(t.Context(), 30*time.Second)
			defer done()
			f, ok := c.ConfirmHint(ask, s, hint)
			answers <- ok && f.Reason == cache.RebuildCorrupt
		})
	}
	// Every confirmation has long since reached the pack's check: what is
	// left of one before it is a stat of three files and a map read.
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	close(answers)
	for ok := range answers {
		if !ok {
			t.Errorf("a confirmation asked while the first asker's hash ran was not answered its verdict, corrupt")
		}
	}
	if got := log.hashed(); len(got) != 1 {
		t.Errorf("hashed %d times, want once for all %d confirmations, the one that left included", len(got), asks+1)
	}
	for outcome, want := range map[string]uint64{"gone": 1, "confirmed": asks} {
		if got := counter(w.rec, metrics.CodeCacheHints, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheHints, outcome, got, want)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheHintHashes, "outcome", "confirmed"); got != 1 {
		t.Errorf("%s{outcome=confirmed} = %d, want the one hash", metrics.CodeCacheHintHashes, got)
	}
}

// A HINT'S HASH RUNS IN A READ SLOT. Hashing is a core's work requests wait
// on, so it takes a slot of the pool's read lane a piece at a time
// (gitproc.Pool.HoldRead) — as many hashes at once as the lane has slots, and
// none beside them, so a disk's incident that damages many repositories at
// once is hashed on the cores the read lane counts. Inside the hash, the lane
// has a slot fewer free.
//
// Mutation: hash in no slot (hashBundle given no hold) — the hash is never
// told it holds one, and the lane reads all free.
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

// A HINT READS ONLY THE PACKS OF ITS ROWS. A confirmation that holds no full
// read of the rows looks its pack up in a full snapshot's PACKS — the pages
// up to the first that serves a ref, since a full snapshot serves every pack
// before any ref — never every ref of a repository that may hold a hundred
// thousand. A pack those pages do not name is asked once of the log's head,
// linearizable, and no row naming it there is a verdict kept: the next hint
// about it reads nothing.
//
// Mutation: read the whole snapshot — the confirmation reads the refs' pages
// too.
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

// A ROUND HASHES ITS HINT WITHOUT THE WRITE LOCK. A round's own git — the
// peel's cat-file of the tags it sets — can name a damage only in words,
// and the round holds the pack it nominates to its row before it answers;
// that hash is minutes of a slow disk for the largest pack, so it is made
// once the round has let the entry's write lock go, which an adoption takes
// meanwhile (that nothing else of the repository waits on it either is
// TestARoundEndsBeforeItsHintIsHashed's). A sound pack answers the round's
// callers git's own failure, and keeps that verdict once git freshens it; a
// pack that is not its row's answers them with the repair a request's
// confirmed hint gets: THAT pack fetched again over its files in the same
// incarnation, one GET, counted `stored`.
//
// Mutations: confirm the hint inside the round, under its lock — the hash
// finds the lock held; answer every round's hint with git's failure — the
// damaged pack is never repaired; ask the whole rebuild for a round's
// confirmed hint (damagedRound's askRebuild with nothing seeded) — a new
// incarnation, every pack downloaded; read a .pack's modification time into
// its identity (check.go's fileID) — the freshened pack is hashed again.
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

	// The same pack FRESHENED keeps its verdict: git moves a .pack's
	// modification time, and no byte of it, whenever a process writes an
	// object the pack holds — an engine write's build in a scratch leaning on
	// the copy does — so the next round's naming of it takes the verdict and
	// hashes nothing.
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

	// The same pack's NAME, now another file whose bytes are not its row's —
	// written beside it and renamed in, as every writer of a copy places a
	// file, so it is another identity (a write through the pack's own inode
	// keeps its identity, and is the scrub's to find, as media damage is):
	// the next round's hint is confirmed, and the round's callers are
	// answered by the repair of THAT pack, in place.
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

// A ROUND'S OWN PACK DAMAGE IS REPAIRED IN PLACE, as a request's is: ONE GET
// for the damaged pack, never a whole rebuild that downloads every pack the
// repository holds. A new process's first round is full and peels every tag
// it sets, so an annotated tag whose packed object is damaged on disk — its
// compressed bytes (G) or its header's stated size (S1), each in place — is
// met by the round's own cat-file, on a copy before its first maintenance (no
// bitmap and no graph: git inflates the object, and names the pack holding
// it). Real git, on the node pin and on the node floor's own build, in both
// of the ways git names it: through a bare "%s" alone (the pin's S1), and as a
// row of git's format table — a pack's damage — with the pack recorded as a
// hint beside it (the pin's G, and both on the floor). Both readings reach
// the one repair: the hint confirmed against the pack's row, the pack named
// to a forced round of the same incarnation, THAT pack fetched again over its
// files — one GET of a repository holding seven packs, counted `stored`, with
// no scrub of the copy made due beside it (what the round's git reported is
// the round's repair's, reports{outcome=request}) and none run for the
// repair.
//
// Mutations: send a round's pack damage to the whole rebuild (damagedRound's
// askRebuild with nothing seeded) — a new incarnation, all seven packs
// downloaded; read a row and its hint as the row alone (converge's hinted only
// for an unclassified failure) — the arms git classifies have their forced
// round told no pack, which finds nothing in the index files and goes on to
// the scrub (checks{trigger=request}, found=trailer); name no reader on the
// round's peel (refs.go), or never consume what it holds (settleReader) — its
// report makes the copy's scrub due beside the repair (reports{queued}).
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
// nothing else is downloaded. A new process's first round peels an annotated
// tag whose packed object is damaged on disk (G), in a repository of seven
// packs, and the object store refuses that pack's bundle — an outage on its
// key, or a bundle it lost. The round's repair asks its one GET (the in-place
// round's reinstall of the pack the hint named), which fails and holds the
// pack off (repairMark.fetchFailed); the scrub that follows finds the pack and
// leaves it on the repair mark; and the whole rebuild past it would need that
// same pack from the same store, so none is asked while it is held off: the
// sync is answered with the store's refusal. Within the window no round asks
// the store again and none is FORCED to hold every index file to its trailer
// again (checks{trigger=forced}) — a forced round could fetch nothing and
// learn nothing new. Each window that passes asks the store once more, and the
// one after the store answers again installs the pack in place: the same
// incarnation, one GET, counted `stored`, and no rebuild of the repository at
// all.
//
// Mutations: ask the step-4 rebuild while a pack it needs is held off — the
// store is asked twice a window; ask it unseeded, as a scrub answered from its
// pending findings left it — every pack of the repository downloaded each
// window; count a held-off named pack as a pack for step 2's skip, or leave a
// forced round's forces unsettled once it fails at the peel — forced checks
// within the window.
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
			w.objects.fail = func(k objstore.Key) error {
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

// heldHint is a round whose own git — the peel of the tag it sets — named a
// SOUND pack in words, the hash of that pack held in the hash hook until
// letGo: the start of the cases about what a round's hint does to the
// repository's other callers. The stand-in fails no peel once the hash has
// begun, so every round after the hinted one answers.
type heldHint struct {
	w     *syncWorld
	c     *cache.Cache
	repo  string
	row   code.PackRow // the pack the hint names
	first cache.Synced // the copy the hinted round was a delta onto
	tag   string       // the tag the hinted round could not peel
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
	// Let go on every path, and before the cache's Stop — a cleanup
	// registered after it runs first — which waits for the hash.
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

// A ROUND ENDS BEFORE ITS HINT IS HASHED. A linearizable caller never rides
// a round that began before it asked — every answer that names a ref is one,
// so every fetch's and every push's advertisement on the node is — and waits
// for the round queued behind the running one. A round whose own git named
// a pack only in words therefore stops being the repository's running round,
// its incarnation pinned, before it hashes that pack: the round queued
// behind it runs at once and answers its callers while the hash goes on, and
// only the hinted round's own callers wait for the verdict, which answers
// them — here git's own failure, the pack being sound.
//
// Mutation: confirm the hint while the round is still the running one — the
// linearizable sync asked during the hash waits for it.
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

// A HINTED ROUND'S CALLERS FOLLOW THE COPY THAT REPLACED ITS OWN. While a
// round's hint is hashed the repository has no running round, so a rebuild
// asked meanwhile runs, and may replace the incarnation the hinted round's
// git met the hint in before its verdict is reached. That copy serves nobody
// any more, whatever the verdict: the round's callers are queued again for a
// round on the current one — as a Rebuild of a copy already replaced is
// answered by a sync — and never told git's failure in a directory they would
// no longer be handed.
//
// Mutation: answer the callers of a round whose copy was replaced with the
// round's own answer — the hinted caller is told git's failure.
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

// A HINTED ROUND'S CALLERS ARE SYNCED AGAIN PAST A REPAIR. While a round's
// hint is hashed the repository has no running round, so a request's repair
// of the same copy runs meanwhile — here it fetches the hinted pack again
// over its files — and the hash, its files moved under it, decides nothing.
// The round's git failed on what that repair may since have replaced: its
// callers are synced again by a round of the copy as it now stands, as a
// request whose failure predates a repair is run again, and never told a
// failure the copy no longer has.
//
// Mutation: answer a hinted round's callers git's failure whenever its hints
// confirm nothing (no predates clause in Cache.run's replaced) — the caller
// is told git's failure over a repaired copy.
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
