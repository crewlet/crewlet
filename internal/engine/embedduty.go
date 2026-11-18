package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// The embedding duty: the fleet singleton and only publisher of the vector
// log. Without it semantic search answers nothing while `search_degraded`
// stays clean, since an empty corpus skips no semantic half. One node holds it
// under a named lease because an embedding is a billed provider call every
// node needs identically; it lives in engine, as the trim does, the one
// package holding the provider, the state log's publisher and the leases.
//
// A caught-up tick reads the index head, its list counts and one count of the
// embedding space, runs one indexed anti-join that returns nothing and calls
// no provider; a backlog tick is capped at [search.EmbedSourcesPerTick]
// sources in [search.EmbedRequestsPerTick] requests, bounding the bill.

// A training tick (ADR-0028) runs minutes: about three at the ≈ 545 000
// sources a node searches through an index, its k-means on half the cores and
// never fewer than two (BenchmarkIndexTraining, BenchmarkIVFTrainingShare,
// [search.IVFMaxLists]), past the interval and near the lease's TTL. So it
// renews the lease as it runs ([dutyRenewal.keep]) and is cut off once a
// renewal says another node holds it, or none has confirmed it for as long as
// the lease is sure to last; a refused or stalled renewal costs nothing and a
// cut-off step publishes nothing, so the singleton never has two writers. A
// renewal first asks whether a copy the duty decides from went out of service
// (ADR-0079); if one has, the tick is cut off unrenewed and gives the duty
// back once it has returned ([embedDuty.declinedMidTick]).

// Every tick is bounded by PROGRESS rather than time ([embedTickBudget],
// [tickBound]): renewals alone would let a wedged tick hold the duty for ever,
// and a time budget cuts off a one-core node's eleven-minute training, every
// moment of it advancing, on every tick, so it never finishes.

// embedDutyName is the fleet singleton the embedding duty claims.
const embedDutyName = "embeddings"

// embedDutyTTL is how long the duty survives without a re-claim: three ticks,
// as every singleton here, so one missed tick does not hand it to a peer and
// bill the company twice.
const embedDutyTTL = 3 * search.EmbedInterval

// embedTickBudget is how long one tick may go without showing progress
// before it is cut off, the lease renewed or not ([tickBound]); cut off, it
// publishes nothing more and the next tick starts over.
// Five minutes, longer than any stretch a LIVE tick goes without progress. It
// shows progress at each provider answer (bounded by [embeddings.BatchTimeout],
// a minute, no retries), each vector published or withdrawn (one by one, since
// a bulk purge withdraws 1 024 a corpus), each rollout batch, and every 1 024
// rows the training reads, a third of a second apart on the slowest node
// measured (BenchmarkIndexTraining -cpu 1: 331 µs a row); the index's
// arithmetic runs exempt ([search.Budget]) because it cannot wedge. The reads
// between them that do not stream take seconds at the largest corpus, so five
// silent minutes is a wait on something that never answered.
const embedTickBudget = 5 * time.Minute

// embedDuty is the loop.
type embedDuty struct {
	engine    *Engine
	publisher *statelog.Publisher
	corpora   []search.Corpus
	claim     func(context.Context) (bool, error)
	metrics   *metrics.Recorder

	// log is the vector log this duty embeds into (its runner and stream give
	// the index step's standing), and db the node whose replicated estate the
	// corpora are read from.
	log *runningLog
	db  *store.DB

	// renewal is how a running tick keeps the duty's lease: renewed four
	// times an [embedDutyTTL], so a renewal the store refuses or holds on to
	// is followed by another inside the window the last confirmation bought
	// (coord.KeptRenewals).
	renewal dutyRenewal

	// budget is how long a tick may go without progress: [embedTickBudget],
	// and a test's shorter one.
	budget time.Duration

	// refusals is what the provider refused alone, kept ACROSS ticks because
	// the search package rebuilds its duty every tick, and refusalsFor the
	// configuration it belongs to ([embedConfiguration]): a provider configured
	// otherwise starts with none ([search.Refusals]), one an apply rebuilt the
	// same keeps them. Touched only by the loop's own ticks.
	refusals    *search.Refusals
	refusalsFor embedConfiguration

	// copies names the first of the given domains whose copy on this node is
	// out of service ([Engine.copyOutOfService]; nil: every copy serves);
	// declines is whether this node declines the duty because a copy it
	// decides from is ([embedDuty.decidesFrom]); giveBack gives a held duty
	// back when it does ([Engine.workerGiveBack]; nil: no lease to give).
	copies   func(ctx context.Context, domains ...string) (string, bool)
	declines dutyDecline
	giveBack func(context.Context) (bool, error)

	stop context.CancelFunc
	done chan struct{}
}

// tickReport is what one tick did, for the tests that hold the duty's wiring:
// the loop reads nothing of it, and every fact in it is also a log line.
type tickReport struct {
	// published is how many records the tick published.
	published int
	// unwired says the embedder refused the tick's wiring and nothing ran.
	unwired bool
	// overran says the tick went its budget without progress and was cut
	// off ([errTickOverran]).
	overran bool
	// declined names the domain whose copy, out of service on this node,
	// made the tick decline the duty ([dutyDecline]); empty where it did not.
	declined string
}

// startEmbedding arms the duty, or does nothing on a node that cannot run it.
//
// THE GATE IS THE PUBLISHER, not the provider: `providers.embeddings` is
// checked per TICK, so an epoch that adds the block starts embedding without
// a restart and one that removes it stops.
func (e *Engine) startEmbedding(ctx context.Context, s *stateLog) {
	d := e.newEmbedDuty(s)
	if d == nil {
		return
	}
	// DETACHED from the caller's context, like every long-running loop here:
	// bound to a signal context it would stop at SIGTERM, unlike the appliers
	// it publishes into.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	e.embedding = d
	go d.run(loop)
}

// newEmbedDuty is the duty over s's vector log, or nil on a node that cannot
// run it — see [Engine.startEmbedding].
func (e *Engine) newEmbedDuty(s *stateLog) *embedDuty {
	if s == nil || e.backends == nil {
		return nil
	}
	running := s.Domain(search.Domain{}.Name())
	if running == nil || running.publisher == nil {
		return nil
	}
	corpora := e.corpora(s.estate().Reader())
	if len(corpora) == 0 {
		return nil
	}
	return &embedDuty{
		engine:    e,
		publisher: running.publisher,
		corpora:   corpora,
		claim:     e.workerDuty(embedDutyName, embedDutyTTL),
		copies:    e.copyOutOfService,
		giveBack:  e.workerGiveBack(embedDutyName),
		metrics:   e.metrics,
		log:       running,
		db:        e.backends.Store,
		renewal:   newDutyRenewal(embedDutyName, embedDutyTTL),
		budget:    embedTickBudget,
		done:      make(chan struct{}),
	}
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

// corpora is every source kind this node can embed out of the replicated estate.
//
// ONE CONSTRUCTION SITE, because the coverage gauge and the duty must count
// and fill the same set, or the gauge reports a company permanently short, or
// complete while a source kind is unsearchable by meaning.
func (e *Engine) corpora(estate store.ReplicatedReader) []search.Corpus {
	if e.backends == nil || e.backends.Store == nil || estate.IsZero() {
		return nil
	}
	// NOT CHAT: a conversation is a keyword corpus with no vectors
	// (ADR-0071), since a year of messages is several times the whole vector
	// budget the knowledge base is sized for.
	return []search.Corpus{
		search.TaskCorpus{DB: estate},
		search.PageCorpus{DB: estate},
	}
}

// embedModel is the model id and width the current epoch embeds at, and false
// when this company has no embeddings configured.
//
// READ PER TICK, never captured at start: every config apply replaces the
// provider, and a captured one would go on writing rows at a retired model's
// id, so the rows a change supersedes would never be selected again.
func (e *Engine) embedModel() (embeddings.BatchEmbedder, string, bool) {
	held := e.embeddings.Load()
	if held == nil || *held == nil {
		return nil, "", false
	}
	batch, ok := (*held).(embeddings.BatchEmbedder)
	if !ok {
		// A PROVIDER THAT CANNOT BATCH DOES NOT RUN THIS DUTY: a cold fill
		// one source a round trip is 110 000 round trips, past a tick. The
		// one-at-a-time fallback is for callers embedding a single thing.
		return nil, "", false
	}
	company := e.Company()
	if company == nil || company.Config == nil {
		return nil, "", false
	}
	cfg := company.Config.Providers.Embeddings
	if cfg == nil {
		return nil, "", false
	}
	if !company.Config.VectorsEnabled() {
		// `knowledge.vectors: false` is read here alone: a company keeping
		// a provider for its diary without its corpus embedded. The duty,
		// the coverage gauge and the query vector all read this function,
		// so it stops all three.
		return nil, "", false
	}
	model := e.resolver().Value(cfg.Model)
	if model == "" {
		return nil, "", false
	}
	return batch, model, true
}

// queryModel is [Engine.embedModel] as a search's query vector reads it.
//
// THE SAME FUNCTION as the duty's, so a query is embedded at exactly the
// corpus's model and width: a vector from another space ranks against no row,
// and the search would answer empty and call itself complete.
func (e *Engine) queryModel() (embeddings.Embedder, string, bool) {
	provider, model, on := e.embedModel()
	if !on {
		return nil, "", false
	}
	return provider, model, true
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

// decidesFrom is every domain whose rows the duty decides from: the vectors it
// publishes and withdraws, and the domain each corpus's sources are read from
// ([sourceDomains]) — what a tick's stale and withdrawn selections join. A
// corpus whose source maps to no domain is named by its source, which no
// verdict knows and a test refuses.
func (d *embedDuty) decidesFrom() []string {
	out := []string{search.Domain{}.Name()}
	for _, c := range d.corpora {
		domain, known := sourceDomains[c.Source()]
		if !known {
			domain = string(c.Source())
		}
		if !slices.Contains(out, domain) {
			out = append(out, domain)
		}
	}
	return out
}

// declinedMidTick is whether this node now declines the duty, asked before
// every renewal of a running tick ([dutyRenewal.declining]): a training tick
// runs for minutes, and one renewed only on whether the lease was still held
// would carry on deciding from a copy that went out of service after it began.
func (d *embedDuty) declinedMidTick(ctx context.Context) bool {
	_, out := d.declines.check(ctx, embedDecline, d.copies, d.decidesFrom())
	return out
}

// tick embeds one tick's worth of sources, if this node holds the duty, and
// reports what it did.
//
// IT DECLINES FIRST, before it claims, while this node's copy of a domain it
// decides from is out of service ([dutyDecline]): the vectors it publishes are
// every node's, and would be decided from rows this node stopped serving.
func (d *embedDuty) tick(ctx context.Context) tickReport {
	var report tickReport
	if domain, out := d.declines.check(ctx, embedDecline, d.copies, d.decidesFrom()); out {
		d.declines.giveBack(ctx, embedDecline, d.giveBack)
		report.declined = domain
		return report
	}
	work := ctx
	if d.claim != nil {
		asked := d.renewal.clock.Now() // BEFORE the claim: what it confirms runs from here
		mine, err := d.claim(ctx)
		if err != nil {
			log.WarnContext(ctx, "embed_duty_unclaimed", "err", err)
			return report
		}
		if !mine {
			return report
		}
		// FROM HERE THE TICK KEEPS THE DUTY AS IT RUNS ([dutyRenewal.keep]),
		// cut off once a copy it decides from goes out of service
		// ([embedDuty.declinedMidTick]). Nothing else depends on Stop's
		// answer: the broker arbitrated what it published, and the next
		// tick claims afresh.
		var kept *keptDuty
		work, kept = d.renewal.declining(d.declinedMidTick).keep(ctx, d.claim, asked)
		defer func() {
			// A TICK CUT OFF FOR A COPY OUT OF SERVICE gives the duty back
			// once it has RETURNED, never from the renewal that cut it off,
			// or a publish on its way lands after another node took it.
			if _, err := kept.Stop(); errors.Is(err, errDutyDeclined) {
				d.declines.giveBack(ctx, embedDecline, d.giveBack)
			}
		}()
	}
	provider, model, configured := d.engine.embedModel()
	if !configured {
		return report
	}
	// THE MEMORY FOLLOWS THE CONFIGURATION, read off the very provider this
	// tick embeds with, so an apply between two reads cannot pair one
	// provider's refusals with another's.
	if configuration := configurationOf(provider); d.refusals == nil || d.refusalsFor != configuration {
		d.refusals, d.refusalsFor = search.NewRefusals(), configuration
	}
	tick, bound, release := boundTick(work, d.budget)
	defer release()
	duty, err := search.NewEmbedder(search.EmbedDeps{
		Publisher: d.publisher,
		// For the semantic index the duty keeps beside its vectors (ADR-0028).
		Estate:   d.db.Replicated().Reader(),
		Log:      d.log.spec.Name,
		Standing: d.standing,
		Embedder: provider,
		Model:    model,
		Corpora:  d.corpora,
		Refusals: d.refusals,
		// The tick's own bound, which its steps show progress to ([tickBound]).
		Budget: bound,
		// No Logger: the search package's own default says
		// `component=search`, like every other line it writes.
	})
	if err != nil {
		// REFUSED WIRING IS SAID ONCE A TICK: every case
		// [search.NewEmbedder] refuses is a company that never gets a
		// vector, and silence looks like a complete corpus.
		log.WarnContext(ctx, "embed_duty_unwired", "err", err)
		report.unwired = true
		return report
	}
	published, err := duty.Tick(tick)
	report.published = published
	if err != nil {
		log.WarnContext(ctx, "embed_tick_failed", "err", err,
			"published", published)
	}
	if errors.Is(context.Cause(tick), errTickOverran) {
		report.overran = true
		charged, exempt := bound.spent()
		log.WarnContext(ctx, "embed_tick_overran", "budget", d.budget,
			"charged", charged, "exempt", exempt,
			"detail", "the tick went its whole budget without progress — no "+
				"batch answered, no stretch of rows read — so something it "+
				"waited on did not answer; it was cut off and published nothing "+
				"after it, and the next tick starts over")
	}
	if published > 0 {
		log.InfoContext(ctx, "embed_tick", "records", published,
			"model", model, "width", provider.Width())
	}
	return report
}

// standing is the vector log as the index step must know it
// ([search.LogStanding]): whether this node has applied all of it.
//
// THE END IS READ BEFORE THE POSITION: only in that order does a position at
// or past it mean this node applied all the log held when the step began. A
// node holding a deferred record is not current either.
//
// THE REACHED POSITION ([statelog.Runner.Reached]), never the committed
// checkpoint: the question is whether this node's ROWS are the log's state,
// and a barrier at the end whose checkpoint fsync stalled leaves them whole.
func (d *embedDuty) standing(ctx context.Context) (search.LogStanding, error) {
	var out search.LogStanding
	stats, err := d.log.log.Stats(ctx)
	if err != nil {
		return out, fmt.Errorf("read the vector log's end: %w", err)
	}
	_, deferring := d.log.runner.Deferred()
	out.Current = d.log.runner.Reached().Seq >= stats.LastSeq && !deferring
	return out, nil
}

// vectorCoverage is the fraction of this node's sources carrying a current
// vector, and false when this company has no embeddings configured.
//
// FROM THE SAME CORPORA THE DUTY EMBEDS, at its current model and width: one
// measured against another model reads zero the instant an operator changes
// it, useless as an alarm while the duty is already refilling the rows.
func (e *Engine) vectorCoverage(ctx context.Context) (float64, bool, error) {
	provider, model, configured := e.embedModel()
	if !configured {
		return 0, false, nil
	}
	return search.Coverage(ctx, e.corpora(e.domainEstate()), model, provider.Width())
}

// indexReading is the semantic index's alarm input (ADR-0028): its latest
// measurement against the exact scan, the worst query shape's recall and the
// floor its training judged it against (ADR-0015).
//
// ONLY FOR THE SPACE THE COMPANY EMBEDS IN NOW: an index trained under a
// retired model serves no query until the duty retrains, so alarming on its
// recall pages about a model already left. A failed read reports nothing: an
// unreadable index is not a failing one.
func (e *Engine) indexReading(ctx context.Context, out *statelog.Reading) {
	provider, model, configured := e.embedModel()
	if !configured || e.backends == nil || e.backends.Store == nil {
		return
	}
	var head search.IndexHead
	var indexed bool
	if err := e.domainEstate().Read(ctx, func(tx *sql.Tx) error {
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
