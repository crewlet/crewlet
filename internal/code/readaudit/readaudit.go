// Package readaudit is WHO READ A UNIT'S REPOSITORY as the node that served
// the reads keeps them between flushes: every clone and fetch the code host
// served, and every server-side read the git reads answered, of a
// `visibility: unit` repository, folded per reader into ONE `code_fetched`
// record a flush (types.CodeFetched) — which the node's event store writes and
// its usage publisher derives into the day's repo_read record (internal/usage).
//
// # Off the read's path
//
// A read used to be published as it was answered: the fetch's response, and
// the git-lane slot a server-side read is answered in, waited for the broker's
// replicated, fsynced acknowledgement and the event store's commit behind it.
// While the event stream has no leader that is the client's five-second
// default a read, and sixty-four such reads hold every slot of a node's git
// lane, so the reads of every repository there — a company one's included —
// queue behind an audit nobody is waiting for. A refused publish lost its
// record for good. And every read was a message each replica of the event
// stream keeps for thirty days and a row the day's derivation scans again on
// every tick the day moves. The ledger is an internal/coalesce ledger, so a
// read is a fold under one lock, its record is published by a flush that
// holds up no read, and a refused record is published again by the next.
//
// # One record per reader per flush
//
// A record's key is everything a reader is and how it read: the company day,
// the repository, the principal kind and principal, the way (git or tool), a
// server-side read's kind and the seat it acted as. So a CI token fetching a
// repository every few seconds is a record a flush rather than a record a
// fetch, and a seat reading a file a line range at a time is one record of
// its reads of each kind. A record counts its reads and sums their bytes,
// names the repository as its newest read did, carries its first read's trace
// and is stamped with its newest read's instant — inside the day its key
// names, which keeps every read of the record inside that day.
//
// # What it costs an answer
//
// A read reaches its record at the next flush — within fifteen seconds, the
// usage domain's cadence — and the day's repo_read record at the usage
// publisher's tick after that. A process that dies loses the reads since its
// last flush and, during a broker outage, the refused records it still held
// (up to coalesce.MaxPending); its orderly stop flushes both once more, after
// the code host stops and before the usage publisher's own last flush, so a
// node stopped that way leaves its last reads in its last day.
package readaudit

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coalesce"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("code.readaudit")

// Read is one read the ledger is handed: as its producer described it (one
// read, types.CodeFetched with Reads 1), when it was answered and the company
// day that instant falls in, and the trace it was served under.
type Read struct {
	Fetched types.CodeFetched
	At      time.Time
	Day     string
	Trace   events.TraceContext
}

// key is one record's identity: everything a reader is and how it read.
type key struct {
	day, repo, principalKind, principal, via, readKind, seat string
}

// bucket is one key's reads since its last flush.
type bucket struct {
	// name is the repository as the newest of the reads named it, last that
	// read's instant.
	name string
	last time.Time
	// reads and bytes are what the reads count and were sent.
	reads, bytes int64
	// trace is the first read's.
	trace events.TraceContext
}

// Ledger is one node's reads of its `visibility: unit` repositories between
// flushes, published as one `code_fetched` record per key a flush every
// coalesce.FlushInterval. SAFE FOR CONCURRENT USE: every fetch and every
// server-side read the node answers adds to it, and nothing it does waits on
// the broker.
type Ledger struct {
	l *coalesce.Ledger[key, bucket]
}

// New is an empty ledger publishing through pub. A nil publisher is a ledger
// that keeps and publishes nothing — a node with no queue, which records no
// event of any kind.
func New(pub coalesce.Publisher) *Ledger {
	return &Ledger{l: coalesce.New(pub, coalesce.Records[key, bucket]{
		Type: types.CodeFetched{}.EventType(),
		Seal: seal,
		Held: func(held []*bucket) []any {
			var reads int64
			for _, b := range held {
				reads += b.reads
			}
			return []any{"reads", reads}
		},
		Lost:   "these reads are in no record, so who read each repository answers without them",
		Logger: log,
	})}
}

// Add files a read in its reader's record and returns: it holds the ledger's
// lock for a fold, and never waits on the broker. The read counts what its
// Reads says, and one where that says none, as a call the auxiliary-spend
// ledger is handed counts one. Nil-safe.
func (l *Ledger) Add(r Read) {
	if l == nil {
		return
	}
	f := r.Fetched
	k := key{day: r.Day, repo: f.RepoID, principalKind: string(f.PrincipalKind), principal: f.Principal,
		via: string(f.Via), readKind: f.Kind, seat: f.Seat}
	l.l.Add(k, func(b *bucket, fresh bool) {
		if fresh {
			b.trace = r.Trace
		}
		b.reads += max(f.Reads, 1)
		b.bytes += max(f.Bytes, 0)
		if fresh || !r.At.Before(b.last) {
			b.name, b.last = f.RepoName, r.At
		}
	})
}

// seal cuts one key's bucket into its record.
func seal(k key, b *bucket) *events.Event {
	rec := types.CodeFetched{RepoID: k.repo, RepoName: b.name,
		PrincipalKind: types.CodeReaderKind(k.principalKind), Principal: k.principal, Seat: k.seat,
		Via: types.CodeFetchedVia(k.via), Kind: k.readKind, Reads: b.reads, Bytes: b.bytes}
	// THE ID AND THE INSTANT ARE FIXED HERE, once, so every attempt to
	// publish the record is the same event, which the store's (time, id) key
	// collapses.
	ev := events.New(rec, b.trace)
	// THE NEWEST READ'S INSTANT, not the flush's: the day the reads were made
	// in, which the key keeps every read of the record inside, and the
	// newest read the readers' answer reports.
	if !b.last.IsZero() {
		ev.Timestamp = b.last.UTC()
	}
	// A SEAT'S READS ARE SOURCED TO THE SEAT, as a seat's every event is; a
	// credential's name nobody, its reader being the record's own.
	if rec.PrincipalKind == types.CodeReaderAgent {
		ev.Source = rec.Seat
	}
	return ev
}

// Flush publishes every record now, and every one still waiting from a
// refused publish, oldest first. Nil-safe.
func (l *Ledger) Flush(ctx context.Context) {
	if l == nil {
		return
	}
	l.l.Flush(ctx)
}

// Run flushes on coalesce.FlushInterval until [Ledger.Stop]. A second Run while
// one is running does nothing. Nil-safe.
func (l *Ledger) Run(ctx context.Context) {
	if l == nil {
		return
	}
	l.l.Run(ctx)
}

// Stop ends the timer, waits for a flush in flight, and flushes once more on
// ctx — the stop's own budget. Its caller ORDERS it after the code host stops,
// so no read is answered after it, and before the usage publisher's last flush
// and the stream's close. Nil-safe; a second Stop flushes what arrived since.
func (l *Ledger) Stop(ctx context.Context) {
	if l == nil {
		return
	}
	l.l.Stop(ctx)
}
