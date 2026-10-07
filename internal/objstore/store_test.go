package objstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

func newStore(t *testing.T, backend objstore.Backend) *objstore.Store {
	t.Helper()
	s, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func patterned(n int, mul int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * mul)
	}
	return b
}

// trickle hands its bytes over a few at a time and offers nothing but Read,
// as a request body does.
type trickle struct{ r io.Reader }

func (t trickle) Read(p []byte) (int, error) { return t.r.Read(p[:min(len(p), 7)]) }

func put(t *testing.T, s *objstore.Store, body []byte) objstore.Object {
	t.Helper()
	o, err := s.Put(t.Context(), trickle{bytes.NewReader(body)}, int64(len(body)), objstore.PutMeta{})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return o
}

func names(t *testing.T, b objstore.Backend) []string {
	t.Helper()
	var out []string
	if err := b.List(t.Context(), func(info objstore.Info) error {
		out = append(out, info.Name)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func readAll(t *testing.T, s *objstore.Store, o objstore.Object) ([]byte, error) {
	t.Helper()
	r, err := s.Open(t.Context(), o)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

// A PUT COUNTS AND HASHES THE STREAM ITSELF, and stores it whole under the
// key it minted, in the engine's namespace — what a row records is this
// store's own account of the bytes, never a backend's.
func TestAPutHashesAndCountsTheStream(t *testing.T) {
	t.Parallel()
	backend := memobj.New()
	s := newStore(t, backend)
	for _, size := range []int{0, 1, 2, 3*objstore.MiB + 99} {
		body := patterned(size, 7)
		o := put(t, s, body)
		sum := sha256.Sum256(body)
		if o.Size != int64(size) || o.Hash != objstore.Hash(hex.EncodeToString(sum[:])) {
			t.Fatalf("a put of %d bytes answered %+v", size, o)
		}
		if err := o.Validate(); err != nil {
			t.Fatalf("a put answered an object no row could name: %v", err)
		}
		info, err := backend.Stat(t.Context(), o.Key.Name())
		if err != nil || info.Size != int64(size) {
			t.Fatalf("the backend holds %+v, %v under %s", info, err, o.Key.Name())
		}
		got, err := readAll(t, s, o)
		if err != nil || !bytes.Equal(got, body) {
			t.Fatalf("a %d-byte object read back %d bytes, %v", size, len(got), err)
		}
	}
}

// EVERY PUT MINTS A KEY NEVER WRITTEN BEFORE — the same content twice is two
// objects — and mints it at the store's own clock, which is what a write and
// the collector both judge its age by.
func TestEveryPutMintsAKeyNeverWrittenBefore(t *testing.T) {
	t.Parallel()
	backend := memobj.New()
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	s, err := objstore.NewStoreAt(backend, func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("the same bytes, twice")
	seen := map[objstore.Key]bool{}
	for range 50 {
		o := put(t, s, body)
		if seen[o.Key] {
			t.Fatalf("key %s was minted twice", o.Key)
		}
		seen[o.Key] = true
		if !o.Key.Minted().Equal(at) {
			t.Fatalf("a key minted at %v says %v", at, o.Key.Minted())
		}
	}
	if got := len(names(t, backend)); got != 50 {
		t.Fatalf("fifty puts of one content left %d objects, want fifty", got)
	}
}

// failing yields its bytes and then fails, as a client whose connection drops
// does.
type failing struct {
	r    io.Reader
	fail error
}

func (f *failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.fail
	}
	return n, err
}

// A PUT THAT FAILS LEAVES NOTHING AND ANSWERS ITS OWN FAILURE — one byte past
// the limit, the reader's own error, the caller giving up — whatever the
// backend wrapped it in.
func TestAPutThatFailsLeavesNothing(t *testing.T) {
	t.Parallel()
	dropped := errors.New("the client went away")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, c := range map[string]struct {
		ctx   context.Context
		r     io.Reader
		limit int64
		want  error
	}{
		"one byte past the limit": {t.Context(), bytes.NewReader(make([]byte, 101)), 100, objstore.ErrTooLarge},
		"the reader failing":      {t.Context(), &failing{bytes.NewReader(make([]byte, 10)), dropped}, 100, dropped},
		"a body cut short":        {t.Context(), &failing{bytes.NewReader(make([]byte, 10)), io.ErrUnexpectedEOF}, 100, io.ErrUnexpectedEOF},
		"the caller giving up":    {cancelled, bytes.NewReader(make([]byte, 10)), 100, context.Canceled},
	} {
		t.Run(name, func(t *testing.T) {
			backend := memobj.New()
			s := newStore(t, backend)
			o, err := s.Put(c.ctx, c.r, c.limit, objstore.PutMeta{})
			if !errors.Is(err, c.want) {
				t.Fatalf("Put = %+v, %v; want %v", o, err, c.want)
			}
			if got := names(t, backend); len(got) != 0 {
				t.Fatalf("a failed put left %v", got)
			}
		})
	}
	s := newStore(t, memobj.New())
	if o, err := s.Put(t.Context(), bytes.NewReader(make([]byte, 100)), 100, objstore.PutMeta{}); err != nil || o.Size != 100 {
		t.Fatalf("a put of exactly its limit = %+v, %v", o, err)
	}
}

// early is a backend whose put stops reading after a few bytes and calls the
// object stored.
type early struct{ objstore.Backend }

func (early) Put(_ context.Context, _ string, r io.Reader, _ objstore.PutMeta) error {
	_, err := r.Read(make([]byte, 3))
	return err
}

// A BACKEND THAT STOPS READING BEFORE THE END is not a stored object: the row
// would record the part it read as the whole.
func TestABackendThatStopsReadingEarlyIsRefused(t *testing.T) {
	t.Parallel()
	s := newStore(t, early{memobj.New()})
	if o, err := s.Put(t.Context(), bytes.NewReader(make([]byte, 10)), 100, objstore.PutMeta{}); err == nil {
		t.Fatalf("a put the backend stopped reading was answered %+v", o)
	}
}

// readsBefore reads r until it fails and answers how many bytes it handed
// over first.
func readsBefore(r io.Reader) (int, error) {
	var n int
	buf := make([]byte, 5)
	for {
		m, err := r.Read(buf)
		n += m
		if err != nil {
			return n, err
		}
	}
}

// A DAMAGED OBJECT FAILS AT ITS END AND NEVER HANDS OVER THE BYTE THAT
// COMPLETES IT — whether it is short of its size, longer, or the right length
// with the wrong bytes — so a download of it ends short of its
// Content-Length and a backup of it is not a file of the right size.
func TestADamagedObjectNeverHandsOverItsEnd(t *testing.T) {
	t.Parallel()
	const size = 4000
	body := patterned(size, 3)
	for name, stored := range map[string][]byte{
		"the wrong bytes":     append(patterned(size-1, 3), 0xff),
		"one byte short":      body[:size-1],
		"far short":           body[:10],
		"one byte too many":   append(append([]byte{}, body...), 0),
		"nothing at all":      {},
		"a flipped first bit": append([]byte{body[0] ^ 1}, body[1:]...),
	} {
		t.Run(name, func(t *testing.T) {
			backend := memobj.New()
			s := newStore(t, backend)
			o := put(t, s, body)
			backend.Corrupt(o.Key.Name(), stored)
			r, err := s.Open(t.Context(), o)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			n, err := readsBefore(r)
			if !errors.Is(err, objstore.ErrCorrupt) {
				t.Fatalf("a damaged object read %d bytes and ended %v, want ErrCorrupt", n, err)
			}
			if n >= size {
				t.Fatalf("a damaged object handed over %d of its %d bytes before it failed — "+
					"a download of it would end whole", n, size)
			}
		})
	}
}

// AN EMPTY OBJECT IS CHECKED TOO: its digest is the empty content's, and a
// row naming another digest for it is told so rather than answered nothing.
func TestAnEmptyObjectIsCheckedAgainstItsRow(t *testing.T) {
	t.Parallel()
	s := newStore(t, memobj.New())
	o := put(t, s, nil)
	if got, err := readAll(t, s, o); err != nil || len(got) != 0 {
		t.Fatalf("an empty object read %q, %v", got, err)
	}
	lying := o
	lying.Hash = objstore.HashOf([]byte("something"))
	if _, err := readAll(t, s, lying); !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("an empty object under a row naming other content = %v, want ErrCorrupt", err)
	}
}

// broken is a backend whose stream hands over some bytes and then fails, as a
// connection that drops does.
type broken struct {
	objstore.Backend
	after int
	err   error
}

func (b broken) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	r, err := b.Backend.Get(ctx, name, off, n)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{&failing{io.LimitReader(r, int64(b.after)), b.err}, r}, nil
}

// A READ THAT FAILS BEFORE THE END IS A FAILED READ, NOT CORRUPTION: the
// object may be whole, and a backup that recorded it lost or an alarm that
// called it damaged would send somebody to restore a file nothing is wrong
// with.
func TestAReadThatFailsBeforeTheEndIsNotCorruption(t *testing.T) {
	t.Parallel()
	dropped := errors.New("the connection went away")
	for _, after := range []int{0, 10, 3999} {
		backend := memobj.New()
		o := put(t, newStore(t, backend), patterned(4000, 3))
		s := newStore(t, broken{Backend: backend, after: after, err: dropped})
		_, err := readAll(t, s, o)
		if !errors.Is(err, dropped) || errors.Is(err, objstore.ErrCorrupt) {
			t.Fatalf("a read cut after %d bytes = %v, want the cut and not ErrCorrupt", after, err)
		}
	}
}

// AN OBJECT THE STORE DOES NOT HOLD IS NOT FOUND AT OPEN, before a caller has
// promised anybody a file.
func TestAMissingObjectIsNotFoundAtOpen(t *testing.T) {
	t.Parallel()
	s := newStore(t, memobj.New())
	o := objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.HashOf(nil)}
	if _, err := s.Open(t.Context(), o); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Open of an object never put = %v, want ErrNotFound", err)
	}
}

// watching is a backend recording the deadline its reads were asked under,
// whether its stream was closed, and — when block is set — a stream that
// hands over nothing until its context ends.
type watching struct {
	objstore.Backend
	block bool

	mu       sync.Mutex
	deadline time.Time
	closed   atomic.Bool
}

func (b *watching) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	b.mu.Lock()
	b.deadline, _ = ctx.Deadline()
	b.mu.Unlock()
	r, err := b.Backend.Get(ctx, name, off, n)
	if err != nil {
		return nil, err
	}
	return &watchedStream{ctx: ctx, r: r, b: b}, nil
}

type watchedStream struct {
	ctx context.Context
	r   io.ReadCloser
	b   *watching
}

func (w *watchedStream) Read(p []byte) (int, error) {
	if w.b.block {
		<-w.ctx.Done()
		return 0, w.ctx.Err()
	}
	return w.r.Read(p)
}

func (w *watchedStream) Close() error {
	w.b.closed.Store(true)
	return w.r.Close()
}

// A READ IS BOUNDED BY ITS OWN BUDGET when its caller carries no deadline —
// every backend refuses one without — and by the caller's when that is
// sooner.
func TestAReadIsBoundedByItsBudget(t *testing.T) {
	t.Parallel()
	backend := &watching{Backend: memobj.New()}
	s := newStore(t, backend)
	o := put(t, s, patterned(3*objstore.MiB, 1))
	before := time.Now()
	if _, err := readAll(t, s, o); err != nil {
		t.Fatal(err)
	}
	budget := objstore.ReadBudget(o.Size)
	if backend.deadline.Before(before.Add(budget)) || backend.deadline.After(time.Now().Add(budget)) {
		t.Fatalf("the backend read under %v, want %v from the read", backend.deadline, budget)
	}
	soon, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	want, _ := soon.Deadline()
	r, err := s.Open(soon, o)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	if !backend.deadline.Equal(want) {
		t.Fatalf("the backend read under %v, want the caller's sooner %v", backend.deadline, want)
	}
}

// A STALLED READ IS ENDED, AND A CLOSED ONE STOPS THE BACKEND'S: a backend
// that hands over nothing is cut off at the stall rather than at the end of a
// budget that is hours long for a large file, and a reader closed half way
// closes the stream under it.
func TestAStalledReadIsEndedAndAClosedOneStops(t *testing.T) {
	t.Parallel()
	backend := &watching{Backend: memobj.New()}
	s := newStore(t, backend)
	o := put(t, s, patterned(1000, 1))
	objstore.SetStall(s, 50*time.Millisecond)

	backend.block = true
	started := time.Now()
	_, err := readAll(t, s, o)
	if err == nil || errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("a stalled read = %v, want a failed read", err)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatalf("a stalled read took %v to end", took)
	}

	backend.block = false
	backend.closed.Store(false)
	r, err := s.Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !backend.closed.Load() {
		t.Fatal("closing a reader half way left the backend's stream open")
	}
}

// counting counts the reads that reached the backend.
type counting struct {
	objstore.Backend
	gets atomic.Int32
}

func (b *counting) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	b.gets.Add(1)
	return b.Backend.Get(ctx, name, off, n)
}

// A RANGED READ ANSWERS ITS RANGE: short at the end, empty past it — without
// asking the backend — and refused where the backend holds fewer bytes there
// than the row says, which is the one check a page can be held to.
func TestARangedReadAnswersItsRange(t *testing.T) {
	t.Parallel()
	backend := &counting{Backend: memobj.New()}
	s := newStore(t, backend)
	body := patterned(10_000, 11)
	o := put(t, s, body)
	for _, c := range []struct{ off, n int64 }{
		{0, 10}, {9_990, 10}, {9_995, 100}, {5_000, 0}, {0, 10_000},
	} {
		got, err := s.ReadAt(t.Context(), o, c.off, c.n)
		end := min(c.off+c.n, int64(len(body)))
		if err != nil || !bytes.Equal(got, body[c.off:end]) {
			t.Fatalf("ReadAt(%d, %d) = %d bytes, %v", c.off, c.n, len(got), err)
		}
	}
	before := backend.gets.Load()
	for _, off := range []int64{10_000, 20_000} {
		if got, err := s.ReadAt(t.Context(), o, off, 10); err != nil || len(got) != 0 {
			t.Fatalf("ReadAt past the end = %q, %v", got, err)
		}
	}
	if backend.gets.Load() != before {
		t.Fatal("a read at or past the end asked the backend")
	}
	backend.Backend.(*memobj.Backend).Corrupt(o.Key.Name(), body[:9_000])
	if _, err := s.ReadAt(t.Context(), o, 8_990, 100); !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("a page the backend holds short = %v, want ErrCorrupt", err)
	}
	if _, err := s.ReadAt(t.Context(), o, -1, 10); err == nil {
		t.Fatal("a read at a negative offset was answered")
	}
}

// A DELETE TAKES THE OBJECT AWAY, AND DELETING IT AGAIN IS NOT AN ERROR — two
// collectors judging one pass at once are both right.
func TestADeleteIsIdempotent(t *testing.T) {
	t.Parallel()
	backend := memobj.New()
	s := newStore(t, backend)
	o := put(t, s, []byte("going"))
	if _, err := s.Stat(t.Context(), o.Key); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Delete(t.Context(), o.Key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if _, err := s.Stat(t.Context(), o.Key); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
}
