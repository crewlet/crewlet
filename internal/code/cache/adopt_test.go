package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// adoption is one push received into a scratch: its pack's row stored and
// written, ready to be adopted.
type adoption struct {
	s   cache.Synced
	sc  *cache.Scratch
	tip string
	row code.PackRow
}

// receive makes a scratch over a linearizable sync of repo, packs a new
// commit into it as a receive-pack would, stores the pack's bundle and
// writes its row — the state a push's session is in when it adopts.
func (w *syncWorld) receive(t *testing.T, c *cache.Cache, repo string) adoption {
	t.Helper()
	return w.receiveOver(t, c, w.sync(t, c, repo), repo)
}

// receiveOver is receive over a sync the case already holds: several pushes
// received over one sync, as group commit answers them, each a scratch of its
// own whose row is written and none of whose packs a sync has installed.
func (w *syncWorld) receiveOver(t *testing.T, c *cache.Cache, s cache.Synced, repo string) adoption {
	t.Helper()
	pushID := uuid.Must(uuid.NewV7()).String()
	sc, err := c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	t.Cleanup(sc.Remove)
	tip, cs := w.packInto(t, repo, sc.Dir, nil)
	row := w.storeBundle(t, w.bundle(cs), pushID)
	row.Added = w.log.write(repo, []code.PackRow{row}, map[string]string{"refs/heads/main": tip})
	return adoption{s: s, sc: sc, tip: tip, row: row}
}

// ADOPTED PACKS ARE NOT DOWNLOADED: the node that took a push links the pack
// its receive-pack wrote into the cache once the push's row has applied, so
// its next sync finds the pack installed and never asks the object store for
// it — and the cache it leaves is exactly the one a cache built from nothing
// holds. The pack is counted once, though its files have two names until the
// scratch goes; and adopting it again changes nothing.
func TestAdoptedPacksAreNotDownloaded(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	a := w.receive(t, c, repo)

	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	dir := w.dir(t, c, repo)
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		name := "pack-" + a.row.GitChecksum + ext
		installed, err := os.Stat(filepath.Join(dir, "objects", "pack", name))
		if err != nil {
			t.Fatalf("the adopted %s is not in the cache: %v", name, err)
		}
		received, err := os.Stat(filepath.Join(a.sc.Dir, "objects", "pack", name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(installed, received) {
			t.Errorf("the cache's %s is a copy, not the scratch's own file", name)
		}
	}
	before := packFiles(t, dir)
	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("a second Adopt: %v", err)
	}
	if got := packFiles(t, dir); !slices.Equal(got, before) {
		t.Errorf("a second Adopt changed the pack files from %v to %v", before, got)
	}
	for outcome, want := range map[string]uint64{"adopted": 1, "held": 1} {
		if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheAdoptions, outcome, got, want)
		}
	}
	w.tmpEmpty(t)

	a.sc.Remove()
	a.s.Release()
	cache.JanitorPass(c)
	if got, want := cache.Total(c), heldBy(t, dir); got != want {
		t.Errorf("with the scratch gone the cache counts %d bytes, want the %d its incarnation holds", got, want)
	}
	s := w.sync(t, c, repo)
	if opens, _, _ := w.objects.counts(); opens[a.row.Object] != 0 {
		t.Errorf("the next sync opened the adopted pack's object %d times, want none", opens[a.row.Object])
	}
	if got := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(got, a.tip+" refs/heads/main") {
		t.Errorf("the sync after the adoption does not name the push's tip:\n%s", got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after an adoption:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
	if got, want := cache.Total(c), heldBy(t, s.Dir); got != want {
		t.Errorf("after the sync the cache counts %d bytes, want the %d its incarnation holds", got, want)
	}
}

// AN ADOPTED PACK LEARNS WHEN IT ARRIVED FROM ITS ROW: a pack is adopted under
// its ref alone, which says nothing of when its content arrived, and the next
// sync that serves its row hands the plans that weigh it (Packs) the row's
// instant — the one an expunge on its maintainer's schedule keeps a young pack
// whole by, and passes over a pack it does not know. Without it every pack a
// node's own pushes brought would be unknown on that node for the
// incarnation's life, and its scheduled expunge would never take one.
//
// Mutation: leave a row already live as it is in setLive, the arrival
// included, and the adopted pack's arrival stays zero.
func TestAnAdoptedPackLearnsWhenItArrivedFromItsRow(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	pushID := uuid.Must(uuid.NewV7()).String()
	sc, err := c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	t.Cleanup(sc.Remove)
	tip, cs := w.packInto(t, repo, sc.Dir, nil)
	row := w.storeBundle(t, w.bundle(cs), pushID)
	arrived := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	row.Arrived = arrived
	row.Added = w.log.write(repo, []code.PackRow{row}, map[string]string{"refs/heads/main": tip})
	if err := c.Adopt(t.Context(), repo, sc, row.Ref()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	s.Release()

	s = w.sync(t, c, repo)
	for _, p := range c.Packs(s) {
		if p.PackID != row.PackID {
			continue
		}
		if !p.Arrived.Equal(arrived) {
			t.Errorf("the adopted pack's row, once a sync served it, arrived at %v, want its row's %v",
				p.Arrived, arrived)
		}
		return
	}
	t.Fatalf("the sync after the adoption does not list the adopted pack %s", row.PackID)
}

// ONE ADOPTION, EVERY PACK IT NAMES: a compaction's scratch holds a pack an
// output, and the git node that built them adopts them all in one call once
// their rows have applied — each linked by the scratch's own files, each
// counted, the directory synced ONCE for the call — so its next sync
// downloads none of them, and the cache it leaves is the one a cache built
// from nothing holds. A pack the scratch does not hold refuses the call
// whole, nothing linked.
func TestOneAdoptionLinksEveryPackItNames(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var (
		mu    sync.Mutex
		syncs int
	)
	c := w.open(t, func(o *cache.Options) { cache.AdoptionSyncsCounted(o, &mu, &syncs) })
	s := w.sync(t, c, repo)
	id := uuid.Must(uuid.NewV7()).String()
	sc, err := c.NewScratch(t.Context(), s, id)
	if err != nil {
		t.Fatalf("NewScratch: %v", err)
	}
	t.Cleanup(sc.Remove)
	_, first := w.packInto(t, repo, sc.Dir, nil)
	tip, second := w.packInto(t, repo, sc.Dir, nil)
	rows := []code.PackRow{
		w.storeBundle(t, w.bundle(first), code.DeriveOutputPackID(id, 0)),
		w.storeBundle(t, w.bundle(second), code.DeriveOutputPackID(id, 1)),
	}
	w.log.write(repo, rows, map[string]string{"refs/heads/main": tip})
	refs := []code.PackRef{rows[0].Ref(), rows[1].Ref()}
	dir := w.dir(t, c, repo)
	before := packFiles(t, dir)

	stranger := refs[1]
	stranger.GitChecksum = strings.Repeat("e", len(stranger.GitChecksum))
	if err := c.Adopt(t.Context(), repo, sc, refs[0], stranger); !errors.Is(err, cache.ErrInvalid) ||
		!strings.Contains(err.Error(), stranger.GitChecksum) {
		t.Fatalf("an adoption naming a pack its scratch does not hold = %v, want ErrInvalid naming it", err)
	}
	if got := packFiles(t, dir); !slices.Equal(got, before) {
		t.Fatalf("a refused adoption changed the pack files from %v to %v", before, got)
	}

	if err := c.Adopt(t.Context(), repo, sc, refs...); err != nil {
		t.Fatalf("Adopt of two packs: %v", err)
	}
	for _, cs := range []string{first, second} {
		sameFiles(t, dir, sc.Dir, cs)
	}
	if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", "adopted"); got != 2 {
		t.Errorf("%s{outcome=adopted} = %d, want one a pack", metrics.CodeCacheAdoptions, got)
	}
	mu.Lock()
	synced := syncs
	mu.Unlock()
	if synced != 1 {
		t.Errorf("one adoption of two packs synced the directory %d times, want once", synced)
	}
	w.tmpEmpty(t)

	sc.Remove()
	s.Release()
	after := w.sync(t, c, repo)
	defer after.Release()
	opens, _, _ := w.objects.counts()
	for _, row := range rows {
		if opens[row.Object] != 0 {
			t.Errorf("the next sync opened adopted pack %s's object %d times, want none", row.PackID,
				opens[row.Object])
		}
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after one adoption of two packs:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// ADOPT REFUSES A PACK THAT IS NOT THE ROW'S: the files it links are the
// scratch's and the name it gives them is the row's, so a checksum, or a
// section's size, that the two disagree on — or a .rev the scratch does not
// hold — would install bytes no row names. Each is refused and nothing is
// linked; and only a push's own scratch, of the repository named, is adopted.
func TestAdoptRefusesAPackThatIsNotTheRows(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo, other := w.repo(t, 1), w.repo(t, 2)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, other, "refs/heads/main", nil)
	c := w.open(t)
	a := w.receive(t, c, repo)
	dir := w.dir(t, c, repo)
	held := packFiles(t, dir)

	ref := a.row.Ref()
	wider := ref
	wider.Objects++
	wider.IndexBytes, wider.ReverseBytes = code.IndexBytesFor(wider.Objects), code.ReverseBytesFor(wider.Objects)
	wider.Size = ref.PackBytes() + wider.IndexBytes + wider.ReverseBytes
	cases := map[string]code.PackRef{
		"another checksum": func() code.PackRef { r := ref; r.GitChecksum = strings.Repeat("ab", 20); return r }(),
		"another size":     func() code.PackRef { r := ref; r.Size++; return r }(),
		"another index, its pack section the scratch's": wider,
		"another reverse index": func() code.PackRef {
			r := ref
			r.ReverseBytes += 4
			r.Size += 4
			return r
		}(),
	}
	if wider.PackBytes() != ref.PackBytes() {
		t.Fatalf("the wider row's pack section is %d bytes, want the scratch's %d", wider.PackBytes(), ref.PackBytes())
	}
	for name, bad := range cases {
		if err := c.Adopt(t.Context(), repo, a.sc, bad); !errors.Is(err, cache.ErrInvalid) {
			t.Errorf("Adopt of %s = %v, want ErrInvalid", name, err)
		}
	}
	view, err := c.AdvertiseView(t.Context(), a.s)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Remove()
	for name, try := range map[string]func() error{
		"another repository's push": func() error { return c.Adopt(t.Context(), other, a.sc, ref) },
		"an advertisement view":     func() error { return c.Adopt(t.Context(), repo, view, ref) },
		"no scratch":                func() error { return c.Adopt(t.Context(), repo, nil, ref) },
		"no pack":                   func() error { return c.Adopt(t.Context(), repo, a.sc) },
	} {
		if err := try(); !errors.Is(err, cache.ErrInvalid) {
			t.Errorf("Adopt of %s = %v, want ErrInvalid", name, err)
		}
	}
	if err := os.Remove(filepath.Join(a.sc.Dir, "objects", "pack", "pack-"+ref.GitChecksum+".rev")); err != nil {
		t.Fatal(err)
	}
	if err := c.Adopt(t.Context(), repo, a.sc, ref); !errors.Is(err, cache.ErrInvalid) || !strings.Contains(err.Error(), ".rev") {
		t.Errorf("Adopt of a scratch without its .rev = %v, want ErrInvalid naming the .rev", err)
	}
	if got := packFiles(t, dir); !slices.Equal(got, held) {
		t.Errorf("refused adoptions changed the cache's pack files from %v to %v", held, got)
	}
}

// sameFiles fails t unless the pack cs's three files in dir are the scratch's
// own files under a second name — linked, never copied, and never downloaded.
func sameFiles(t *testing.T, dir, scratch, cs string) {
	t.Helper()
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		name := "pack-" + cs + ext
		installed, err := os.Stat(filepath.Join(dir, "objects", "pack", name))
		if err != nil {
			t.Errorf("the adopted %s is not in the cache: %v", name, err)
			continue
		}
		received, err := os.Stat(filepath.Join(scratch, "objects", "pack", name))
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(installed, received) {
			t.Errorf("the cache's %s is not the scratch's own file", name)
		}
	}
}

// AN ADOPTION THAT MEETS A ROUND HANDS IT THE PACK: a round writing the
// directory — a full one sweeps what its snapshot does not name and recounts
// what is installed — is never written beside, and never waited for, since a
// push's answer would wait behind somebody else's downloads. So the adoption
// stages the pack's files and returns, and the round installs them before it
// lets the directory go — even a pack its rows were read too early to name.
// The push is never downloaded back.
func TestARoundInstallsWhatAnAdoptionHandedIt(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	script := &codefault.Script{}
	c := w.open(t, func(o *cache.Options) { o.Faults = script })
	s := w.sync(t, c, repo)
	w.push(t, repo, "refs/heads/dev", nil)
	reached, release := make(chan struct{}), make(chan struct{})
	script.Arm(codefault.SyncRefs, codefault.Action{Block: release, Reached: reached})
	done := make(chan error, 1)
	go func() {
		held, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			held.Release()
		}
		done <- err
	}()
	// The round has read its rows, and holds the directory.
	<-reached

	pushID := uuid.Must(uuid.NewV7()).String()
	sc, err := c.NewScratch(t.Context(), s, pushID)
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Remove()
	tip, cs := w.packInto(t, repo, sc.Dir, nil)
	row := w.storeBundle(t, w.bundle(cs), pushID)
	row.Added = w.log.write(repo, []code.PackRow{row}, map[string]string{"refs/heads/main": tip})
	adopted := make(chan error, 1)
	go func() { adopted <- c.Adopt(context.Background(), repo, sc, row.Ref()) }()
	select {
	case err := <-adopted:
		if err != nil {
			t.Errorf("Adopt beside a running round = %v, want nil", err)
		}
	case <-time.After(hangDetector):
		t.Fatal("Adopt waited for the running round")
	}
	if statOK(t, filepath.Join(w.dir(t, c, repo), "objects", "pack", "pack-"+cs+".idx")) {
		t.Error("Adopt linked a pack beside a running round")
	}
	lines := w.logs.lines(t, "code_cache_adopt_handed")
	if len(lines) != 1 || lines[0]["repo"] != repo {
		t.Errorf("code_cache_adopt_handed = %v, want one line naming %s", lines, repo)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Installed by the round before it let the directory go, so before
	// anything else could sync.
	sameFiles(t, w.dir(t, c, repo), sc.Dir, cs)
	w.tmpEmpty(t)
	w.sync(t, c, repo)
	if opens, _, _ := w.objects.counts(); opens[row.Object] != 0 {
		t.Errorf("the push handed to a round was downloaded %d times, want never", opens[row.Object])
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after a handed adoption:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
	if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", "handed"); got != 1 {
		t.Errorf("%s{outcome=handed} = %d, want 1", metrics.CodeCacheAdoptions, got)
	}
}

// A ROUND ABOUT TO DOWNLOAD A PACK IT WAS HANDED INSTALLS IT FROM THE
// STAGING: a round that read the rows after the push applied — the push's own
// sync, a fetch's, any linearizable one, and on a busy repository one is
// almost always in flight when a push is answered — names the push's pack,
// and three links take the place of its GET.
func TestARoundTakesAHandedPackInPlaceOfItsDownload(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	a := w.receive(t, c, repo)
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Repo == repo && call.Page == 1 {
				once.Do(func() {
					close(reached)
					<-release
				})
			}
			return nil
		}
	})
	done := make(chan error, 1)
	go func() {
		held, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			held.Release()
		}
		done <- err
	}()
	// The round holds the directory, and has not read its rows yet.
	<-reached
	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt beside a running round: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if opens, _, _ := w.objects.counts(); opens[a.row.Object] != 0 {
		t.Errorf("the round downloaded the pack it was handed %d times, want never", opens[a.row.Object])
	}
	sameFiles(t, w.dir(t, c, repo), a.sc.Dir, a.row.GitChecksum)
	w.tmpEmpty(t)
	if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", "handed"); got != 1 {
		t.Errorf("%s{outcome=handed} = %d, want 1", metrics.CodeCacheAdoptions, got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after a claimed adoption:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}

// A PACK WHOSE DOWNLOAD HAD BEGUN IS DOWNLOADED, AND INSTALLED ONCE: a round
// learns of a push's pack from its row, so the one round that still downloads
// the receiving node's own push is one that read the row and began the pack's
// GET before the adoption handed it the staging — the window from the row's
// apply on this node to the adoption's registration. The GET is not
// abandoned for the staging; the staging is found held as the round lets the
// directory go, and dropped: the pack is the downloaded one, counted once,
// and tmp/ holds nothing of the push.
func TestAPackWhoseDownloadHadBegunIsInstalledOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	a := w.receive(t, c, repo)
	reached, release := make(chan struct{}), make(chan struct{})
	began := sync.OnceFunc(func() { close(reached) })
	w.objects.mu.Lock()
	w.objects.hold = func(k objstore.Key) <-chan struct{} {
		if k != a.row.Object {
			return nil
		}
		began()
		return release
	}
	w.objects.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		held, err := c.Sync(context.Background(), repo, linearizable)
		if err == nil {
			held.Release()
		}
		done <- err
	}()
	// The round read the push's row and has begun the pack's GET.
	<-reached
	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt beside a round downloading its pack: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if opens, _, _ := w.objects.counts(); opens[a.row.Object] != 1 {
		t.Errorf("the round opened the pack it had begun to download %d times, want 1", opens[a.row.Object])
	}
	dir := w.dir(t, c, repo)
	installed, err := os.Stat(filepath.Join(dir, "objects", "pack", "pack-"+a.row.GitChecksum+".pack"))
	if err != nil {
		t.Fatalf("the downloaded pack is not in the cache: %v", err)
	}
	received, err := os.Stat(filepath.Join(a.sc.Dir, "objects", "pack", "pack-"+a.row.GitChecksum+".pack"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(installed, received) {
		t.Error("the staging replaced the pack the round downloaded")
	}
	w.tmpEmpty(t)
	for outcome, want := range map[string]uint64{"handed": 1, "adopted": 0} {
		if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheAdoptions, outcome, got, want)
		}
	}
	if lines := w.logs.lines(t, "code_cache_adopt_failed"); len(lines) != 0 {
		t.Errorf("code_cache_adopt_failed = %v, want none: a held pack is no failure", lines)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image after a handed pack the round downloaded:\n%s\nwant a fresh cache's:\n%s", got, want)
	}

	a.sc.Remove()
	a.s.Release()
	cache.JanitorPass(c)
	if got, want := cache.Total(c), heldBy(t, dir); got != want {
		t.Errorf("with the scratch gone the cache counts %d bytes, want the %d its incarnation holds", got, want)
	}
}

// ADOPTIONS OF ONE REPOSITORY NEVER EXCLUDE ONE ANOTHER: group commit answers
// up to code.MaxPushesPerRecord pushes of a repository with one record, so
// every push session on the node that took them adopts at the same moment,
// each linking files of its own. Held inside the directory together, every
// one links its own pack — none is handed to another or left to a download.
func TestAdoptionsOfOneRepositoryNeverExcludeOneAnother(t *testing.T) {
	t.Parallel()
	const n = 8
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var in sync.WaitGroup
	in.Add(n)
	together := make(chan struct{})
	go func() {
		in.Wait()
		close(together)
	}()
	c := w.open(t, func(o *cache.Options) {
		cache.WhileAdopting(o, func(string) {
			in.Done()
			select {
			case <-together:
			case <-time.After(hangDetector):
			}
		})
	})
	s := w.sync(t, c, repo)
	received := make([]adoption, n)
	for i := range received {
		received[i] = w.receiveOver(t, c, s, repo)
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i, a := range received {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = c.Adopt(context.Background(), repo, a.sc, a.row.Ref())
		}()
	}
	wg.Wait()
	select {
	case <-together:
	default:
		t.Errorf("not every adoption held the directory at once")
	}
	dir := w.dir(t, c, repo)
	for i, a := range received {
		if errs[i] != nil {
			t.Errorf("Adopt %d: %v", i, errs[i])
		}
		sameFiles(t, dir, a.sc.Dir, a.row.GitChecksum)
	}
	for outcome, want := range map[string]uint64{"adopted": n, "handed": 0, "held": 0} {
		if got := counter(w.rec, metrics.CodeCacheAdoptions, "outcome", outcome); got != want {
			t.Errorf("%s{outcome=%s} = %d, want %d", metrics.CodeCacheAdoptions, outcome, got, want)
		}
	}
	w.sync(t, c, repo)
	opens, _, _ := w.objects.counts()
	for i, a := range received {
		if opens[a.row.Object] != 0 {
			t.Errorf("push %d was downloaded %d times after its adoption, want never", i, opens[a.row.Object])
		}
	}
}

// A PACK ADOPTED IS KEPT WHEN ITS DIRECTORY CANNOT BE SYNCED: its names are
// in the directory and served, exactly as a round's install keeps what it
// renamed in when the sync after fails — what a failed sync costs is the
// names surviving a crash, and a boot's first round is a full one, which
// installs whatever a crash lost. So the pack stays and is counted once, and
// the failure is returned; taken back, it would cost the next sync a GET to
// repair nothing.
func TestAnAdoptionWhoseDirectoryCannotBeSyncedKeepsItsPack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	broken := errors.New("the disk refused the sync")
	c := w.open(t, func(o *cache.Options) { cache.AdoptionSyncFails(o, broken) })
	a := w.receive(t, c, repo)

	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); !errors.Is(err, broken) {
		t.Fatalf("Adopt whose directory sync fails = %v, want the sync's failure", err)
	}
	dir := w.dir(t, c, repo)
	sameFiles(t, dir, a.sc.Dir, a.row.GitChecksum)
	w.tmpEmpty(t)
	a.sc.Remove()
	a.s.Release()
	cache.JanitorPass(c)
	if got, want := cache.Total(c), heldBy(t, dir); got != want {
		t.Errorf("the cache counts %d bytes, want the %d its incarnation holds", got, want)
	}
	w.sync(t, c, repo)
	if opens, _, _ := w.objects.counts(); opens[a.row.Object] != 0 {
		t.Errorf("the next sync downloaded the kept pack %d times, want never", opens[a.row.Object])
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the image:\n%s\nwant a fresh cache's:\n%s", got, want)
	}
}
