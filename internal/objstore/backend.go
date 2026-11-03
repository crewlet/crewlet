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
// AND A NAME THE BACKEND HOLDS SOMETHING UNDER THAT IS NO OBJECT AT ALL — the
// broker's object store holding metadata that does not decode — which its
// stat and its get answer with this rather than with a failure: it is as
// definite as a wrong digest, since asking again reads the same thing and no
// get will ever hand the object's bytes back, and a failure there would have
// every reader retry for ever, the audit stop on it every day without naming
// the file, and a backup refuse to finish rather than record it lost.
//
// ONLY A DEFINITE ANSWER IS CORRUPTION. A read that FAILED before the end — a
// deadline, a dropped link, a broker that stopped answering — is a failed
// read and nothing more, whatever its library calls it: the object may be
// whole, a later read may get it, and an error that said otherwise would send
// an operator to restore a file nothing is wrong with, and a backup to record
// it as lost.
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

// ErrFull is a put the backend refused because the store has no room for it:
// the fleet's own bucket (natsobj) holds as many bytes as it was created to
// reserve, and refuses a new object rather than discard an old one; or an
// S3-compatible bucket's provider refuses the write for want of room — its
// drives at their floor, a quota spent (s3obj). Nothing is stored, as for
// every failed put ([Backend.Put]).
//
// A SENTINEL rather than the backend's own text, because the caller's answer
// is specific: the upload is refused for room the deployment has to make, never
// retried as a blip. A backend answers it only inside a [*Full], which names
// that room: errors.Is(err, ErrFull) holds of every such answer, and a surface
// that refuses an upload for it takes the *Full with errors.As to say what an
// operator does.
var ErrFull = errors.New("objstore: the object store has no room left")

// Full is a put a backend refused for room ([ErrFull]): Remedy is the room an
// operator makes, and Err the backend's own answer.
//
// THE REMEDY IS THE BACKEND'S, carried by its answer, because the room is
// made where the backend keeps its bytes and only the backend knows where that
// is. Every surface that refuses an upload for a full store splices Remedy into
// a sentence of its own vocabulary — a pushed ref's line, a tool's answer, an
// HTTP refusal's hint — after "until" or "once", so it is one clause, naming
// gestures that are each an operator's, that the uploader cannot make and that
// no retry stands in for: asked again before one is made, the store answers the
// same. In plain words rather than markup, because a git client prints it as
// it is.
//
// ERR NAMES THE BUCKET, its endpoint and its keys, which none of those readers
// can act on and a model reads into its prompt: it is the node's log's, never a
// surface's sentence.
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
// CLASSIFIED ONCE, HERE, by [Store] — because every surface that stores a
// person's file has to tell this from a fault of the node, and each was
// telling it differently: a project file's upload answered it as a store that
// would answer again, an artifact's upload as the node's own fault, and their
// two tools the same two ways. It is the reading every object-store surface
// already gives a READ it cannot name (a download, the backup, the
// migration): the store did not answer. A put the backend stopped reading
// before its end is not this but a fault, and a put whose caller's own
// context ended carries that context's error instead.
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

	// Written is the instant the backend dates the object by, in UTC and
	// on the backend's own clock: NO EARLIER THAN ITS PUT BEGAN AND NO
	// LATER THAN IT BECAME WHOLE AND VISIBLE — and where in that span
	// differs by backend. The broker's object store and the twin date an
	// object when its last byte is stored; S3 dates one assembled from
	// parts by when its upload BEGAN (s3obj), which for a large object at
	// the [MiBPace] floor is hours before anybody could read it. So
	// anything judging an object's age from Written — the collector's
	// grace — reads it as the EARLIEST the object might have been stored,
	// never as when it was finished: an age measured from Written can
	// exceed the time the object has been visible by as long as its upload
	// took.
	//
	// It is one of the two instants the collector's grace is measured
	// from; the other is the key's own ([Key.Minted]). A listing never
	// hands on an object with no such instant ([ErrUndated]).
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
// Implemented by the fleet's own JetStream object store (natsobj), by an
// S3-compatible bucket (s3obj) and by the in-memory twin (memobj), and all of
// them certified by ONE suite (objstoretest).
//
// # A dumb store of opaque names
//
// A backend keeps bytes under whatever name it is handed and hands every name
// back verbatim. It parses none: what a name means — an object's key under
// the engine's namespace ([Key.Name]) or somebody else's object entirely —
// is this package's grammar and the collector's judgement, so it is written
// once, here, rather than once per backend, and a backend can never hide an
// object from the collector by declining to list a name it did not
// understand.
//
// A NAME IS PUT ONCE. The store never reuses one ([Key]), so what a second put
// of a held name does is no backend's promise, and nothing here depends on
// it.
//
// # Streamed, both ways
//
// A put reads its bytes from a reader to the end and a get hands them back as
// a reader, so no verb holds an object in memory: what a backend buffers is
// its own unit of transfer — a broker message, an S3 part — and never the
// object.
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
	// backend holds under name is no object it can read. A range that
	// begins at or past the end is an empty stream; one that runs past it
	// is short.
	//
	// ctx bounds the WHOLE read, the stream included, and must carry a
	// deadline: a read that stops moving has to end somewhere, and a
	// backend refuses one with none ([ErrNoDeadline]) rather than pick a
	// bound of its own. Every backend begins with [CheckRead]. And ctx
	// ENDING ends the read: a Get still opening the stream, and a Read
	// blocked on the backend, each return with ctx's error once ctx is
	// done — which is how [Store]'s stall watchdog stops a read that has
	// stopped moving, or has never begun.
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
// AN OPTIONAL HALF OF THE CONTRACT, because only some stores keep their names
// in order — the broker's object store lists in the order its metadata was
// written — and it exists for one reason: the audit asks the store about every
// object a live row names, and the rows are walked in key order, so on a store
// that lists in order one listing of a range answers what a stat of each object
// in it would, at a request a page of names rather than one an object.
// Declared HERE, beside the contract, rather than by the consumer alone,
// because two packages hold it to one meaning — the suite that certifies it
// (objstoretest, which also refuses a backend that implements it uncertified)
// and the store that reads it ([Store.ListKeys]) — and two declarations would
// be two contracts nothing held together.
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

// Fill reads r into p until p is full or r stops, and says which: n bytes and
// end false is a full p, end true is r ENDED after n bytes, and an error is r
// FAILED after n bytes — with r's own error, unchanged, whatever it is.
//
// THE END IS io.EOF ITSELF, compared by equality, as io.ReadAll and the
// broker's object store compare it — never anything that resembles it.
// io.ReadFull, which the backends read through before, answers its own short
// read as io.ErrUnexpectedEOF, which is also what a request body answers when
// its client goes away mid-body; a caller taking the one for the end took the
// other with it, and stored the part of an upload that arrived as the whole of
// it. A WRAPPED io.EOF is a failure for the same reason: the end is a
// reader's plain answer, and one carrying more than io.EOF is reporting
// something io.EOF does not say.
//
// An error that arrives with the bytes that filled p is answered with them,
// where io.ReadFull drops it: a reader that failed once is not promised to
// fail again on the next read. And a reader answering nothing and no error
// [maxEmptyReads] times running fails with io.ErrNoProgress, as bufio judges
// one, rather than holding its caller in a loop for ever.
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
// ONE FLOOR FOR EVERY LEG, and every bound on a transfer derives from it
// rather than restating it: two figures that agree today by coincidence are
// two figures somebody retunes apart, and then a budget meant to outlast the
// client's own pace no longer does.
//
// THIRTY SECONDS, a floor of about 35 KB/s: far under any link a node or a
// bucket is reached over, and far over a request that has stopped moving. A
// bound derived from the SIZE rather than one figure for every request,
// because the requests here run from a stat of no bytes to a part of eight
// mebibytes, and any single figure is either a cap on the large ones or no
// bound on the small.
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

// Stall is the longest one transfer of an object may wait on its backend
// before it is ended as stalled ([ErrStalled]): for a read, the stream to
// begin and each piece of it after; for a write, the backend to take more of
// it or answer.
//
// A MINUTE — two of [MiBPace], the time a mebibyte is given: no backend
// streaming hands over its next piece more than a mebibyte apart, the largest
// piece any of them moves is an eight-mebibyte part read in pieces far
// smaller, and a read that has heard nothing for a minute is waiting on a
// broker that lost the object's pieces, a connection that has gone or a
// bucket that keeps failing the request it asks again — any of which would
// otherwise hold the reader until its whole budget ran out, hours for a large
// file. The open is held to it as each piece is ([ReadBase]): a backend asks
// for its first bytes before it hands a stream back, and one whose client
// asks a failed request again until its caller's deadline, or that reaches a
// peer which accepts the request and never answers it, would otherwise make
// of an open it cannot get answered a wait as long as the read's whole
// budget.
//
// THE SAME MINUTE FOR A WRITE, which each backend watches for itself because
// only the backend can see a write move: the S3 backend at its socket — the
// body taken, the bytes the bucket's side acknowledged, its answer — and the
// broker's by its acknowledgements. A write moving at the floor moves a
// mebibyte a half-minute, so a minute with nothing moving is a backend that
// stopped, which a deadline sized for the write's bytes would hold for up to
// sixteen and a half minutes; and a person reads one figure for every way the
// store can fail a transfer — a read, a write, and a request the bucket keeps
// failing, which the S3 backend gives up at the same minute. A constant,
// never configuration: nothing honest differs by deployment.
const Stall = 2 * MiBPace
