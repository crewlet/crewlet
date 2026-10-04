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
// which only the records that install a gate may use.
//
// # Why a full log needs room it refuses everybody else
//
// A log that fills refuses appends rather than dropping records, and what
// fills one is almost always a trim that cannot advance — and the commonest
// reason a trim cannot advance is a node that is gone and still counted,
// pinning the applied term at its last position. The one gesture that unpins
// it is an EVICTION, which is a record on that log. A log full to its ceiling
// refuses that record like any other, so the operator was told `log_full`, ran
// the eviction again, and was refused again, for ever: the only way out of a
// full log was through the log. So the ceiling the broker enforces is not the
// ceiling ordinary writes are held to. They are refused `log_full` at the SOFT
// ceiling, [Reservation.Bytes] below it, and the records a gate is installed or
// lifted by go on landing in the space between.
//
// # Which records are gate records
//
// An eviction and the readmission that inverts it — the two a node's standing
// on a log is written by, and the two halves of one gesture an operator has to
// be able to finish. A reanchor's generation record goes straight to the
// broker too ([appendGeneration]): it is appended to a stream every node's
// fence refuses to write until it lands, so it meets a log nothing else can
// have filled, at most once per generation. A PURGE IS NOT ONE, although it
// installs a deletion marker: it frees nothing — the records it gates stay on
// the log until the trim removes them — and nothing bounds how many an
// operator runs, so a purge allowed into the reserve could spend the room an
// eviction is kept it for.
//
// # How usage is measured, and how stale a reading may be
//
// From the broker's own stream state, the one figure that counts every node's
// writes, READ AFTER THE ADMISSION BEGAN: a reading an admission shares must
// have started after that admission took its place, the arrival rule
// [ReadIndex] applies to a barrier. So the only bytes a reading cannot see
// are appends in flight beside it. This node's own are counted exactly: an
// ordinary append holds its size in [Reserve]'s budget from before its
// reading until the broker answers. What no node can see is its PEERS'
// appends in flight at that instant, and those are what the reserve's fleet
// term is sized against ([GateReserveDivisor]).
//
// # Where it applies
//
// Only on a log that claims identity ([KeepsGateReserve]), because only such a
// log carries gate records. The vector changelog claims none: a reserve there
// would refuse its ordinary writes early to keep room nothing is ever written
// into.

// GateReserveDivisor and GateReserveFleet size the reserve, which is the
// larger of two terms ([Reservation.Bytes]): a SIXTEENTH of the log's byte
// ceiling, and the FLEET TERM — what [GateReserveFleet] nodes' appends in
// flight can carry a log past its soft ceiling, plus [GateRoom] for the gate
// records themselves.
//
// # The fleet term, and why it is the log's and not the ceiling's
//
// The overshoot the reserve has to absorb is every ordinary append admitted
// against a reading that could not see its peers' — the bytes the OTHER nodes
// hold in flight at the instant the soft ceiling is crossed, since the
// admitting node counts its own. Each node holds at most one log's
// [StreamSpec.MaxAppendBytes] of ordinary appends in flight ([Reserve]'s
// budget: the PUBLISH CONCURRENCY, in bytes), and no record is larger than
// that ([StreamSpec.MaxRecordBytes], the MAXIMUM RECORD SIZE). So the
// overshoot of a fleet of eight is seven of those: a function of the log's
// LARGEST RECORD, and of nothing about its ceiling.
//
// IT WAS A SIXTEENTH ALONE, argued at one ceiling: a gibibyte, called Tier A's
// floor for every domain, where a sixteenth is 64 MiB — seven records at the
// transport's eight mebibytes, and room to spare. It was not every domain's
// floor. The identity estate's Tier A floor is 64 MiB, where a sixteenth is
// 4 MiB — less than ONE record at the size the budget admitted — so on exactly
// the smallest log an eviction on a log full for ordinary writes could find the
// reserve spent by appends it was kept against.
// Stated per log, from the log's own largest record, the fleet term holds at
// every ceiling a log may have, whatever its floor.
//
// # The sixteenth, and why it is still here
//
// It GROWS WITH THE CEILING while the fleet term does not, so a larger log
// absorbs a larger fleet. On the corpus-sized logs — a gibibyte floor, records
// up to the transport's eight mebibytes — it is the larger term from the floor
// up: 64 MiB against 57. The identity estate's — a 64 MiB floor, records of at
// most 128 KiB — are small enough that the sixteenth is the larger from its
// floor up, 4 MiB against under 2.
//
// # What it costs
//
// The reserve is ceiling that ordinary writes never use — the headroom alarm
// fires at a tenth of what is left for them, so an operator hears about a
// filling log long before either line is reached. Past the reserve a gate
// record is refused like any other, and the refusal says to raise the ceiling
// (`crewlet retention set-capacity`), not to run the gesture again.
//
// A NODE ON AN OLDER BUILD ADMITS NOTHING, so while a rolling upgrade runs
// its appends fill the log to the broker's ceiling as they always did; the
// reserve holds once every node counts its own.
const GateReserveDivisor = 16

// GateReserveFleet is how many nodes' appends in flight the fleet term
// absorbs — the admitting node and the seven peers whose appends its reading
// cannot see. See [GateReserveDivisor].
const GateReserveFleet = 8

// GateRoom is what the fleet term keeps beyond the peers' appends, for the
// gate records themselves.
//
// A MEBIBYTE: a thousand gate records at under a kibibyte each — an eviction
// and a readmission per node per log, many times over. It is the one part of
// the reserve that exists for the records it is kept for; the rest is what
// the admission's blind spot may already have spent.
const GateRoom = 1 << 20

// appendOverhead bounds what a stored record carries beside its payload.
//
// FOUR KIBIBYTES: the signature frame is its magic, version, key id and a
// 32-byte MAC — under three hundred bytes with the longest key id a keyring
// may name — the file store frames a record in about thirty bytes, the two
// headers an append carries are well under two hundred, and the longest
// subject any domain writes — a page title's bounded token — is a few hundred.
// Counted per append, so it also keeps a flood of tiny records from reading
// as free.
const appendOverhead = 4 << 10

// MaxTransportRecordBytes is the largest record the transport carries once it
// is signed and sent: [queue.MaxPayloadBytes] less what a stored record carries
// beside its payload ([appendOverhead]).
//
// THE CEILING EVERY DECLARATION IS HELD TO ([StreamSpec.Validate]), and what a
// log whose records are bounded by caps of their own declares. The transport's
// limit is on the whole MESSAGE — the signed payload and its headers — and not
// on the payload a decide forms, so a declaration at the transport's own
// number admitted a record the publisher passed and the broker then refused
// past its max_payload: the declaration was no longer the one refusal a
// record meets before it is sent, and the refusal it did meet named the
// server's setting on the broker the engine configures itself.
const MaxTransportRecordBytes = queue.MaxPayloadBytes - appendOverhead

// MaxAppendBytes is the most one append to this log is counted at: its
// largest record ([StreamSpec.MaxRecordBytes]) and what a stored record
// carries beside it.
//
// ONE NUMBER FOR THREE READERS, which is why it is a method rather than an
// addition each does for itself: it is the per-node in-flight budget
// [Reserve] admits against, the per-node term in the fleet term
// ([Reservation.Bytes]), and the stream's max_msg_size the broker refuses a
// larger message at — so the broker's refusal and the reserve's arithmetic
// are about the same record.
func (s StreamSpec) MaxAppendBytes() int64 { return s.MaxRecordBytes + appendOverhead }

// KeepsGateReserve reports whether d's log holds a gate reserve under its
// ceiling: whether it claims identity, which is what carries gate records.
func KeepsGateReserve(d Domain) bool { return d.ClaimsIdentity() }

// Reservation is how one log's byte ceiling divides between its ordinary
// appends and the records that install or lift a gate.
//
// THE ZERO VALUE KEEPS NO RESERVE, which is the vector changelog's shape and a
// real setting rather than an absent one: a log that claims no identity
// carries no gate record, and a reserve there would refuse its ordinary writes
// early to keep room nothing is ever written into.
type Reservation struct {
	// Kept reports that the log keeps a reserve ([KeepsGateReserve]).
	Kept bool

	// MaxAppend is the log's [StreamSpec.MaxAppendBytes]: the most one
	// peer's appends in flight can hold, which is what the fleet term is
	// counted in.
	MaxAppend int64
}

// ReservationOf is d's reservation, read off its declaration.
func ReservationOf(d Domain) Reservation {
	if !KeepsGateReserve(d) {
		return Reservation{}
	}
	return Reservation{Kept: true, MaxAppend: d.Stream().MaxAppendBytes()}
}

// Bytes is how much of a byte ceiling of maxBytes is kept for gate records —
// the larger of a sixteenth of it and the fleet term, see
// [GateReserveDivisor] — and never more than the ceiling. Zero for a log that
// keeps none and for an unbounded log.
//
// ALL OF A CEILING SMALLER THAN THE FLEET TERM, which no Tier A floor comes
// near: such a log has nowhere to put an ordinary write that the fleet's
// appends in flight could not carry past it, so it refuses every one and
// keeps the whole of itself for the records that can unpin it.
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

// Headroom is how much of the ceiling ordinary appends may use is still
// unused, as a fraction — nil for a log with no ceiling, where a fraction
// means nothing and zero is what the headroom alarm fires on — and zero on a
// log whose reserve is all of it, where no ordinary append is admitted.
//
// OF THE ORDINARY CEILING, not the broker's, because the question it answers
// is how long until writes are refused: at zero the log refuses every
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
// because the budget it keeps is the node's appends in flight on the log,
// whoever makes them.
type Reserve struct {
	stream string
	read   func(ctx context.Context) (Usage, error)

	// reservation is the log's, and maxAppend its
	// [StreamSpec.MaxAppendBytes]: the budget's cap, and the largest
	// append it admits.
	reservation Reservation
	maxAppend   int64

	// budget is the node's ordinary appends in flight, in bytes, capped
	// at maxAppend; inflight is what it holds now, which the semaphore
	// does not expose.
	//
	// A WEIGHTED SEMAPHORE from the Go project's own extended library
	// rather than one written here, for the two properties a buffered
	// channel does not have: an acquire of many units that honours
	// cancellation, and FIFO service, so a maximum record is not starved
	// by a stream of small ones taking the room as it frees.
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
// through read — the broker's own stream state.
//
// THE SPEC AND NOT A NAME, because the budget is the log's own: one append of
// its largest record ([StreamSpec.MaxAppendBytes]) is what one node may hold
// in flight, and that is the per-node term the reserve above the soft ceiling
// is sized in ([Reservation.Bytes]). A budget any larger would let this node
// hold more in flight than every peer's reserve assumed of it.
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
// `log_full` when the log is past its ordinary ceiling. On a nil error the
// caller MUST call release once the broker has answered the append.
func (r *Reserve) Admit(ctx context.Context, size int64) (release func(), err error) {
	if size > r.maxAppend {
		// A RECORD TOO LARGE, and its own reason rather than a full log's:
		// the log may have room to spare, and nothing about its ceiling or
		// its trim moves this limit. The detail names the one that did.
		// The publisher refuses such a record against the same declaration
		// before it is ever admitted; this is the budget's own guard, since
		// an append above it would wait for room it can never hold.
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
		return nil, fmt.Errorf("statelog: read %s's usage before appending to it: %w",
			r.stream, err)
	}
	usage := run.usage
	if usage.MaxBytes == 0 {
		// AN UNBOUNDED LOG, and nothing else: a bounded log whose reserve
		// is all of it has an ordinary ceiling of zero too, and it refuses
		// every ordinary append below rather than admitting them all.
		return release, nil
	}
	ceiling := r.reservation.Ordinary(usage.MaxBytes)
	// WHAT THE BROKER HELD, AND WHAT THIS NODE HAD IN FLIGHT WHEN IT WAS
	// ASKED, this append included: an append in flight then either landed
	// before the broker answered, and is counted twice — which only
	// refuses a write a little early — or did not, and is counted once.
	// Read when the reading STARTED rather than now, because an append
	// that lands and releases while the reading is out would otherwise be
	// in neither figure.
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
// arrived — after its bytes were counted in flight.
//
// SHARED, and only forward in the order readings start: a reading already
// out when this caller arrived may have been answered before a peer's append
// this caller must count, and it captured this node's in-flight bytes before
// this caller's were among them — joining it would admit against figures
// older than the admission. So an arrival is the number of the next reading
// to start, taken under the lock every reading is started under, and a
// reading below it is waited out rather than used.
//
// BY COUNT RATHER THAN BY CLOCK, because the count is taken under the same
// lock as the in-flight figure a reading captures ([Reserve.claim]): a
// reading numbered at or above an arrival is one whose figure includes that
// arrival's bytes, which is the property the check below rests on, where two
// clock readings taken on either side of the lock would only approximate it.
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
// DETACHED FROM THE OWNER'S CANCELLATION and bounded by the barrier's own
// budget, for the reason [ReadIndex.run] gives: the reading is shared work,
// and a caller that gives up must not fail every admission waiting on it.
func (r *Reserve) run(ctx context.Context, run *usageRun) {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultBarrierBudget)
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.reading = nil
			r.mu.Unlock()
			close(run.done)
		}()
		run.usage, run.err = r.read(readCtx)
	}()
}

// appendBytes is what one append is counted at in a reserve's budget: its
// payload and what the stored record carries beside it.
func appendBytes(payload []byte) int64 {
	return int64(len(payload)) + appendOverhead
}
