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
// ceiling, [GateReserve] below it, and the records a gate is installed or
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
// appends in flight at that instant, and those are what [GateReserveDivisor]
// is sized against.
//
// # Where it applies
//
// Only on a log that claims identity ([KeepsGateReserve]), because only such a
// log carries gate records. The vector changelog claims none: a reserve there
// would refuse its ordinary writes early to keep room nothing is ever written
// into.

// GateReserveDivisor sets the reserve at a fraction of a log's byte ceiling:
// [GateReserve] is the ceiling divided by it.
//
// SIXTEEN, anchored to three numbers. The overshoot the reserve has to absorb
// is every ordinary append admitted against a reading that could not see its
// peers' — the bytes the OTHER nodes hold in flight at the instant the soft
// ceiling is crossed, since the admitting node counts its own. Each node holds
// at most [MaxAppendBytes] of ordinary appends in flight per log ([Reserve]'s
// budget: the PUBLISH CONCURRENCY, in bytes), and no record is larger than
// that, the transport's own maximum (the MAXIMUM RECORD SIZE). At the MINIMUM
// CEILING any log may have — a gibibyte, Tier A's floor for every domain, the
// floor the engine's division of the broker never scales below and the one
// `crewlet retention set-capacity` refuses to go under — a sixteenth is
// 64 MiB: seven maximum records in flight on seven other nodes, and just under
// 8 MiB to spare for gate records, each under a kibibyte. So the reserve holds
// for a fleet of [GateReserveFleet] at the smallest ceiling, and for more on
// every larger one, since it grows with the ceiling while the overshoot does
// not. The engine's own tests hold that arithmetic against the floors.
//
// Past that, a gate record is refused like any other and the refusal says to
// raise the ceiling (`crewlet retention set-capacity`), not to run the gesture
// again. And the price is a sixteenth of each identity log's ceiling that
// ordinary writes never use — the headroom alarm fires at a tenth of the
// ceiling that is left for them, so an operator hears about a filling log
// long before either line is reached.
//
// A NODE ON AN OLDER BUILD ADMITS NOTHING, so while a rolling upgrade runs
// its appends fill the log to the broker's ceiling as they always did; the
// reserve holds once every node counts its own.
const GateReserveDivisor = 16

// GateReserveFleet is how many nodes' appends in flight [GateReserveDivisor]
// is sized to absorb at the smallest ceiling — the admitting node and the
// peers whose appends its reading cannot see.
const GateReserveFleet = 8

// MaxAppendBytes is the most an ordinary append may hold in one node's
// in-flight budget on one log, and the largest record a reserved log admits.
//
// THE TRANSPORT'S OWN MAXIMUM, [queue.MaxPayloadBytes], plus what a stored
// record carries beside its payload — the subject, the message-id and
// expectation headers, the store's own framing — which a subject of a few
// hundred bytes keeps well inside [appendOverhead]. It is the per-node term in
// [GateReserveDivisor]'s arithmetic, so a record above it could stand in the
// budget for more than the reserve was sized for; it is refused instead, as
// the embedded broker would refuse it anyway.
const MaxAppendBytes = queue.MaxPayloadBytes + appendOverhead

// appendOverhead bounds what a stored record carries beside its payload.
//
// FOUR KIBIBYTES: the file store frames a record in about thirty bytes, the
// two headers an append carries are well under two hundred, and the longest
// subject any domain writes — a page title's bounded token — is a few hundred.
// Counted per append, so it also keeps a flood of tiny records from reading as
// free.
const appendOverhead = 4 << 10

// KeepsGateReserve reports whether d's log holds a gate reserve under its
// ceiling: whether it claims identity, which is what carries gate records.
func KeepsGateReserve(d Domain) bool { return d.ClaimsIdentity() }

// GateReserve is how much of a byte ceiling of maxBytes is kept for gate
// records — see [GateReserveDivisor]. Zero for an unbounded log.
func GateReserve(maxBytes uint64) uint64 { return maxBytes / GateReserveDivisor }

// OrdinaryCeiling is the ceiling ordinary appends are held to on a log of
// maxBytes: the whole of it on a log that keeps no reserve, and [GateReserve]
// below it on one that does. Zero for an unbounded log.
func OrdinaryCeiling(maxBytes uint64, reserved bool) uint64 {
	if !reserved {
		return maxBytes
	}
	return maxBytes - GateReserve(maxBytes)
}

// Headroom is how much of the ceiling ordinary appends may use is still
// unused, as a fraction — nil for a log with no ceiling, where a fraction
// means nothing and zero is what the headroom alarm fires on.
//
// OF THE ORDINARY CEILING, not the broker's, because the question it answers
// is how long until writes are refused: at zero the log refuses every
// ordinary write and every linearizable read, while gate records still land.
func Headroom(bytes, maxBytes uint64, reserved bool) *float64 {
	ceiling := OrdinaryCeiling(maxBytes, reserved)
	if ceiling == 0 {
		return nil
	}
	left := float64(ceiling-min(bytes, ceiling)) / float64(ceiling)
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

	// budget is the node's ordinary appends in flight, in bytes, capped
	// at [MaxAppendBytes]; inflight is what it holds now, which the
	// semaphore does not expose.
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

// NewReserve builds the reserve for one log, reading its usage through read —
// the broker's own stream state.
func NewReserve(stream string, read func(ctx context.Context) (Usage, error)) (*Reserve, error) {
	if read == nil {
		return nil, fmt.Errorf("statelog: the reserve on %s has no usage reading", stream)
	}
	return &Reserve{stream: stream, read: read,
		budget: semaphore.NewWeighted(MaxAppendBytes)}, nil
}

// Admit takes room for one ordinary append of size bytes, and refuses it
// `log_full` when the log is past its ordinary ceiling. On a nil error the
// caller MUST call release once the broker has answered the append.
func (r *Reserve) Admit(ctx context.Context, size int64) (release func(), err error) {
	if size > MaxAppendBytes {
		return nil, fmt.Errorf("statelog: a %d-byte record is larger than any %s "+
			"admits (%d): %w", size, r.stream, int64(MaxAppendBytes), queue.ErrTooLarge)
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
	ceiling := OrdinaryCeiling(usage.MaxBytes, true)
	if ceiling == 0 {
		return release, nil
	}
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
				usage.MaxBytes, GateReserve(usage.MaxBytes)),
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
