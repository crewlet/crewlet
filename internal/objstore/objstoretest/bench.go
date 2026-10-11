package objstoretest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// BenchBackend is one benchmark's backend, as a [BenchFactory] builds it.
type BenchBackend struct {
	objstore.Backend

	// Requests, where something on the backend's far side counts them, is
	// how many requests the backend has been sent so far. The put and list
	// cases report how many one operation made from it (`requests/op`): a
	// multipart upload's parts, a listing's pages — the number a provider's
	// round trip multiplies. Nil where nothing counts them, and the metric
	// is left out.
	Requests func() int64
}

// sent is how many requests the backend has been sent so far, or zero where
// nothing counts them.
func (bb BenchBackend) sent() int64 {
	if bb.Requests == nil {
		return 0
	}
	return bb.Requests()
}

// reportRequests reports requests made over b.N operations, where they are
// counted at all.
func (bb BenchBackend) reportRequests(b *testing.B, requests int64) {
	if bb.Requests != nil {
		b.ReportMetric(float64(requests)/float64(b.N), "requests/op")
	}
}

// BenchFactory builds an EMPTY backend for one benchmark, on that benchmark's
// own *testing.B, and takes away everything put into it when the benchmark
// ends: a broker or a fake by stopping, a bucket that outlives the run by
// [Clear]ing the prefix the factory gave this benchmark alone.
//
// THE SUITE DELETES NOTHING AT THE END OF A CASE. Its populations run to a
// hundred thousand objects, and deleting them one request at a time from a
// backend about to be thrown away is minutes of teardown that measure nothing.
type BenchFactory func(b *testing.B) BenchBackend

// Bench runs every object-store benchmark against backends newBackend builds,
// one backend per case, so no case measures another's objects.
//
// Every case goes through [objstore.Store] where a caller would — a put is
// counted and hashed on the way in, a whole read checked against its row on
// the way out — except `get`, which is the backend's own stream with nothing
// checked, so the two figures side by side say what the store's verification
// costs on top of the backend and its transport; and `stat` and `list`, which
// are the backend's own verbs, as the collector calls them. The cases, by the
// names a comparison keys on:
//
//   - put/size=…: [objstore.Store.Put] of a fresh object.
//   - get/size=…: [objstore.Backend.Get] of a whole object, unverified.
//   - open/size=…: [objstore.Store.Open] of a whole object, every byte
//     verified and the last one held back until it is.
//   - ranged/size=64KiB/of=128MiB: [objstore.Store.ReadAt] of one page at a
//     random offset of a large object — what a seat's page read pays.
//   - parallel/streams=8/size=16MiB: eight verified whole reads at once, the
//     figure reported for all eight together.
//   - stat/objects=1/workers=…: [objstore.Backend.Stat] of ONE object over
//     and over, from one goroutine and from a pool of 32 — the hottest a
//     store can be (one name, one index entry, one block in cache), and so
//     the most a backend can give.
//   - stat/objects=10000/order=…/workers=…: Stat across a POPULATION of ten
//     thousand objects of mixed sizes, in key order — the audit's, which
//     walks the references by key — and in a random one, from one goroutine
//     and from 32: what an audit of a store that holds something pays.
//   - list/objects=…: one whole [objstore.Backend.List] of a store holding
//     ten thousand one-byte objects, then a hundred thousand — the
//     collector's every pass — reported in names a second.
//
// EVERY READ IS WARM. Each read case reads objects the case wrote moments
// before, again and again, so a read figure is what a node gets reading
// something recently written — never a read from a cold disk or from a
// provider's colder storage, which can only cost more. On a backend this
// process or a broker beside it holds — the twin, the broker, the fake bucket
// — the bytes are wherever that machine keeps what it just wrote or read (the
// operating system's page cache, the broker's own block cache, the fake's
// memory), and reading through several objects in turn would not make them
// cold, since anything a case can afford to write fits in that machine's
// memory. A real bucket's reads are of objects its provider stored seconds
// before, in whatever storage class the bucket writes, and say nothing about
// one a colder class holds.
func Bench(b *testing.B, newBackend BenchFactory) {
	for _, s := range benchSizes {
		b.Run("put/size="+s.name, func(b *testing.B) { benchPut(b, newBackend(b), s.n) })
	}
	BenchReads(b, newBackend)
	// THE BACKEND AND ITS OBJECTS LIVE IN THE PARENT. The stat children
	// count b.N themselves (see benchStat), and a benchmark that does is run
	// again for every step of the ramp to its final b.N — so a backend built
	// inside the child would be a broker or a bucket, and ten thousand
	// objects, built per step.
	b.Run("stat/objects=1", func(b *testing.B) {
		backend := newBackend(b)
		one := []objstore.Object{keep(b, benchStore(b, backend.Backend), content(pageBytes, seedStat))}
		for _, workers := range []int{1, auditWorkers} {
			b.Run(fmt.Sprintf("workers=%d", workers), func(b *testing.B) {
				benchStat(b, backend, one, []int{0}, workers)
			})
		}
	})
	b.Run(fmt.Sprintf("stat/objects=%d", statPopulation), func(b *testing.B) {
		backend := newBackend(b)
		objects := populate(b, benchStore(b, backend.Backend), 0, statPopulation, seedStat,
			func(i int) int64 { return populationSizes[i%len(populationSizes)] })
		for _, o := range []struct {
			name  string
			order []int
		}{
			{"key", keyOrder(objects)},
			{"random", rand.New(rand.NewPCG(seedStat, 6)).Perm(len(objects))},
		} {
			for _, workers := range []int{1, auditWorkers} {
				b.Run(fmt.Sprintf("order=%s/workers=%d", o.name, workers), func(b *testing.B) {
					benchStat(b, backend, objects, o.order, workers)
				})
			}
		}
	})
	// ONE POPULATION, GROWN: the store the larger listing reads is the
	// smaller one's with the rest added, so the ten thousand are put once.
	b.Run("list", func(b *testing.B) {
		backend := newBackend(b)
		s := benchStore(b, backend.Backend)
		held := 0
		for _, n := range listPopulations {
			populate(b, s, held, n, seedList, func(int) int64 { return 1 })
			held = n
			b.Run(fmt.Sprintf("objects=%d", n), func(b *testing.B) { benchList(b, backend, n) })
		}
	})
}

// BenchReads runs [Bench]'s read cases alone — get, open, ranged and parallel,
// under the same names — for an arm whose difference from another touches
// only what a read moves: a bucket whose every connection carries at most a
// provider's rate, read one range at a time and then several. A put, a stat
// and a listing move no body that difference reaches, and the populations
// behind the stat and list cases are minutes of setup an arm like that would
// measure nothing new with.
func BenchReads(b *testing.B, newBackend BenchFactory) {
	for _, s := range benchSizes {
		b.Run("get/size="+s.name, func(b *testing.B) { benchRead(b, newBackend(b), s.n, false) })
	}
	for _, s := range benchSizes {
		b.Run("open/size="+s.name, func(b *testing.B) { benchRead(b, newBackend(b), s.n, true) })
	}
	b.Run("ranged/size=64KiB/of=128MiB", func(b *testing.B) { benchRanged(b, newBackend(b)) })
	b.Run(fmt.Sprintf("parallel/streams=%d/size=16MiB", parallelReads), func(b *testing.B) {
		benchParallel(b, newBackend(b))
	})
}

// benchSizes are the object sizes every put, get and open is measured at.
//
// Sixty-four kibibytes is a small file, where a request's own cost shows. A
// mebibyte is one S3 request and eight broker messages. Sixteen is two
// S3 parts and 128 broker messages, so the multipart path and the message
// stream are both in it. 128 is sixteen parts and 1,024 messages, the largest
// a case can repeat several times inside a few seconds of benchtime.
var benchSizes = []struct {
	name string
	n    int64
}{
	{"64KiB", 64 << 10},
	{"1MiB", 1 << 20},
	{"16MiB", 16 << 20},
	{"128MiB", 128 << 20},
}

// populationSizes are the sizes a stat population's objects take in turn: a
// kibibyte to a quarter of a mebibyte, about 84 KiB on average.
//
// MIXED, AND NONE TINY, because what a stat costs on a backend that keeps an
// object's metadata in the same store as its bytes — the broker keeps both in
// one stream, a 256 KiB object's two pieces between its metadata and the next
// object's — turns on how far apart the metadata lies. Tiny objects would pack
// it together, which no company's store does, and flatter every stat.
var populationSizes = []int64{1 << 10, 16 << 10, 64 << 10, 256 << 10}

// listPopulations are the stores a listing is measured over, in one-byte
// objects: ten thousand, then a hundred thousand, so a rate that holds across
// the two can be carried to a store of millions and one that falls cannot.
// One-byte objects because a listing reads names, never bytes, and a store of
// a hundred thousand large ones would not fit the machines this runs on.
var listPopulations = []int{10_000, 100_000}

const (
	// pageBytes and pagedBytes are a page at a random offset of a large
	// object. A seat's page read is 32 KiB by default and 48 KiB at most,
	// so 64 KiB bounds the bytes it moves; what the case is about is the
	// OFFSET, since a backend with no ranged read (the broker's) walks the
	// object's pieces to it.
	pageBytes  = 64 << 10
	pagedBytes = 128 << 20

	// parallelReads and parallelBytes are several whole reads at once, each
	// large enough to be mostly bytes rather than its request.
	//
	// EIGHT, the downloads a cold cache's sync has in flight in the design
	// the git host is sized by — a quantity of the WORK, never of the
	// machine measuring it, so the figure means one thing everywhere: eight
	// streams' worth of fetching and hashing, which on four vCPUs is more
	// than the CPU can hash and on sixty-four is not.
	parallelReads = 8
	parallelBytes = 16 << 20

	// auditWorkers is a pool of stats in flight at once, measured beside
	// one at a time: a stat is a round trip, so how many are in flight is
	// what decides how long an audit of a large store takes, and the two
	// figures say what the backend gives each.
	auditWorkers = 32

	// statPopulation is how many objects the population a stat walks holds.
	statPopulation = 10_000

	// populateWorkers is how many puts build a population at once: enough
	// that a bucket a round trip away fills a hundred thousand objects in
	// about a minute rather than over an hour, and no more than a broker's
	// stream takes in at once anyway.
	populateWorkers = 64

	// clearWorkers is how many deletes [Clear] has in flight.
	clearWorkers = 64

	// copyBuffer is what a read is drained through: io.Copy's own size,
	// which is what an HTTP response or a backup's file write reads with.
	copyBuffer = 32 << 10
)

// The seeds of each case's content, distinct so no two cases store the same
// bytes and a backend deduplicating behind the engine's back could not
// flatter one with another's. A population's objects take its seed in the
// high word and their own index in the low one.
const (
	seedPut      = 1
	seedRead     = 2
	seedRanged   = 3
	seedStat     = 5
	seedList     = 6
	seedParallel = 10 // one per stream, from here
)

// content is n bytes of seed's stream.
//
// RANDOM, so nothing a backend or a link could compress flatters a figure —
// the content the design moves (a git pack, a file a seat wrote) is mostly
// compressed already. DETERMINISTIC, so two runs store the same bytes.
func content(n int64, seed uint64) []byte {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	data := make([]byte, n)
	_, _ = rand.NewChaCha8(key).Read(data)
	return data
}

func benchStore(b *testing.B, backend objstore.Backend) *objstore.Store {
	b.Helper()
	s, err := objstore.NewStore(backend)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

// store puts data through s as a request body would arrive: through
// [streamed], which hides the length and the seek a bytes.Reader offers.
//
// BOUNDED BY [objstore.ReadBudget] — the engine's own arithmetic for moving
// this many bytes across two legs at [objstore.MiBPace] — so a put the engine
// would have given up on fails the case rather than becoming its figure.
func store(ctx context.Context, s *objstore.Store, data []byte) (objstore.Object, error) {
	size := int64(len(data))
	ctx, cancel := context.WithTimeout(ctx, objstore.ReadBudget(size))
	defer cancel()
	return s.Put(ctx, streamed{bytes.NewReader(data)}, size,
		objstore.PutMeta{ContentType: "application/octet-stream"})
}

// keep puts an object a case reads.
func keep(b *testing.B, s *objstore.Store, data []byte) objstore.Object {
	b.Helper()
	o, err := store(b.Context(), s, data)
	if err != nil {
		b.Fatalf("put %d bytes: %v", len(data), err)
	}
	return o
}

// populate puts the objects from..to-1 of a population through s,
// [populateWorkers] at a time — the i-th of size(i) bytes of a stream of its
// own — and answers them in that order.
func populate(b *testing.B, s *objstore.Store, from, to int, seed uint64, size func(int) int64) []objstore.Object {
	b.Helper()
	objects := make([]objstore.Object, to-from)
	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	errs := make([]error, populateWorkers)
	for w := range populateWorkers {
		wg.Go(func() {
			for {
				i := from + int(next.Add(1)-1)
				if i >= to {
					return
				}
				o, err := store(b.Context(), s, content(size(i), seed<<32|uint64(i)))
				if err != nil {
					errs[w] = fmt.Errorf("put object %d of a population of %d: %w", i, to, err)
					return
				}
				objects[i-from] = o
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		b.Fatal(err)
	}
	return objects
}

// keyOrder is the indexes of objects in the order of their names.
func keyOrder(objects []objstore.Object) []int {
	order := make([]int, len(objects))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int {
		return strings.Compare(objects[i].Key.Name(), objects[j].Key.Name())
	})
	return order
}

// Clear deletes every object backend lists, [clearWorkers] at a time, and
// abandons every upload it holds begun and never finished — what a
// [BenchFactory] whose backend outlives the benchmark (a real bucket)
// registers as that benchmark's cleanup, under a context of its own, since
// the benchmark's has ended by the time it runs.
func Clear(ctx context.Context, backend objstore.Backend) error {
	var names []string
	if err := backend.List(ctx, func(info objstore.Info) error {
		names = append(names, info.Name)
		return nil
	}); err != nil {
		return fmt.Errorf("list what to clear: %w", err)
	}
	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	errs := make([]error, clearWorkers)
	for w := range clearWorkers {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(names) {
					return
				}
				if err := backend.Delete(ctx, names[i]); err != nil {
					errs[w] = fmt.Errorf("delete %s: %w", names[i], err)
					return
				}
			}
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	return backend.Pending(ctx, func(p objstore.Pending) error {
		if err := backend.Abandon(ctx, p); err != nil {
			return fmt.Errorf("abandon the upload %s of %s: %w", p.ID, p.Name, err)
		}
		return nil
	})
}

// discard is a writer that keeps nothing.
//
// NOT io.Discard, which is an io.ReaderFrom: io.CopyBuffer would hand it the
// read and it would read in 8 KiB pieces of its own pool's, where everything
// a stream is actually copied into reads at [copyBuffer].
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func benchPut(b *testing.B, backend BenchBackend, size int64) {
	s := benchStore(b, backend.Backend)
	data := content(size, seedPut)
	var requests int64
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		before := backend.sent()
		o, err := store(b.Context(), s, data)
		if err != nil {
			b.Fatalf("put %d bytes: %v", size, err)
		}
		requests += backend.sent() - before
		// DELETED OUTSIDE THE TIMER, so the store does not grow by a
		// copy per iteration — a broker's stream or a fake's map that
		// grew through the case would measure its own growth — and the
		// delete's cost, and its request, are not counted as the put's.
		b.StopTimer()
		ctx, cancel := context.WithTimeout(b.Context(), time.Minute)
		err = s.Delete(ctx, o.Key)
		cancel()
		if err != nil {
			b.Fatalf("delete %s: %v", o.Key, err)
		}
		b.StartTimer()
	}
	backend.reportRequests(b, requests)
}

// benchRead is b.N whole reads of ONE object, written just before — so every
// read after the first is served warm (see [Bench]).
func benchRead(b *testing.B, backend BenchBackend, size int64, verified bool) {
	s := benchStore(b, backend.Backend)
	o := keep(b, s, content(size, seedRead))
	buf := make([]byte, copyBuffer)
	b.SetBytes(size)
	b.ReportAllocs()
	for b.Loop() {
		if err := readWhole(b.Context(), s, o, buf, verified); err != nil {
			b.Fatal(err)
		}
	}
}

// readWhole drains o — through the store's verified [objstore.Store.Open], or
// the backend's own unverified stream under the budget Open would set — and
// checks it was the whole object.
func readWhole(ctx context.Context, s *objstore.Store, o objstore.Object, buf []byte, verified bool) error {
	var (
		r   io.ReadCloser
		err error
	)
	if verified {
		r, err = s.Open(ctx, o)
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, objstore.ReadBudget(o.Size))
		defer cancel()
		r, err = s.Backend().Get(ctx, o.Key.Name(), 0, -1)
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", o.Key, err)
	}
	defer func() { _ = r.Close() }()
	n, err := io.CopyBuffer(discard{}, r, buf)
	switch {
	case err != nil:
		return fmt.Errorf("read %s: %w", o.Key, err)
	case n != o.Size:
		return fmt.Errorf("read %d of the %d bytes of %s", n, o.Size, o.Key)
	}
	return nil
}

func benchRanged(b *testing.B, backend BenchBackend) {
	s := benchStore(b, backend.Backend)
	o := keep(b, s, content(pagedBytes, seedRanged))
	// A FIXED SEED, so every run and every backend reads the same offsets
	// — uniform over the object, a page apart from its end at the most.
	at := rand.New(rand.NewPCG(seedRanged, 4))
	b.SetBytes(pageBytes)
	b.ReportAllocs()
	for b.Loop() {
		off := at.Int64N(pagedBytes - pageBytes + 1)
		page, err := s.ReadAt(b.Context(), o, off, pageBytes)
		if err != nil || len(page) != pageBytes {
			b.Fatalf("read %d bytes at %d: got %d, %v", pageBytes, off, len(page), err)
		}
	}
}

func benchParallel(b *testing.B, backend BenchBackend) {
	s := benchStore(b, backend.Backend)
	objects := make([]objstore.Object, parallelReads)
	bufs := make([][]byte, parallelReads)
	for i := range objects {
		// EIGHT DISTINCT OBJECTS, as a cache sync's eight downloads are:
		// eight reads of one object at once would let a backend serve
		// seven of them from the work the first is doing — one walk of
		// one stream's blocks, one object's bytes pulled into memory —
		// and the figure would be one stream's. Each is as warm as the
		// single-stream cases' one object (see [Bench]).
		objects[i] = keep(b, s, content(parallelBytes, seedParallel+uint64(i)))
		bufs[i] = make([]byte, copyBuffer)
	}
	b.SetBytes(parallelReads * parallelBytes)
	b.ReportAllocs()
	for b.Loop() {
		var wg sync.WaitGroup
		errs := make([]error, parallelReads)
		for i, o := range objects {
			wg.Go(func() { errs[i] = readWhole(b.Context(), s, o, bufs[i], true) })
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			b.Fatal(err)
		}
	}
}

// benchStat is b.N stats across workers goroutines of the objects order
// names, in that order and round again, reported as stats a second.
//
// b.N COUNTED BY HAND rather than through b.Loop: a pool fed through b.Loop
// would still be finishing its last stats after Loop returned false and the
// timer stopped, so the figure would leave out up to a stat per worker. Here
// the timer runs until every worker has returned.
func benchStat(b *testing.B, backend BenchBackend, objects []objstore.Object, order []int, workers int) {
	var (
		next atomic.Int64
		wg   sync.WaitGroup
	)
	errs := make([]error, workers)
	b.ReportAllocs()
	b.ResetTimer()
	started := time.Now()
	for w := range workers {
		wg.Go(func() {
			for {
				i := next.Add(1) - 1
				if i >= int64(b.N) {
					return
				}
				o := objects[order[i%int64(len(order))]]
				info, err := backend.Stat(b.Context(), o.Key.Name())
				if err == nil && info.Size != o.Size {
					err = fmt.Errorf("stat of %s says %d bytes, want %d", o.Key, info.Size, o.Size)
				}
				if err != nil {
					errs[w] = err
					return
				}
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(started)
	b.StopTimer()
	if err := errors.Join(errs...); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "stats/s")
}

// benchList is whole listings of a store holding want objects, each checked
// to have named every one, reported as names a second — and as the requests
// one listing made, where they are counted.
func benchList(b *testing.B, backend BenchBackend, want int) {
	var requests int64
	b.ReportAllocs()
	for b.Loop() {
		before := backend.sent()
		named := 0
		if err := backend.List(b.Context(), func(objstore.Info) error {
			named++
			return nil
		}); err != nil {
			b.Fatalf("list a store of %d objects: %v", want, err)
		}
		if named != want {
			b.Fatalf("a listing of a store of %d objects named %d", want, named)
		}
		requests += backend.sent() - before
	}
	b.ReportMetric(float64(want)*float64(b.N)/b.Elapsed().Seconds(), "names/s")
	backend.reportRequests(b, requests)
}
