package collect

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkAudit measures what the audit's bound buys across the walk's page
// reads: how many references a second it gets through, how many questions it
// keeps out on average against its bound of [auditConcurrency], and how many
// requests each reference costs — over a store that answers each request after
// a fixed latency and an estate whose every page read takes one of its own.
//
// MANY PAGES of references, so the walk reads on while questions are out —
// the claim under test is that a page read does not drain the bound — and
// four stores. One asked about each object by name ("by_name"), as a store
// that keeps no order is once its listing has failed. Two that keep no order
// and are listed whole, a window at a time, as the broker's is: in one window
// ("listed") and in eight ("listed_windows", the window cut to an eighth of
// the references) — where each listing names every object again, which is
// what [auditWindow] trades. And one that lists ranges in order a page of
// [rangePage] names a request, as S3 does, given more references than its
// bound has ranges out at once, and holding objects no required row names
// between them — one in fifty, one in five, as the last day's keys may, and one
// beside every reference ("ranged_derived"), the most an index kept over packs
// puts among them — since what a range holds besides its references is
// what the walk's measured cut answers ([auditor.cut]). The latencies are the
// model's inputs, not a measurement of any store
// (a whole listing's is a name's cost at the broker's measured pace, about
// eighty thousand names a second): what comes out is the depth the audit
// keeps and the requests it makes, which no store's speed changes.
//
//	go test -run '^$' -bench BenchmarkAudit -benchtime 3x ./internal/objstore/collect/
func BenchmarkAudit(b *testing.B) {
	const (
		pageRead = time.Millisecond
		stat     = 2 * time.Millisecond
		listing  = 10 * time.Millisecond
		listName = time.Second / 80_000
	)
	for _, store := range []struct {
		name       string
		ranged     bool
		listed     bool
		windows    int
		references int
		// unnamed is how many referenced objects are stored to each
		// object no row names — zero for none.
		unnamed int
	}{
		{"by_name", false, false, 0, 16 * objstore.WalkPage, 0},
		{"listed", false, true, 1, 16 * objstore.WalkPage, 0},
		{"listed_windows", false, true, 8, 16 * objstore.WalkPage, 0},
		// MORE RANGES THAN THE BOUND, so the depth it keeps is measured.
		{"ranged", true, false, 0, 2 * auditConcurrency * rangeChunk, 50},
		{"ranged_dense", true, false, 0, 2 * auditConcurrency * rangeChunk, 5},
		{"ranged_derived", true, false, 0, 2 * auditConcurrency * rangeChunk, 1},
	} {
		b.Run(store.name, func(b *testing.B) {
			mem := memobj.New()
			seed, err := objstore.NewStore(mem)
			if err != nil {
				b.Fatal(err)
			}
			src := &paged{read: pageRead}
			var names []string
			for i := range store.references {
				o, err := seed.Put(b.Context(), strings.NewReader(fmt.Sprintf("object %d", i)), 1<<20, objstore.PutMeta{})
				if err != nil {
					b.Fatal(err)
				}
				src.refs = append(src.refs, objstore.Reference{Object: o, NamedBy: fmt.Sprintf("ENG/%06d.md", i)})
				names = append(names, o.Key.Name())
				if store.unnamed > 0 && i%store.unnamed == 0 {
					// MINTED NEXT, so its key falls between two
					// referenced ones.
					u, err := seed.Put(b.Context(), strings.NewReader(fmt.Sprintf("replaced %d", i)), 1<<20, objstore.PutMeta{})
					if err != nil {
						b.Fatal(err)
					}
					names = append(names, u.Key.Name())
				}
			}
			slices.SortFunc(src.refs, func(x, y objstore.Reference) int { return x.Object.Key.Compare(y.Object.Key) })
			slices.Sort(names)
			slow := &latent{Backend: mem, stat: stat}
			var backend objstore.Backend
			switch {
			case store.ranged:
				backend = &latentRanged{latent: slow, mem: mem, names: names, listing: listing}
			case store.listed:
				backend = &latentListed{latent: slow, name: listName}
			default:
				backend = &unlisted{latent: slow}
			}
			s, err := objstore.NewStore(backend)
			if err != nil {
				b.Fatal(err)
			}
			c, err := New(Options{Store: s, References: References{src}})
			if err != nil {
				b.Fatal(err)
			}
			if store.windows > 0 {
				c.window = (store.references + store.windows - 1) / store.windows
			}
			start := time.Now()
			slow.reset(start)
			for b.Loop() {
				if r, err := c.Audit(b.Context()); err != nil || r.Referenced != store.references || r.Found.Lost() != 0 {
					b.Fatalf("Audit = %+v, %v", r, err)
				}
			}
			end := time.Now()
			mean, requests := slow.measured(end)
			b.ReportMetric(float64(store.references*b.N)/end.Sub(start).Seconds(), "refs/s")
			b.ReportMetric(mean, "mean-in-flight")
			b.ReportMetric(float64(requests)/float64(store.references*b.N), "requests/ref")
		})
	}
}

// paged is a source whose references, held sorted, are walked a page of
// [objstore.WalkPage] at a time, each page read taking read.
type paged struct {
	refs []objstore.Reference
	read time.Duration
}

func (*paged) Name() string { return "tracker" }

func (*paged) Barrier(context.Context) (statelog.Position, error) { return statelog.Position{}, nil }

func (*paged) Referenced(context.Context, []objstore.Key, statelog.Position) (map[objstore.Key]struct{}, bool, error) {
	return map[objstore.Key]struct{}{}, true, nil
}

func (p *paged) Required(statelog.Position) []objstore.Walk { return []objstore.Walk{&pagedWalk{p: p}} }

// Moved is the audit's walk too: every reference is a required table's.
func (p *paged) Moved(at statelog.Position) []objstore.Walk { return p.Required(at) }

type pagedWalk struct {
	p    *paged
	next int
}

func (w *pagedWalk) Next(context.Context) ([]objstore.Reference, bool, error) {
	if w.next >= len(w.p.refs) {
		return nil, true, nil
	}
	time.Sleep(w.p.read)
	end := min(w.next+objstore.WalkPage, len(w.p.refs))
	page := w.p.refs[w.next:end]
	w.next = end
	return page, true, nil
}

// latent is a backend answering every stat after a fixed latency, and keeping
// the time-weighted count of requests out at once.
type latent struct {
	objstore.Backend
	stat time.Duration

	mu       sync.Mutex
	out      int
	from     time.Time
	last     time.Time
	area     float64 // request-seconds
	requests int
}

func (l *latent) reset(at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out, l.from, l.last, l.area, l.requests = 0, at, at, 0, 0
}

// moved records the count of requests out changing by d.
func (l *latent) moved(d int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.area += float64(l.out) * now.Sub(l.last).Seconds()
	l.last = now
	l.out += d
	if d > 0 {
		l.requests++
	}
}

// measured is the mean count of requests out since the last reset, and how
// many were made.
func (l *latent) measured(now time.Time) (float64, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	area := l.area + float64(l.out)*now.Sub(l.last).Seconds()
	return area / now.Sub(l.from).Seconds(), l.requests
}

func (l *latent) wait(ctx context.Context, d time.Duration) error {
	l.moved(1)
	defer l.moved(-1)
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *latent) Stat(ctx context.Context, name string) (objstore.Info, error) {
	if err := l.wait(ctx, l.stat); err != nil {
		return objstore.Info{}, err
	}
	return l.Backend.Stat(ctx, name)
}

// latentRanged is [latent] listing ranges in order from names held sorted, as
// S3 does: a request a page of [rangePage] names, each after a fixed latency,
// until a page reaches the range's last name — so a range holding more than a
// page costs a request for each page it spans.
type latentRanged struct {
	*latent
	mem     *memobj.Backend
	names   []string
	listing time.Duration
}

func (l *latentRanged) ListRange(ctx context.Context, after, through string, visit func(objstore.Info) error) error {
	i := sort.SearchStrings(l.names, after)
	if i < len(l.names) && l.names[i] == after {
		i++
	}
	for {
		if err := l.wait(ctx, l.listing); err != nil {
			return err
		}
		page := l.names[i:min(i+rangePage, len(l.names))]
		for _, name := range page {
			if name > through {
				return nil
			}
			info, err := l.mem.Stat(ctx, name)
			if err != nil {
				return err
			}
			if err := visit(info); err != nil {
				return err
			}
		}
		i += len(page)
		if len(page) < rangePage || page[len(page)-1] >= through {
			return nil
		}
	}
}

// latentListed is [latent] listed whole, as the broker's object store is: one
// request, answered once every name it holds has been handed on at name a
// piece.
type latentListed struct {
	*latent
	name time.Duration
}

func (l *latentListed) List(ctx context.Context, visit func(objstore.Info) error) error {
	n := 0
	if err := l.Backend.List(ctx, func(info objstore.Info) error {
		n++
		return visit(info)
	}); err != nil {
		return err
	}
	return l.wait(ctx, time.Duration(n)*l.name)
}

// unlisted is [latent] whose every listing fails at once, so the audit asks
// it about each object by name.
type unlisted struct{ *latent }

func (u *unlisted) List(ctx context.Context, _ func(objstore.Info) error) error {
	u.moved(1)
	defer u.moved(-1)
	return errors.New("the listing failed")
}

// BenchmarkAuditWindow measures what one window of the audit's listing of a
// store that keeps no order holds in memory, in bytes a key, against which
// [auditWindow] is chosen: the window kept in its map and its heap, each
// object's size and digest — the digest a string of its own, as the broker's
// listing decodes one for every name — after a collection, from a listing of
// three windows' worth of names. And how fast the window takes names, which
// says that the listing, never the window, is what an audit's listing waits
// on.
//
//	go test -run '^$' -bench BenchmarkAuditWindow -benchtime 3x ./internal/objstore/collect/
func BenchmarkAuditWindow(b *testing.B) {
	listing := generatedListing{n: 3 * auditWindow}
	s, err := objstore.NewStore(listing)
	if err != nil {
		b.Fatal(err)
	}
	a := &auditor{store: s, window: auditWindow}
	var held float64
	for b.Loop() {
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		w, err := a.listWindow(b.Context(), objstore.Key{})
		if err != nil || len(w.held) != auditWindow || w.last {
			b.Fatalf("listWindow = %d keys (last %v), %v; want a window of %d", len(w.held), w.last, err, auditWindow)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		held = float64(after.HeapAlloc-before.HeapAlloc) / float64(len(w.held))
		runtime.KeepAlive(w)
	}
	b.ReportMetric(held, "bytes/key")
	b.ReportMetric(float64(listing.n*b.N)/b.Elapsed().Seconds(), "names/s")
}

// generatedListing is a store that keeps no order whose listing names n
// objects, each with a size and a digest decoded afresh, as the broker's
// listing hands them on; it answers nothing else.
type generatedListing struct {
	objstore.Backend
	n int
}

func (g generatedListing) List(_ context.Context, visit func(objstore.Info) error) error {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range g.n {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d", i))
		if err := visit(objstore.Info{Name: objstore.KeyAt(at.Add(time.Duration(i) * time.Millisecond)).Name(),
			Size: int64(i), Written: at, Digest: objstore.Hash(hex.EncodeToString(sum[:]))}); err != nil {
			return err
		}
	}
	return nil
}
