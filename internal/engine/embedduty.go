package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
// A tick with nothing stale is one indexed anti-join returning no rows, and it
// stops there — no provider call, no publish. A tick with a backlog spends at
// most [search.EmbedBatchesPerTick] round trips, which is what bounds both the
// provider bill per minute and how much of the lease's TTL one tick can own.

// embedDutyName is the fleet singleton the embedding duty claims.
const embedDutyName = "embeddings"

// embedDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, the ratio every other singleton here uses: one missed tick must
// not hand the duty to a peer, because two nodes embedding at once is two
// provider bills for one company — the exact cost the singleton exists to
// avoid.
const embedDutyTTL = 3 * search.EmbedInterval

// embedDuty is the loop.
type embedDuty struct {
	engine    *Engine
	publisher *statelog.Publisher
	corpora   []search.Corpus
	claim     func(context.Context) (bool, error)
	metrics   *metrics.Recorder

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
		engine:    e,
		publisher: running.publisher,
		corpora:   corpora,
		claim:     e.workerDuty(embedDutyName, embedDutyTTL),
		metrics:   e.metrics,
		done:      make(chan struct{}),
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
		Embedder:  provider,
		Model:     model,
		Corpora:   d.corpora,
		Logger:    log,
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
	published, err := duty.Tick(ctx)
	if err != nil {
		log.WarnContext(ctx, "embed_tick_failed", "err", err,
			"published", published)
	}
	if published > 0 {
		log.InfoContext(ctx, "embed_tick", "records", published,
			"model", model, "width", provider.Width())
	}
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
