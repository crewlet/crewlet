package statelog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// BarrierKind is the subject kind of the one subject that names no object.
//
// It is a KIND anyway, because the applier has a case for it and a domain's
// table map must classify it — it declares the EMPTY set, explicitly, so a
// record that writes nothing cannot slip through a completeness walk by
// writing nothing.
const BarrierKind = "barrier"

// BarrierScope is the one-byte scope sentinel a barrier declares.
//
// Chosen so it can never intersect any read's closure: a barrier makes
// nothing stale, so a scope that overlapped a real object would make every
// linearizable read look as though it had been invalidated by the very
// append that establishes it.
const BarrierScope = "s"

// BarrierVersion is 1 from the first release AND FOR EVER.
//
// A record version is normally free to move, and this one is not: a barrier
// a build cannot decode would be DEFERRED, and a deferred barrier is a
// self-inflicted permanent read outage on the node that deferred it. Nothing
// a later version could carry is worth that, because the record has no
// payload to extend.
const BarrierVersion = 1

// ReadIndex is the framework's own: what makes a position authoritative is a
// fact about the BROKER, not about any domain.
//
// # Why an append and not a leader check
//
// A linearizable read needs to know the log end at or after the read arrived.
// Asking the broker for its cluster leader answers from the node's own
// in-memory state with no peer contacted, so the dangerous case — an isolated
// former leader — answers with a NONEMPTY name and its own stale last
// sequence. Measured, that state persisted for fifteen seconds after the
// majority had committed a newer write.
//
// A barrier append cannot lie about it. The server proposes rather than
// stores, the commit returns false until the acknowledgements reach a
// quorum, and the acknowledgement is written from the apply path — so a
// PubAck at N proves that N is committed by a majority and that its emitter
// led the term in which it committed. On a single member it proves the sole
// member wrote it, and under an always-fsync it proves the write reached the
// disk. Two different proofs of the same thing, and it is all the level
// needs.
type ReadIndex struct {
	log     Appender
	stream  string
	subject string
	domain  string
	encode  func(Envelope) ([]byte, error)
	gen     func() uint32
	metrics *metrics.Recorder

	// budget bounds the append itself, and it is separate from any one
	// caller's context for the reason the append is shared: a reader
	// that walks away must not cancel the round trip the readers behind
	// it are waiting on.
	budget time.Duration

	mu sync.Mutex
	// inflight is the append currently running, if any. A caller joins it
	// only when it arrived after that append started — see Read.
	inflight *barrierRun
}

// barrierRun is one in-flight barrier append and everybody waiting on it.
type barrierRun struct {
	started time.Time
	done    chan struct{}
	seq     uint64
	gen     uint32
	err     error
}

// DefaultBarrierBudget bounds one barrier append.
//
// FIVE SECONDS, which is a quorum round trip's own scale multiplied by three
// orders: a barrier that has not committed in five seconds is a broker
// without a quorum rather than a slow one, and a linearizable read is better
// told that than left holding a request open.
const DefaultBarrierBudget = 5 * time.Second

// NewReadIndex builds a domain's read index.
func NewReadIndex(d Domain, log Appender, encode func(Envelope) ([]byte, error),
	gen func() uint32, rec *metrics.Recorder) (*ReadIndex, error) {
	spec := d.Stream()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if encode == nil {
		return nil, fmt.Errorf("statelog: read index for %q has no encoder", d.Name())
	}
	if gen == nil {
		return nil, fmt.Errorf("statelog: read index for %q has no generation source", d.Name())
	}
	if class, ok := d.Tables()[BarrierKind]; ok && class != Local {
		return nil, fmt.Errorf("statelog: domain %q classes the barrier %s — a "+
			"barrier writes no rows at all", d.Name(), class)
	}
	return &ReadIndex{
		log:     log,
		stream:  spec.Name,
		subject: spec.SubjectPrefix + "." + BarrierKind,
		domain:  d.Name(),
		encode:  encode,
		gen:     gen,
		metrics: rec,
		budget:  DefaultBarrierBudget,
	}, nil
}

// Read returns the position a linearizable read must wait for.
//
// # Single-flighted, and the arrival-time rule is what makes it correct
//
// Coalescing concurrent readers onto one append is what keeps a company's
// read rate off the raft log. But a caller may only use an append that
// STARTED AFTER IT ARRIVED: an append already in flight was proposed before
// this read existed, so its sequence is not a bound on anything this reader
// cares about, and joining it would answer a stale position with a quorum's
// authority behind it — the worst shape a read can have.
//
// So a caller that arrives mid-flight waits for that run to finish and then
// starts or joins the NEXT one. The cost is at most one extra round trip
// under contention; the alternative is a linearizable read that is not.
func (r *ReadIndex) Read(ctx context.Context) (Position, error) {
	arrived := time.Now()
	for {
		run, mine := r.claim(arrived)
		if mine {
			r.run(ctx, run)
		}
		select {
		case <-ctx.Done():
			return Position{}, ctx.Err()
		case <-run.done:
		}
		if run.err != nil {
			return Position{}, run.err
		}
		if !run.started.Before(arrived) {
			return Position{Stream: r.stream, Generation: run.gen, Seq: run.seq}, nil
		}
		// The run we joined had already started when we arrived, so its
		// sequence bounds nothing for this reader. Go round again; the
		// next run starts from here.
		arrived = time.Now()
	}
}

// claim joins the in-flight run or becomes it.
func (r *ReadIndex) claim(arrived time.Time) (*barrierRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight != nil {
		return r.inflight, false
	}
	// A run claimed now started after every caller that has already
	// arrived, which is exactly what the arrival rule compares against.
	run := &barrierRun{started: time.Now(), done: make(chan struct{})}
	r.inflight = run
	return run, true
}

// run performs the append and releases everybody waiting on it.
func (r *ReadIndex) run(ctx context.Context, run *barrierRun) {
	// DETACHED FROM THIS CALLER'S CANCELLATION and bounded by its own
	// budget. The append is shared work: a reader that gives up must not
	// cancel the round trip every reader behind it is waiting on, and an
	// append with no bound at all would outlive the process's interest in
	// it. The values travel, so the append keeps the trace it was made
	// under.
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.budget)
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.inflight = nil
			r.mu.Unlock()
			close(run.done)
		}()
		started := time.Now()
		// THE GENERATION IS READ ONCE, before the append, and travels
		// with the run: reading it afterwards would stamp a reanchor
		// that happened during the append onto a sequence from before
		// it, which is a position in a space it does not belong to.
		run.gen = r.gen()
		env := Envelope{
			V:       BarrierVersion,
			Kind:    BarrierKind,
			Subject: Subject{Kind: BarrierKind},
			Gen:     run.gen,
			Scope:   ScopeSet{Paths: []string{BarrierScope}},
		}
		body, err := r.encode(env)
		if err != nil {
			run.err = fmt.Errorf("statelog: encode a barrier: %w", err)
			return
		}
		// NO OP ID, AND THAT IS NOT AN OMISSION. A repeated message id
		// inside the dedupe window is served out of the window with no
		// raft round trip at all — so a barrier carrying one would
		// return a PubAck for a position nothing confirmed, which is
		// exactly the proof the level rests on. A duplicate here is a
		// refusal rather than an answer.
		seq, duplicate, err := r.log.Append(appendCtx, r.subject, "", nil, body)
		if err != nil {
			run.err = fmt.Errorf("statelog: append a barrier on %s: %w", r.subject, err)
			return
		}
		if duplicate {
			run.err = &Unavailable{
				Reason: ReasonSkew,
				Detail: fmt.Sprintf("the broker served the barrier on %s out of "+
					"its duplicate window, so its sequence proves nothing about "+
					"the log's end — a barrier must carry no message id",
					r.subject),
			}
			return
		}
		run.seq = seq
		if r.metrics != nil {
			r.metrics.Observe(metrics.StatelogBarrierDuration, time.Since(started),
				metrics.Attrs{"domain": r.domain})
			r.metrics.Add(metrics.StatelogBarrierAppends, 1,
				metrics.Attrs{"domain": r.domain})
		}
	}()
}
