// Package objstoretest is the object store's conformance suite: ONE set of
// cases every [objstore.Backend] passes — the JetStream object store, an
// S3-compatible bucket and the in-memory twin alike.
//
// A twin that agreed only with itself would prove nothing, and the store and
// the collector rest on properties no single backend's own tests would think
// to state: that a put which fails or is abandoned leaves nothing anybody can
// find — a body cut short, which reads much like a body's end, included — that
// a read with no deadline is refused rather than bounded by some
// default of the backend's, that a put of a name already held moves its
// written instant, and that a listing visits every name it holds — verbatim,
// once, however slowly it is read.
package objstoretest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Factory builds an empty backend for one case.
type Factory func(t *testing.T) objstore.Backend

// Options tunes the suite to what a backend can promise.
type Options struct {
	// Granularity is the coarsest step the backend's written instants
	// move in — a second for S3's LastModified. The re-put case waits
	// past it before asking whether the instant moved.
	Granularity time.Duration

	// Piece is the most bytes the backend moves in one message or request
	// of a put — an S3 part, a broker message. The large and ranged cases
	// are sized from it, so they cross its boundaries. Zero is
	// [defaultPiece].
	Piece int64

	// Digest says the backend keeps a SHA-256 of what it was handed and
	// reports it, which the suite then holds it to.
	Digest bool

	// SlowListing is how long the slow-visitor case spreads one listing
	// across: long enough to outlast whatever the backend's listing could
	// time out or restart on. Zero is a second.
	SlowListing time.Duration
}

// defaultPiece is a piece for a backend with none of its own.
const defaultPiece = 256 << 10

func (o Options) piece() int64 {
	if o.Piece > 0 {
		return o.Piece
	}
	return defaultPiece
}

// Run runs every case against backends built by newBackend.
func Run(t *testing.T, newBackend Factory, opts Options) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, b objstore.Backend, opts Options)
	}{
		{"an_object_reads_back_exactly", anObjectReadsBackExactly},
		{"an_empty_object_round_trips", anEmptyObjectRoundTrips},
		{"a_large_object_streams_both_ways", aLargeObjectStreamsBothWays},
		{"a_ranged_get_answers_its_range", aRangedGetAnswersItsRange},
		{"a_name_never_put_is_not_found", aNameNeverPutIsNotFound},
		{"a_read_without_a_deadline_is_refused", aReadWithoutADeadlineIsRefused},
		{"a_deleted_object_is_gone_and_a_second_delete_is_fine", aDeletedObjectIsGone},
		{"a_re_put_moves_the_written_instant", aRePutMovesTheWrittenInstant},
		{"a_put_whose_reader_fails_leaves_nothing", aPutWhoseReaderFailsLeavesNothing},
		{"a_put_whose_context_ends_leaves_nothing", aPutWhoseContextEndsLeavesNothing},
		{"a_listing_visits_every_name_once", aListingVisitsEveryNameOnce},
		{"a_listing_stops_at_the_visitors_error", aListingStopsAtTheVisitorsError},
		{"a_listing_waits_for_a_slow_visitor", aListingWaitsForASlowVisitor},
		{"names_are_kept_verbatim", namesAreKeptVerbatim},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, newBackend(t), opts)
		})
	}
}

// readCtx is a context fit for a read: every backend refuses one with no
// deadline.
func readCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func patterned(n int64, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31+i/251) ^ seed
	}
	return b
}

// streamed hides every method of r but Read, so a backend cannot take a
// shortcut a request body would not offer it — a length, or a seek.
type streamed struct{ r io.Reader }

func (s streamed) Read(p []byte) (int, error) { return s.r.Read(p) }

func put(t *testing.T, b objstore.Backend, name string, data []byte) {
	t.Helper()
	if err := b.Put(t.Context(), name, streamed{bytes.NewReader(data)},
		objstore.PutMeta{ContentType: "application/octet-stream"}); err != nil {
		t.Fatalf("Put %s: %v", name, err)
	}
}

func get(t *testing.T, b objstore.Backend, name string, off, n int64) ([]byte, error) {
	t.Helper()
	r, err := b.Get(readCtx(t), name, off, n)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func sha(data []byte) objstore.Hash {
	sum := sha256.Sum256(data)
	return objstore.Hash(hex.EncodeToString(sum[:]))
}

// listed is every name a listing visited, with how often and what it said.
func listed(t *testing.T, b objstore.Backend) map[string][]objstore.Info {
	t.Helper()
	out := map[string][]objstore.Info{}
	if err := b.List(t.Context(), func(info objstore.Info) error {
		out[info.Name] = append(out[info.Name], info)
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	return out
}

// gone asserts nothing a stat or a listing could find is held under name.
func gone(t *testing.T, b objstore.Backend, name, after string) {
	t.Helper()
	if info, err := b.Stat(t.Context(), name); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat after %s = %+v, %v; want ErrNotFound", after, info, err)
	}
	if got := listed(t, b)[name]; len(got) != 0 {
		t.Fatalf("a listing after %s visited %s: %+v", after, name, got)
	}
}

func anObjectReadsBackExactly(t *testing.T, b objstore.Backend, opts Options) {
	data := []byte("an object, whole")
	before := time.Now().Add(-time.Minute)
	put(t, b, "whole", data)
	got, err := get(t, b, "whole", 0, -1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v; want %q", got, err, data)
	}
	info, err := b.Stat(t.Context(), "whole")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Name != "whole" || info.Size != int64(len(data)) {
		t.Fatalf("Stat = %+v, want the name and %d bytes", info, len(data))
	}
	if info.Written.Before(before) || info.Written.After(time.Now().Add(time.Minute)) {
		t.Fatalf("Stat says written %v, want about now", info.Written)
	}
	if info.Written.Location() != time.UTC {
		t.Fatalf("Stat answered %v in %v; every instant here is UTC", info.Written, info.Written.Location())
	}
	checkDigest(t, info, data, opts)
	l := listed(t, b)["whole"]
	if len(l) != 1 {
		t.Fatalf("the object was listed %d times, want once", len(l))
	}
	if l[0].Size != int64(len(data)) || !l[0].Written.Equal(info.Written) {
		t.Fatalf("the listing says %+v and Stat %+v", l[0], info)
	}
	checkDigest(t, l[0], data, opts)
}

// checkDigest holds a backend that keeps a digest to it, and every backend
// to never reporting a wrong one.
func checkDigest(t *testing.T, info objstore.Info, data []byte, opts Options) {
	t.Helper()
	switch {
	case opts.Digest && info.Digest != sha(data):
		t.Fatalf("%s reports digest %q, want the SHA-256 of what was put, %s",
			info.Name, info.Digest, sha(data))
	case !opts.Digest && info.Digest != "" && info.Digest != sha(data):
		t.Fatalf("%s reports digest %q, which is not the SHA-256 of what was put",
			info.Name, info.Digest)
	}
}

func anEmptyObjectRoundTrips(t *testing.T, b objstore.Backend, opts Options) {
	put(t, b, "empty", nil)
	got, err := get(t, b, "empty", 0, -1)
	if err != nil || len(got) != 0 {
		t.Fatalf("Get of an empty object = %q, %v", got, err)
	}
	info, err := b.Stat(t.Context(), "empty")
	if err != nil || info.Size != 0 {
		t.Fatalf("Stat of an empty object = %+v, %v", info, err)
	}
	checkDigest(t, info, nil, opts)
	if got := listed(t, b)["empty"]; len(got) != 1 {
		t.Fatalf("an empty object was listed %d times, want once", len(got))
	}
}

// large is an object two pieces and a ragged tail long, so it crosses two of
// the backend's own boundaries and ends inside a third.
func large(opts Options) []byte { return patterned(2*opts.piece()+17, 0x5a) }

func aLargeObjectStreamsBothWays(t *testing.T, b objstore.Backend, opts Options) {
	data := large(opts)
	put(t, b, "large", data)
	got, err := get(t, b, "large", 0, -1)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a large object read back %d bytes, %v; want the %d put", len(got), err, len(data))
	}
	info, err := b.Stat(t.Context(), "large")
	if err != nil || info.Size != int64(len(data)) {
		t.Fatalf("Stat = %+v, %v", info, err)
	}
	checkDigest(t, info, data, opts)
}

func aRangedGetAnswersItsRange(t *testing.T, b objstore.Backend, opts Options) {
	data := large(opts)
	put(t, b, "ranged", data)
	size, p := int64(len(data)), opts.piece()
	for _, c := range []struct{ off, n int64 }{
		{0, 10},
		{p - 5, 10},      // across the first boundary
		{p, p},           // exactly the second piece
		{2*p - 1, 2},     // across the second
		{size - 10, 100}, // short at the end
		{size - 1, -1},   // to the end, from its last byte
		{p + 3, -1},      // to the end, from inside
		{0, size},        // the whole, by its length
		{size, 5},        // at the end: nothing
		{size + 9, -1},   // past the end: nothing
	} {
		got, err := get(t, b, "ranged", c.off, c.n)
		if err != nil {
			t.Fatalf("Get(%d, %d): %v", c.off, c.n, err)
		}
		start, end := min(c.off, size), size
		if c.n >= 0 {
			end = min(start+c.n, size)
		}
		if want := data[start:end]; !bytes.Equal(got, want) {
			t.Fatalf("Get(%d, %d) answered %d bytes, want %d", c.off, c.n, len(got), len(want))
		}
	}
	for _, c := range []struct{ off, n int64 }{{-1, 5}, {0, 0}} {
		if _, err := get(t, b, "ranged", c.off, c.n); err == nil {
			t.Errorf("Get(%d, %d) was answered; a negative offset and an empty range are refused", c.off, c.n)
		}
	}
}

func aNameNeverPutIsNotFound(t *testing.T, b objstore.Backend, _ Options) {
	if _, err := get(t, b, "absent", 0, -1); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Get of a name never put = %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(t.Context(), "absent"); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat of a name never put = %v, want ErrNotFound", err)
	}
}

// A READ WITH NO DEADLINE IS REFUSED BY EVERY BACKEND, rather than bounded by
// a default one of them picked: the broker's library would cut it five
// seconds in, whole download included, and the others would let a stalled
// read hold its caller for ever.
func aReadWithoutADeadlineIsRefused(t *testing.T, b objstore.Backend, _ Options) {
	put(t, b, "deadline", []byte("read me in time"))
	r, err := b.Get(t.Context(), "deadline", 0, -1)
	if err == nil {
		_ = r.Close()
	}
	if !errors.Is(err, objstore.ErrNoDeadline) {
		t.Fatalf("Get with no deadline = %v, want ErrNoDeadline", err)
	}
}

func aDeletedObjectIsGone(t *testing.T, b objstore.Backend, _ Options) {
	data := []byte("deleted")
	put(t, b, "deleted", data)
	if err := b.Delete(t.Context(), "deleted"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := get(t, b, "deleted", 0, -1); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	gone(t, b, "deleted", "a delete")
	if err := b.Delete(t.Context(), "deleted"); err != nil {
		t.Fatalf("a second Delete = %v; deleting what is not there is not an error", err)
	}
	if err := b.Delete(t.Context(), "never-put"); err != nil {
		t.Fatalf("a Delete of a name never put = %v", err)
	}
	put(t, b, "deleted", data)
	if got, err := get(t, b, "deleted", 0, -1); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("an object put again after a delete reads %q, %v", got, err)
	}
}

// THE COLLECTOR'S GRACE RESTS ON THIS while a file may re-use a chunk: the
// re-put is what makes it young again.
func aRePutMovesTheWrittenInstant(t *testing.T, b objstore.Backend, opts Options) {
	data := []byte("re-put")
	put(t, b, "re-put", data)
	first, err := b.Stat(t.Context(), "re-put")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(max(opts.Granularity, 10*time.Millisecond) + 100*time.Millisecond)
	put(t, b, "re-put", data)
	second, err := b.Stat(t.Context(), "re-put")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Written.After(first.Written) {
		t.Fatalf("a re-put left the written instant at %v (first put %v); the collector "+
			"would delete a chunk a new file had just re-used", second.Written, first.Written)
	}
	l := listed(t, b)["re-put"]
	if len(l) != 1 || !l[0].Written.After(first.Written) {
		t.Fatalf("the listing says %+v for an object re-put after %v", l, first.Written)
	}
}

// failing yields its bytes and then fails, as a client whose connection
// drops does.
type failing struct {
	r    io.Reader
	fail error
}

func (f *failing) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.fail
	}
	return n, err
}

// A PUT WHOSE READER FAILS LEAVES NOTHING — not the pieces it had already
// sent, which no listing would show and nothing would ever delete — and fails
// with the reader's own error.
//
// AND THE END OF THE OBJECT IS io.EOF ITSELF, nothing that resembles it. A
// request body cut short fails with io.ErrUnexpectedEOF — net/http's word for
// a client that went away mid-body — and the API wraps it to say whose failure
// it was; a backend that read either, or a wrapped io.EOF, as the end stored
// the part of an upload that arrived as the whole of it and reported the put
// a success. So every failure is tried at the three places a backend's own
// reading can meet it: inside its first piece, exactly at a piece's end, and
// part of the way through a later one.
func aPutWhoseReaderFailsLeavesNothing(t *testing.T, b objstore.Backend, opts Options) {
	failures := []struct {
		name string
		err  error
	}{
		{"its_own_error", errors.New("the client went away")},
		{"unexpected_eof", io.ErrUnexpectedEOF},
		{"a_wrapped_unexpected_eof", fmt.Errorf("the file's body: %w", io.ErrUnexpectedEOF)},
		{"a_wrapped_eof", fmt.Errorf("the file's body: %w", io.EOF)},
	}
	sizes := []struct {
		name string
		n    int64
	}{
		{"inside_the_first_piece", 1000},
		{"at_the_end_of_a_piece", opts.piece()},
		{"inside_a_later_piece", opts.piece() + opts.piece()/2},
	}
	for _, f := range failures {
		for _, s := range sizes {
			t.Run(f.name+"/"+s.name, func(t *testing.T) {
				name := "failed-" + f.name + "-" + s.name
				err := b.Put(t.Context(), name, &failing{r: bytes.NewReader(patterned(s.n, 1)),
					fail: f.err}, objstore.PutMeta{})
				if !errors.Is(err, f.err) {
					t.Fatalf("Put over a reader failing with %q after %d bytes = %v, "+
						"want the reader's own error", f.err, s.n, err)
				}
				gone(t, b, name, "a failed put")
			})
		}
	}
}

// cancelling cancels its context once it has yielded after bytes, and keeps
// yielding — so only a put that watches its context stops.
type cancelling struct {
	r      io.Reader
	after  int64
	read   int64
	cancel context.CancelFunc
}

func (c *cancelling) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read >= c.after {
		c.cancel()
	}
	return n, err
}

// A PUT WHOSE CONTEXT ENDS MID-STREAM LEAVES NOTHING: the caller giving up is
// a failed put, and cleaned up after as one.
func aPutWhoseContextEndsLeavesNothing(t *testing.T, b objstore.Backend, opts Options) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body := patterned(3*opts.piece(), 2)
	err := b.Put(ctx, "abandoned", &cancelling{r: bytes.NewReader(body),
		after: opts.piece() + opts.piece()/2, cancel: cancel}, objstore.PutMeta{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put whose context ended = %v, want context.Canceled", err)
	}
	gone(t, b, "abandoned", "an abandoned put")
}

func aListingVisitsEveryNameOnce(t *testing.T, b objstore.Backend, _ Options) {
	want := map[string]int{}
	for i := range 25 {
		name := fmt.Sprintf("listed-%02d", i)
		put(t, b, name, bytes.Repeat([]byte{'x'}, i))
		want[name] = i
	}
	put(t, b, "listed-gone", []byte("deleted before the listing"))
	if err := b.Delete(t.Context(), "listed-gone"); err != nil {
		t.Fatal(err)
	}
	seen := listed(t, b)
	for name, size := range want {
		got := seen[name]
		if len(got) != 1 {
			t.Errorf("%s was listed %d times, want once", name, len(got))
			continue
		}
		if got[0].Size != int64(size) || got[0].Written.IsZero() {
			t.Errorf("%s was listed as %+v, want %d bytes and a written instant", name, got[0], size)
		}
	}
	if len(seen["listed-gone"]) != 0 {
		t.Errorf("a deleted object was listed")
	}
	if len(seen) != len(want) {
		t.Errorf("listed %d names, want %d", len(seen), len(want))
	}
}

func aListingStopsAtTheVisitorsError(t *testing.T, b objstore.Backend, _ Options) {
	for i := range 3 {
		put(t, b, fmt.Sprintf("stop-%d", i), []byte("x"))
	}
	stop := errors.New("enough")
	visits := 0
	err := b.List(t.Context(), func(objstore.Info) error {
		visits++
		return stop
	})
	if !errors.Is(err, stop) || visits != 1 {
		t.Fatalf("List = %v after %d visits; want the visitor's error after one", err, visits)
	}
}

// A LISTING WAITS FOR A SLOW VISITOR — the collector judges and deletes as it
// goes — and neither skips nor repeats a name for it.
func aListingWaitsForASlowVisitor(t *testing.T, b objstore.Backend, opts Options) {
	const names = 20
	for i := range names {
		put(t, b, fmt.Sprintf("slow-%02d", i), []byte("x"))
	}
	pace := max(opts.SlowListing, time.Second) / names
	seen := map[string]int{}
	if err := b.List(t.Context(), func(info objstore.Info) error {
		seen[info.Name]++
		time.Sleep(pace)
		return nil
	}); err != nil {
		t.Fatalf("a slowly read listing: %v", err)
	}
	if len(seen) != names {
		t.Fatalf("a slowly read listing visited %d names, want %d", len(seen), names)
	}
	for name, n := range seen {
		if n != 1 {
			t.Errorf("a slowly read listing visited %s %d times", name, n)
		}
	}
}

// NAMES ARE KEPT VERBATIM, whatever they look like: what a name means is the
// object store's grammar and the collector's judgement, never a backend's, so
// a backend that filtered or rewrote them would hide objects the collector
// has to see.
func namesAreKeptVerbatim(t *testing.T, b objstore.Backend, _ Options) {
	names := []string{
		string(sha([]byte("a content address"))),
		"0199b4f6-6d2a-7c3e-8f10-2b3c4d5e6f70",
		"files/0199b4f6-6d2a-7c3e-8f10-2b3c4d5e6f71",
		"Not-Ours_1.txt",
	}
	for i, name := range names {
		put(t, b, name, []byte{byte(i)})
	}
	seen := listed(t, b)
	got := make([]string, 0, len(seen))
	for name := range seen {
		got = append(got, name)
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(names))
	if !slices.Equal(got, want) {
		t.Fatalf("listed %q, want exactly %q", got, want)
	}
	for i, name := range names {
		if data, err := get(t, b, name, 0, -1); err != nil || !bytes.Equal(data, []byte{byte(i)}) {
			t.Fatalf("Get %q = %v, %v", name, data, err)
		}
	}
}
