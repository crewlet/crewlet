package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// The embedding duty, armed — the loop that fills the semantic half.
//
// # Without it the vector domain is a log nobody writes to
//
// Everything downstream of a vector existed and was certified: the domain is
// registered, its applier writes `kb_vectors` on every node, the two-stage
// retrieval reads them and the fusion ranks them. What did not exist was the
// only thing that ever PUBLISHES a vector record — so a company's semantic
// search returned nothing, for ever, while every surface reported a healthy
// domain applying a log that happened to be empty. That is the worst shape a
// missing wire has: `search_degraded` is a fraction of answers that skipped the
// semantic half, and an answer over an empty corpus does not skip it.
//
// # Why it is a fleet singleton and why it is here
//
// An embedding is a provider call the company is BILLED for, and every node
// needs the same vector. Run per node it would be N bills for one value, and
// N nodes racing to publish records whose subject is the source's own identity
// — which the broker would arbitrate correctly and charge for anyway. So one
// node holds a named lease, publishes, and every node's applier writes the
// rows. This file is where that lands for the same reason the trim's is:
// `engine` is the entanglement point, the one package that holds the company's
// provider, the state log's publisher and the fleet's leases at once.
//
// # What a tick costs, and why a caught-up company pays almost nothing
//
// A tick on a caught-up company reads the semantic index's head, its per-list
// counts (at most a few thousand rows) and one count of the embedding space
// over kb_vectors' model index, then runs one indexed anti-join that returns
// no rows, and stops there — no provider call, no publish. A tick with a
// backlog spends at most [search.EmbedBatchesPerTick] round trips, which is
// what bounds the provider bill per minute.
//
// # The one tick that is long, and how it keeps its lease
//
// A tick that TRAINS the semantic index (ADR-0022) reads every code and makes
// one exact pass over the wide table, then runs a k-means across every core —
// about 120 µs a source for the reading and ≈ 33 s of k-means at the largest
// list count, which projects to a little over two minutes at the ≈ 545 000
// sources a node searches inside its budget through an index
// (BenchmarkIndexTraining, [search.IVFMaxLists]). That is longer than the
// interval and within reach of the lease's TTL, so the tick RENEWS the lease
// on the interval while it runs ([embedDuty.keepClaimed]) and is cut off the
// moment a renewal does not confirm it: a step cut off publishes nothing, so
// the singleton never has two writers. And every tick is bounded outright by
// [embedTickBudget], because a renewal is also what would keep a WEDGED tick
// holding the duty for ever.

// embedDutyName is the fleet singleton the embedding duty claims.
const embedDutyName = "embeddings"

// embedDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, the ratio every other singleton here uses: one missed tick must
// not hand the duty to a peer, because two nodes embedding at once is two
// provider bills for one company — the exact cost the singleton exists to
// avoid.
const embedDutyTTL = 3 * search.EmbedInterval

// embedTickBudget bounds one tick outright, the lease renewed or not.
//
// FIVE MINUTES: the longest legitimate tick is a training at the largest
// partition an index serves, projected at a little over two minutes on four
// cores under load from the measured per-source reading and the k-means at
// [search.IVFMaxLists] lists (BenchmarkIndexTraining,
// BenchmarkIVFRecallAtScale) — so this is more than twice that. A tick past it
// is a wedged one, which the lease renewal would otherwise let hold the duty
// for ever while no node embedded anything; cut off, it publishes nothing and
// the next tick starts over.
const embedTickBudget = 5 * time.Minute

// embedDuty is the loop.
type embedDuty struct {
	engine    *Engine
	publisher *statelog.Publisher
	corpora   []search.Corpus
	claim     func(context.Context) (bool, error)
	metrics   *metrics.Recorder

	// log is the vector log's running domain, whose runner and stream the
	// index step's standing is read from, and fleet and leases the
	// positions register and the presence leases its counted set is.
	log    *runningDomain
	fleet  coord.Fleet
	leases liveLeases

	// identity is every domain that claims identity, whose eviction records
	// are how the fleet says a node is gone — the vector log carries none
	// of its own ([embedDuty.evicted]) — and db the store they are read
	// from.
	identity []*runningDomain
	db       *store.DB

	// renewEvery is how often a running tick renews the duty's lease:
	// [search.EmbedInterval], the cadence the lease is claimed on anyway,
	// which leaves two renewals inside every TTL.
	renewEvery time.Duration

	stop context.CancelFunc
	done chan struct{}
}

// startEmbedding arms the duty, or does nothing on a node that cannot run it.
//
// THE GATE IS THE PUBLISHER, not the provider: a company with no
// `providers.embeddings` legitimately has no vectors, and that is checked per
// TICK rather than here — an epoch that adds the block must start embedding
// without a restart, and one that removes it must stop.
func (e *Engine) startEmbedding(ctx context.Context, s *stateLog) {
	if s == nil || e.backends == nil {
		return
	}
	running := s.domains[search.Domain{}.Name()]
	if running == nil || running.publisher == nil {
		return
	}
	corpora := e.corpora()
	if len(corpora) == 0 {
		return
	}
	d := &embedDuty{
		engine:     e,
		publisher:  running.publisher,
		corpora:    corpora,
		claim:      e.workerDuty(embedDutyName, embedDutyTTL),
		metrics:    e.metrics,
		log:        running,
		fleet:      e.backends.Fleet,
		leases:     e.backends.Coord,
		identity:   s.identityDomains(),
		db:         e.backends.Store,
		renewEvery: search.EmbedInterval,
		done:       make(chan struct{}),
	}
	// DETACHED from the caller's context, for the reason every other
	// long-running loop here is: a loop bound to a signal context stops at
	// SIGTERM, which would make its lifetime differ from the appliers it
	// publishes into for no reason a reader could find.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	e.embedding = d
	go d.run(loop)
}

// stopEmbedding ends the duty, waiting for an in-flight tick.
func (e *Engine) stopEmbedding() {
	if e.embedding == nil {
		return
	}
	e.embedding.stop()
	<-e.embedding.done
	e.embedding = nil
}

// corpora is every source kind this node can embed.
//
// ONE CONSTRUCTION SITE, because the coverage gauge and the duty must be
// counting and filling the same set: a corpus the duty embeds and the gauge
// does not reports a company as permanently short of coverage, and the reverse
// reports it as complete while a whole source kind is unsearchable by meaning.
func (e *Engine) corpora() []search.Corpus {
	if e.backends == nil || e.backends.Store == nil {
		return nil
	}
	// BOTH SOURCE KINDS. A duty that embedded only one would leave the
	// other unsearchable by meaning for ever, with a coverage gauge
	// reporting the half it did cover as the whole — which is the reading
	// an operator would act on.
	return []search.Corpus{
		search.TaskCorpus{DB: e.backends.Store},
		search.PageCorpus{DB: e.backends.Store},
	}
}

// embedModel is the model id and width the current epoch embeds at, and false
// when this company has no embeddings configured.
//
// READ PER TICK rather than captured at start, because the provider is
// replaced on every config apply: a duty holding the embedder it was built
// with would go on writing rows at the retired model's id after an operator
// changed it, and the rows a change is meant to supersede would never be
// selected again.
func (e *Engine) embedModel() (embeddings.BatchEmbedder, string, bool) {
	held := e.embeddings.Load()
	if held == nil || *held == nil {
		return nil, "", false
	}
	batch, ok := (*held).(embeddings.BatchEmbedder)
	if !ok {
		// A PROVIDER THAT CANNOT BATCH DOES NOT RUN THIS DUTY. One
		// source per round trip is 110 000 round trips for a cold fill,
		// which does not fit in the tick — and the one-at-a-time
		// fallback belongs to the callers that embed a single thing,
		// never to a corpus walk.
		return nil, "", false
	}
	cfg := e.Company().Config.Providers.Embeddings
	if cfg == nil {
		return nil, "", false
	}
	model := e.resolver().Value(cfg.Model)
	if model == "" {
		return nil, "", false
	}
	return batch, model, true
}

// run ticks until the context ends.
func (d *embedDuty) run(ctx context.Context) {
	defer close(d.done)
	ticker := time.NewTicker(search.EmbedInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick embeds one tick's worth of sources, if this node holds the duty.
func (d *embedDuty) tick(ctx context.Context) {
	if d.claim != nil {
		mine, err := d.claim(ctx)
		if err != nil {
			log.WarnContext(ctx, "embed_duty_unclaimed", "err", err)
			return
		}
		if !mine {
			return
		}
	}
	provider, model, configured := d.engine.embedModel()
	if !configured {
		return
	}
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: d.publisher,
		// THE PARTITION'S OWN FILE AND LOG, for the semantic index the
		// duty keeps beside its vectors (ADR-0022): the one replicated
		// file and the one vector log this layout has.
		Store:    d.engine.backends.Store,
		Log:      search.Domain{}.Stream().Name,
		Standing: d.standing,
		Embedder: provider,
		Model:    model,
		Corpora:  d.corpora,
		// No Logger: an absent one is the search package's own, so the
		// duty's batch failures say `component=search` like every other
		// line that package writes.
	})
	if err != nil {
		// REFUSED WIRING IS AN OPERATOR'S PROBLEM, said once a tick
		// rather than swallowed: every case [search.NewEmbedder]
		// refuses is a company that will never get a vector, and a
		// silent return here is indistinguishable from a corpus that is
		// already complete.
		log.WarnContext(ctx, "embed_duty_unwired", "err", err)
		return
	}
	tick, cancel := context.WithTimeout(ctx, embedTickBudget)
	defer cancel()
	if d.claim != nil {
		defer d.keepClaimed(tick, cancel)()
	}
	published, err := duty.Tick(tick)
	if err != nil {
		log.WarnContext(ctx, "embed_tick_failed", "err", err,
			"published", published)
	}
	if errors.Is(tick.Err(), context.DeadlineExceeded) {
		log.WarnContext(ctx, "embed_tick_overran", "budget", embedTickBudget,
			"detail", "the tick was cut off at its budget and published nothing "+
				"after it; a training that cannot finish inside it leaves the "+
				"partition on its current first stage")
	}
	if published > 0 {
		log.InfoContext(ctx, "embed_tick", "records", published,
			"model", model, "width", provider.Width())
	}
}

// keepClaimed renews the duty's lease on the interval while a tick runs, and
// cancels the tick the moment a renewal does not confirm it — an error
// included, because a node that cannot say it holds the duty must not publish
// as its holder. The returned function stops the renewals and waits for them,
// so their goroutine never outlives the tick.
func (d *embedDuty) keepClaimed(ctx context.Context, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(d.renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			mine, err := d.claim(ctx)
			if err != nil || !mine {
				log.WarnContext(ctx, "embed_duty_lost_mid_tick", "err", err,
					"held", mine, "detail", "the tick is cut off and publishes "+
						"nothing more; the node that holds the duty now carries on")
				cancel()
				return
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// standing is the vector log as the index step must know it
// ([search.LogStanding]): whether this node has applied all of it, and which
// build every node applying it reads.
//
// THE END IS READ BEFORE THE CHECKPOINT, which is the only order in which a
// checkpoint at or past it means this node applied everything the log held
// when the step began; the other order reads a record that landed between the
// two as one this node missed. A node holding a deferred record is not
// current either: its rows are the log's minus that record.
//
// THE READERS ARE THE COUNTED SET — the positions register's rows for this
// log, and every live data node that has not reported yet — because that is
// every node that applies the log, less every node the fleet has EVICTED
// ([embedDuty.evicted]): an operator's word that a node is not coming back,
// and without it an old build's row on a machine nobody will start again would
// hold the index back for the life of the deployment.
func (d *embedDuty) standing(ctx context.Context) (search.LogStanding, error) {
	var out search.LogStanding
	stats, err := d.log.log.Stats(ctx)
	if err != nil {
		return out, fmt.Errorf("read the vector log's end: %w", err)
	}
	_, deferring := d.log.runner.Deferred()
	out.Current = d.log.runner.Committed().Seq >= stats.LastSeq && !deferring

	rows, err := d.fleet.Positions(ctx)
	if err != nil {
		return out, fmt.Errorf("read the positions register: %w", err)
	}
	var live []statelog.Presence
	if d.leases != nil {
		if live, err = livePresences(ctx, d.leases); err != nil {
			return out, err
		}
	}
	tombs, err := d.evicted(ctx)
	if err != nil {
		return out, err
	}
	name := d.log.domain.Name()
	out.Readers = statelog.Readers(statelog.CountedSet(time.Now().UTC(),
		reportedPositions(rows, name), live, tombs))
	return out, nil
}

// evicted is every node the fleet has evicted and not readmitted, as a
// tombstone the counted set subtracts once its fence window has passed.
//
// FROM THE IDENTITY-CLAIMING LOGS, because the vector log carries no eviction
// of its own — a node behind on it is a coverage figure, never a node that
// cannot resume — and an eviction is the fleet's one gesture for a node that
// is gone. A node counts as evicted only where EVERY identity log holds its
// eviction, dated by the latest of them: an eviction still going round the
// logs is not yet the fleet's word. A log whose evictions cannot be read is
// an error rather than none, because "evicted nowhere" read off a table nobody
// read would hold the index back for a node an operator released — or, read
// the other way, release it for one they did not.
func (d *embedDuty) evicted(ctx context.Context) ([]statelog.Tombstone, error) {
	if len(d.identity) == 0 || d.db == nil {
		return nil, nil
	}
	type seen struct {
		logs int
		at   time.Time
	}
	evicted := map[string]*seen{}
	for _, running := range d.identity {
		lister, ok := running.domain.(evictionLister)
		if !ok {
			return nil, fmt.Errorf("the %s log lists no evictions", running.domain.Name())
		}
		rows, err := lister.Evictions(ctx, d.db)
		if err != nil {
			return nil, fmt.Errorf("read the %s log's evictions: %w",
				running.domain.Name(), err)
		}
		for _, row := range rows {
			if row.Back {
				continue
			}
			s := evicted[row.NodeID]
			if s == nil {
				s = &seen{}
				evicted[row.NodeID] = s
			}
			s.logs++
			if row.At.After(s.at) {
				s.at = row.At
			}
		}
	}
	var out []statelog.Tombstone
	for node, s := range evicted {
		if s.logs == len(d.identity) {
			out = append(out, statelog.Tombstone{NodeID: node, At: s.at})
		}
	}
	return out, nil
}

// vectorCoverage is the fraction of this node's sources carrying a current
// vector, and false when this company has no embeddings configured.
//
// FROM THE SAME CORPORA THE DUTY EMBEDS, at the model and width it is
// embedding at right now. A coverage measured against a different model would
// read as zero the instant an operator changed one — which is true of the rows
// and useless as an alarm, because the duty is already refilling them.
func (e *Engine) vectorCoverage(ctx context.Context) (float64, bool, error) {
	provider, model, configured := e.embedModel()
	if !configured {
		return 0, false, nil
	}
	return search.Coverage(ctx, e.corpora(), model, provider.Width())
}

// indexReading is the semantic index's alarm input (ADR-0022): the latest
// measurement of the partition's index against the exact scan — the worst
// query shape's recall and the floor its training judged it against, the
// evaluation's own curve borrowed rather than restated (ADR-0015).
//
// ONLY FOR THE SPACE THE COMPANY EMBEDS IN NOW. An index trained under a
// model the company has since changed serves no query — every one is in the
// new space and scans until the duty retrains — so its recall describes
// nothing a search does, and alarming on it would page an operator about a
// model they have already left. A read that fails reports nothing, for
// vectorCoverage's reason: an unreadable index is not a failing one.
func (e *Engine) indexReading(ctx context.Context, out *statelog.Reading) {
	provider, model, configured := e.embedModel()
	if !configured || e.backends == nil || e.backends.Store == nil {
		return
	}
	var head search.IndexHead
	var indexed bool
	if err := e.backends.Store.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		head, indexed, err = search.ReadIndex(ctx, tx)
		return err
	}); err != nil {
		log.WarnContext(ctx, "vector_index_unreadable", "err", err)
		return
	}
	if !indexed || head.Measurement == nil || !head.InSpace(model, provider.Width()) {
		return
	}
	m := *head.Measurement
	out.IVFRecall = &m.Recall
	out.IVFRecallFloor = m.Floor
	out.IVFMeasuredOn = m.Sources
	out.IVFShape = string(m.Shape)
	if m.ShapeSource != "" {
		out.IVFShape += ":" + string(m.ShapeSource)
	}
}
