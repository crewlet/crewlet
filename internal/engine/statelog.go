package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/version"
)

// This node's STATE-LOG RUNTIME: which domains it runs, and the four objects
// each of them needs.
//
// # The four, and why each is separate
//
// A domain is a declaration. What makes it run is a LOG to append to, a
// CONSUMER to read it back, an APPLY LOOP that turns records into rows, and a
// PUBLISHER that decides what to append. They are four objects rather than one
// because two of them are the broker's and two are the store's, and every
// failure that matters is one of them being fine while another is not: a
// consumer that is delivering into an applier that has stopped, a publisher
// forming expectations from a checkpoint that stopped moving.
//
// # Why the consumer resumes from the ROWS
//
// The applier's checkpoint commits in the same transaction as the rows it
// covers, so it is the only durable statement of where this node is. The
// consumer's own acknowledgement floor is a second, weaker number. So the
// consumer is created at the checkpoint's own sequence — which is also the
// only way a node that has applied a million records avoids a million
// redeliveries on every restart.
//
// # And why seat admission waits on EVERY registered domain
//
// A seat whose mailbox attached before its node's tracker was established
// would answer "there is no such task" to its own tools — which is an answer
// it acts on, by filing a duplicate or abandoning work it was told to do. That
// gate is per DOMAIN and not per node: a domain whose health does not gate
// admission says so itself ([statelog.Domain.ReadinessInput]), which is how
// the vector domain's coverage number stays a number rather than an outage.

// domainHost is the broker as this node's state log needs it.
//
// DECLARED HERE, by the caller, and satisfied by the JetStream queue. It is
// the one place in the engine that names a queue backend, and it names it once
// and loudly: a state log needs a conditional append, a per-node durable
// consumer and a stream whose settings make an interior gap impossible, and
// none of those is in the EventQueue contract because nothing else needs them.
type domainHost interface {
	EnsureDomainStream(ctx context.Context, spec jetstream.DomainStream) error

	// StreamBudget is what the broker will actually let this account
	// store. A ceiling is a RESERVATION the broker refuses if it cannot
	// honour it, and the number it compares against is the account's own
	// limit rather than the disk — so a ceiling derived from free space
	// alone is refused on machines that have the space.
	StreamBudget(ctx context.Context) (limit, used int64, err error)
	DomainLog(ctx context.Context, stream string) (*jetstream.DomainLog, error)
	DomainConsumer(ctx context.Context, stream, nodeID string, after uint64) (*jetstream.DomainConsumer, error)
}

// runningDomain is one domain this node runs, with everything it took.
type runningDomain struct {
	domain    statelog.Domain
	runner    *statelog.Runner
	publisher *statelog.Publisher
	log       *jetstream.DomainLog
	consumer  *jetstream.DomainConsumer

	// createdAt is the broker's own creation instant for the stream, which
	// is what DETECTS a recreated one — the generation is the response.
	createdAt time.Time

	// evicted is this domain's own eviction gate, taken from the write
	// fence so readiness reads the row the write path reads.
	//
	// THE SAME SOURCE ON BOTH PATHS, deliberately: an evicted node whose
	// writes are dropped by every peer while its reads answer normally is
	// the split D109 (b) calls the worst shape a node can be in, and two
	// sources for one fact is how it arises. Nil for a domain with no
	// eviction gate, which answers "not evicted" rather than refusing.
	evicted func(ctx context.Context) (bool, error)

	// progress is what a snapshot cannot see — whether the applied prefix
	// is moving, and how long an undecodable record has been held. See
	// [progress] for why it lives beside the runner rather than inside
	// [statelog.Health].
	progress progress

	// reader is this domain's READ authority — the four levels, the
	// refusal ladder, the coverage probe and the barrier wait — and it is
	// what a domain's own reader answers through.
	//
	// Nil for a domain that declares no barrier encoder, which is the
	// honest state for one whose reads make no freshness claim: the
	// vectors are DERIVED and compacted, so "as of a position" is not a
	// question that has an answer about them.
	reader *statelog.Reader
}

// stateLog is this node's whole state-log runtime.
type stateLog struct {
	domains map[string]*runningDomain

	// order is the register's own order, so every surface that walks the
	// domains renders them the same way. A map's iteration order would
	// make one screen's rows move between refreshes.
	order []string

	nodeID string
	db     *store.DB
	fleet  coord.Fleet

	// metrics is the process's one recorder, threaded down so the apply
	// loop's instruments are observed rather than merely declared.
	metrics *metrics.Recorder

	// skills is this build's tool-skill parser and nudge, threaded down
	// because the PAGES APPLIER is what notices a skill page arriving or
	// leaving: natively there is no page webhook and the change feed
	// deliberately drops those changes, so the apply is the only thing
	// that sees both halves. Nil answers "not a skill" and nudges nobody,
	// which is a build with no skill parser wired.
	skills      pages.SkillDetector
	nudgeSkills func()

	// ceilings is the byte ceiling each domain's stream is CREATED with,
	// from Tier A. It overrides the domain's own default, which is the
	// value a domain declares in the absence of an operator — and the
	// override is only ever applied at creation: a stream's configuration
	// has one writer and a booting node is not it, so re-applying it would
	// let restart order decide a shared limit and let a late node lower a
	// ceiling an emergency grant had just raised.
	ceilings map[string]int64

	// run is the context every apply loop runs under, and stop is what
	// ends them. HELD rather than re-derived, for the reason the native
	// runtime states: a goroutine started under the CALLER's context is
	// one stop can never end, and the wait then blocks for ever.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// startStateLog brings up every domain this node runs.
//
// PROVISION, RESUME, RUN — in that order, per domain, and the order is the
// contract. A consumer opened before the stream exists creates nothing to read
// from; a runner started before its consumer resumed from the rows reads from
// wherever the broker's default happened to put it.
func (e *Engine) startStateLog(ctx context.Context, boot *config.Bootstrap,
	nodeID string, epoch map[string]any) (*stateLog, error) {

	host, ok := e.backends.Queue.(domainHost)
	if !ok {
		return nil, fmt.Errorf("engine: this node's broker cannot host a state "+
			"log (%T) — the native tracker needs a conditional append and a "+
			"per-node durable consumer, and neither is in the event-queue "+
			"contract because nothing else needs them", e.backends.Queue)
	}
	if nodeID == "" {
		return nil, fmt.Errorf("engine: a state-log node has no id — it names " +
			"this node's own consumer, stamps every record it writes and is " +
			"what the eviction gate compares against")
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &stateLog{
		domains: map[string]*runningDomain{},
		nodeID:  nodeID, db: e.backends.Store, fleet: e.backends.Fleet,
		metrics: e.metrics,
		skills:  skillDetector{}, nudgeSkills: e.nudgeSkills,
		ceilings: ceilingsFor(ctx, host, boot),
		run:      runCtx, stop: cancel,
	}
	// PROVISION EVERY LOG FIRST, and only then decide whether this node
	// can replay from where it is.
	//
	// The join below reads each stream's FIRST SURVIVING SEQUENCE, which
	// is a fact about a stream that exists. Interleaving provision with
	// the decision — the obvious per-domain loop — would ask the question
	// of the first domain before the second's stream had been created,
	// and a stream created a moment ago reports a first sequence of 1,
	// which reads as "nothing was trimmed" for a log the fleet has been
	// writing to for months.
	logs := map[string]*jetstream.DomainLog{}
	for _, domain := range registeredDomains() {
		appendTo, err := s.provision(ctx, host, domain)
		if err != nil {
			s.Stop()
			return nil, err
		}
		logs[domain.Name()] = appendTo
	}

	// ADOPT BEFORE ANY APPLIER RUNS, because a join REPLACES the
	// replicated database file — and it can only do that while nothing
	// holds a transaction open on it. An applier started first would be
	// mid-batch when the rename landed, writing rows into an inode with
	// no name and reporting a checkpoint nobody will ever read.
	if err := e.joinIfBehind(ctx, s, logs); err != nil {
		s.Stop()
		return nil, err
	}

	for _, domain := range registeredDomains() {
		running, err := s.start(ctx, host, domain, logs[domain.Name()], epoch)
		if err != nil {
			// EVERY DOMAIN OR NONE. A node running half its register
			// serves rows derived from one log while another's records
			// pile up unapplied, and nothing above it can tell that
			// from a node that is merely behind.
			s.Stop()
			return nil, err
		}
		s.domains[domain.Name()] = running
		s.order = append(s.order, domain.Name())
	}
	// AFTER EVERY DOMAIN IS RUNNING. The heartbeat reports each domain's
	// position and the snapshot gate reads each one's health, so both need
	// the loops they describe to exist.
	s.startPositionHeartbeat()
	e.startSnapshots(ctx, boot, s)
	log.InfoContext(ctx, "statelog_started", "node", nodeID, "domains", s.order)
	return s, nil
}

// Stop ends every apply loop this node started and waits for them.
func (s *stateLog) Stop() {
	if s == nil {
		return
	}
	s.stop()
	s.done.Wait()
}

// registeredDomains is every domain this build runs, in a FIXED order.
//
// The list is here rather than in a registry each domain writes itself into,
// because the order is load-bearing for the operator surfaces and an
// init-order registration is exactly the thing nobody can read off the source.
func registeredDomains() []statelog.Domain {
	return []statelog.Domain{tracker.Domain{}, search.Domain{}, pages.Domain{}}
}

// provision creates one domain's stream if it is not there and opens it.
//
// SEPARATED FROM [stateLog.start] because the join between them needs every
// stream to exist before it reads any of their bounds — see the comment at
// the call.
func (s *stateLog) provision(ctx context.Context, host domainHost,
	domain statelog.Domain) (*jetstream.DomainLog, error) {

	spec := domain.Stream()
	if err := host.EnsureDomainStream(ctx, jetstream.DomainStream{
		Name:          spec.Name,
		Subjects:      spec.Subjects,
		MaxBytes:      s.ceilingFor(domain, spec),
		MaxPerSubject: spec.MaxPerSubject,
		MaxAge:        spec.MaxAge,
		Duplicates:    spec.Duplicates,
	}); err != nil {
		return nil, fmt.Errorf("engine: provision %s's log: %w", domain.Name(), err)
	}
	appendTo, err := host.DomainLog(ctx, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s's log: %w", domain.Name(), err)
	}
	return appendTo, nil
}

// start brings up one domain on the log [stateLog.provision] opened.
func (s *stateLog) start(ctx context.Context, host domainHost, domain statelog.Domain,
	appendTo *jetstream.DomainLog, epoch map[string]any) (*runningDomain, error) {

	spec := domain.Stream()

	// THE ROWS SAY WHERE THIS NODE IS. The checkpoint commits with them,
	// so it is the only durable statement of it — and resuming a consumer
	// anywhere else is either a hole (at the head) or a million
	// redeliveries (at the beginning).
	at, created, _, err := statelog.CursorFor(ctx, s.db.Replicated(), spec.Name)
	if err != nil {
		return nil, err
	}
	consumer, err := host.DomainConsumer(ctx, spec.Name, s.nodeID, at.Seq)
	if err != nil {
		return nil, fmt.Errorf("engine: open %s's consumer: %w", domain.Name(), err)
	}

	applier, err := s.applierFor(domain)
	if err != nil {
		return nil, err
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: domain, Applier: applier, Fetch: consumer,
		// THE REPLICATED HANDLE, not the node one. The applier PINS a
		// connection for the life of its loop, and the pins live on the
		// replicated estate's pool — that is where every applier writes,
		// and it is the pool [store.Options.PinnedWriters] grows. Handed
		// the node handle it refuses at its first round with "declared 0
		// pinned writers", which stops the loop before it applies a
		// single record: the domain's rows never move, and the only
		// symptom is a node that stays behind for ever.
		//
		// The seams beside it take the NODE handle and reach the peer
		// themselves ([tracker.NewGates] also reads this node's own
		// adoption row, which is deliberately not replicated), so the
		// asymmetry here is real rather than an oversight.
		DB: s.db.Replicated(), Generation: at.Generation, StreamCreatedAt: created,
		Epoch: epoch, Metrics: s.metrics,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's applier: %w", domain.Name(), err)
	}

	publisher, evicted, err := s.publisherFor(domain, appendTo, runner)
	if err != nil {
		return nil, err
	}

	running := &runningDomain{
		domain: domain, runner: runner, publisher: publisher,
		log: appendTo, consumer: consumer, createdAt: created,
		evicted: evicted,
	}
	// AFTER the struct exists, because the health closure the reader
	// holds reads through it — a reader built first would capture a
	// half-assembled domain and report its progress as never observed.
	if running.reader, err = s.readerFor(domain, appendTo, runner, running); err != nil {
		return nil, err
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		if err := runner.Run(s.run); err != nil {
			// A STOPPED APPLIER IS NOT A CRASHED NODE. Its rows are
			// frozen and every read of them says so through the
			// coverage it reports, so what this costs is that the
			// node stops taking seats — which is what the readiness
			// gate below already does with it.
			log.ErrorContext(s.run, "statelog_applier_stopped",
				"domain", domain.Name(), "error", err.Error(),
				"detail", "this node stops claiming seats for that domain and "+
					"its rows are going stale; a build that can read what it "+
					"could not is what resumes it")
		}
	}()
	return running, nil
}

// publisherFor builds one domain's write authority.
//
// THE FENCE AND THE GATES ARE THE DOMAIN'S, and that is the difference the two
// domains here make visible: the tracker's fence reads its own eviction rows
// and its gates read a permanent deletion marker, while the vector domain
// declares both OPEN — a derived value the next duty tick recomputes has no
// history for a fence to protect, and what actually stops an evicted node
// writing there is the fleet singleton's lease.
// It also hands back the domain's EVICTION READER, because the fence it is
// built from is the one place that knows how to ask — and readiness must ask
// the same question the write path asks, from the same row. Two readers of
// one tombstone is how a node comes to refuse its writes and answer its reads.
func (s *stateLog) publisherFor(domain statelog.Domain, appendTo *jetstream.DomainLog,
	runner *statelog.Runner) (*statelog.Publisher, func(context.Context) (bool, error), error) {

	deps := statelog.Deps{
		Domain: domain, Log: appendTo, Waiter: runner, NodeID: s.nodeID,
		// READ FRESH ON EVERY PUBLISH rather than captured: a reanchor
		// moves the generation under a running process, and a publisher
		// stamping the old one would write records every applier reads
		// as safely stale.
		Generation: func() uint32 { return runner.Committed().Generation },
	}
	var evicted func(context.Context) (bool, error)
	switch domain.Name() {
	case tracker.Domain{}.Name():
		rows, err := tracker.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		fence := tracker.NewFence(s.db, s.nodeID)
		fence.Cursor = runner.Committed
		fence.Floor = s.trimFloor(domain.Name())
		deps.Rows, deps.Fence, deps.Gates = rows, fence, tracker.NewGates(s.db)
		evicted = fence.Evicted
	case search.Domain{}.Name():
		rows, err := search.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		// NO EVICTION READER, and that is the domain rather than an
		// omission: the vectors are DERIVED and compacted, so there is no
		// tombstone table to read and nothing an evicted node could serve
		// that a re-embed would not replace.
		deps.Rows, deps.Fence, deps.Gates = rows, search.NewFence(), search.NewGates()
	case pages.Domain{}.Name():
		rows, err := pages.NewRows(s.db)
		if err != nil {
			return nil, nil, fmt.Errorf("engine: build %s's read seam: %w", domain.Name(), err)
		}
		fence := pages.NewFence(s.db, s.nodeID)
		fence.Cursor = runner.Committed
		fence.Floor = s.trimFloor(domain.Name())
		deps.Rows, deps.Fence, deps.Gates = rows, fence, pages.NewGates(s.db)
		evicted = fence.Evicted
	default:
		return nil, nil, fmt.Errorf("engine: domain %q is registered and has no "+
			"write authority, so nothing could ever append to its log",
			domain.Name())
	}
	publisher, err := statelog.NewPublisher(deps)
	if err != nil {
		return nil, nil, fmt.Errorf("engine: build %s's write authority: %w", domain.Name(), err)
	}
	return publisher, evicted, nil
}

// readerFor builds a domain's READ authority: the four levels, the refusal
// ladder, the coverage probe and the quorum-committed barrier a linearizable
// read waits through.
//
// # Why the barrier encoder is a switch and not a method on Domain
//
// A barrier is the framework's append and the DOMAIN's record — the read
// index decides when one goes out and what its acknowledgement proves, and
// the domain decides what a record on its log looks like. A domain that has
// no barrier encoder gets no read index and therefore no `linearizable`,
// which is the correct answer for one whose reads make no freshness claim
// rather than a gap: the vectors are derived and compacted, so "as of a
// position" is not a question about them.
//
// Until this existed [statelog.NewReader] and [statelog.NewReadIndex] were
// constructed only by their own tests. Every domain reader read its rows
// straight out of the replicated estate and ECHOED the level back in the
// answer — `tracker.Reader.Tasks` assigning `answer.Level = q.Level`, and
// `Task` taking a level argument whose only use in the file was
// `out.Level = level`. So a seat tool asking for `session` got whatever this
// node happened to hold, a dashboard's `max_lag_seconds` bounded nothing, and
// `linearizable` appended no barrier at all.
func (s *stateLog) readerFor(domain statelog.Domain, appendTo *jetstream.DomainLog,
	runner *statelog.Runner, running *runningDomain) (*statelog.Reader, error) {

	var encode func(statelog.Envelope) ([]byte, error)
	switch domain.Name() {
	case tracker.Domain{}.Name():
		encode = tracker.EncodeBarrier
	case pages.Domain{}.Name():
		encode = pages.EncodeBarrier
	}

	deps := statelog.ReaderDeps{
		Domain: domain,
		DB:     s.db.Replicated(),
		Waiter: runner,
		// READ FRESH ON EVERY READ, because every one of its terms can
		// change between two of them — and because a captured value
		// would freeze the very refusals this seam exists to deliver.
		Health: func() statelog.Health {
			h, err := s.health(context.Background(), running)
			if err != nil {
				// AN UNREADABLE TERM REFUSES rather than serving. The
				// health read reaches the broker and coordination, and
				// a read certified against a health nobody could
				// establish is certified against nothing.
				return statelog.Health{Err: err.Error()}
			}
			return h
		},
		Drain:   runner.Drain,
		Metrics: s.metrics,
	}
	if encode != nil {
		index, err := statelog.NewReadIndex(domain, appendTo, encode,
			func() uint32 { return runner.Committed().Generation }, s.metrics)
		if err != nil {
			return nil, fmt.Errorf("engine: build %s's read index: %w", domain.Name(), err)
		}
		deps.Index = index
	}
	reader, err := statelog.NewReader(deps)
	if err != nil {
		return nil, fmt.Errorf("engine: build %s's read authority: %w", domain.Name(), err)
	}
	return reader, nil
}

// trimFloor is the published floor for one domain, as the write fence reads
// it.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is what the fence does with
// the error: this is the one check where failing open is a lost update rather
// than a duplicate.
func (s *stateLog) trimFloor(domain string) func(context.Context) (uint64, error) {
	return func(ctx context.Context) (uint64, error) {
		rows, err := s.fleet.Positions(ctx)
		if err != nil {
			return 0, fmt.Errorf("engine: read the fleet's positions: %w", err)
		}
		// THE LOWEST COUNTED POSITION IS THE FLOOR THIS NODE MAY ASSUME.
		// It is a conservative reading of the published floor rather
		// than the floor itself: the trim's own decision is six terms
		// wide and every one of them can only move the point DOWN, so a
		// writer comparing against this can be too cautious and never
		// too bold.
		var floor uint64
		for i, row := range rows {
			at, ok := row.Domains[domain]
			if !ok {
				continue
			}
			if i == 0 || at.Seq < floor {
				floor = at.Seq
			}
		}
		return floor, nil
	}
}

// Domain answers one running domain by name, or nil.
func (s *stateLog) Domain(name string) *runningDomain {
	if s == nil {
		return nil
	}
	return s.domains[name]
}

// Established reports whether every domain whose health gates seat admission
// has one.
//
// PER DOMAIN, from the domain's own declaration. A compacted domain's gap is a
// coverage number rather than a fault, so shedding a company's seats for one
// would be the outage the number exists to avoid — and stating it on the
// domain is what keeps that a property of the domain rather than a list
// somewhere of the ones to skip.
func (s *stateLog) Established(ctx context.Context, strict bool) (bool, statelog.ReadRefusal) {
	if s == nil {
		return true, ""
	}
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ReadinessInput() {
			continue
		}
		health, err := s.health(ctx, running)
		if err != nil {
			return false, statelog.RefuseBrokerUnreachable
		}
		if ok, refusal := health.Established(strict); !ok {
			return false, refusal
		}
	}
	return true, ""
}

// Healthy reports whether every registered domain permits this node to KEEP
// the seats it holds, and names the first that does not.
//
// # This is a different question from Established, and the difference is what
// # separates withholding work from giving it back
//
// [stateLog.Established] gates ADMISSION: a node mid-hydration keeps what it
// holds and claims nothing new, which is right, because its rows are merely
// incomplete and its seats' work would only wait. This one gates HOLDING, and
// the states it fires on are ones where the seats' work would be WRONG:
//
//   - the applier has STOPPED, so its rows are frozen at the record that
//     halted it and every later object is missing its consequences;
//   - the node is EVICTED, so its peers drop every record it publishes and
//     its rows have already stopped being the fleet's;
//   - the node is BELOW THE TRIM FLOOR, so records it never applied have been
//     deleted and its rows have a hole nothing will fill;
//   - it has held a record it CANNOT DECODE past [statelog.DeferralGrace],
//     which is D122: under the grace nothing changes, because that covers
//     every rolling upgrade; past it the honest reading is "this node cannot
//     run this company's records" rather than "this node is briefly behind".
//
// Until this had a caller the `deferred_old` alarm told an operator "its seats
// move at 30m0s" and its remedy said "its seats have already moved", and
// neither was true — [seat.Host] sheds only on a lost lease, a drain and a
// rebalance, and its own log line says a node that is not ready "keeps what it
// holds". The alarm was reporting a mitigation the engine did not perform.
//
// A DOMAIN WHOSE HEALTH CANNOT BE READ DOES NOT SHED. An unreachable broker is
// the outage during which a company most needs its seats to keep running, and
// tearing them down on an unread number is the failure mode `unknown` exists
// throughout this package to prevent.
func (s *stateLog) Healthy(ctx context.Context) (bool, string) {
	if s == nil {
		return true, ""
	}
	now := time.Now()
	for _, name := range s.order {
		running := s.domains[name]
		if !running.domain.ReadinessInput() {
			continue
		}
		health, err := s.health(ctx, running)
		if err != nil {
			continue
		}
		if !health.Healthy(now, running.progress.deferredSinceValue()) {
			return false, name
		}
	}
	return true, ""
}

// health assembles one domain's readiness from the four places it lives: this
// node's own checkpoint, its consumer's backlog, the stream's own bounds, and
// the fleet's published floor.
func (s *stateLog) health(ctx context.Context, running *runningDomain) (statelog.Health, error) {
	now := time.Now()
	at := running.runner.Committed()
	health := statelog.Health{Position: at, AppliedThrough: at.Seq}
	// THE APPLIER'S OWN STOP, FIRST. A halted applier's rows are frozen at
	// the record that stopped it, so every later record's consequences are
	// missing and no read over them can be certified — which is what
	// [statelog.Health.Refusal]'s `stalled` arm exists to say, and it could
	// never fire while nothing assigned this field.
	//
	// A STOP THIS PROCESS ASKED FOR IS NOT A FAULT. Every applier returns
	// its run context's error on shutdown, and reading that as a halt makes
	// a node declare itself broken on the way out — refusing the reads it
	// is still serving and shedding seats a drain is already handing back
	// in order. The distinction is the cause, not the state: a cancelled
	// context is this node stopping, and anything else is the applier
	// stopping underneath it.
	if err := running.runner.Stopped(); err != nil && !errors.Is(err, context.Canceled) {
		health.Err = err.Error()
	}
	health.Stalled = running.progress.stalled(now)
	if deferral, ok := running.runner.Deferred(); ok {
		health.Deferred = 1
		health.DeferredFrom = deferral.Position.Seq
		if deferral.Position.Seq > 0 {
			health.AppliedThrough = deferral.Position.Seq - 1
		}
	}
	// THE EVICTION ROW, from the write fence's own reader. An evicted node
	// must stop answering as well as stop writing: its peers drop every
	// record it publishes, so its rows stop advancing while its position
	// keeps being published, and a read served from them is served from a
	// copy the fleet has already abandoned.
	if running.evicted != nil {
		evicted, err := running.evicted(ctx)
		if err != nil {
			return health, err
		}
		health.Evicted = evicted
	}
	first, end, err := running.log.Bounds(ctx)
	if err != nil {
		// NEITHER HALF OF THE INEQUALITY CAN BE EVALUATED, and both
		// failures are silent: a consumer above the stream's last
		// sequence waits for a record that never arrives, and one below
		// its first is clamped upward with no error and reports itself
		// caught up over a hole.
		return health, err
	}
	health.FirstSeq = &first
	lag := uint64(0)
	if end > at.Seq {
		lag = end - at.Seq
	}
	health.Lag = &lag
	health.CaughtUp = lag == 0
	floor, err := s.trimFloor(running.domain.Name())(ctx)
	if err != nil {
		return health, err
	}
	health.TrimFloor = &floor
	// THE THREE-VALUED FLOOR, stamped with the instant it was read. Both
	// halves are load-bearing and the field carries them together for the
	// reason its own doc gives: a state whose age nobody carries ages
	// silently into an assertion, and this one decides whether a node is
	// serving rows the fleet has already trimmed out from under it.
	//
	// FirstSeq may only RAISE it — it arrives on the same stream info from
	// a possibly-non-authoritative member, and a stale one is LOWER than
	// the truth, so trusting it downward is how a node below the real
	// floor keeps serving.
	health.Floor = statelog.Floor{State: statelog.FloorOK, ReadAt: now}
	below := at.Seq < floor
	if health.FirstSeq != nil && at.Seq < *health.FirstSeq {
		below = true
	}
	if below {
		health.Floor.State = statelog.FloorBelow
	}
	return health, nil
}

// applierFor is the state machine each domain declares.
//
// A SWITCH RATHER THAN A METHOD on the domain, because an applier is not part
// of the declaration: the declaration is what a snapshot, a claim and a sweep
// read, and it must be answerable by a build that cannot construct the applier
// at all. What this costs is that a new domain fails HERE, at boot, naming
// itself — rather than being registered with no state machine and applying
// nothing.
func (s *stateLog) applierFor(domain statelog.Domain) (statelog.Applier, error) {
	switch domain.Name() {
	case tracker.Domain{}.Name():
		return tracker.NewApplier(s.nodeID), nil
	case search.Domain{}.Name():
		return search.NewApplier(), nil
	case pages.Domain{}.Name():
		// THE PARSER AND THE NUDGE COME FROM HERE, because the apply is
		// what notices a tool-skill page arriving or leaving and there is
		// no other delivery to hang the resync off. The nudge is safe to
		// take before the native runtime exists: it is a non-blocking
		// send that returns when there is nothing to send to.
		return pages.NewApplier(s.nodeID, s.skills, s.nudgeSkills), nil
	}
	return nil, fmt.Errorf("engine: domain %q is registered and has no applier, "+
		"so its records would be consumed and produce no rows on this node",
		domain.Name())
}

// Epoch is the per-epoch configuration every registered domain's applier
// reads, as a plain map.
//
// # Why an applier reads a MAP rather than the config
//
// An applier is a pure function of (rows, record, options), and the purity is
// the identity claim: two nodes applying one record must produce the same
// rows, so anything an applier could read from the world instead of from its
// arguments is a value the two can differ on. The framework fills this once
// per apply-loop generation and hands it in — which is also why the one table
// whose contents depend on it is classed as DIVERGING rather than compared.
//
// Keys are qualified by the domain that reads them, so a second domain adding
// one cannot collide with the first's.
func (c *Company) Epoch() map[string]any {
	if c == nil || c.Config == nil {
		return nil
	}
	epoch := map[string]any{}
	if native := c.Config.Tracker.Native; native != nil && native.InboxRetentionDays > 0 {
		epoch["tracker.native.inbox_retention_days"] = native.InboxRetentionDays
	}
	return epoch
}

// ceilingsFor is each domain's stream ceiling, from Tier A.
//
// # Why the mutation log's is DERIVED and the vector changelog's is not
//
// A fixed default for the mutation log is wrong in both directions: the same
// number is five years of history at the modelled write rate and one boot on a
// small disk. So an unset value takes a quarter of the stream volume's free
// space, clamped — the same volume holds this node's databases and its
// snapshots, and a log allowed to fill the disk takes down the store it is
// applied TO.
//
// The vector changelog's default is fixed because its peak is not a function
// of the disk: the stream keeps one message per source and bounds their age, so
// a week's minting is small — but changing the embedding model rewrites every
// source in a few hours, and for the following week every source's current
// message is inside the window. The default is sized for that operation rather
// than for the steady state, because sizing it from the steady state would
// refuse the one operation it exists to survive.
func ceilingsFor(ctx context.Context, host domainHost, boot *config.Bootstrap) map[string]int64 {
	if boot == nil {
		return nil
	}
	free, err := freeSpace(boot.Store.Path)
	if err != nil {
		// LOGGED AND CARRIED with a zero, which the derivation clamps up
		// to its floor. A node that cannot measure its own disk still has
		// to boot, and the floor is a value that fits on any volume this
		// engine runs on.
		log.WarnContext(ctx, "statelog_free_space_unmeasured",
			"path", boot.Store.Path, "error", err.Error(),
			"detail", "the mutation log's derived ceiling falls back to its "+
				"floor; set stream.tracker_log_max_bytes to choose one")
	}
	ceiling, derived := boot.Stream.LogMaxBytes(free)
	vectors, vectorsCapped := boot.Stream.VectorsMaxBytes(free)
	out := map[string]int64{
		tracker.Domain{}.Name(): ceiling,
		search.Domain{}.Name():  vectors,
	}

	// AND THEN THE BROKER'S OWN ANSWER, which is what a reservation is
	// actually compared against. Every derived ceiling is scaled down to
	// fit inside the account's remaining budget, SHARED between the
	// domains — deriving each from the same free space independently
	// double-counts, and the failure is a node that will not boot with
	// nothing naming the number it exceeded.
	//
	// An operator's EXPLICIT ceiling is not scaled. They named a limit for
	// a broker they can see, and silently lowering it would be the engine
	// deciding a limit an emergency grant had just raised — a refused
	// boot naming the field is the honest answer there.
	limit, used, err := host.StreamBudget(ctx)
	available := max(limit-used, 0)
	if err != nil || limit < 0 {
		// UNLIMITED IS NOT UNBOUNDED. An account with no configured
		// limit reports -1, and the broker still refuses a reservation
		// the DISK cannot back — so the disk is what bounds it, which is
		// the same number the derivation started from and the same
		// share applies. An unreadable limit is carried the same way,
		// for the reason the disk measurement is: a derived value is a
		// default, and a node that cannot read one still boots.
		available = free
	}
	var claimed int64
	for _, value := range out {
		claimed += value
	}
	// A SHARE RATHER THAN ALL OF IT: the same broker holds every mailbox,
	// every coordination bucket and the snapshots a join reads, and a log
	// allowed to reserve the whole account starves the estate it is part of.
	budget := int64(float64(available) * StreamBudgetShare)
	if claimed > budget && claimed > 0 {
		for name, value := range out {
			if explicit(boot, name) {
				continue
			}
			out[name] = max(value*budget/claimed, MinDomainCeiling)
		}
	}
	log.InfoContext(ctx, "statelog_ceilings",
		"free", free, "tracker", out[tracker.Domain{}.Name()],
		"tracker_derived", derived, "vectors", out[search.Domain{}.Name()],
		"vectors_capped", vectorsCapped,
		"broker_limit", limit, "broker_used", used, "budget", budget)
	return out
}

// StreamBudgetShare is how much of the broker's remaining storage the state
// logs' derived ceilings may reserve between them.
//
// HALF. The same broker holds every seat's mailbox, every coordination bucket
// — the leases, the ledgers, the counters, the company's sealed credentials —
// and the snapshot a joining node reads. A log allowed to reserve the whole
// account starves the estate it is part of, and the failure is not a full log:
// it is a company that cannot claim a seat.
const StreamBudgetShare = 0.5

// MinDomainCeiling is the floor a scaled-down ceiling never goes below.
//
// A gibibyte, which is the same floor Tier A's own validation enforces on an
// explicit value: below it a log is not a log, it is a window that refuses
// appends within a week of a company starting work.
const MinDomainCeiling int64 = 1 << 30

// explicit reports whether the operator named this domain's ceiling.
func explicit(boot *config.Bootstrap, domain string) bool {
	switch domain {
	case tracker.Domain{}.Name():
		return boot.Stream.TrackerLogMaxBytes > 0
	case search.Domain{}.Name():
		return boot.Stream.TrackerVectorsMaxBytes > 0
	}
	return false
}

// ceilingFor is one domain's ceiling, falling back to what the domain itself
// declares.
func (s *stateLog) ceilingFor(domain statelog.Domain, spec statelog.StreamSpec) int64 {
	if ceiling, held := s.ceilings[domain.Name()]; held && ceiling > 0 {
		return ceiling
	}
	return spec.MaxBytes
}

// freeSpace is what an unprivileged process may actually use on the volume
// holding path.
//
// Bavail rather than Bfree: the reserve only root can reach is not space this
// engine has, and counting it would derive a ceiling the disk cannot honour.
func freeSpace(path string) (int64, error) {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		dir = "."
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(dir, &fs); err != nil {
		return 0, fmt.Errorf("engine: measure the free space on %s: %w", dir, err)
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}

// joinIfBehind adopts a peer's snapshot when this node cannot replay its way
// back, and does nothing at all when it can.
//
// # The question, and why it is asked here
//
// A node's checkpoint is a sequence on each domain's log. It can replay from
// there as long as the log still HOLDS that sequence — and it may not: the
// trim deletes records every node has applied, and a node that was down long
// enough, or that has never run at all, wakes up below the floor. There is
// nothing on the log for it to read, and no amount of waiting produces one.
//
// So the check is per domain: is this node's checkpoint at or above the first
// sequence the stream still holds? A node below it on ANY domain joins, and
// the join is wholesale — a snapshot names every registered domain or a
// recipient refuses it, because a domain the artefact does not name is one
// this node would believe it was caught up on.
//
// # Why a fresh node does not join
//
// A stream nobody has written to reports a first sequence of 0 and a node that
// has applied nothing has a checkpoint of 0, so a brand-new company's first
// node is at the floor rather than below it. That is the common case and it
// must not pay a fleet round trip: [statelog.OfferWindow] is five seconds, and
// a company that boots in five seconds of silence on every start is one whose
// operator learns to distrust the boot.
//
// # And why no offer is not a failure
//
// A single-node company has nobody to donate, and so does a fleet where every
// peer is as far behind as this node. Refusing to boot then would take a
// company that has merely lost history and make it a company that cannot
// start. The node comes up on what it has, its coverage says what it cannot
// account for, and the domains whose health gates seat admission keep it from
// claiming work it cannot answer for — which is the mechanism that already
// exists for exactly this state.
func (e *Engine) joinIfBehind(ctx context.Context, s *stateLog,
	logs map[string]*jetstream.DomainLog) error {

	conn, ok := e.backends.Queue.(interface{ Conn() *nats.Conn })
	if !ok || conn.Conn() == nil {
		// A JOIN NEEDS THE BROKER'S OWN CONNECTION — a snapshot is
		// megabytes over request/reply rather than anything the queue
		// contract carries. A backend with none is the memory twin, in
		// a test, with no peer to donate anyway.
		return nil
	}

	behind, need, generations, err := s.replayable(ctx, logs)
	if err != nil {
		return err
	}
	if len(behind) == 0 {
		return nil
	}
	log.WarnContext(ctx, "statelog_below_the_floor",
		"node", s.nodeID, "domains", behind,
		"detail", "this node's checkpoint is below what the log still holds, so "+
			"it cannot replay the records it is missing; it will ask the fleet "+
			"for a snapshot")

	startedAt := time.Now().UTC()
	adopter, err := statelog.NewAdopter(statelog.AdoptDeps{
		Domains:  s.registered(),
		LivePath: e.backends.Store.ReplicatedPath(),
		NodeID:   s.nodeID,
		Conn:     conn.Conn(),
		Need: func(context.Context) (map[string]uint64, map[string]uint32, error) {
			return need, generations, nil
		},
		StillUsable: func(ctx context.Context, m statelog.Manifest) error {
			return s.stillUsable(ctx, logs, m)
		},
		Hold:   s.holdTail,
		Close:  func(context.Context) error { return e.backends.Store.CloseReplicated() },
		Reopen: e.backends.Store.ReopenReplicated,
		Record: func(ctx context.Context, donor string, m statelog.Manifest,
			phase statelog.AdoptionPhase) error {

			return statelog.RecordAdoption(ctx, e.backends.Store, startedAt,
				donor, m, phase)
		},
		Logger: log,
	})
	if err != nil {
		return err
	}
	manifest, err := adopter.Join(ctx)
	switch {
	case errors.Is(err, statelog.ErrNoOffer):
		// NOT A FAILURE — see the doc comment. The node comes up on what
		// it has and its coverage says what it cannot account for.
		log.WarnContext(ctx, "statelog_no_snapshot_offered",
			"node", s.nodeID, "domains", behind,
			"detail", "no peer could donate a usable snapshot, so this node "+
				"comes up on the history it has; reads report the coverage "+
				"they could not account for, and the domains that gate seat "+
				"admission keep it from claiming work it cannot answer for")
		return nil
	case err != nil:
		return fmt.Errorf("engine: this node is below the log's floor on %v and "+
			"the join failed: %w", behind, err)
	}
	log.InfoContext(ctx, "statelog_adopted",
		"node", s.nodeID, "donor", manifest.NodeID, "sha256", manifest.SHA256,
		"taken_at", manifest.TakenAt, "bytes", manifest.Bytes)
	return nil
}

// replayable answers, for every registered domain, whether this node can reach
// the log from where its rows say it is — and what it would need if it cannot.
//
// NEED IS ONE BELOW THE FLOOR, per domain, which is the artefact's own
// acceptance test: a snapshot at that position leaves the node with the whole
// surviving log ahead of it and nothing missing in between. The generation
// travels beside it because a bare sequence from before a reanchor names a
// dead number space and would compare as if it were current.
func (s *stateLog) replayable(ctx context.Context, logs map[string]*jetstream.DomainLog) (
	behind []string, need map[string]uint64, generations map[string]uint32, err error) {

	need = map[string]uint64{}
	generations = map[string]uint32{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		appendTo, held := logs[name]
		if !held {
			return nil, nil, nil, fmt.Errorf("engine: %s's log was not "+
				"provisioned before the join asked what it holds", name)
		}
		first, _, err := appendTo.Bounds(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		floor, err := s.trimFloor(name)(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		at, _, _, err := statelog.CursorFor(ctx, s.db.Replicated(), domain.Stream().Name)
		if err != nil {
			return nil, nil, nil, err
		}
		generations[name] = at.Generation

		// WHAT AN ARTEFACT MUST COVER is one below the HIGHER of the
		// two: the stream's first surviving sequence is what is gone
		// already, and the published floor is what the trim has been
		// told it may delete and has not necessarily reached. A node
		// that accepted an artefact chosen from `first` alone would
		// install one the trim was about to pass.
		if usable := max(first, floor); usable > 0 {
			need[name] = usable - 1
		}

		// WHETHER THIS NODE IS BEHIND is a different question, and it
		// is asked of `first` ALONE. The floor says what MAY be
		// deleted; only the stream says what IS. Testing against the
		// floor would make a single-node company adopt from itself:
		// the floor is a minimum over the fleet's published positions,
		// which on one node IS that node's own — so every moment its
		// published row ran ahead of its checkpoint would read as a
		// node below the floor, and the boot would spend the offer
		// window asking a fleet of one for a snapshot of itself.
		//
		// `first` is the first sequence the stream still HOLDS, and an
		// empty stream reports one past its last — so a fresh node at
		// checkpoint 0 against `first == 1` is a node with the whole
		// log ahead of it rather than one that has missed anything.
		if first > at.Seq+1 {
			behind = append(behind, name)
		}
	}
	return behind, need, generations, nil
}

// stillUsable re-checks, after the transfer, that every position the artefact
// carries is still above the floor.
//
// THE HOLD IS THE MECHANISM AND THIS IS THE BELT. A fleet that trimmed past
// the artefact anyway — a peer on a build that does not honour holds, an
// operator forcing one — is one this node must not follow into a hole, and the
// only moment it can still refuse is before the install.
func (s *stateLog) stillUsable(ctx context.Context, logs map[string]*jetstream.DomainLog,
	m statelog.Manifest) error {

	for _, domain := range registeredDomains() {
		name := domain.Name()
		at, named := m.Domains[name]
		if !named {
			return fmt.Errorf("the artefact names no position for %s", name)
		}
		first, _, err := logs[name].Bounds(ctx)
		if err != nil {
			return err
		}
		floor, err := s.trimFloor(name)(ctx)
		if err != nil {
			return err
		}
		// THE FLOOR IS INCLUDED HERE, unlike the behind test above, and
		// the asymmetry is the point: this asks whether the artefact is
		// still safe to install, and a position the trim has been told
		// it may delete is one that will be gone by the time this node
		// replays from it.
		if usable := max(first, floor); usable > 0 && at.Seq+1 < usable {
			return fmt.Errorf("%s's log now starts at %d and the artefact is "+
				"at %d, so adopting it would leave a hole", name, usable, at.Seq)
		}
	}
	return nil
}

// holdTail pins the replay tail for the whole transfer and returns the
// release.
//
// ONE HOLD FOR THE WHOLE JOIN, keyed on this node and this purpose, so a
// second attempt replaces the first rather than accumulating pins nobody
// releases. The release is called on every path out of the join — including
// the failures — because a hold nobody released stops the trim for its whole
// stale window, which is the one way a repair makes the fleet worse.
func (s *stateLog) holdTail(ctx context.Context, at map[string]uint64) (func(), error) {
	owner := s.nodeID + ":join"
	// KEYED BY STREAM, which is what [coord.TrimHold.Streams] holds: a
	// hold is a pin on a LOG, and the backup's own hold — taken from
	// `statelog_cursor`, which is keyed on the stream — has to land in the
	// same key space or the trim reads one of the two as pinning nothing.
	streams := make(map[string]coord.Position, len(at))
	for _, domain := range registeredDomains() {
		name := domain.Name()
		seq, held := at[name]
		if !held {
			continue
		}
		// THE GENERATION COMES FROM THIS NODE'S OWN CURSOR, because a
		// bare sequence names a number space: a hold stated in the
		// wrong generation pins a position on a log that no longer
		// exists, which the trim reads as no hold at all.
		cursor, _, _, err := statelog.CursorFor(ctx, s.db.Replicated(),
			domain.Stream().Name)
		if err != nil {
			return nil, err
		}
		streams[domain.Stream().Name] = coord.Position{
			Stream: domain.Stream().Name, Generation: cursor.Generation, Seq: seq,
		}
	}
	if err := s.fleet.PutHold(ctx, coord.TrimHold{
		Owner: owner, At: time.Now().UTC(), Streams: streams,
		Reason: "this node is below the log's floor and is fetching a peer's snapshot",
	}); err != nil {
		return nil, fmt.Errorf("engine: pin the replay tail for the join: %w", err)
	}
	return func() {
		// WithoutCancel: the release is undoing the hold, and the
		// failure it is most often undoing is the cancellation itself —
		// a release that inherited a dead context would leave the pin
		// standing for its whole stale window.
		if err := s.fleet.ReleaseHold(context.WithoutCancel(ctx), owner); err != nil {
			log.ErrorContext(ctx, "statelog_join_hold_not_released",
				"owner", owner, "error", err.Error(),
				"detail", "the trim is pinned at this node's join position until "+
					"the hold goes stale")
		}
	}, nil
}

// registered is every domain this node runs, as the framework's own surfaces
// want it: by name, with the health this node reports for each.
//
// The health is what a SNAPSHOT gates on — a node donates only what it can
// vouch for — and the artefact's acceptance is what a join gates on. Both walk
// the same map so a domain added to the register cannot appear in one and not
// the other.
func (s *stateLog) registered() map[string]statelog.Registered {
	out := map[string]statelog.Registered{}
	for _, domain := range registeredDomains() {
		name := domain.Name()
		entry := statelog.Registered{Domain: domain}
		if running, held := s.domains[name]; held {
			entry.StreamCreatedAt = running.createdAt
			entry.Health = func() statelog.Health {
				health, err := s.health(context.Background(), running)
				if err != nil {
					// AN UNREADABLE HEALTH IS NOT A HEALTHY ONE:
					// the zero value has CaughtUp false and no
					// first sequence, which every gate reads as
					// "cannot vouch for this".
					return statelog.Health{}
				}
				return health
			}
		}
		out[name] = entry
	}
	return out
}

// Status is what this node reports about each domain's apply loop.
//
// PER DOMAIN, and every registered one appears — including the domains whose
// health does NOT gate seat admission. The gate and the report answer
// different questions: the gate asks whether a seat may attach, and the vector
// domain deliberately answers "that is not mine to say"; the report asks how
// far along this node's copies are, which is a fact about every one of them.
// A domain missing from the count is one an operator cannot see falling
// behind.
func (s *stateLog) Status(ctx context.Context) []ReplicationStatus {
	if s == nil {
		return nil
	}
	out := make([]ReplicationStatus, 0, len(s.order))
	for _, name := range s.order {
		running := s.domains[name]
		row := ReplicationStatus{Name: name, Kind: "domain"}
		health, err := s.health(ctx, running)
		switch {
		case err != nil:
			// UNREADABLE IS NOT READY. The alternative reads as a
			// caught-up loop on a node whose broker is unreachable,
			// which is the one state this count exists to surface.
			row.Detail = "this node cannot read the domain's own position: " +
				err.Error()
		case !health.CaughtUp:
			row.Detail = fmt.Sprintf("applying: %d record(s) behind the log's head",
				lagOf(health))
		case health.Deferred > 0:
			// CAUGHT UP AND STILL NOT READY. A deferred record is one
			// this build cannot decode: the position moved past it
			// and the rows it would have written are not there, so a
			// loop reporting itself caught up would be claiming a
			// copy it does not have.
			row.Detail = fmt.Sprintf("caught up, holding %d record(s) this "+
				"build cannot decode from sequence %d — a newer build is "+
				"what applies them", health.Deferred, health.DeferredFrom)
		default:
			row.Ready = true
		}
		out = append(out, row)
	}
	return out
}

// lagOf is a health's lag as a number, with the unmeasured case reported as
// zero rather than as a nil dereference. An unmeasured lag never reaches here
// — CaughtUp is false without one — so the fallback is a guard rather than a
// case.
func lagOf(h statelog.Health) uint64 {
	if h.Lag == nil {
		return 0
	}
	return *h.Lag
}

// startSnapshots runs the two halves of the fleet's own recovery path: this
// node TAKES snapshots of its replicated estate on a timer, and SERVES them to
// a peer that asks.
//
// # Why both halves, and why neither is optional
//
// A node too far behind to replay adopts a peer's snapshot — that is
// [Engine.joinIfBehind], and it is the recipient. A recipient with no donor is
// a mechanism that can never complete: the join asks the fleet, nothing
// answers, and the node comes up on the history it has for ever. Half of this
// wired is worse than none, because the half that IS wired reports itself
// working.
//
// So they start together, and they are the same node's: every member both
// takes and serves, because there is no leader here to be the designated
// donor and a fleet whose only donor was down would have nothing to give.
//
// # Both are BEST EFFORT and neither gates the boot
//
// A node that cannot take a snapshot still serves its company perfectly — what
// it loses is the ability to help a peer that falls behind, which is a fleet
// property rather than this node's. A node that cannot serve one is the same
// fact from the other side. So a failure here is logged and the node comes up;
// the alternative is a company that will not start because a recovery path
// nobody is currently using could not be armed.
func (e *Engine) startSnapshots(ctx context.Context, boot *config.Bootstrap, s *stateLog) {
	if s == nil || boot == nil || e.backends == nil {
		return
	}
	// TWO SEAMS, deliberately: the join RIDES the engine's own connection
	// for one short request/reply exchange, and the donor gets its OWN
	// because it closes what it is given — a server of a subject for the
	// life of a node owns its connection, and handed the engine's it would
	// take the whole broker down when it stopped.
	broker, ok := e.backends.Queue.(interface {
		Conn() *nats.Conn
		DialOwned() (*nats.Conn, error)
	})
	if !ok || broker.Conn() == nil {
		// NO BROKER CONNECTION, which is the memory twin in a test. A
		// transfer is megabytes over request/reply rather than anything
		// the event-queue contract carries.
		return
	}
	dir := boot.Store.SnapshotDirFor()

	snapshotter, err := statelog.NewSnapshotter(statelog.SnapshotDeps{
		Domains:       slices.Collect(maps.Values(s.registered())),
		DB:            e.backends.Store,
		Dir:           dir,
		NodeID:        s.nodeID,
		EngineVersion: version.String(),
		Counted:       e.countedNodes,
		Interval:      boot.Stream.TrackerRetention.SnapshotInterval(),
		Logger:        log,
	})
	if err != nil {
		log.ErrorContext(ctx, "statelog_snapshots_unavailable", "error", err.Error(),
			"detail", "this node takes no snapshots, so it can donate nothing to "+
				"a peer that falls below the log's floor; the fleet's other "+
				"members still can")
		return
	}
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: s.nodeID,
		Dial:   func(context.Context) (*nats.Conn, error) { return broker.DialOwned() },
		Newest: func() (statelog.Manifest, bool) { return newestSnapshot(dir) },
		Path: func(m statelog.Manifest) string {
			return filepath.Join(dir, fmt.Sprintf("snapshot-%d.db", newestSeqOf(m)))
		},
		Logger: log,
	})
	if err != nil {
		log.ErrorContext(ctx, "statelog_donor_unavailable", "error", err.Error(),
			"detail", "this node answers no offer request, so a peer below the "+
				"log's floor cannot adopt from it")
		return
	}

	s.done.Add(2)
	go func() {
		defer s.done.Done()
		// THE DONOR FIRST and for the node's whole life: a peer asks at
		// ITS boot, which is any moment at all, so there is no window in
		// which not answering is acceptable.
		if err := donor.Serve(s.run); err != nil && s.run.Err() == nil {
			log.ErrorContext(s.run, "statelog_donor_stopped", "error", err.Error(),
				"detail", "a peer below the log's floor can no longer adopt from "+
					"this node; the fleet's other members still answer")
		}
	}()
	go func() {
		defer s.done.Done()
		e.snapshotLoop(s.run, snapshotter, boot.Stream.TrackerRetention.SnapshotInterval())
	}()
}

// snapshotLoop takes one at boot and then on the interval — but a SKIP is not
// the interval's business.
//
// # Why a skipped tick retries soon and a taken one waits
//
// [statelog.Snapshotter.Take] has five preconditions, and every one of them is
// TRANSIENT AT BOOT: caught up on each domain, not too far behind, more than
// one node counted in the fleet, room on the volume, nothing deferred. A node
// coming up fails several of them for the first seconds of its life — the
// fleet is not counted until its peers have published a position, and it is
// not caught up until its appliers have drained.
//
// So a loop that only ever ticked on the configured interval would take its
// first snapshot a DAY after the node started, and a node restarted more often
// than that would take none at all. The fleet would then have no donor, and
// the only symptom is a peer that falls below the floor finding nothing to
// adopt — months later, in the one situation where it matters.
//
// A taken snapshot is different: the preconditions held, and the next one is a
// question about staleness rather than about readiness. That is what the
// operator's interval is for, and it is what waits.
//
// The retry is deliberately not tight. A skip is a state that clears on its
// own in seconds to minutes, the gate itself is a few reads, and a node that
// is genuinely unable to snapshot must not spend its life asking.
func (e *Engine) snapshotLoop(ctx context.Context, s *statelog.Snapshotter,
	interval time.Duration) {

	// everTook is what decides how LOUD a skip is, and the distinction is
	// the one an operator actually has: a node that has taken a snapshot
	// and skips a tick is a node whose fleet has a donor, and a node that
	// has never taken one is a node the fleet cannot recover from. The
	// snapshotter reports every skip at info, which is right for the first
	// case and much too quiet for the second.
	var everTook bool
	for {
		wait := interval
		switch _, err := s.Take(ctx); {
		case err == nil:
			everTook = true
		case !isSkip(err):
			log.WarnContext(ctx, "statelog_snapshot_failed",
				"error", err.Error(), "ever_took", everTook,
				"detail", "this node's newest artefact is older than the "+
					"interval, so a peer adopting from it replays further")
			wait = min(snapshotSkipRetry, interval)
		case !everTook:
			reason, _ := statelog.Skipped(err)
			log.WarnContext(ctx, "statelog_no_snapshot_yet",
				"reason", string(reason), "retry_in", snapshotSkipRetry,
				"detail", "this node has never taken a snapshot, so it can "+
					"donate nothing to a peer that falls below the log's "+
					"floor; the preconditions clear on their own as this node "+
					"catches up and its peers publish their positions")
			wait = min(snapshotSkipRetry, interval)
		default:
			wait = min(snapshotSkipRetry, interval)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// isSkip reports a tick that declined rather than failed.
func isSkip(err error) bool {
	_, skipped := statelog.Skipped(err)
	return skipped
}

// snapshotSkipRetry is how soon a skipped attempt is retried.
//
// THIRTY SECONDS, which is a boot's own settling time rather than a guess: a
// node's peers publish their positions on the heartbeat, its appliers drain
// what the log holds, and both are seconds on a healthy fleet. Shorter would
// spend a wedged node's life on a gate it cannot pass; much longer would leave
// a restarted node without an artefact for minutes, which is exactly the
// window a rolling upgrade lives in.
const snapshotSkipRetry = 30 * time.Second

// countedNodes is how many members the fleet counts, which is what decides
// whether there is anybody to donate to at all.
//
// FROM THE POSITIONS REGISTER rather than from the lease view, because that is
// the register the trim reads: a node counted there is one whose position
// holds the log back, and one holding the log back is exactly a node that
// might one day need a snapshot.
func (e *Engine) countedNodes(ctx context.Context) (int, error) {
	if e.backends == nil || e.backends.Fleet == nil {
		return 0, fmt.Errorf("engine: no coordination to count the fleet with")
	}
	rows, err := e.backends.Fleet.Positions(ctx)
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// newestSnapshot reads the newest complete manifest in a directory.
//
// THE MANIFEST IS THE CLAIM, which is [internal/backup]'s rule and holds here
// for the same reason: the manifest is written last, so a directory entry
// without one is the debris of a run that did not finish rather than a
// snapshot somebody can adopt.
func newestSnapshot(dir string) (statelog.Manifest, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return statelog.Manifest{}, false
	}
	var newest statelog.Manifest
	var found bool
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		m, err := statelog.ReadManifest(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		// AND THE BYTES BESIDE IT. A manifest whose artefact was rotated
		// away describes a transfer that would fail after the recipient
		// had already chosen it over every other offer.
		if _, err := os.Stat(filepath.Join(dir,
			fmt.Sprintf("snapshot-%d.db", newestSeqOf(m)))); err != nil {
			continue
		}
		if !found || m.TakenAt.After(newest.TakenAt) {
			newest, found = m, true
		}
	}
	return newest, found
}

// newestSeqOf is the sequence a manifest's file name is built from — the
// highest position it names, which is what [statelog.Snapshotter] names it
// after. Derived rather than stored, so the two cannot disagree about which
// file a manifest describes.
func newestSeqOf(m statelog.Manifest) uint64 {
	var newest uint64
	for _, at := range m.Domains {
		if at.Seq > newest {
			newest = at.Seq
		}
	}
	return newest
}

// startPositionHeartbeat publishes this node's row in the fleet's position
// register, for as long as the node runs.
//
// # What reads it, and what an absent row costs
//
// The register is not telemetry. Three things read it and each one is wrong
// without this node's row:
//
//   - THE TRIM takes a minimum across every counted node's position to decide
//     what records the fleet has finished with. A node that publishes nothing
//     is a node the trim cannot see, so the log is trimmed past records this
//     node still needs — and the node discovers that by falling below the
//     floor and having to adopt a snapshot.
//   - THE WRITE FENCE reads the published floor to decide whether an
//     expectation of zero is safe on a quiet subject. With nobody publishing,
//     the floor is zero for ever, which is the conservative direction — but
//     it is conservative by accident rather than by design.
//   - THE SNAPSHOT GATE counts the register to decide whether there is
//     anybody to donate to. An empty register reads as a fleet of one, so no
//     node ever takes a snapshot and no node can ever donate one. That is not
//     a hypothetical: it is what a three-node fleet did before this existed.
//
// # Why it is a heartbeat rather than a write per commit
//
// The position moves on every applied record — thousands a minute on a busy
// company — and the register is a coordination bucket the whole fleet reads.
// Writing it per commit would put the fleet's slowest shared store on the
// applier's own loop. What every reader actually needs is a recent number
// rather than a current one: the trim is conservative in the safe direction
// with a stale row, and the gate's question changes on the scale of nodes
// joining. So it is a timer, and the row carries its own instant so a reader
// can say how stale it is.
func (s *stateLog) startPositionHeartbeat() {
	if s == nil || s.fleet == nil || len(s.order) == 0 {
		return
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		tick := time.NewTicker(PositionHeartbeat)
		defer tick.Stop()
		for {
			// AT ONCE, then on the tick: a node that published nothing
			// for its first interval is a node the trim cannot see for
			// that interval, and a node restarted more often than the
			// interval would never appear at all.
			s.publishPositions(s.run)
			select {
			case <-s.run.Done():
				return
			case <-tick.C:
			}
		}
	}()
}

// PositionHeartbeat is how often a node republishes its row.
//
// TEN SECONDS, taken from what the readers need rather than from what the
// writer can afford. The trim runs on a horizon of days and is conservative
// with a stale row; the snapshot gate's question — is there anybody else —
// changes when a node joins or leaves, which an operator expects to see
// reflected in seconds rather than minutes. Ten is well inside that and is a
// single small write per node per interval against a bucket with no age.
const PositionHeartbeat = 10 * time.Second

// publishPositions writes one row describing every domain this node runs.
//
// EVERY DOMAIN IN ONE ROW, which is the register's own shape: the trim asks
// "what has every node applied" about all of them at once, and a row per
// domain would be N writes saying one thing.
//
// A FAILURE IS LOGGED AND THE LOOP CONTINUES. The row is a recent number
// rather than a current one by construction, so one missed interval costs
// nothing a reader can notice — and a node that stopped its heartbeat because
// coordination blinked would then be invisible to the trim, which is the
// failure this whole loop exists to prevent.
func (s *stateLog) publishPositions(ctx context.Context) {
	row := coord.NodePositions{
		NodeID:        s.nodeID,
		At:            time.Now().UTC(),
		EngineVersion: version.String(),
		Domains:       make(map[string]coord.DomainPosition, len(s.order)),
	}
	for _, name := range s.order {
		running := s.domains[name]
		at := running.runner.Committed()
		pos := coord.DomainPosition{
			Seq: at.Seq, Generation: at.Generation, AppliedThrough: at.Seq,
		}
		// APPLIED_THROUGH IS LOWER WHEN SOMETHING IS DEFERRED, and the
		// two numbers are what tell a lagging node from a stalled one:
		// a position that advances while nothing is applied is exactly
		// what a retained record produces.
		deferral, held := running.runner.Deferred()
		if held {
			pos.Deferred = 1
			if deferral.Position.Seq > 0 {
				pos.AppliedThrough = deferral.Position.Seq - 1
			}
		}
		// THE OBSERVATION RIDES THIS LOOP, because "has not moved" needs a
		// previous look and this is the one place that already takes one
		// every interval. See [progress]: it is what makes `Stalled` and
		// the deferral shed derivable at all, and doing it here rather
		// than on a health read is what keeps a health read pure.
		//
		// BEHIND IS READ FROM THE BROKER'S OWN LAST SEQUENCE rather than
		// from the consumer's backlog: an idle node's applied prefix does
		// not move because there is nothing to move it, and a stall is
		// only a stall when there is work it owes.
		behind := false
		if _, end, err := running.log.Bounds(ctx); err == nil {
			behind = end > at.Seq
		}
		running.progress.observe(row.At, pos.AppliedThrough, behind, held)
		row.Domains[name] = pos
	}
	s.positionGauges(row)
	if err := s.fleet.PutPositions(ctx, row); err != nil {
		if ctx.Err() != nil {
			return
		}
		log.WarnContext(ctx, "statelog_position_not_published",
			"node", s.nodeID, "error", err.Error(),
			"detail", "the trim cannot see this node until it publishes again, "+
				"so it may delete records this node still needs; the next "+
				"heartbeat retries")
	}
}

// positionGauges publishes each domain's progress on the same beat the
// register row is written.
//
// ON THE HEARTBEAT rather than in the apply loop, because these are STATES
// rather than events: what a collector wants is "how far behind is this node
// now", and setting it per applied batch would make the answer a function of
// how busy the log is — a quiet log would leave the last burst's lag standing
// for as long as nothing was published.
func (s *stateLog) positionGauges(row coord.NodePositions) {
	if s.metrics == nil {
		return
	}
	for name, at := range row.Domains {
		attrs := metrics.Attrs{"domain": name}
		s.metrics.Set(metrics.StatelogAppliedThrough, float64(at.AppliedThrough), attrs)
		s.metrics.Set(metrics.StatelogDeferredCount, float64(at.Deferred), attrs)
		running := s.domains[name]
		if running == nil {
			continue
		}
		s.metrics.Set(metrics.StatelogDrainRowsPerSecond, running.runner.Drain(), attrs)
		health, err := s.health(context.Background(), running)
		if err != nil || health.Lag == nil {
			// UNREADABLE IS NOT ZERO, and a gauge has no third
			// value — so the lag gauges are left at whatever they
			// last held rather than being set to a number that
			// reads as caught up. The alarm reading takes the same
			// view from the same health.
			continue
		}
		s.metrics.Set(metrics.StatelogApplyLagSeq, float64(*health.Lag), attrs)
		s.metrics.Set(metrics.StatelogApplyLagSeconds,
			applyLagOf(health, running).Seconds(), attrs)
	}
}

// domainOf is the domain name whose log is this stream, or empty.
//
// A REVERSE LOOKUP over the register rather than a stored map, because the
// register is small and fixed and a second map is a second thing to keep in
// step with it.
func (s *stateLog) domainOf(stream string) string {
	if s == nil {
		return ""
	}
	for _, name := range s.order {
		if running := s.domains[name]; running != nil &&
			running.domain.Stream().Name == stream {
			return name
		}
	}
	return ""
}
