package statelog

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/queue"
)

// THE GATE RESERVE: the top of every identity-claiming log's byte ceiling,
// which only the records that install or lift a node's gate may use, so a log
// full for ordinary writes still takes the eviction that unpins it (package
// doc, "The gate reserve").

// GateReserveDivisor and GateReserveFleet size the reserve, the larger of two
// terms ([Reservation.Bytes]): a SIXTEENTH of the log's byte ceiling, and the
// FLEET TERM — what [GateReserveFleet] nodes' appends in flight can carry a
// log past its soft ceiling, plus [GateRoom] for the gate records themselves.
//
// The fleet term is the log's, set by its largest record and not its ceiling,
// so it holds on the smallest log; the sixteenth grows with the ceiling, so a
// larger log absorbs a larger fleet (package doc, "The gate reserve"). Past
// the reserve a gate record is refused like any other, and the refusal says to
// raise the ceiling (`crewlet retention set-capacity`).
const GateReserveDivisor = 16

// GateReserveFleet is how many nodes' appends in flight the fleet term
// absorbs — the admitting node and the seven peers whose appends its reading
// cannot see. See [GateReserveDivisor].
const GateReserveFleet = 8

// GateRoom is what the fleet term keeps beyond the peers' appends, for the
// gate records themselves: a mebibyte, a thousand gate records at under a
// kibibyte each. It is the one part of the reserve the admission's blind spot
// cannot already have spent.
const GateRoom = 1 << 20

// appendOverhead bounds what a stored record carries beside its payload.
// FOUR KIBIBYTES covers the signature frame (under 300 bytes with the longest
// key id a keyring may name), the file store's ~30-byte frame, the append's two
// headers (under 200) and the longest subject any domain writes (a page
// title's bounded token, a few hundred). Counted per append, it also keeps a
// flood of tiny records from reading as free.
const appendOverhead = 4 << 10

// MaxTransportRecordBytes is the largest record the transport carries once it
// is signed and sent: [queue.MaxPayloadBytes] less what a stored record carries
// beside its payload ([appendOverhead]).
//
// THE CEILING EVERY DECLARATION IS HELD TO ([StreamSpec.Validate]), and what a
// log whose records have caps of their own declares. The transport's limit is
// on the whole MESSAGE, signed payload and headers, so a declaration at the
// transport's own number would admit a record the broker then refuses past its
// max_payload: the declaration must stay the one refusal a record meets
// before it is sent.
const MaxTransportRecordBytes = queue.MaxPayloadBytes - appendOverhead

// MaxDeliveryBytes is the most one stored record of ANY log is counted at when
// the broker delivers it: the largest message the transport carries
// ([queue.MaxPayloadBytes], payload and headers) plus the record's subject and
// the acknowledgement subject, which [appendOverhead] holds with room to spare.
//
// THE TRANSPORT'S, NOT A LOG'S ([StreamSpec.MaxAppendBytes]): a reader pulling
// one record at a time from a log it did not provision (the backup's replay
// window) knows only that the broker admits nothing past the transport. A pull
// bounded below this is refused a record at that ceiling with nothing
// delivered, on every pull, so the reader would stop at that record for ever.
const MaxDeliveryBytes = queue.MaxPayloadBytes + appendOverhead

// MaxAppendBytes is the most one append to this log is counted at: its
// largest record ([StreamSpec.MaxRecordBytes]) plus [appendOverhead].
//
// ONE NUMBER FOR THREE READERS, so the broker's refusal and the reserve's
// arithmetic are about the same record: [Reserve]'s per-node in-flight budget,
// the per-node term in the fleet term ([Reservation.Bytes]), and the stream's
// max_msg_size.
func (s StreamSpec) MaxAppendBytes() int64 { return s.MaxRecordBytes + appendOverhead }

// Reservation is how one log's byte ceiling divides between its ordinary
// appends and the records that install or lift a gate.
//
// THE ZERO VALUE KEEPS NO RESERVE, a real setting rather than an absent one:
// the shape of a log that claims no identity and so carries no gate record.
type Reservation struct {
	// Kept reports that the log keeps a reserve: that it claims identity,
	// which is what carries gate records ([ReservationOf]).
	Kept bool

	// MaxAppend is the log's [StreamSpec.MaxAppendBytes]: the most one
	// peer's appends in flight can hold, the fleet term's unit.
	MaxAppend int64
}

// ReservationOf is d's reservation, read off its declaration: a reserve only on
// a log that claims identity, which is what carries gate records. THE ONE
// ANSWER the publisher's admission, a ceiling's validation and the headroom
// alarm all ask; a predicate beside it would be a second.
func ReservationOf(d Domain) Reservation {
	if !d.ClaimsIdentity() {
		return Reservation{}
	}
	return Reservation{Kept: true, MaxAppend: d.Stream().MaxAppendBytes()}
}

// Bytes is how much of a byte ceiling of maxBytes is kept for gate records:
// the larger of a sixteenth of it and the fleet term ([GateReserveDivisor]),
// never more than the ceiling. Zero for a log that keeps none and for an
// unbounded log.
//
// ALL OF A CEILING SMALLER THAN THE FLEET TERM, which no Tier A floor comes
// near: the fleet's appends in flight could carry any ordinary write past it,
// so it refuses every one and keeps itself for the records that unpin it.
func (r Reservation) Bytes(maxBytes uint64) uint64 {
	if !r.Kept || maxBytes == 0 {
		return 0
	}
	fleet := uint64(GateReserveFleet-1)*uint64(max(r.MaxAppend, 0)) + GateRoom
	return min(maxBytes, max(maxBytes/GateReserveDivisor, fleet))
}

// Ordinary is the ceiling ordinary appends are held to on a log of maxBytes:
// the whole of it on a log that keeps no reserve, and [Reservation.Bytes]
// below it on one that does. Zero for an unbounded log.
func (r Reservation) Ordinary(maxBytes uint64) uint64 {
	return maxBytes - r.Bytes(maxBytes)
}

// Headroom is the unused fraction of the ceiling ordinary appends may use: nil
// for a log with no ceiling (zero is what the headroom alarm fires on), and
// zero on a log whose reserve is all of it.
//
// OF THE ORDINARY CEILING, not the broker's: at zero the log refuses every
// ordinary write and every linearizable read, while gate records still land.
func (r Reservation) Headroom(bytes, maxBytes uint64) *float64 {
	if maxBytes == 0 {
		return nil
	}
	ceiling := r.Ordinary(maxBytes)
	left := 0.0
	if ceiling > 0 {
		left = float64(ceiling-min(bytes, ceiling)) / float64(ceiling)
	}
	return &left
}

// Usage is one reading of a log's size and the ceiling the broker enforces.
type Usage struct {
	Bytes, MaxBytes uint64
}

// Reserve admits a node's ordinary appends to one reserved log.
//
// ONE PER LOG PER NODE, shared by that log's publisher and its read index,
// because its budget is the node's appends in flight on the log, whoever
// makes them.
type Reserve struct {
	stream string
	read   func(ctx context.Context) (Usage, error)

	// maxAppend is the log's [StreamSpec.MaxAppendBytes]: the budget's
	// cap, and the largest append it admits.
	reservation Reservation
	maxAppend   int64

	// budget is the node's ordinary appends in flight, in bytes, capped
	// at maxAppend; inflight is what it holds now, which the semaphore
	// does not expose. A WEIGHTED SEMAPHORE rather than a channel: a
	// many-unit acquire that honours cancellation, and FIFO service, so a
	// maximum record is not starved by small ones taking the room.
	budget   *semaphore.Weighted
	inflight atomic.Int64

	// mu guards the reading in flight and runs, how many readings have
	// been started — the order an admission's arrival is compared in.
	mu      sync.Mutex
	reading *usageRun
	runs    uint64
}

// usageRun is one reading of the log's usage and everybody waiting on it.
type usageRun struct {
	// n is this reading's place in the order readings were started, and
	// inflight what this node held in flight at that instant.
	n        uint64
	inflight int64

	done  chan struct{}
	usage Usage
	err   error
}

// NewReserve builds the reserve for the log spec declares, reading its usage
// through read, the broker's own stream state.
//
// THE SPEC AND NOT A NAME, because the budget is one append of the log's
// largest record ([StreamSpec.MaxAppendBytes]): the per-node term every
// peer's reserve assumes of this node ([Reservation.Bytes]).
func NewReserve(spec StreamSpec, read func(ctx context.Context) (Usage, error)) (*Reserve, error) {
	if read == nil {
		return nil, fmt.Errorf("statelog: the reserve on %s has no usage reading", spec.Name)
	}
	if spec.MaxRecordBytes <= 0 {
		return nil, fmt.Errorf("statelog: the reserve on %s has no largest record "+
			"to size its budget by", spec.Name)
	}
	maxAppend := spec.MaxAppendBytes()
	return &Reserve{stream: spec.Name, read: read,
		reservation: Reservation{Kept: true, MaxAppend: maxAppend},
		maxAppend:   maxAppend,
		budget:      semaphore.NewWeighted(maxAppend)}, nil
}

// Admit takes room for one ordinary append of size bytes, and refuses it
// `log_full` when the log is past its ordinary ceiling, and `usage_unknown`
// when the log's usage cannot be read. On a nil error the caller MUST call
// release once the broker has answered the append.
func (r *Reserve) Admit(ctx context.Context, size int64) (release func(), err error) {
	if size > r.maxAppend {
		// TOO LARGE, not a full log: no ceiling or trim moves this limit.
		// The publisher refuses such a record first; this guard keeps it
		// from waiting for room the budget can never hold.
		return nil, &Unavailable{
			Reason: ReasonRecordTooLarge,
			Detail: fmt.Sprintf("the record needs %d bytes on %s and no append past "+
				"%d is admitted there — the log's largest record plus what a stored "+
				"record carries beside it — so no retry places it, here or on any "+
				"node: split the change into smaller writes", size, r.stream,
				r.maxAppend),
			Cause: queue.ErrTooLarge,
		}
	}
	if err = r.budget.Acquire(ctx, size); err != nil {
		return nil, err
	}
	// COUNTED BEFORE THE ARRIVAL IS TAKEN, so every reading this
	// admission may use captures it — see [Reserve.fresh].
	r.inflight.Add(size)
	var once sync.Once
	release = func() {
		once.Do(func() {
			r.inflight.Add(-size)
			r.budget.Release(size)
		})
	}
	run, err := r.fresh(ctx)
	if err != nil {
		release()
		if ended := ctx.Err(); ended != nil {
			// THE CALLER'S OWN CONTEXT ENDED, which says nothing about
			// the log.
			return nil, fmt.Errorf("statelog: read %s's usage before appending to it: %w",
				r.stream, ended)
		}
		// TYPED HERE, the one place that knows nothing was appended:
		// untyped, a committer would call the push unknown and never ask
		// the runner-up. The broker's words go to the log ([Reserve.run]),
		// never onto a detail a caller is sent.
		return nil, &Unavailable{
			Reason: ReasonUsageUnknown,
			Detail: fmt.Sprintf("%s's usage could not be read before appending to it, so "+
				"the reserve kept for gate records could not be checked and nothing was "+
				"appended; the reason is in this node's log", r.stream),
			Cause: err,
		}
	}
	usage := run.usage
	if usage.MaxBytes == 0 {
		// AN UNBOUNDED LOG only: a bounded log whose reserve is all of it
		// has an ordinary ceiling of zero too, and refuses below.
		return release, nil
	}
	ceiling := r.reservation.Ordinary(usage.MaxBytes)
	// WHAT THE BROKER HELD, AND WHAT THIS NODE HAD IN FLIGHT WHEN THE
	// READING STARTED, this append included. One that landed before the
	// broker answered is counted twice, which only refuses a little early;
	// taken now instead, one that landed and released while the reading
	// was out would be in neither figure.
	if held := usage.Bytes + uint64(run.inflight); held > ceiling {
		release()
		return nil, &Unavailable{
			Reason: ReasonLogFull,
			Detail: fmt.Sprintf("%s holds %d bytes and ordinary writes are held to "+
				"%d of its %d-byte ceiling; the last %d are kept for the records "+
				"that install or lift a gate, so an eviction still lands and can "+
				"unpin the log. Raise the ceiling with `crewlet retention "+
				"set-capacity` or unblock the trim (`crewlet retention status` "+
				"names the term holding it)", r.stream, usage.Bytes, ceiling,
				usage.MaxBytes, r.reservation.Bytes(usage.MaxBytes)),
		}
	}
	return release, nil
}

// fresh is a reading of the log's usage that STARTED after this admission
// arrived, after its bytes were counted in flight.
//
// SHARED, and only forward: a reading already out on arrival may have been
// answered before a peer's append this caller must count, and captured this
// node's in-flight bytes without this caller's, so it is waited out. Arrival
// is BY COUNT, not clock: the next reading's number, taken under the lock
// [Reserve.claim] captures the in-flight figure under, so a reading numbered
// at or above it is exactly one whose figure includes this caller's bytes.
func (r *Reserve) fresh(ctx context.Context) (*usageRun, error) {
	r.mu.Lock()
	need := r.runs + 1
	r.mu.Unlock()
	for {
		run, owner := r.claim()
		if owner {
			r.run(ctx, run)
		}
		select {
		case <-run.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if run.err != nil {
			return nil, run.err
		}
		if run.n >= need {
			return run, nil
		}
	}
}

// claim joins the reading in flight or starts the next one, capturing what
// this node has in flight as it does.
func (r *Reserve) claim() (*usageRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reading != nil {
		return r.reading, false
	}
	r.runs++
	run := &usageRun{n: r.runs, inflight: r.inflight.Load(), done: make(chan struct{})}
	r.reading = run
	return run, true
}

// run takes one reading and releases everybody waiting on it.
//
// DETACHED FROM THE OWNER'S CANCELLATION and bounded by the longest barrier's
// budget ([ElectionBarrierBudget]), for the reason [ReadIndex.run] gives: the
// reading is shared work, and a caller that gives up must not fail every
// admission waiting on it. Each caller still leaves on its own context, so the
// bound holds nobody past their own deadline; a reading across an election of
// the log's leader is asked again inside it rather than failed at five seconds.
func (r *Reserve) run(ctx context.Context, run *usageRun) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ElectionBarrierBudget)
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.reading = nil
			r.mu.Unlock()
			close(run.done)
		}()
		run.usage, run.err = r.read(readCtx)
		if run.err != nil {
			// LOGGED HERE, once a reading, for every admission refused on it.
			log.WarnContext(readCtx, "statelog_usage_unread", "stream", r.stream,
				"error", run.err.Error())
		}
	}()
}

// appendBytes is what one append is counted at in a reserve's budget: its
// payload and what the stored record carries beside it.
func appendBytes(payload []byte) int64 {
	return int64(len(payload)) + appendOverhead
}
