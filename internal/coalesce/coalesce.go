// Package coalesce is how a node publishes an event it would otherwise publish
// many times a second: every occurrence FOLDED into one bucket per key between
// flushes, every bucket SEALED into one event a flush and published off the
// caller's path, and an event the broker refused KEPT, sealed, for the next
// flush — up to a bound.
//
// # One implementation, for every such record
//
// A record made at a rate no broker should be asked to keep pace with — what
// the seats' auxiliary model spends (internal/auxspend: a compaction of
// seventy rewrites, a turn's prefetch) — is counted by its node's usage day,
// and so is every other record a ledger here carries. Each owner says what its
// records are — the key, how a bucket becomes its event, what a lost one costs
// ([Records]) — and everything else is here, once (ADR-0008): the fold under
// one lock, the seal, the flush, the backlog and the order it is kept in, the
// timer and the stop. The backlog's order is the half a second copy would get
// wrong, as the first copy did ([Ledger.keep]).
//
// # Off the caller's path
//
// [Ledger.Add] holds the ledger's lock for one map lookup and the caller's
// fold, and for nothing else: no publish ever runs under it. So a broker that
// is slow — a stream electing a leader, a quorum lost — holds up no caller,
// and a request answers at the speed of its own work. That is the point for a
// record made on a request's path: a publish waits for the broker's
// replicated, fsynced acknowledgement, and where its caller set no deadline for
// the client's own default of five seconds, so a record published inline held
// the request, and the slot it was answered in, for as long as the broker
// took.
//
// # A retry is the same event
//
// A flush seals each bucket into its event ONCE, its id and its instant fixed
// ([Records.Seal]), and a refused publish keeps that event, so the retry the
// next flush makes is the same event: a publish reported failed that had in
// fact landed is collapsed by the event store's (time, id) key and the live
// window's id index rather than counted twice. The first refusal ends a flush
// — a broker that refused one record is down for the next, and asking it for
// each of a backlog of thousands would spend the flush on refusals and the log
// on a line apiece — and what it held back is kept, oldest first, up to
// [MaxPending].
//
// # What a crash costs
//
// A bucket lives in memory until it is published, and so does a record whose
// publish failed. A process that dies loses the buckets still open — at most
// one [FlushInterval] of records — PLUS every refused record it was holding
// when it dies during a broker outage; an orderly stop flushes both once more
// ([Ledger.Stop]). Each owner says what that costs its readers
// ([Records.Lost]).
package coalesce

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

var log = logging.Get("coalesce")

const (
	// FlushInterval is how often a running ledger publishes what it holds.
	//
	// FIFTEEN SECONDS, the usage domain's cadence (usage.FlushInterval, held
	// equal by this package's suite): every record a ledger here carries is
	// counted by its node's usage day, which the usage publisher derives on
	// that tick, so a record reaches a published day within two cadences —
	// this flush, and the tick that derives the day it landed in. Shorter
	// publishes more records for a day derived no sooner; longer leaves the
	// day a cadence behind what the node did.
	FlushInterval = 15 * time.Second

	// MaxPending bounds the sealed records a ledger keeps for a retry after
	// their publish failed.
	//
	// FOUR THOUSAND AND NINETY-SIX: a record is about six hundred bytes in
	// memory — its event and the bucket it was sealed from — so a full
	// backlog is under three megabytes, whichever records it holds. How long
	// an outage that is depends on how many keys a node fills a flush: a busy
	// company's auxiliary spend fills a few dozen, so most of an hour. Past it
	// the oldest is dropped, and said with what it held ([Records.Held]), so
	// the gap can be named.
	MaxPending = 4096
)

// Publisher is the queue's publish, as a ledger uses it.
type Publisher interface {
	Publish(ctx context.Context, subject string, ev *events.Event) error
}

// Records is what one ledger carries, said once, when it is made ([New]): K is
// a record's key — every dimension it is filed under — and B the bucket its
// occurrences are folded into until a flush seals it.
type Records[K comparable, B any] struct {
	// Type is the event type every record is, and the stem of the lines the
	// ledger logs: <Type>_unpublished, <Type>_dropped and <Type>_lost_at_stop.
	Type string

	// Seal cuts key k's bucket into its record — the event, its id and its
	// instant fixed here, ONCE, so every attempt to publish it is that event.
	// Called under the ledger's lock, so it must not block.
	Seal func(k K, b *B) *events.Event

	// Held is what records hold, as the attributes of a line saying they
	// were refused, dropped or lost — the spend's tokens, the reads — so the
	// gap can be named.
	Held func(held []*B) []any

	// Lost is what a record that never reaches the stream costs its readers,
	// said on every line reporting one dropped or lost.
	Lost string

	// Logger is where those lines go: the owning package's, so they carry its
	// component. Nil is this package's.
	Logger *slog.Logger
}

// sealed is a record a flush has cut from its bucket: its event fixed, so
// every attempt to publish it is the same event, and the bucket it was cut
// from, for what a line says it held.
type sealed[B any] struct {
	// seq is the order the ledger sealed it in, which is what "oldest" means
	// to the backlog: see [Ledger.keep].
	seq    uint64
	ev     *events.Event
	bucket *B
}

// Ledger is one node's records of one kind between flushes.
//
// SAFE FOR CONCURRENT USE: every producer adds to it, and a flush may run on
// the timer, at an owner's own moment and at the stop at once. A publish never
// runs under its lock, so a slow broker holds up no [Ledger.Add].
type Ledger[K comparable, B any] struct {
	pub     Publisher
	records Records[K, B]
	// bound is the most refused records the backlog keeps: [MaxPending] in
	// every ledger [New] builds, and nothing else in production. A field so
	// this package's suite can drive the trim and the merge at a bound it can
	// fill in a handful of flushes, since every refused flush copies the whole
	// backlog and filling 4096 of them one at a time is quadratic.
	bound int

	mu   sync.Mutex
	open map[K]*B
	// pending is what a refused publish left, in the order it was sealed —
	// oldest first, always, so the bound trims from the front.
	pending []sealed[B]
	// seq counts every record cut from a bucket, for [sealed.seq].
	seq uint64

	runMu sync.Mutex
	stop  context.CancelFunc
	done  chan struct{}
}

// New is an empty ledger of r's records publishing through pub. A nil
// publisher is a ledger that keeps and publishes nothing — a node with no
// queue, which records no event of any kind.
func New[K comparable, B any](pub Publisher, r Records[K, B]) *Ledger[K, B] {
	return &Ledger[K, B]{pub: pub, records: r, bound: MaxPending, open: map[K]*B{}}
}

// Add folds one occurrence into key k's bucket: fold is handed the bucket — a
// zero one, and fresh, where k has none open — under the ledger's lock, so it
// must only fold, never block. Nil-safe.
func (l *Ledger[K, B]) Add(k K, fold func(b *B, fresh bool)) {
	if l == nil || l.pub == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.open[k]
	if !ok {
		b = new(B)
		l.open[k] = b
	}
	fold(b, !ok)
}

// Flush publishes every open bucket and every record still waiting from a
// refused publish, oldest first. It reports nothing: a record that could not
// be published is kept for the next flush, and one past [MaxPending] is
// dropped and said. Nil-safe.
func (l *Ledger[K, B]) Flush(ctx context.Context) {
	l.flush(ctx, nil, true)
}

// FlushWhere publishes now the open buckets whose key pick chooses, and
// nothing else — no other bucket, and none of the backlog, which waits for the
// timer like every bucket pick leaves: an owner's own moment, such as a turn's
// end publishing that turn's records before the event that makes a reader ask
// for them. Nil-safe.
func (l *Ledger[K, B]) FlushWhere(ctx context.Context, pick func(K) bool) {
	l.flush(ctx, pick, false)
}

// flush seals the buckets pick chooses — every one, for a nil pick — and,
// where retry, takes the backlog with them, then publishes them in the order
// they were sealed.
func (l *Ledger[K, B]) flush(ctx context.Context, pick func(K) bool, retry bool) {
	if l == nil || l.pub == nil {
		return
	}
	l.mu.Lock()
	var out []sealed[B]
	if retry {
		out, l.pending = l.pending, nil
	}
	for k, b := range l.open {
		if pick != nil && !pick(k) {
			continue
		}
		l.seq++
		out = append(out, sealed[B]{seq: l.seq, ev: l.records.Seal(k, b), bucket: b})
		delete(l.open, k)
	}
	l.mu.Unlock()

	// THE FIRST REFUSAL ENDS THE FLUSH. A broker that refused one record is
	// down for the next, and asking it for each of a backlog of thousands
	// would spend the flush on refusals and the log on one line apiece; what
	// is left is kept, untried, for the next flush, and the refusal is said
	// once, with what it holds back.
	for i, s := range out {
		err := l.pub.Publish(ctx, topics.Event(s.ev.Type), s.ev)
		if err == nil {
			continue
		}
		failed := append([]sealed[B](nil), out[i:]...)
		l.logger().WarnContext(ctx, l.records.Type+"_unpublished", append(append([]any{
			"error", err.Error(), "records", len(failed)}, l.held(failed)...),
			"detail", "kept, and published again by the next flush")...)
		l.keep(ctx, failed)
		return
	}
}

// keep puts records a publish refused back in the backlog IN THE ORDER THEY
// WERE SEALED, and drops the oldest past the ledger's bound ([MaxPending]).
//
// MERGED BY SEAL ORDER rather than put in front or behind, because neither
// place is right for every flush that is refused. A full flush takes the
// backlog with it, so what it hands back is the oldest there is; a chosen
// flush ([Ledger.FlushWhere]) takes only its own new buckets, so what IT hands
// back is newer than every record already waiting — and put in front, the
// bound's trim dropped a turn's fresh records as "the oldest" at every turn's
// end of an outage while an hour-old backlog stayed, and the next retry
// published newest first. Two flushes refused at once interleave either way.
// failed and the backlog are each in seal order, so one merge keeps the whole
// of it so.
func (l *Ledger[K, B]) keep(ctx context.Context, failed []sealed[B]) {
	l.mu.Lock()
	defer l.mu.Unlock()
	merged := make([]sealed[B], 0, len(l.pending)+len(failed))
	i, j := 0, 0
	for i < len(l.pending) && j < len(failed) {
		if l.pending[i].seq <= failed[j].seq {
			merged = append(merged, l.pending[i])
			i++
			continue
		}
		merged = append(merged, failed[j])
		j++
	}
	merged = append(merged, l.pending[i:]...)
	merged = append(merged, failed[j:]...)
	l.pending = merged
	if over := len(l.pending) - l.bound; over > 0 {
		l.logger().WarnContext(ctx, l.records.Type+"_dropped", append(append([]any{
			"records", over}, l.held(l.pending[:over])...),
			"detail", "the broker refused these records for longer than the ledger keeps them: "+
				l.records.Lost)...)
		l.pending = append([]sealed[B](nil), l.pending[over:]...)
	}
}

// held is what records hold, as a line's attributes ([Records.Held]).
func (l *Ledger[K, B]) held(records []sealed[B]) []any {
	if l.records.Held == nil {
		return nil
	}
	buckets := make([]*B, len(records))
	for i, s := range records {
		buckets[i] = s.bucket
	}
	return l.records.Held(buckets)
}

// logger is where the ledger's lines go: its owner's, or this package's.
func (l *Ledger[K, B]) logger() *slog.Logger {
	if l.records.Logger != nil {
		return l.records.Logger
	}
	return log
}

// Run flushes on [FlushInterval] until [Ledger.Stop]. A second Run while one
// is running does nothing. Nil-safe.
func (l *Ledger[K, B]) Run(ctx context.Context) {
	if l == nil || l.pub == nil {
		return
	}
	l.runMu.Lock()
	if l.stop != nil {
		l.runMu.Unlock()
		return
	}
	loop, cancel := context.WithCancel(ctx)
	l.stop, l.done = cancel, make(chan struct{})
	done := l.done
	l.runMu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(FlushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loop.Done():
				return
			case <-ticker.C:
				l.Flush(loop)
			}
		}
	}()
}

// Stop ends the timer, waits for a flush in flight, and flushes once more on
// ctx — the stop's own budget, since the node is going and anything left is
// lost, which it says with what was held. Its caller ORDERS it after every
// producer of the records has stopped and before the stream closes.
// Nil-safe; a second Stop flushes what arrived since, since a stopped ledger
// still takes records.
func (l *Ledger[K, B]) Stop(ctx context.Context) {
	if l == nil {
		return
	}
	l.runMu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.runMu.Unlock()
	if stop != nil {
		stop()
		<-done
	}
	l.Flush(ctx)
	l.mu.Lock()
	left := l.pending
	l.pending = nil
	l.mu.Unlock()
	if len(left) > 0 {
		l.logger().WarnContext(ctx, l.records.Type+"_lost_at_stop", append(append([]any{
			"records", len(left)}, l.held(left)...),
			"detail", "the broker refused these records at the stop: "+l.records.Lost)...)
	}
}
