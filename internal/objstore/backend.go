package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

// ErrNotFound is a name the backend holds nothing under.
var ErrNotFound = errors.New("objstore: no such object")

// ErrCorrupt is an object whose bytes are not the ones it was stored with: the
// backend ended it short of the size its row records, handed over more, or
// handed over exactly that many that hash to something other than the row's
// digest. Never handed to a reader as the object ([Store.Open]).
//
// ONLY A DEFINITE ANSWER IS CORRUPTION. A read that FAILED before the end — a
// deadline, a dropped link, a broker that stopped answering — is a failed
// read and nothing more, whatever its library calls it: the object may be
// whole, a later read may get it, and an error that said otherwise would send
// an operator to restore a file nothing is wrong with, and a backup to record
// it as lost.
var ErrCorrupt = errors.New("objstore: an object's bytes are not the ones it was stored with")

// ErrNoDeadline is a read asked for under a context with no deadline.
var ErrNoDeadline = errors.New("objstore: a read must carry a deadline")

// ErrUndated is what a listing ([Backend.List], [Backend.Pending]) fails with,
// once it has handed on everything it could date, when the backend answered
// an object or an upload with no instant: an S3-compatible gateway may leave
// the date out of a listing. Such an entry is NOT handed on, because an
// instant nobody gave reads as the zero time — older than every grace — and
// the collector would delete or abandon whatever it named the moment it was
// listed, an upload still in flight among them.
var ErrUndated = errors.New("objstore: the backend listed an entry with no date")

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
// the engine's namespace ([Key.Name]), a chunk an earlier build stored, or
// somebody else's object entirely — is this package's grammar and the
// collector's judgement, so it is written once, here, rather than once per
// backend, and a backend can never hide an object from the collector by
// declining to list a name it did not understand.
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
	Put(ctx context.Context, name string, r io.Reader, m PutMeta) error

	// Get streams n bytes of name from offset off — to the end when n is
	// negative — or answers [ErrNotFound]. A range that begins at or past
	// the end is an empty stream; one that runs past it is short.
	//
	// ctx bounds the WHOLE read, the stream included, and must carry a
	// deadline: a read that stops moving has to end somewhere, and a
	// backend refuses one with none ([ErrNoDeadline]) rather than pick a
	// bound of its own. Every backend begins with [CheckRead]. And ctx
	// ENDING ends the stream: a Read blocked on the backend returns, with
	// ctx's error, once ctx is done — which is how [Store]'s stall
	// watchdog stops a read that has stopped moving.
	//
	// The bytes are what the backend holds; checking them is [Store]'s.
	Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error)

	// Stat answers what the backend holds under name, or [ErrNotFound].
	Stat(ctx context.Context, name string) (Info, error)

	// Delete removes name. Deleting what is not there is not an error.
	Delete(ctx context.Context, name string) error

	// List hands every object the backend holds to visit, each name once
	// and in no particular order, stopping at the first error visit
	// returns. A visitor may be slow — it judges and deletes as it goes —
	// and a listing does not skip or repeat a name for it.
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
// in a minute is not going to.
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
// A BOUND, not a pace: [ReadStall] is what ends a read that stops moving.
// This is what ends one that trickles a byte a minute, which no stall
// watchdog ever sees.
func ReadBudget(size int64) time.Duration { return ReadBase + 2*PaceFor(size) }

// ReadStall is the longest one read of an object may wait on its backend
// before the read is ended as stalled.
//
// A MINUTE — two of [MiBPace], the time a mebibyte is given: no backend
// streaming hands over its next piece more than a mebibyte apart, the largest
// piece any of them moves is an eight-mebibyte part read in pieces far
// smaller, and a read that has heard nothing for a minute is waiting on a
// broker that lost the object's pieces or a connection that has gone, either
// of which would otherwise hold the reader until its whole budget ran out —
// hours, for a large file.
const ReadStall = 2 * MiBPace
