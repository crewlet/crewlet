// Package objstoretest is the object store's conformance suite: ONE set of
// cases every [objstore.Backend] passes — the JetStream object store, an
// S3-compatible bucket and the in-memory twin alike.
//
// A twin that agreed only with itself would prove nothing, and the store and
// the collector rest on properties no single backend's own tests would think
// to state: that a put which fails or is abandoned leaves nothing anybody can
// find — a body cut short, which reads much like a body's end, included — that
// a read with no deadline is refused rather than bounded by some default of
// the backend's, that a read whose context ends stops rather than hands over
// what it had in hand, that a delete leaves nothing a listing could find, and
// that a listing visits every name it holds — verbatim, once, however slowly
// it is read — that an upload begun and never finished, which no listing
// of the names shows, is found and removed by the two verbs that exist for it,
// and that a name holding something the backend cannot read as an object is a
// definite answer, listed and deletable, rather than a failure. A backend that
// lists a range of its names in order ([objstore.Ranged]) is held to that too,
// and so is one that deletes many names in one request
// ([objstore.BatchDeleter]); one that does not claim either is held to not
// offering it — an ordering nothing certified is one an audit would read
// objects as lost by, and a batch nothing certified one that could delete a
// file somebody keeps.
//
// And ONE BENCHMARK SUITE ([Bench]) every backend runs from a
// `BenchmarkBackend` of its own, as it runs [Run] from `TestContract`: the
// figures the design argues from are measured through the same calls on every
// backend, so two backends' figures differ by the backend and nothing else.
// Each backend's package doc keeps what it measured, on what machine;
// `make bench` measures again.
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

// Factory builds an empty backend for one case. The cases run in parallel, so
// it is called concurrently and builds each backend on nothing another holds.
type Factory func(t *testing.T) objstore.Backend

// Options tunes the suite to what a backend can promise.
type Options struct {
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

	// Unfinished leaves an upload begun and never finished in b — by the
	// backend's own means, as a process killed mid-upload would — and
	// answers it as [objstore.Backend.Pending] should: its name (empty
	// where the backend cannot know one) and its ID. Nil is a backend that
	// can never hold one, which the suite then holds to listing none.
	Unfinished func(t *testing.T, b objstore.Backend) objstore.Pending

	// Spoil leaves under name, in b, something the backend holds and
	// cannot read as an object — by the backend's own means, as a client
	// writing garbage into its format or a disk returning it would — which
	// the suite then holds to [objstore.ErrCorrupt]. Nil is a backend that
	// can never hold one: an S3 bucket answers a name with an object or
	// with nothing.
	Spoil func(t *testing.T, b objstore.Backend, name string)

	// Ranged says the backend lists a range of its names in order
	// ([objstore.Ranged]), which the suite then holds it to. Unset, the
	// suite holds it to NOT implementing the interface, so no backend can
	// offer an ordering nothing certified.
	Ranged bool

	// BatchDeletes says the backend deletes many names in one request
	// ([objstore.BatchDeleter]), which the suite then holds it to. Unset,
	// the suite holds it to NOT implementing the interface, so the
	// collector never hands a batch to a deletion nothing certified.
	BatchDeletes bool
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
		{"a_get_whose_context_ends_stops", aGetWhoseContextEndsStops},
		{"a_deleted_object_is_gone_and_a_second_delete_is_fine", aDeletedObjectIsGone},
		{"a_put_whose_reader_fails_leaves_nothing", aPutWhoseReaderFailsLeavesNothing},
		{"a_put_whose_context_ends_leaves_nothing", aPutWhoseContextEndsLeavesNothing},
		{"a_listing_visits_every_name_once", aListingVisitsEveryNameOnce},
		{"a_listing_stops_at_the_visitors_error", aListingStopsAtTheVisitorsError},
		{"a_listing_waits_for_a_slow_visitor", aListingWaitsForASlowVisitor},
		{"names_are_kept_verbatim", namesAreKeptVerbatim},
		{"an_unfinished_upload_is_pending_until_abandoned", anUnfinishedUploadIsPendingUntilAbandoned},
		{"a_name_holding_no_object_is_corrupt_listed_and_deletable", aNameHoldingNoObjectIsCorrupt},
		{"a_range_lists_its_names_in_order", aRangeListsItsNamesInOrder},
		{"a_batch_deletion_removes_exactly_its_names", aBatchDeletionRemovesExactlyItsNames},
	}
	for _, c := range cases {
		// IN PARALLEL: each case is handed a backend of its own from the
		// factory — a broker, a bucket server or a map nothing else holds —
		// so no case sees another's objects, and one after another the
		// suite cost the sum of its slowest cases.
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
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

// A GET WHOSE CONTEXT ENDS STOPS at the next read, with the context's own
// error — whatever the backend had already read ahead. It is how a reader
// that stopped moving is ended (the store's stall watchdog cancels the
// context), and how a download whose client went away stops costing the
// backend anything: a stream that answered from what it held in hand would
// run on for as long as its read-ahead lasted, and one blocked on the backend
// would never notice at all.
func aGetWhoseContextEndsStops(t *testing.T, b objstore.Backend, opts Options) {
	data := large(opts)
	put(t, b, "stopped", data)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	r, err := b.Get(ctx, "stopped", 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	if _, err = io.ReadFull(r, make([]byte, 100)); err != nil {
		t.Fatalf("the first read: %v", err)
	}
	cancel()
	n, err := r.Read(make([]byte, 100))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a read after the context ended = %d bytes, %v; want context.Canceled", n, err)
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

// pending is every upload a pending listing visited.
func pending(t *testing.T, b objstore.Backend) []objstore.Pending {
	t.Helper()
	var out []objstore.Pending
	if err := b.Pending(t.Context(), func(p objstore.Pending) error {
		out = append(out, p)
		return nil
	}); err != nil {
		t.Fatalf("Pending: %v", err)
	}
	return out
}

// AN UPLOAD BEGUN AND NEVER FINISHED IS PENDING UNTIL IT IS ABANDONED — and
// nothing else is: neither an object that finished, nor a put that failed
// while it ran, which cleans up after itself. It is the one thing a backend
// holds that no listing of its names shows, so a backend that could not find
// it would keep it, and bill for it, for ever; and one that answered a
// finished object here would have the collector abandon live bytes.
func anUnfinishedUploadIsPendingUntilAbandoned(t *testing.T, b objstore.Backend, opts Options) {
	put(t, b, "finished", large(opts))
	if err := b.Put(t.Context(), "failed", &failing{r: bytes.NewReader(large(opts)),
		fail: errors.New("the client went away")}, objstore.PutMeta{}); err == nil {
		t.Fatal("a put over a failing reader succeeded")
	}
	if opts.Unfinished == nil {
		if got := pending(t, b); len(got) != 0 {
			t.Fatalf("a backend that cannot hold an unfinished upload listed %+v", got)
		}
		if err := b.Abandon(t.Context(), objstore.Pending{Name: "finished", ID: "anything"}); err != nil {
			t.Fatalf("abandoning what is not there = %v", err)
		}
		return
	}
	before := time.Now().Add(-time.Minute)
	want := []objstore.Pending{opts.Unfinished(t, b), opts.Unfinished(t, b)}
	got := pending(t, b)
	if len(got) != len(want) {
		t.Fatalf("Pending listed %+v, want exactly the two unfinished uploads %+v — "+
			"never the finished object or the failed put", got, want)
	}
	for _, w := range want {
		i := slices.IndexFunc(got, func(p objstore.Pending) bool { return p.ID == w.ID })
		switch {
		case i < 0:
			t.Fatalf("Pending did not list %+v; it listed %+v", w, got)
		case got[i].Name != w.Name:
			t.Fatalf("Pending named %s %q, want %q", w.ID, got[i].Name, w.Name)
		case got[i].Started.Before(before) || got[i].Started.After(time.Now().Add(time.Minute)):
			t.Fatalf("Pending dated %s %v, want about now", w.ID, got[i].Started)
		case got[i].Started.Location() != time.UTC:
			t.Fatalf("Pending dated %s in %v; every instant here is UTC", w.ID, got[i].Started.Location())
		}
	}

	stop := errors.New("enough")
	visits := 0
	if err := b.Pending(t.Context(), func(objstore.Pending) error {
		visits++
		return stop
	}); !errors.Is(err, stop) || visits != 1 {
		t.Fatalf("Pending = %v after %d visits; want the visitor's error after one", err, visits)
	}

	if err := b.Abandon(t.Context(), got[0]); err != nil {
		t.Fatalf("Abandon %+v: %v", got[0], err)
	}
	if rest := pending(t, b); len(rest) != 1 || rest[0].ID != got[1].ID {
		t.Fatalf("after abandoning %s, Pending listed %+v; want %s alone", got[0].ID, rest, got[1].ID)
	}
	if err := b.Abandon(t.Context(), got[0]); err != nil {
		t.Fatalf("abandoning an upload a second time = %v; abandoning what is gone is not an error", err)
	}
	if err := b.Abandon(t.Context(), got[1]); err != nil {
		t.Fatalf("Abandon %+v: %v", got[1], err)
	}
	if rest := pending(t, b); len(rest) != 0 {
		t.Fatalf("after abandoning both, Pending listed %+v", rest)
	}
	if data, err := get(t, b, "finished", 0, -1); err != nil || !bytes.Equal(data, large(opts)) {
		t.Fatalf("abandoning the unfinished uploads touched a finished object: %d bytes, %v", len(data), err)
	}
}

// A NAME HOLDING SOMETHING THE BACKEND CANNOT READ AS AN OBJECT IS A DEFINITE
// ANSWER: a stat and a get answer [objstore.ErrCorrupt] — never a failure,
// which a reader would retry for ever and the audit would stop on every day
// without naming the file — a listing hands the name on, holding nothing, so
// the collector can judge it, and a delete clears it like any other name. A
// whole object beside it is untouched throughout.
func aNameHoldingNoObjectIsCorrupt(t *testing.T, b objstore.Backend, opts Options) {
	data := []byte("a whole object")
	put(t, b, "whole", data)
	if opts.Spoil == nil {
		// A BACKEND THAT CAN HOLD NO SUCH NAME never answers one: its
		// whole objects are never called corrupt.
		if _, err := b.Stat(t.Context(), "whole"); err != nil {
			t.Fatalf("Stat of a whole object = %v", err)
		}
		return
	}
	put(t, b, "spoiled", []byte("bytes about to be made unreadable"))
	before := time.Now().Add(-time.Minute)
	opts.Spoil(t, b, "spoiled")

	if info, err := b.Stat(t.Context(), "spoiled"); !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("Stat of a name holding no object = %+v, %v; want ErrCorrupt", info, err)
	}
	if _, err := get(t, b, "spoiled", 0, -1); !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("Get of a name holding no object = %v; want ErrCorrupt", err)
	}
	all := listed(t, b)
	switch got := all["spoiled"]; {
	case len(got) != 1:
		t.Fatalf("a listing visited the spoiled name %d times, want once: %+v", len(got), all)
	case got[0].Size != 0 || got[0].Digest != "":
		t.Fatalf("a listing said the spoiled name holds %+v; want no bytes and no digest", got[0])
	case got[0].Written.Before(before) || got[0].Written.After(time.Now().Add(time.Minute)) ||
		got[0].Written.Location() != time.UTC:
		t.Fatalf("a listing dated the spoiled name %v, want about now, in UTC", got[0].Written)
	}
	if got := all["whole"]; len(got) != 1 || got[0].Size != int64(len(data)) {
		t.Fatalf("a listing beside a spoiled name visited the whole object as %+v", got)
	}

	if err := b.Delete(t.Context(), "spoiled"); err != nil {
		t.Fatalf("Delete of a name holding no object: %v", err)
	}
	gone(t, b, "spoiled", "a delete of a name holding no object")
	if got, err := get(t, b, "whole", 0, -1); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the whole object beside the spoiled name read back %q, %v", got, err)
	}
}

// A RANGE LISTS EXACTLY ITS NAMES, IN THE BYTE ORDER OF THE NAMES: every name
// after its start and through its end, once, ascending — a name equal to the
// start left out and one equal to the end kept, so ranges cut at the names a
// caller already holds tile the store with nothing read twice and nothing
// skipped — whatever the names spell, across the backend's own pages, and
// stopping at its visitor's error. An audit reads an object missing from a
// range as one the store does not hold.
func aRangeListsItsNamesInOrder(t *testing.T, b objstore.Backend, opts Options) {
	r, ok := b.(objstore.Ranged)
	if !opts.Ranged {
		if ok {
			t.Fatal("the backend lists ranges and the suite was not told to certify it: " +
				"set Options.Ranged")
		}
		return
	}
	if !ok {
		t.Fatal("the suite was told the backend lists ranges, and it does not implement objstore.Ranged")
	}
	// NAMES WHOSE ORDER A STRING SORT AND A NAIVE ONE DISAGREE ON: a prefix
	// before its extensions, a digit before a letter, an upper case before a
	// lower, a slash before a dash — and more than any backend's page.
	names := []string{"a", "a-b", "a/b", "a0", "aa", "B", "b", "files/", "files/0", "files/a", "z"}
	for i := range 2*defaultPageOf + 3 {
		names = append(names, fmt.Sprintf("files/k%03d", i))
	}
	for _, n := range names {
		put(t, b, n, []byte(n))
	}
	sorted := slices.Sorted(slices.Values(names))
	within := func(after, through string) []string {
		var out []string
		for _, n := range sorted {
			if n > after && n <= through {
				out = append(out, n)
			}
		}
		return out
	}
	listRange := func(after, through string) []string {
		t.Helper()
		var got []string
		if err := r.ListRange(t.Context(), after, through, func(info objstore.Info) error {
			if info.Size != int64(len(info.Name)) || info.Written.IsZero() {
				t.Errorf("ListRange said %q holds %+v", info.Name, info)
			}
			got = append(got, info.Name)
			return nil
		}); err != nil {
			t.Fatalf("ListRange(%q, %q): %v", after, through, err)
		}
		return got
	}
	for _, c := range []struct{ after, through string }{
		{"", "zz"},                   // everything
		{"", "a"},                    // from the first, through one name
		{"a", "a0"},                  // the start left out, the end kept
		{"a-", "a/b"},                // bounds that are no name at all
		{"files/", "files/k" + "ÿ"},  // the namespace, across pages
		{"files/k005", "files/k017"}, // inside a page and across one
		{"z", "zz"},                  // past the last name: nothing
		{"b", "a"},                   // an end before its start: nothing
	} {
		if got, want := listRange(c.after, c.through), within(c.after, c.through); !slices.Equal(got, want) {
			t.Errorf("ListRange(%q, %q) = %q, want %q", c.after, c.through, got, want)
		}
	}

	// RANGES CUT AT THE LAST NAME EACH ONE LISTED TILE THE STORE.
	var tiled []string
	for after := ""; ; {
		page := within(after, "zz")
		if len(page) == 0 {
			break
		}
		through := page[min(len(page), 5)-1]
		tiled = append(tiled, listRange(after, through)...)
		after = through
	}
	if !slices.Equal(tiled, sorted) {
		t.Errorf("ranges cut at the names they ended on listed %q, want every name once: %q", tiled, sorted)
	}

	stop := errors.New("enough")
	visits := 0
	if err := r.ListRange(t.Context(), "", "zz", func(objstore.Info) error {
		visits++
		return stop
	}); !errors.Is(err, stop) || visits != 1 {
		t.Fatalf("ListRange = %v after %d visits; want the visitor's error after one", err, visits)
	}
}

// defaultPageOf is more names than any backend's listing page in the suite
// holds — s3fake pages seven at a time — so a range crosses pages.
const defaultPageOf = 10

// A BATCH DELETION REMOVES EXACTLY ITS NAMES: every name it is handed is gone —
// a full batch of [objstore.MaxDeleteBatch] included, names that were never
// there and a name holding no object the backend can read among them — every
// name it is not handed is untouched, and an empty batch removes nothing. The
// collector deletes a batch of the names no row names; a batch that reached a
// name beside them would delete a file somebody keeps.
func aBatchDeletionRemovesExactlyItsNames(t *testing.T, b objstore.Backend, opts Options) {
	d, ok := b.(objstore.BatchDeleter)
	if !opts.BatchDeletes {
		if ok {
			t.Fatal("the backend deletes in batches and the suite was not told to certify it: " +
				"set Options.BatchDeletes")
		}
		return
	}
	if !ok {
		t.Fatal("the suite was told the backend deletes in batches, and it does not implement " +
			"objstore.BatchDeleter")
	}
	var doomed []string
	for i := range objstore.MaxDeleteBatch - 2 {
		name := fmt.Sprintf("files/doomed-%04d", i)
		put(t, b, name, []byte(name))
		doomed = append(doomed, name)
	}
	if opts.Spoil != nil {
		put(t, b, "files/doomed-spoiled", []byte("bytes about to be made unreadable"))
		opts.Spoil(t, b, "files/doomed-spoiled")
	}
	doomed = append(doomed, "files/doomed-spoiled", "files/never-put")
	kept := []string{"files/doomed", "files/doomed-0000x", "files/kept"}
	for _, name := range kept {
		put(t, b, name, []byte(name))
	}

	if err := d.DeleteBatch(t.Context(), nil); err != nil {
		t.Fatalf("DeleteBatch of no names: %v", err)
	}
	// A LISTING BEFORE THE DELETION, as the collector's comes before its
	// batches: a backend that answers a listing from an index it built
	// must forget what a batch deletes, or the next listing names objects
	// that are gone — the collector's second pass, an audit — while every
	// read of them answers not found.
	listed(t, b)
	if err := d.DeleteBatch(t.Context(), doomed); err != nil {
		t.Fatalf("DeleteBatch of %d names: %v", len(doomed), err)
	}
	all := listed(t, b)
	for _, name := range doomed {
		if got := all[name]; len(got) != 0 {
			t.Fatalf("a listing after the batch deletion visited %s: %+v", name, got)
		}
	}
	gone(t, b, doomed[0], "a batch deletion")
	gone(t, b, "files/doomed-spoiled", "a batch deletion")
	for _, name := range kept {
		if got, err := get(t, b, name, 0, -1); err != nil || string(got) != name {
			t.Fatalf("%s, beside the names a batch deletion was handed, read back %q, %v", name, got, err)
		}
	}
	// AGAIN, every name now gone: deleting what is not there is not an
	// error, as one deletion of it is not.
	if err := d.DeleteBatch(t.Context(), doomed[:3]); err != nil {
		t.Fatalf("DeleteBatch of names already gone: %v", err)
	}
}
