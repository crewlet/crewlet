package backup_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/worklet"
)

// gate holds every call that passes it until it is opened, counting how many
// it holds at once and the most it ever held — the collector's own measure of
// a bound, applied to the backup's.
type gate struct {
	open     chan struct{}
	openOnce sync.Once

	mu         sync.Mutex
	held, peak int
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

// pass is a call arriving: counted, then held until the gate opens or ctx
// ends — whose error it answers.
func (g *gate) pass(ctx context.Context) error {
	g.mu.Lock()
	g.held++
	g.peak = max(g.peak, g.held)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.held--
		g.mu.Unlock()
	}()
	select {
	case <-g.open:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// release opens the gate, once however often it is called.
func (g *gate) release() { g.openOnce.Do(func() { close(g.open) }) }

// counts is how many calls the gate holds now, and the most it has held.
func (g *gate) counts() (held, peak int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held, g.peak
}

// waitHeld fails the test unless n calls are held at once within ten seconds.
func (g *gate) waitHeld(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		held, peak := g.counts()
		if held >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gate never held %d calls at once (at most %d): the calls are not "+
				"made concurrently", n, peak)
		}
		time.Sleep(time.Millisecond)
	}
}

// manyFiles uploads n small files and names each from the copy, as
// `ENG/file-<i>`.
func manyFiles(t *testing.T, db *store.DB, files *objstore.Store, n int) []stored {
	t.Helper()
	bodies := make([]string, n)
	for i := range bodies {
		bodies[i] = fmt.Sprintf("file %d", i)
	}
	held := upload(t, files, bodies...)
	filesNaming(t, db, held...)
	return held
}

// boundedService is a backup of db whose objects are reached through objs.
func boundedService(t *testing.T, db *store.DB, objs *backup.Objects) *backup.Service {
	t.Helper()
	fleet := memory.NewFleet()
	return build(t, backup.Options{
		Store: db, Estate: backup.HoldsReplicated, NodeID: "n", Holds: fleet, Backups: fleet,
		Now: func() time.Time { return clock }, Objects: objs,
	})
}

// takeAsync runs Take in the background, answering its result on a channel.
func takeAsync(t *testing.T, svc *backup.Service, dir string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := svc.Take(context.WithoutCancel(t.Context()), dir)
		done <- err
	}()
	return done
}

// A BACKUP READS UP TO ITS BOUND OF OBJECTS AT ONCE, AND NEVER MORE. A first
// copy is bound by each GET's first byte, so too few out leaves the link idle
// for hours of a copy the trim hold spans — sixteen at a 100 ms first byte
// took the large model's first copy to about five hours where thirty-two took
// about three (BenchmarkCarry) — and with no bound it would flood the prefix
// the company's own downloads share.
func TestABackupReadsAtMostItsBoundAtOnce(t *testing.T) {
	t.Parallel()
	// THIRTY-TWO, the value backupFetchConcurrency's doc derives and
	// BenchmarkCarry measures: a change to it is a decision this test makes
	// somebody restate.
	if backup.FetchConcurrency != 32 {
		t.Fatalf("the backup reads %d objects at once; its doc derives thirty-two", backup.FetchConcurrency)
	}
	db := openStore(t)
	files, _ := filesStore(t)
	held := manyFiles(t, db, files, 3*backup.FetchConcurrency)
	g := newGate()
	defer g.release()
	svc := boundedService(t, db, &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
		if err := g.pass(ctx); err != nil {
			return nil, err
		}
		return files.Open(ctx, o)
	}})
	dir := filepath.Join(t.TempDir(), "bounded")
	done := takeAsync(t, svc, dir)
	g.waitHeld(t, backup.FetchConcurrency)
	time.Sleep(50 * time.Millisecond)
	if _, peak := g.counts(); peak != backup.FetchConcurrency {
		t.Fatalf("the backup had %d objects out at once, want its bound of %d", peak, backup.FetchConcurrency)
	}
	g.release()
	if err := <-done; err != nil {
		t.Fatalf("Take: %v", err)
	}
	if heldNow, peak := g.counts(); heldNow != 0 || peak != backup.FetchConcurrency {
		t.Fatalf("after the backup: %d calls held, %d at most; want none and %d", heldNow, peak,
			backup.FetchConcurrency)
	}
	restoresWhole(t, filepath.Join(dir, "objects"), held)
}

// A BACKUP OF A STREAM ASKS UP TO ITS BOUND AT ONCE, AND NEVER MORE. Asked one
// at a time, two million objects on the broker's bucket are minutes of round
// trips end to end; thirty-two out is the audit's own depth for the same
// question.
func TestABackupAsksTheStreamStoreAtMostItsBoundAtOnce(t *testing.T) {
	t.Parallel()
	// THIRTY-TWO, the collector's audit depth for the same question.
	if backup.StreamCheckConcurrency != 32 {
		t.Fatalf("the backup asks %d at once; its doc holds it to the audit's thirty-two",
			backup.StreamCheckConcurrency)
	}
	db := openStore(t)
	files, _ := filesStore(t)
	manyFiles(t, db, files, 3*backup.StreamCheckConcurrency)
	g := newGate()
	defer g.release()
	svc := boundedService(t, db, &backup.Objects{Stream: "OBJ_crewlet_files",
		Stat: func(ctx context.Context, k objstore.Key) (objstore.Info, error) {
			if err := g.pass(ctx); err != nil {
				return objstore.Info{}, err
			}
			return files.Stat(ctx, k)
		},
		Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
			t.Error("an object was read although the stream snapshot carries it")
			return nil, errors.New("not expected")
		}})
	done := takeAsync(t, svc, filepath.Join(t.TempDir(), "bounded-stream"))
	g.waitHeld(t, backup.StreamCheckConcurrency)
	time.Sleep(50 * time.Millisecond)
	if _, peak := g.counts(); peak != backup.StreamCheckConcurrency {
		t.Fatalf("the backup asked %d at once, want its bound of %d", peak, backup.StreamCheckConcurrency)
	}
	g.release()
	if err := <-done; err != nil {
		t.Fatalf("Take: %v", err)
	}
	if heldNow, peak := g.counts(); heldNow != 0 || peak != backup.StreamCheckConcurrency {
		t.Fatalf("after the backup: %d calls held, %d at most; want none and %d", heldNow, peak,
			backup.StreamCheckConcurrency)
	}
}

// THE FIRST OBJECT THE STORE DOES NOT ANSWER FOR STOPS THE BACKUP, once it has
// been asked about it as often as a backup asks, with nothing left in flight.
// It is refused for that object whatever the rest does — the object may be
// intact in a store that did not answer — so reading on is a terabyte sent for
// a refusal, and asking on is a store that stopped answering asked about every
// object it holds. The others wait at a gate that opens a second after the
// last failure: a backup that stopped there has ended them by then, and one
// that carried on past it would reach every object.
func TestABackupStopsAtTheFirstObjectTheStoreDoesNotAnswerFor(t *testing.T) {
	t.Parallel()
	const files = 200
	refuse := errors.New("the store did not answer")
	// first is the first key in key order — the one the walk hands on first.
	first := func(held []stored) objstore.Key {
		k := held[0].object.Key
		for _, o := range held[1:] {
			if o.object.Key.Compare(k) < 0 {
				k = o.object.Key
			}
		}
		return k
	}
	// refused answers the unanswered object's every question with refuse,
	// opening the gate a second after the last one a backup asks — NOT AT
	// ONCE, which raced the pass's own ending: the waiting questions went
	// on, and freed slots for more, before the failure had cancelled them.
	refused := func(g *gate, asked *atomic.Int32) error {
		if asked.Add(1) == backup.FetchAttempts {
			time.AfterFunc(time.Second, g.release)
		}
		return refuse
	}
	// stopped checks a backup refused for unanswered, which it asked about as
	// often as a backup asks and no more, having asked about fewer than two
	// bounds' worth of objects in all.
	stopped := func(t *testing.T, err error, unanswered objstore.Key, asked, calls int32,
		bound int, g *gate, dir string) {
		t.Helper()
		if !errors.Is(err, backup.ErrObjectsUnreachable) || !strings.Contains(err.Error(), unanswered.String()) {
			t.Fatalf("Take = %v; want ErrObjectsUnreachable naming %s", err, unanswered)
		}
		if asked != backup.FetchAttempts {
			t.Errorf("the store was asked about the unanswered object %d times, want %d",
				asked, backup.FetchAttempts)
		}
		if calls >= int32(2*bound) {
			t.Errorf("the store was asked about %d objects; a backup that stops at the first it "+
				"does not answer for asks fewer than %d", calls, 2*bound)
		}
		if heldNow, _ := g.counts(); heldNow != 0 {
			t.Errorf("%d questions were still out when Take returned", heldNow)
		}
		if _, serr := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(serr, os.ErrNotExist) {
			t.Fatal("a backup refused for an object wrote its manifest")
		}
	}

	t.Run("copied", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		bucket, _ := filesStore(t)
		held := manyFiles(t, db, bucket, files)
		unanswered := first(held)
		g := newGate()
		defer g.release()
		var calls, asked atomic.Int32
		svc := boundedService(t, db, &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
			if o.Key == unanswered {
				return nil, refused(g, &asked)
			}
			calls.Add(1)
			if err := g.pass(ctx); err != nil {
				return nil, err
			}
			return bucket.Open(ctx, o)
		}})
		dir := filepath.Join(t.TempDir(), "unanswered")
		_, err := svc.Take(t.Context(), dir)
		stopped(t, err, unanswered, asked.Load(), calls.Load(), backup.FetchConcurrency, g, dir)
	})

	t.Run("in a stream", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		bucket, _ := filesStore(t)
		held := manyFiles(t, db, bucket, files)
		unanswered := first(held)
		g := newGate()
		defer g.release()
		var calls, asked atomic.Int32
		svc := boundedService(t, db, &backup.Objects{Stream: "OBJ_crewlet_files",
			Stat: func(ctx context.Context, k objstore.Key) (objstore.Info, error) {
				if k == unanswered {
					return objstore.Info{}, refused(g, &asked)
				}
				calls.Add(1)
				if err := g.pass(ctx); err != nil {
					return objstore.Info{}, err
				}
				return bucket.Stat(ctx, k)
			},
			Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
				return nil, errors.New("not expected")
			}})
		dir := filepath.Join(t.TempDir(), "unanswered-stream")
		_, err := svc.Take(t.Context(), dir)
		stopped(t, err, unanswered, asked.Load(), calls.Load(), backup.StreamCheckConcurrency, g, dir)
	})
}

// AN OBJECT THE STORE FAILS ONCE IS ASKED ABOUT AGAIN, and the backup carries
// it. A first copy is millions of reads over hours, and a reset part way
// through one body — which nothing below the backup retries — used to refuse
// the whole of it, with every object read again by the next attempt. And a
// DEFINITE answer is never asked again: an object the store says it does not
// hold is asked about once, and recorded lost.
func TestABackupAsksAgainAboutAnObjectTheStoreFailedOnce(t *testing.T) {
	t.Parallel()
	const files, lost = 40, 3
	// flaky fails the first question about every fifth object it holds and
	// counts the questions about each. WHICH ones fail is chosen by place in
	// the upload, never by a character of the key: a key's last digits are
	// crypto/rand's, and a draw that picked none failed the case about one
	// run in two thousand over nothing the backup did.
	type flaky struct {
		mu     sync.Mutex
		asked  map[objstore.Key]int
		lost   objstore.Key
		failed map[objstore.Key]bool // fails its first question
	}
	newFlaky := func(held []stored) *flaky {
		f := &flaky{asked: map[objstore.Key]int{}, lost: held[lost].object.Key,
			failed: map[objstore.Key]bool{}}
		for i := 0; i < len(held); i += 5 {
			if i != lost {
				f.failed[held[i].object.Key] = true
			}
		}
		return f
	}
	answer := func(f *flaky, k objstore.Key) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.asked[k]++
		switch {
		case k == f.lost:
			return fmt.Errorf("gone: %w", objstore.ErrNotFound)
		case f.asked[k] == 1 && f.failed[k]:
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	check := func(t *testing.T, f *flaky, held []stored, m backup.Manifest, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("Take: %v", err)
		}
		if len(f.failed) == 0 {
			t.Fatal("no object fails its first question; the case tests nothing")
		}
		for _, o := range held {
			want := 1
			if f.failed[o.object.Key] {
				want = 2
			}
			if n := f.asked[o.object.Key]; n != want {
				t.Errorf("object %s (lost %v, failed once %v) was asked about %d times, want %d",
					o.object.Key, o.object.Key == f.lost, f.failed[o.object.Key], n, want)
			}
		}
		if o := m.Objects; o == nil || o.Objects != files-1 || len(o.Lost) != 1 {
			t.Fatalf("manifest objects = %+v, want every object but the lost one carried", o)
		}
	}

	t.Run("copied", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		bucket, _ := filesStore(t)
		held := manyFiles(t, db, bucket, files)
		f := newFlaky(held)
		svc := boundedService(t, db, &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
			if err := answer(f, o.Key); err != nil {
				return nil, err
			}
			return bucket.Open(ctx, o)
		}})
		dir := filepath.Join(t.TempDir(), "retried")
		m, err := svc.Take(t.Context(), dir)
		check(t, f, held, m, err)
		var kept []stored
		for _, o := range held {
			if o.object.Key != f.lost {
				kept = append(kept, o)
			}
		}
		restoresWhole(t, filepath.Join(dir, "objects"), kept)
	})

	t.Run("in a stream", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		bucket, _ := filesStore(t)
		held := manyFiles(t, db, bucket, files)
		f := newFlaky(held)
		svc := boundedService(t, db, &backup.Objects{Stream: "OBJ_crewlet_files",
			Stat: func(ctx context.Context, k objstore.Key) (objstore.Info, error) {
				if err := answer(f, k); err != nil {
					return objstore.Info{}, err
				}
				return bucket.Stat(ctx, k)
			},
			Open: func(context.Context, objstore.Object) (io.ReadCloser, error) {
				return nil, errors.New("not expected")
			}})
		m, err := svc.Take(t.Context(), filepath.Join(t.TempDir(), "retried-stream"))
		check(t, f, held, m, err)
	})
}

// THE FETCH BOUND RIDES WARM CONNECTIONS: past the shared transport's idle
// pool per host, a GET over the bound would open a connection of its own and
// pay a handshake the pool exists to save — the collector holds its audit's
// bound the same way.
func TestTheFetchBoundRidesWarmConnections(t *testing.T) {
	t.Parallel()
	for name, bound := range map[string]int{
		"fetch": backup.FetchConcurrency, "stream check": backup.StreamCheckConcurrency,
	} {
		if bound > httpx.MaxIdleConnsPerHost {
			t.Errorf("the backup's %s bound is %d, past the %d connections the shared "+
				"transport keeps warm per host", name, bound, httpx.MaxIdleConnsPerHost)
		}
	}
}

// THE COPY IS WALKED A PAGE AT A TIME, never gathered first. A company of two
// million objects is two million references, and a backup that read them all
// before copying the first held half a gibibyte of them — twice; walked, it
// holds a page of each table while the objects of the page before are copied.
//
// SO WHILE ITS FIRST OBJECTS ARE COPIED IT HAS READ ONE PAGE OF EACH TABLE IT
// MERGES, and no more: the merge reads every Required table's first page
// before it can say which key comes first (objstore.MergeWalks), and the
// tracker's first page holds far more references than a fetch's width. The
// bound is counted from objstore/references rather than written down, because
// a table a domain declares is a page the walk reads at its start — it was the
// literal 2 while Worklet's files and the code host's packs were the only
// Required tables, and every table declared since read as a gathering walk.
func TestABackupWalksItsCopyAPageAtATime(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, _ := filesStore(t)
	held := manyFiles(t, db, files, 4*objstore.WalkPage+1)
	g := newGate()
	defer g.release()
	svc := boundedService(t, db, &backup.Objects{Open: func(ctx context.Context, o objstore.Object) (io.ReadCloser, error) {
		if err := g.pass(ctx); err != nil {
			return nil, err
		}
		return files.Open(ctx, o)
	}})
	var pages atomic.Int32
	backup.SetPageObserver(svc, func() { pages.Add(1) })
	dir := filepath.Join(t.TempDir(), "paged")
	done := takeAsync(t, svc, dir)
	g.waitHeld(t, backup.FetchConcurrency)
	// One page of Worklet's files and one of every other table the walk
	// merges — a gathering walk has read the tracker's five by now.
	tables, _ := othersDeclared()
	if read, merged := pages.Load(), int32(1+tables); read > merged {
		t.Fatalf("the backup had read %d pages of the copy while copying its first %d objects, "+
			"past the one page of each of the %d tables it merges: it gathers the copy's "+
			"references before copying them", read, backup.FetchConcurrency, merged)
	}
	if objstore.WalkPage <= backup.FetchConcurrency {
		t.Fatalf("a walk page holds %d references, no more than the %d a fetch copies at once; "+
			"this case cannot tell a walk from a gather", objstore.WalkPage, backup.FetchConcurrency)
	}
	g.release()
	if err := <-done; err != nil {
		t.Fatalf("Take: %v", err)
	}
	// FOUR WHOLE PAGES AND THE SHORT ONE THAT ENDS THE WALK — and no read
	// past it — plus the one empty page that ends the walk of every other
	// domain's table, of which this copy holds no row.
	if read, want := pages.Load(), int32(5+tables); read != want {
		t.Fatalf("the backup read the copy in %d pages, want %d: five of the "+
			"tracker's and one for each of the %d other declared tables", read, want, tables)
	}
	restoresWhole(t, filepath.Join(dir, "objects"), held)
}

// othersDeclared is how many Required tables every domain but the tracker
// declares, and how many logs write them — the copies these cases take hold
// none of their rows, so a backup walks each such table in one empty page and
// reads each such log's deferred records in one.
func othersDeclared() (tables, logs int) {
	seen := map[string]bool{}
	for _, ref := range references.All {
		if ref.Standing != objstore.Required || ref.Domain == (worklet.Domain{}).Name() {
			continue
		}
		tables++
		if !seen[ref.Log] {
			seen[ref.Log] = true
			logs++
		}
	}
	return tables, logs
}

// A KEY THE COPY NAMES AS TWO OBJECTS REFUSES THE BACKUP. A key is minted for
// one upload, so two rows giving it two digests are a copy that contradicts
// itself, and a backup that verified the object against either would certify
// the other wrong. The walk hands the two rows on adjacent, which is where the
// contradiction is caught.
func TestABackupRefusesAKeyNamedAsTwoObjects(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	files, _ := filesStore(t)
	held := upload(t, files, "the bytes one row records")
	filesNaming(t, db, held...)
	other := held[0].object
	other.Hash = objstore.HashOf([]byte("the bytes another row records"))
	if err := db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_files
			(id, project_key, path, hash, size, object, created_at, updated_at, version, document)
			VALUES ('ENG.other', 'ENG', 'other', ?, ?, ?, 0, 0, 1, x'7b7d')`,
			string(other.Hash), other.Size, other.Key.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc := boundedService(t, db, &backup.Objects{Open: files.Open})
	dir := filepath.Join(t.TempDir(), "contradicted")
	_, err := svc.Take(t.Context(), dir)
	if err == nil || !strings.Contains(err.Error(), "ENG/file-0") || !strings.Contains(err.Error(), "ENG/other") {
		t.Fatalf("Take = %v; want it refused naming both rows", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, backup.ManifestName)); !errors.Is(serr, os.ErrNotExist) {
		t.Fatal("a backup over a copy that contradicts itself wrote its manifest")
	}
}
