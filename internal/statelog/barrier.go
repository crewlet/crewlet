package statelog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// BarrierKind is the subject kind of the one subject that names no object.
//
// It is a KIND anyway, because every domain's applier has a case for it: an
// applier faults on a kind it has no case for, so a domain that forgot the
// barrier would stop at the first linearizable read rather than skip a record
// it did not know.
const BarrierKind = "barrier"

// FrameworkScope is the one-byte scope sentinel every FRAMEWORK record
// declares — a record whose whole effect is the framework's own and which
// writes no domain row on any node: the read index's barrier, and the trim's
// trimmed record ([TrimmedKind]).
//
// Chosen so it can never intersect any read's closure: such a record makes
// nothing stale, so a scope that overlapped a real object would make every
// read of it wait behind the record — for a barrier, every linearizable read
// invalidated by the very append that establishes it.
//
// ONE SENTINEL FOR EVERY FRAMEWORK RECORD rather than one each: a new
// one-byte sentinel is a path some domain's scope alphabet may already use —
// the tracker's whole-domain path is `t` — so each would be a fresh chance to
// collide with a real object, and one that is known not to is the only safe
// choice.
const FrameworkScope = "s"

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
//
// AND TWO DIFFERENT FAILURES, which is why the index knows how many members
// store its log ([ReadIndex.unconfirmed]). A barrier several members did not
// commit in time is a majority that did not agree — an election, a member
// down or cut off. On one member there is no majority to miss and no election
// to wait out: its acknowledgement needs nobody else's, so a barrier it did
// not acknowledge in time is its own write that did not complete — a disk not
// taking the fsync `stream.sync: always` makes of every append, a member that
// stopped serving. Told as a missed quorum, a single node on a stalling disk
// sent its operator looking for peers it does not have, with an election's
// retry hint.
type ReadIndex struct {
	log     Appender
	stream  string
	subject string
	domain  string
	encode  func(Envelope) ([]byte, error)
	gen     func() uint32
	metrics *metrics.Recorder

	// members is how many members store the log, read when a barrier is
	// refused rather than when the index is built: an operator may
	// re-replicate a stream under a running node.
	members func() int

	// admission holds a barrier out of the log's gate reserve like any
	// other ordinary append: barriers go on for as long as reads do, and
	// allowed into the reserve they would spend, a record at a time, the
	// room an eviction is kept.
	admission Admission

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
// FIVE SECONDS, past what a healthy barrier costs at any member count by two
// orders of magnitude or more — its p99 measured at 3.6 ms on one member and
// 42–62 ms at three replicas (the package doc's figures) — so a barrier that
// has not committed in five seconds is a broker that cannot commit rather
// than a slow one: on several members, one without a quorum (`no_quorum`);
// on one member, whose acknowledgement waits on nobody else's, one whose own
// write has stalled (`broker_stalled` — [ReadIndex.unconfirmed]). Either way
// a linearizable read is better told than left holding a request open.
//
// NOT LENGTHENED FOR A STALLING DISK. A single node's broker was seen, in a
// test runner whose disk two loaded suites shared, holding its writes for
// 9.27 s; a budget long enough to outlast that holds every linearizable read
// on the node for as long, where a read refused at five seconds, with a code
// that names the stall, is one its caller can act on.
const DefaultBarrierBudget = 5 * time.Second

// NewReadIndex builds the read index of one of a domain's logs — the one spec
// names, whose own barrier subject the index appends to.
//
// ONE PER LOG, AND SINGLE-FLIGHTED PER LOG: a barrier proves where ONE
// stream's end is, so each log a node runs has its own index proving its own
// end, and a reader of one log never waits on an append to another.
//
// signer is the fleet keyring's signer for the domain: a barrier is a record
// on the log like any other, so it is signed like any other.
//
// members is how many members store the log, as its broker last reported it
// (the engine's is the stream's own replication factor, never Tier A's floor
// on it): what decides whether a barrier that did not commit was a majority
// that did not agree or the sole member's own write — see [ReadIndex].
//
// admission is the log's gate reserve ([Reserve]) — required of a domain that
// keeps one, the one every other ordinary append on this log on this node is
// admitted through, and refused on a domain that keeps none.
func NewReadIndex(d Domain, spec StreamSpec, log Appender, members func() int, signer *Signer,
	admission Admission, encode func(Envelope) ([]byte, error), gen func() uint32,
	rec *metrics.Recorder) (*ReadIndex, error) {
	if err := spec.Instantiates(d); err != nil {
		return nil, err
	}
	if members == nil {
		return nil, fmt.Errorf("statelog: read index for %q has no member count, and "+
			"a barrier that did not commit means something different on one member",
			d.Name())
	}
	// A BARRIER IS A RECORD ON THE DOMAIN'S OWN LOG, so it is signed like
	// any other: an unsigned one would let anything the broker admits
	// satisfy a linearizable read's proof of quorum.
	signed, err := signing(log, signer, d.Name())
	if err != nil {
		return nil, err
	}
	if encode == nil {
		return nil, fmt.Errorf("statelog: read index for %q has no encoder", d.Name())
	}
	switch {
	case ReservationOf(d).Kept && admission == nil:
		return nil, fmt.Errorf("statelog: read index for %q has no admission, so "+
			"its barriers would fill the gate reserve an eviction needs", d.Name())
	case !ReservationOf(d).Kept && admission != nil:
		return nil, fmt.Errorf("statelog: read index for %q was given an "+
			"admission, and its log keeps no gate reserve", d.Name())
	}
	if gen == nil {
		return nil, fmt.Errorf("statelog: read index for %q has no generation source", d.Name())
	}
	return &ReadIndex{
		log:       signed,
		stream:    spec.Name,
		subject:   wireSubject(spec.SubjectPrefix, Subject{Kind: BarrierKind}),
		domain:    d.Name(),
		encode:    encode,
		gen:       gen,
		metrics:   rec,
		members:   members,
		admission: admission,
		budget:    DefaultBarrierBudget,
	}, nil
}

// unconfirmed is what a barrier on this log that did not commit is refused
// as; ranOut is whether its wait ran out of time — its own budget, or what its
// asker's attempt left — rather than ending on an error.
//
// ON SEVERAL MEMBERS IT IS `no_quorum`, however it ended: the barrier was
// proposed to the log's leader and a majority did not commit it in time, or
// no leader answered — an election, a member down or cut off.
//
// ON ONE IT IS NEVER `no_quorum`: no majority is asked, no election is held,
// and the four seconds an election takes ([ElectionRetryHint]) describe
// nothing. A barrier the sole member did not acknowledge in time is its own
// write that did not complete (`broker_stalled`), and one that ended on an
// error first — nobody answering the append, a stream that closed under it —
// is a broker that did not answer (`broker_unreachable`).
func (r *ReadIndex) unconfirmed(ranOut bool) ReadRefusal {
	switch {
	case r.members() > 1:
		return RefuseNoQuorum
	case ranOut:
		return RefuseBrokerStalled
	}
	return RefuseBrokerUnreachable
}

// Read returns the read index a linearizable read rests on: the position of a
// barrier the broker committed after the caller arrived. The read waits for
// every record BEFORE it that writes a row to commit ([Reader.Read]), and for
// no barrier's commit — this one's, or that of any barrier before it: a barrier
// writes no row on any node, so its commit is a checkpoint and an fsync that
// change nothing the read reads, and this node's applier reaches it the moment
// it decodes it ([Runner.Reached]).
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
//
// # The arrival is taken ONCE
//
// Every run that starts after a caller arrived bounds its read, however long
// the caller then waited, so its arrival is the instant it called and is
// never taken again. Taken again each time the caller woke from a run it
// could not use, it held itself to a run started after IT WOKE — and the
// first waiter to wake claims the next run at once, before the rest have
// woken, so every waiter but that one went round again, and the next, and the
// one after: sixteen readers on one node made sixteen appends between them
// where one round trip should have served fifteen
// (TestReadersThatArriveDuringOneBarrierAreServedByTheNext; `make bench`'s
// read arms at readers=16 report the appends a read made).
func (r *ReadIndex) Read(ctx context.Context) (Position, error) {
	arrived := time.Now()
	for {
		run, mine := r.claim()
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
		// sequence bounds nothing for this reader. Go round again: the
		// next run starts after this one ended, which is after we arrived.
	}
}

// claim joins the in-flight run or becomes it.
//
// IT TAKES NO ARRIVAL TIME, deliberately: the run it may create is stamped
// with the instant the lock was won, which is after EVERY caller already
// waiting rather than after this one — and that is the comparison
// [ReadIndex.Read] makes. Stamping it with one caller's arrival would hand
// every other member of the same run a bound that predates them.
func (r *ReadIndex) claim() (*barrierRun, bool) {
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
			Scope:   ScopeSet{Paths: []string{FrameworkScope}},
		}
		body, err := r.encode(env)
		if err != nil {
			// LOGGED HERE, once, for the reason [frameworkAppendError]
			// gives: every reader waiting on this run is told the
			// refusal's own sentence, never these words.
			log.Warn("statelog_barrier_unconfirmed", "subject", r.subject,
				"stage", "encode", "error", err.Error())
			run.err = fmt.Errorf("statelog: encode a barrier: %w", err)
			return
		}
		// NO OP ID, AND THAT IS NOT AN OMISSION. A repeated message id
		// inside the dedupe window is served out of the window with no
		// raft round trip at all — so a barrier carrying one would
		// return a PubAck for a position nothing confirmed, which is
		// exactly the proof the level rests on. A duplicate here is a
		// refusal rather than an answer.
		// HELD OUT OF THE RESERVE, and refused `log_full` at the ordinary
		// ceiling as every other ordinary append is — which the read path
		// renders as the same refusal a full log gives.
		release := func() {}
		if r.admission != nil {
			if release, err = r.admission.Admit(appendCtx, appendBytes(body)); err != nil {
				run.err = err
				return
			}
		}
		seq, duplicate, err := r.log.Append(appendCtx, r.subject, "", nil, body)
		release()
		if err != nil {
			run.err = frameworkAppendError(BarrierKind, r.subject, err)
			if refused := (*Unavailable)(nil); !errors.As(run.err, &refused) {
				// LOGGED HERE, once per append, for the reason
				// [frameworkAppendError] gives: what nobody answered
				// is told to no reader in these words.
				log.Warn("statelog_barrier_unconfirmed", "subject", r.subject,
					"stage", "append", "error", err.Error())
			}
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

// frameworkAppendError is what an append of a framework record the broker
// did not store (a barrier, a trimmed record) means to whoever waits on it,
// decided by the same [classify] a write's append is.
//
// A REFUSAL THE BROKER NAMED IS A REFUSAL, NOT A MISSED QUORUM. The error went
// back wrapped and unclassified, and the read mapped anything that was not
// already an [Unavailable] to `no_quorum`, so a log at its ceiling, which
// refuses the barrier exactly as it refuses a write, was answered as a failed
// election: retryable, with a four-second hint, from a node that will refuse
// the next barrier identically until an operator resizes the log. And
// `log_full`, the refusal the read vocabulary has for it, was reachable only
// from a test's fake appender that returned one ready-made. The trim's line
// names a refused trimmed record `log_full` by the same rule.
//
// What nobody answered stays unclassified, and is the unconfirmed barrier it
// was ([ReadIndex.unconfirmed]: a missed quorum on several members, a broker
// that did not answer on one); and so does an answer saying the record may
// yet land ([faultUnsettled]): a framework record carries no message id, so
// the one such answer it can meet is a store that closed under an entry raft
// had committed, which is a stream restarting and clears exactly as an
// unanswered append does. The barrier's caller
// LOGS that error, once per append, and never tells it to a reader: it is the
// transport's or the broker's own words, and every reader waiting on the
// append is told [barrierRefusal]'s sentence instead. A refusal the broker
// NAMED is different: its words are the remedy, so they travel on the
// refusal's detail.
func frameworkAppendError(record, subject string, err error) error {
	switch f, detail := classify(err); f {
	case faultFull:
		return &Unavailable{
			Reason: ReasonLogFull,
			Detail: fmt.Sprintf("the broker refused to store the %s record on %s: "+
				"%s; a full log refuses appends rather than dropping records, "+
				"so raise the stream's byte ceiling or unblock the trim "+
				"(`crewlet retention status` names the term holding it)",
				record, subject, detail),
			Cause: err,
		}
	case faultTooLarge, faultRefused:
		// A FRAMEWORK RECORD IS A FEW HUNDRED BYTES, so a broker that
		// refuses one as too large is one whose limit refuses every
		// record; either way it is the broker's decision, and the detail
		// carries its words (and, for a limit, which one and what moves
		// it).
		return &Unavailable{
			Reason: ReasonBrokerRefused,
			Detail: fmt.Sprintf("the broker refused to store the %s record on %s: %s",
				record, subject, detail),
			Cause: err,
		}
	}
	return fmt.Errorf("statelog: append a %s record on %s: %w", record, subject, err)
}
