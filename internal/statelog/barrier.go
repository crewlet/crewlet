package statelog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// BarrierKind is the subject kind of the one subject that names no object.
//
// It is a kind anyway because an applier faults on a kind it has no case for:
// a domain that forgot the barrier stops at the first linearizable read rather
// than skipping a record it does not know.
const BarrierKind = "barrier"

// FrameworkScope is the one-byte scope every FRAMEWORK record declares: one
// that writes no domain row on any node (the barrier, [TrimmedKind]).
//
// It intersects no read's closure, since such a record makes nothing stale; a
// scope overlapping a real object would make every read of it wait behind the
// record, and every linearizable read behind its own barrier. One sentinel for
// them all, not one each: each new one-byte path is another chance to collide
// with a domain's scope alphabet (the tracker's whole-domain path is `t`).
const FrameworkScope = "s"

// BarrierVersion is 1 from the first release AND FOR EVER: a barrier a build
// cannot decode is deferred, a permanent read outage on that node, and the
// record has no payload a later version could extend.
const BarrierVersion = 1

// ReadIndex is the framework's own: what makes a position authoritative is a
// fact about the BROKER, not about any domain.
//
// It appends a barrier rather than asking for the cluster leader, which a
// member answers from its own memory, so an isolated former leader names
// itself with a stale last sequence. A PubAck at N proves N committed: by a
// quorum, acknowledged from the apply path by that term's leader, on several
// members; by the sole member, on disk under an always-fsync, on one. So an
// unconfirmed barrier is a missed quorum on several members but the member's
// own stalled write on one, and the index counts its log's members
// ([ReadIndex.unconfirmed]).
type ReadIndex struct {
	log     Appender
	stream  string
	subject string
	domain  string
	encode  func(Envelope) ([]byte, error)
	gen     func() uint32
	metrics *metrics.Recorder

	// members is how many members store the log, read at each refusal rather
	// than at build: an operator may re-replicate a stream under a running node.
	members func() int

	// admission holds barriers out of the log's gate reserve like any
	// ordinary append: they go on as long as reads do, and would spend the
	// room kept for an eviction.
	admission Admission

	mu sync.Mutex
	// inflight is the append running, if any; Read says who may join it.
	inflight *barrierRun
}

// barrierRun is one in-flight barrier append and everybody waiting on it.
type barrierRun struct {
	started time.Time
	done    chan struct{}
	seq     uint64
	gen     uint32
	err     error
	// leaderless is the last answer that the log had no leader while the
	// run waits an election out, nil once anything else answered: what a
	// reader that leaves mid-run is told ([ReadIndex.Read]).
	leaderless atomic.Pointer[error]
}

// SoloBarrierBudget bounds a barrier on a log ONE member stores, where no
// election is involved.
//
// Five seconds is two orders of magnitude past a healthy barrier's p99 (3.6 ms;
// package doc, "What a write costs, measured"), so a barrier still uncommitted
// is the member's own stalled write, `broker_stalled` ([ReadIndex.unconfirmed]).
// Not lengthened for a stalling disk: a budget that outlasts one holds every
// linearizable read on the node as long, where a refusal naming the stall is
// one its caller can act on.
const SoloBarrierBudget = 5 * time.Second

// ElectionBarrierBudget bounds a barrier on a log SEVERAL members store: one
// election of the log's leader at the pinned broker's raft settings
// ([jsprovision.Election]: the 9 s longest election timeout, server/raft.go:329,
// and the new leader's first 1 s heartbeat, raft.go:331), waited out inside it,
// against 42–62 ms for a healthy barrier there. A barrier uncommitted past it
// is `no_quorum`.
const ElectionBarrierBudget = jsprovision.Election

// barrierBudget is the budget of a barrier on a log members store.
func barrierBudget(members int) time.Duration {
	if members > 1 {
		return ElectionBarrierBudget
	}
	return SoloBarrierBudget
}

// NewReadIndex builds the read index of the log spec names, appending to its
// own barrier subject. One per log, single-flighted per log: a barrier proves
// one stream's end, so a reader of one log never waits on another's append.
//
// signer is the domain's fleet keyring signer: a barrier is signed like any
// record. members is how many members store the log as its broker last
// reported it (the stream's own replication factor, never Tier A's floor),
// which decides what an unconfirmed barrier means ([ReadIndex]). admission
// is the log's gate reserve ([Reserve]), the one its other ordinary appends
// on this node pass: required of a domain that keeps one, refused on one
// that keeps none.
func NewReadIndex(d Domain, spec StreamSpec, log Appender, members func() int, signer *recordseal.Signer,
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
	// SIGNED: an unsigned barrier would let anything the broker admits
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
	}, nil
}

// unconfirmed is what a barrier on this log that did not commit is refused
// as; ranOut is whether its wait ran out of time (its budget, or what its
// asker's attempt left) rather than ending on an error.
//
// On several members it is `no_quorum` however it ended: a majority did not
// commit it in time, or no leader answered. On one it never is, since no
// majority or election is involved and [ElectionRetryHint] describes nothing:
// a timeout is the member's own stalled write (`broker_stalled`), an error
// first a broker that did not answer (`broker_unreachable`).
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
// barrier the broker committed after the caller arrived. The read then waits
// for every row-writing record before it ([Reader.Read]) and for no barrier,
// which writes no row and is reached once decoded ([Runner.Reached]).
//
// Readers coalesce onto one append, but each may use only an append that
// STARTED AFTER IT ARRIVED: one already in flight bounds nothing it asked
// about, and would answer a stale position with a quorum's authority. So a
// caller arriving mid-flight waits that run out and takes the next. Its
// arrival is taken once, never on waking: retaken, every waiter but the first
// went round again (TestReadersThatArriveDuringOneBarrierAreServedByTheNext).
func (r *ReadIndex) Read(ctx context.Context) (Position, error) {
	arrived := time.Now()
	for {
		run, mine := r.claim()
		if mine {
			r.run(ctx, run)
		}
		select {
		case <-ctx.Done():
			if answer := run.leaderless.Load(); answer != nil {
				return Position{}, &jsprovision.NoLeader{Group: r.stream, Answer: *answer, Err: ctx.Err()}
			}
			return Position{}, ctx.Err()
		case <-run.done:
		}
		if run.err != nil {
			return Position{}, run.err
		}
		if !run.started.Before(arrived) {
			return Position{Stream: r.stream, Generation: run.gen, Seq: run.seq}, nil
		}
		// The run started before we arrived, so it bounds nothing for this
		// reader; the next starts after this one ended.
	}
}

// claim joins the in-flight run or becomes it.
//
// It takes no arrival time: a new run is stamped when the lock is won, after
// EVERY caller already waiting, which is what [ReadIndex.Read] compares. One
// caller's arrival would hand the run's other members a bound predating them.
func (r *ReadIndex) claim() (*barrierRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.inflight != nil {
		return r.inflight, false
	}
	run := &barrierRun{started: time.Now(), done: make(chan struct{})}
	r.inflight = run
	return run, true
}

// run performs the append and releases everybody waiting on it.
func (r *ReadIndex) run(ctx context.Context, run *barrierRun) {
	// Detached from this caller's cancellation and bounded by its own
	// budget ([barrierBudget]): the append is shared, and a reader that walks
	// away must not cancel it for the readers behind it. The values travel,
	// so the append keeps its trace.
	appendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), barrierBudget(r.members()))
	go func() {
		defer cancel()
		defer func() {
			r.mu.Lock()
			r.inflight = nil
			r.mu.Unlock()
			close(run.done)
		}()
		started := time.Now()
		// The generation is read once, BEFORE the append: read after, a
		// reanchor during the append would stamp a sequence from before it.
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
			// Logged here, once, for [frameworkAppendError]'s reason.
			log.Warn("statelog_barrier_unconfirmed", "subject", r.subject,
				"stage", "encode", "error", err.Error())
			run.err = fmt.Errorf("statelog: encode a barrier: %w", err)
			return
		}
		// NO OP ID: a repeated id inside the dedupe window is answered
		// from the window with no raft round trip, a PubAck nothing
		// confirmed, so a duplicate here is refused below. A log with no
		// leader is waited out, admission and append alike, appended
		// again until one is heard ([ReadIndex.appendThroughElection]).
		ack, err := r.appendThroughElection(appendCtx, run, body)
		var none *jsprovision.NoLeader
		switch {
		case errors.As(err, &none):
			log.Warn("statelog_barrier_unconfirmed", "subject", r.subject,
				"stage", "election", "error", err.Error())
			run.err = err
			return
		case err != nil && ack.unadmitted != nil:
			run.err = err
			return
		case err != nil:
			run.err = frameworkAppendError(BarrierKind, r.subject, err)
			if refused := (*Unavailable)(nil); !errors.As(run.err, &refused) {
				// Logged here, once per append, for
				// [frameworkAppendError]'s reason.
				log.Warn("statelog_barrier_unconfirmed", "subject", r.subject,
					"stage", "append", "error", err.Error())
			}
			return
		}
		if ack.duplicate {
			run.err = &Unavailable{
				Reason: ReasonSkew,
				Detail: fmt.Sprintf("the broker served the barrier on %s out of "+
					"its duplicate window, so its sequence proves nothing about "+
					"the log's end — a barrier must carry no message id",
					r.subject),
			}
			return
		}
		run.seq = ack.seq
		if r.metrics != nil {
			r.metrics.Observe(metrics.StatelogBarrierDuration, time.Since(started),
				metrics.Attrs{"domain": r.domain})
			r.metrics.Add(metrics.StatelogBarrierAppends, 1,
				metrics.Attrs{"domain": r.domain})
		}
	}()
}

// barrierAck is one barrier's append as the broker answered it, or unadmitted,
// its admission's refusal: nothing was appended.
type barrierAck struct {
	seq        uint64
	duplicate  bool
	unadmitted error
}

// appendThroughElection admits and appends one barrier inside ctx, again
// while the log answers it has no leader ([jsprovision.ThroughElection]), each
// such answer noted on run for a reader that leaves before it ends. Held out
// of the reserve, and refused `log_full` at the ordinary ceiling like any
// ordinary append. On a log several members store an append nobody answers is
// SENT AGAIN, every earlier one still out and the first answer taken
// ([jsprovision.ReadBounded]): a member that has not yet seen its leader go
// swallows it, and only one sent once a new leader leads lands. A second
// barrier is harmless, writing no row; one member holds no election to wait.
func (r *ReadIndex) appendThroughElection(ctx context.Context, run *barrierRun,
	body []byte) (barrierAck, error) {

	once := func(ctx context.Context) (barrierAck, error) {
		release := func() {}
		if r.admission != nil {
			var err error
			if release, err = r.admission.Admit(ctx, appendBytes(body)); err != nil {
				// AN ANSWER, never the hedge's silence, though its
				// cause may be a reading nobody answered: the
				// admission decided, and asking it again is its own.
				return barrierAck{unadmitted: err}, nil
			}
		}
		defer release()
		seq, duplicate, err := r.log.Append(ctx, r.subject, "", nil, body)
		return barrierAck{seq: seq, duplicate: duplicate}, err
	}
	replicated := r.members() > 1
	return jsprovision.ThroughElection(ctx, r.stream, func(ctx context.Context) (barrierAck, error) {
		var (
			got barrierAck
			err error
		)
		if replicated {
			got, err = jsprovision.ReadBounded(ctx, ElectionBarrierBudget, once, nil)
		} else {
			got, err = once(ctx)
		}
		if got.unadmitted != nil {
			err = got.unadmitted
		}
		if jsprovision.Leaderless(err) {
			run.leaderless.Store(&err)
		} else {
			run.leaderless.Store(nil)
		}
		return got, err
	})
}

// frameworkAppendError is what an append of a framework record the broker
// did not store (a barrier, a trimmed record) means to whoever waits on it,
// decided by the same [classify] a write's append is.
//
// A refusal the broker NAMED is that refusal, its words the remedy on the
// detail: a full log refuses every barrier until an operator resizes it, so
// it is `log_full`, never a retryable missed quorum. What nobody answered, or
// an answer that the record may yet land ([faultUnsettled]; with no message
// id, a stream restarting under a committed entry), stays unclassified: the
// unconfirmed barrier ([ReadIndex.unconfirmed]). Its caller logs that error
// once per append, as the transport's words, and readers are told only
// [barrierRefusal]'s sentence.
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
		// A framework record is a few hundred bytes, so too large means a
		// limit refusing every record; either way the broker decided, and
		// the detail carries its words (for a limit, which and what moves it).
		return &Unavailable{
			Reason: ReasonBrokerRefused,
			Detail: fmt.Sprintf("the broker refused to store the %s record on %s: %s",
				record, subject, detail),
			Cause: err,
		}
	}
	return fmt.Errorf("statelog: append a %s record on %s: %w", record, subject, err)
}
