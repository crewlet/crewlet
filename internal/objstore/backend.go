package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"
)

// ErrNotFound is a name the backend holds nothing under.
var ErrNotFound = errors.New("objstore: no such object")

// ErrCorrupt is an object whose bytes are not the ones it was stored with: the
// backend ended it short of the size its row records, handed over more, or
// handed over exactly that many that hash to something other than the row's
// digest. Never handed to a reader as the object ([Store.Open]).
//
// AND A NAME HELD UNDER SOMETHING THAT IS NO OBJECT (broker metadata that does
// not decode) is answered with this by stat and get, not a failure: asking again
// reads the same, and a failure would make readers retry for ever, the audit
// stop daily, and a backup refuse to finish rather than record it lost.
// ONLY A DEFINITE ANSWER IS CORRUPTION: a read that FAILED before the end
// (deadline, dropped link) may find the object whole, and saying otherwise
// sends an operator to restore a healthy file.
var ErrCorrupt = errors.New("objstore: an object's bytes are not the ones it was stored with")

// ErrStalled is a transfer its backend stopped moving for [Stall]: a read
// handed nothing — no stream begun, or no piece of it — for that long, or a
// write taken no further and not answered. The store failing, never a
// refusal: asking again may succeed.
//
// THE BARE WORD, because it is spliced into the sentences that already said
// it ("stalled: the backend began no stream for 1m0s: …"), so a caller can
// branch on the kind with errors.Is while a person reads the sentence they
// always read.
var ErrStalled = errors.New("stalled")

// ErrNoDeadline is a read asked for under a context with no deadline.
var ErrNoDeadline = errors.New("objstore: a read must carry a deadline")

// ErrFull is a put the backend refused because the store has no room: the
// fleet's own bucket (natsobj) holds only the bytes it reserved and refuses a
// new object rather than discard an old one, or an S3-compatible provider
// refuses for want of room (drives at their floor, a quota spent: s3obj).
// Nothing is stored ([Backend.Put]).
//
// A SENTINEL rather than the backend's text, because the answer is specific:
// refused for room the deployment has to make, never retried as a blip. A
// backend answers it only inside a [*Full]; errors.Is(err, ErrFull) holds of
// each, and a surface takes the *Full with errors.As to say what an operator
// does.
var ErrFull = errors.New("objstore: the object store has no room left")

// Full is a put a backend refused for room ([ErrFull]): Remedy is the room an
// operator makes, and Err the backend's own answer.
//
// THE REMEDY IS THE BACKEND'S, because only it knows where its bytes are kept.
// Every surface splices Remedy after "until" or "once" into a sentence of its
// own (a pushed ref's line, a tool's answer, an HTTP hint), so it is one clause
// of gestures only an operator can make, in plain words because a git client
// prints it as is.
//
// ERR NAMES THE BUCKET, its endpoint and its keys, which no reader can act on
// and a model reads into its prompt: it is the node's log's, never a surface's
// sentence.
type Full struct {
	Remedy string
	Err    error
}

// Error is the sentinel's sentence and the backend's own answer.
func (e *Full) Error() string {
	if e.Err == nil {
		return ErrFull.Error()
	}
	return ErrFull.Error() + ": " + e.Err.Error()
}

// Unwrap is [ErrFull] and the backend's own answer, so errors.Is meets both.
func (e *Full) Unwrap() []error { return []error{ErrFull, e.Err} }

// FullWords is [ErrFull] as a person uploading a file is told it: what
// happened and who acts, since no wait clears it and nobody uploading can make
// the room. Here, beside the sentinel, for [SizeWords]' reason: every surface
// that stores a person's file answers it, and each kept its own words.
const FullWords = "There's no room left for the company's files. An " +
	"administrator needs to make room for them."

// ErrUnanswered is a put the backend did not take for a reason none of this
// package's sentinels names: a request that ran out of its own time, a link
// that dropped, a broker or a provider that did not answer, or a refusal its
// library words in its own terms. Nothing is stored ([Backend.Put]), and a
// later put may be.
//
// CLASSIFIED ONCE, HERE, by [Store], because every surface that stores a
// person's file must tell this from a fault of the node, and they disagreed. It
// is the reading every surface already gives a READ it cannot name: the store
// did not answer. A put the backend stopped reading before its end is a fault,
// and one whose caller's context ended carries that context's error instead.
var ErrUnanswered = errors.New("objstore: the object store did not take the bytes")

// Usage is how much of a backend's byte reservation its objects take: Bytes
// stored, against MaxBytes reserved. Only a backend that holds a reservation
// answers one ([Reserver]): the fleet's own bucket, whose reservation is
// carved out of the broker every coordination record shares. A provider's
// bucket reserves nothing.
type Usage struct {
	Bytes    int64 `json:"bytes"`
	MaxBytes int64 `json:"max_bytes"`
}

// ErrUndated is what a listing ([Backend.List], [Backend.Pending]) fails with,
// once it has handed on everything it could date, when the backend answered
// an object or an upload with no instant: an S3-compatible gateway may leave
// the date out of a listing. Such an entry is NOT handed on, because an
// instant nobody gave reads as the zero time — older than every grace — and
// the collector would delete or abandon whatever it named the moment it was
// listed, an upload still in flight among them.
var ErrUndated = errors.New("objstore: the backend listed an entry with no date")

// ErrUnordered is a ranged listing ([Ranged]) the backend cannot give in name
// order: an S3 Express directory bucket, which lists its keys in no order at
// all, or a store caught handing a name back out of order ([Store.ListKeys]).
// The caller asks about each name instead — an answer the order was not kept
// in would have every name past the first stray read as absent.
var ErrUnordered = errors.New("objstore: the backend cannot list a range of names in order")

// ErrNoBatch is a batch deletion ([BatchDeleter]) the store refused as a
// request, having removed nothing: an S3-compatible gateway that does not offer
// the verb, or a backend that has none ([Store.DeleteKeys]). The caller deletes
// each name on its own instead.
var ErrNoBatch = errors.New("objstore: the backend refused a batch deletion as a request")

// MaxDeleteBatch is the most names one batch deletion ([BatchDeleter]) is
// handed: S3's DeleteObjects takes at most a thousand keys a request.
const MaxDeleteBatch = 1000

// DeleteBatchError is a batch deletion the store answered name by name,
// refusing some names: Failed holds why, for each name it did not remove. The
// rest are gone.
type DeleteBatchError struct {
	Failed map[string]error
}

// Error names how many names were refused, and the first of them by name.
func (e *DeleteBatchError) Error() string {
	names := slices.Sorted(maps.Keys(e.Failed))
	if len(names) == 0 {
		return "objstore: a batch deletion refused no name"
	}
	return fmt.Sprintf("objstore: a batch deletion was refused for %d name(s), the first %q: %v",
		len(names), names[0], e.Failed[names[0]])
}

// Unwrap is every refusal, so errors.Is and errors.As reach each.
func (e *DeleteBatchError) Unwrap() []error {
	out := make([]error, 0, len(e.Failed))
	for _, name := range slices.Sorted(maps.Keys(e.Failed)) {
		out = append(out, e.Failed[name])
	}
	return out
}

// Info is one object a backend holds, as it reports it.
type Info struct {
	// Name is the object's name, verbatim — what it was put under.
	Name string

	// Size is how many bytes it holds.
	Size int64

	// Written is the instant the backend dates the object by, in UTC on its
	// own clock: NO EARLIER THAN THE PUT BEGAN AND NO LATER THAN THE OBJECT
	// BECAME WHOLE AND VISIBLE, and where in that span differs by backend. The
	// broker's store and the twin date it at the last byte; S3 dates a
	// multipart object by when its upload BEGAN (s3obj), hours before a large
	// one at the [MiBPace] floor is readable. So an age from Written (the
	// collector's grace) is read as the EARLIEST storage time, and can exceed
	// the time visible by as long as the upload took. It is one of two instants
	// the grace is measured from, the other the key's ([Key.Minted]); a listing
	// never hands on an object with neither ([ErrUndated]).
	Written time.Time

	// Digest is the SHA-256 the backend computed over the bytes it was
	// handed, or "" where it keeps none — an S3 bucket cannot hold one for
	// an object uploaded in parts. A cross-check for an audit, never what a
	// reader trusts: [Store] hashes every byte it hands out itself.
	Digest Hash
}

// Pending is an upload a backend BEGAN and never finished: bytes it holds that
// no name reaches — so no listing shows them, no get can read them and no
// delete of a name can remove them — and that it keeps, and on a bucket bills
// for, until something abandons them ([Backend.Abandon]).
//
// Every one is a put that died part of the way through without cleaning up
// after itself — a process killed mid-upload, a crash between the two halves
// of a delete — since a put that fails while it runs leaves nothing
// ([Backend.Put]). And at any instant, every put in flight is one too: an
// upload is pending until its last byte is stored, which is why the collector
// abandons one only once it is older than any upload is allowed to take.
type Pending struct {
	// Name is what the upload was being stored under, verbatim — or empty
	// where the backend cannot say: the broker's object store names an
	// upload only in the message that completes it, so pieces it never
	// completed belong to no name.
	Name string

	// ID is the backend's own handle on the upload — an S3 upload id, a
	// broker object's piece identifier — which [Backend.Abandon] takes
	// back. Opaque to everything but the backend that answered it.
	ID string

	// Started is when the backend dates the upload's beginning, in UTC and
	// on the backend's own clock — never zero: an upload the backend cannot
	// date is not handed on ([ErrUndated]), since the collector abandons
	// one only once this is older than the grace.
	Started time.Time
}

// PutMeta is what a put records beside the bytes, for an operator reading the
// store with its own tools.
//
// ONLY WHAT CANNOT GO STALE: the bytes under a name never change their type,
// while a file's project and path can move after its bytes are stored, so a
// path written here would be a lie the first time the file was renamed.
type PutMeta struct {
	// ContentType is the bytes' media type; empty records none.
	ContentType string
}

// Backend is where the bytes live: one store the whole fleet shares.
//
// Implemented by natsobj, s3obj and the twin memobj, all certified by ONE suite
// (objstoretest). A DUMB STORE OF OPAQUE NAMES: a backend hands every name back
// verbatim and parses none (what a name means is this package's grammar and the
// collector's judgement), so it cannot hide an object from the collector by
// declining to list a name it did not understand.
//
// A NAME IS PUT ONCE ([Key] never reuses one), so what a second put of a held
// name does is no backend's promise. Both ways are streamed: a backend buffers
// only its own unit of transfer, never an object.
type Backend interface {
	// Put stores everything r yields under name. The object becomes
	// visible only once r is read to its end and every byte is stored; a
	// read that fails, or ctx ending before then, leaves NOTHING under the
	// name a listing or a stat could find, and the put fails with the
	// read's own error. The end is io.EOF ITSELF and nothing else — see
	// [Fill], which every backend reading in pieces reads through.
	//
	// A put the backend STOPS TAKING — nothing of it accepted or answered
	// for [Stall] — fails with [ErrStalled], whatever ctx's deadline, and
	// leaves nothing, as any failed put: a write's deadline is sized for
	// its bytes at the floor, minutes for a large one, and a person
	// waiting on a store that stopped is owed the minute a read is.
	Put(ctx context.Context, name string, r io.Reader, m PutMeta) error

	// Get streams n bytes of name from offset off — to the end when n is
	// negative — or answers [ErrNotFound], or [ErrCorrupt] where what the
	// backend holds under name is no object it can read. A range at or past
	// the end is an empty stream; one running past it is short.
	// ctx bounds the WHOLE read, the stream included, and must carry a
	// deadline: a backend refuses none ([ErrNoDeadline]) rather than pick its
	// own bound, beginning with [CheckRead]. ctx ENDING ends the read: a Get
	// still opening and a Read blocked on the backend each return ctx's error,
	// which is how [Store]'s stall watchdog stops a read that stopped moving.
	//
	// The bytes are what the backend holds; checking them is [Store]'s.
	Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error)

	// Stat answers what the backend holds under name, or [ErrNotFound], or
	// [ErrCorrupt] where what it holds there is no object it can read.
	Stat(ctx context.Context, name string) (Info, error)

	// Delete removes name — what it holds whether or not it can be read as
	// an object. Deleting what is not there is not an error.
	Delete(ctx context.Context, name string) error

	// List hands every object the backend holds to visit, each name once
	// and in no particular order, stopping at the first error visit
	// returns. A visitor may be slow — it judges and deletes as it goes —
	// and a listing does not skip or repeat a name for it.
	//
	// A NAME HOLDING NO OBJECT THE BACKEND CAN READ ([ErrCorrupt]) is handed
	// on too, holding no bytes and dated by when the backend stored what it
	// holds there: the collector judges every name it lists, and one a
	// listing left out would be kept, and billed, for ever.
	List(ctx context.Context, visit func(Info) error) error

	// Pending hands every upload the backend began and has not finished to
	// visit ([Pending]), each once and in no particular order, stopping at
	// the first error visit returns. A backend whose puts leave nothing
	// behind them at any point — the twin, which stores an object whole or
	// not at all — has none to visit.
	Pending(ctx context.Context, visit func(Pending) error) error

	// Abandon removes what p holds, as [Backend.Pending] answered it.
	// Abandoning an upload that is gone — abandoned already, or never
	// there — is not an error.
	//
	// WHETHER p COULD STILL BE FINISHING is the caller's judgement, never
	// the backend's: an upload in flight is pending too, and abandoning it
	// fails that put (or, on the broker, takes the pieces from under the
	// object it is about to complete). The collector abandons only an
	// upload begun longer ago than any upload is allowed to take.
	Abandon(ctx context.Context, p Pending) error
}

// Ranged is a [Backend] that can also list a RANGE of its names in order: an
// S3 bucket, whose listing is in the byte order of its keys and answers a
// thousand of them a request.
//
// AN OPTIONAL HALF OF THE CONTRACT, because only some stores keep names in order
// (the broker's lists in metadata-write order). It exists for the audit, which
// asks about every object a live row names, walking rows in key order: one
// listing of a range then answers what a stat of each would, a request per page
// of names. Declared HERE rather than by the consumer, because two packages
// hold it to one meaning, the suite certifying it (objstoretest, which refuses
// a backend implementing it uncertified) and the reader ([Store.ListKeys]).
type Ranged interface {
	Backend

	// ListRange hands visit every object whose name sorts, as bytes, after
	// after and at or before through — after "" from the first — in that
	// order, each once, stopping at the first error visit returns. What it
	// hands on is what [Backend.List] would, an entry it cannot date
	// included: left out, and the listing then fails with [ErrUndated]. A
	// store this backend cannot list in order answers [ErrUnordered], before
	// it has visited anything.
	ListRange(ctx context.Context, after, through string, visit func(Info) error) error
}

// BatchDeleter is a [Backend] that can also delete many names in one request:
// an S3 bucket, whose DeleteObjects takes up to [MaxDeleteBatch] keys.
//
// AN OPTIONAL HALF OF THE CONTRACT, as [Ranged] is, and for the same kind of
// reason: only some stores have the verb — the broker's object store purges an
// object's metadata and its pieces as writes of their own, which nothing
// batches — and it exists for the collector, which deletes what no row names a
// batch of judged keys at a time. On S3 a bulk removal — a project of a million
// files nobody names any more — is then two thousand requests rather than a
// million. Declared here, beside the contract, for [Ranged]'s reason: the suite
// certifies it (objstoretest, which also refuses a backend that implements it
// uncertified) and the store reads it ([Store.DeleteKeys]).
type BatchDeleter interface {
	Backend

	// DeleteBatch removes every one of names — at most [MaxDeleteBatch],
	// each what [Backend.Delete] would remove — in one request. A name that
	// is not there is not an error. It answers nil when every name is gone;
	// a [*DeleteBatchError] when the store answered name by name and refused
	// some, why for each, the rest being gone; an error wrapping
	// [ErrNoBatch] when the store refused the request itself, having
	// removed nothing; and any other error when the request failed,
	// whatever it may have removed.
	DeleteBatch(ctx context.Context, names []string) error
}

// CheckRead refuses the reads every backend refuses: a negative offset, an
// empty range — nothing asks for none, and S3 has no way to spell one — and a
// context with no deadline ([ErrNoDeadline]). ONE RULE, so the three backends
// cannot each draw it differently.
func CheckRead(ctx context.Context, off, n int64) error {
	if off < 0 || n == 0 {
		return fmt.Errorf("objstore: a read of %d bytes at offset %d", n, off)
	}
	if _, ok := ctx.Deadline(); !ok {
		return ErrNoDeadline
	}
	return nil
}

// ContextReader is r, failing with ctx's error at the first read after ctx
// ends.
//
// IT IS HOW A PUT ENDS: a backend runs its upload under a context that does
// not end with the caller's, and the caller giving up reaches it as a READ
// error instead — the one failure every backend cleans up after with a live
// context. A put cancelled through its own context leaves the broker's
// object store with the pieces it already stored and nothing able to purge
// them, because the purge is made under that same, dead context.
func ContextReader(ctx context.Context, r io.Reader) io.Reader {
	return &contextReader{ctx: ctx, r: r}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Fill reads r into p until p is full or r stops, and says which: end false is
// a full p, end true is r ENDED after n bytes, an error is r FAILED, unchanged.
// THE END IS io.EOF ITSELF, compared by equality, never anything resembling it.
// io.ReadFull answers its own short read as io.ErrUnexpectedEOF, also what a
// request body answers when its client goes away mid-body, so a caller taking
// one for the end stored a partial upload as whole. A WRAPPED io.EOF is a
// failure for the same reason.
//
// An error arriving with the bytes that filled p is answered with them, where
// io.ReadFull drops it: a failed reader is not promised to fail again. A reader
// answering nothing and no error [maxEmptyReads] times running fails with
// io.ErrNoProgress, as bufio judges, rather than loop for ever.
func Fill(r io.Reader, p []byte) (n int, end bool, err error) {
	for empty := 0; n < len(p); {
		m, rerr := r.Read(p[n:])
		n += m
		switch {
		case rerr == io.EOF:
			return n, true, nil
		case rerr != nil:
			return n, false, rerr
		case m > 0:
			empty = 0
		default:
			if empty++; empty >= maxEmptyReads {
				return n, false, io.ErrNoProgress
			}
		}
	}
	return n, false, nil
}

// maxEmptyReads is how many reads in a row may answer nothing and no error
// before [Fill] calls the reader stuck: a hundred, bufio's own figure for the
// same judgement.
const maxEmptyReads = 100

// MiB is a mebibyte.
const MiB = 1 << 20

// MiBPace is how long one mebibyte of an object is given to cross one link
// the engine moves it over — a client's upload or download through the API, a
// part on its way to a bucket, an object on its way back from one — and so
// the slowest transfer the object store accepts.
//
// ONE FLOOR FOR EVERY LEG: every bound on a transfer derives from it, since
// two figures retuned apart stop a budget outlasting the client's own pace.
//
// THIRTY SECONDS, a floor of about 35 KB/s: far under any link a node or bucket
// is reached over, far over a request that stopped moving. Bounds derive from
// the SIZE, as requests run from a stat of no bytes to an eight-mebibyte part
// and any single figure caps the large or leaves the small unbounded.
const MiBPace = 30 * time.Second

// PaceFor is how long n bytes are given at [MiBPace]: one pace for every
// mebibyte begun.
func PaceFor(n int64) time.Duration {
	if n <= 0 {
		return 0
	}
	return time.Duration((n+MiB-1)/MiB) * MiBPace
}

// ReadBase is what a read of an object is given beside its bytes' pace: the
// backend's lookup of the object before it streams it, the request around
// it, and a retry of either.
//
// A MINUTE: every one of those is a round trip of milliseconds against a
// healthy backend, and a minute is room for the leader election or the
// reconnect that makes one of them slow — while a read that cannot even begin
// in a minute is not going to, which [Store] holds it to: the backend's open
// of the stream is watched by [Stall], a minute too, and a read that has
// begun no stream by then is ended as stalled however long its budget, and
// however often its backend would have asked again.
const ReadBase = time.Minute

// ReadBudget is the deadline [Store] reads an object of size bytes under,
// whatever its caller carries: [ReadBase], and a [MiBPace] for every
// mebibyte begun ON EACH OF THE TWO LEGS a read's bytes cross in series — from
// the backend to this node, and from this node to whoever reads the stream: a
// client downloading through the API, which is itself held to MiBPace per
// mebibyte, or a backup writing its disk. Budgeted for one leg, a client at
// the floor would spend the whole budget on its own pace and leave the
// backend a minute for a gibibyte.
//
// A BOUND, not a pace: [Stall] is what ends a read that stops moving.
// This is what ends one that trickles a byte a minute, which no stall
// watchdog ever sees.
func ReadBudget(size int64) time.Duration { return ReadBase + 2*PaceFor(size) }

// Stall is the longest a transfer may wait on its backend before it is ended
// as stalled ([ErrStalled]): a read's stream and pieces, a write's progress.
//
// A MINUTE, two of [MiBPace]: no backend's next piece is over a mebibyte apart,
// so a read silent that long waits on lost pieces, a dead connection or a
// failing bucket, which would otherwise hold the reader for its whole budget.
// The open is held to it too ([ReadBase]), else a retrying backend or silent
// peer would make it as long as that budget.
//
// THE SAME MINUTE FOR A WRITE, each backend watching its own: at the floor a
// mebibyte moves in half a minute, so a silent minute is a stopped backend a
// bytes-sized deadline would hold up to sixteen and a half. A constant.
const Stall = 2 * MiBPace
