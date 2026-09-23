package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The native backends, wired.
//
// # What is per-NODE and what is per-EPOCH, and why the split is not obvious
//
// The apply loops, the projector, the indexer and the change feeds are
// per-NODE: they follow a LOG or a coordination family, and neither changes
// when a company revision does. Rebuilding them on an apply would restart
// every node's applier and re-run a boot reconcile on every configuration
// change, which for a company that edits its org chart twice a day is a node
// that is never established.
//
// The parsers, the prompts and the tool wiring are per-EPOCH, because all
// three depend on the org chart: the lead map a fallback routes through, the
// default project a seat files into, the reserved containers. Those are
// exactly what an apply changes.
//
// # Seat acquisition waits on ESTABLISHMENT, and nothing else does
//
// A seat whose mailbox attached before its node's tracker was established
// would answer "there is no such task" to its own tools — which is an answer
// it acts on, by filing a duplicate or abandoning work it was told to do. So
// [Engine.NativeHydrated] gates the claim, and /ready deliberately does not:
// the control plane's rule is that lag alone never sheds a node, and a node
// that is behind should stop taking new seats rather than be declared
// unhealthy.
//
// The gate is per DOMAIN rather than per node, from each domain's own
// declaration: a compacted domain's gap is a coverage number rather than a
// fault, so shedding a company's seats for one would be the outage the number
// exists to avoid.

// native holds this node's native-backend runtime.
//
// EVERY FIELD IS WRITTEN ONCE, by [Engine.startNative], before the struct is
// published to [Engine.native] — which is why there is no mutex here and why
// adding one would be misleading rather than merely redundant. What the
// running node mutates afterwards is the one field that carries its own
// synchronisation: the wait group.
type native struct {
	// nodeID is who this node is: the name its own domain consumer takes,
	// the writer stamped on every record it publishes, and what the
	// eviction gate compares against. Held here because three subsystems
	// need it after boot and the bootstrap it came from is not kept.
	nodeID string

	// log is this node's state-log runtime: every registered domain, each
	// with its own apply loop and write authority. A native tracker or a
	// native knowledge base starts it ([config.Company.RunsStateLog]) and
	// it runs the whole register or none, so a company on Jira whose pages
	// are the engine's own runs every domain. A company on vendors for both
	// has no native at all.
	log *stateLog

	// It adopts the state log in its own step; until then this node runs
	// one projector and one log side by side, and that is visible here
	// rather than hidden behind a common name.
	// indexer keeps the lexical search index behind the page projection.
	indexer *search.Indexer

	// writer is the tracker's write authority and pages the wiki's.
	writer *tracker.Writer
	pages  *pages.Store

	// trackerReader and pageReader are the read paths.
	trackerReader *tracker.Reader
	pageReader    *pages.Reader

	// searcher answers the knowledge seam natively.
	searcher *pages.Searcher

	// itemSearch is the tracker's own ranked search, over the SAME index
	// and the SAME fan-out — see worksearch.go. Its own field because the
	// two verbs are the tracker's and the wiki's, and a caller holding one
	// must not be able to reach the other's corpus.
	itemSearch *tracker.Searcher

	// stopSlices withdraws this node as an answerer for the fleet's
	// search fan-out. Nil when there is no queue to serve on, which is
	// every embedded engine and every test.
	stopSlices queue.Unsubscribe

	// run is the context every goroutine this node started runs under, and
	// stop is what ends it.
	//
	// HELD, not re-derived. The feeds start later than the projectors — a
	// feed publishes onto the inbound edge, so it is armed with the rest
	// of the node rather than at construction — and a goroutine registered
	// on done but started under the CALLER's context would never be ended
	// by stop, which then blocks on the wait for ever. That is not a
	// hypothetical: it wedged every engine test that shut down before its
	// own context expired.
	run  context.Context
	stop context.CancelFunc
	done sync.WaitGroup
}

// startNative opens this node's native backends, once.
//
// PER NODE, called from [Engine.New] before anything claims a seat, and NOT
// re-run on an apply. It returns without waiting for hydration: the reconcile
// is O(keys) and a node that blocked here would not serve its dashboard,
// answer a probe or run a duty until it finished.
//
// The store and the fleet are not nil-checked: [New] refuses a Backends
// without either, so every engine that reaches this holds both.
func (e *Engine) startNative(ctx context.Context, boot *config.Bootstrap, c *Company) error {
	// AN IN-MEMORY STREAM NEVER GETS THIS FAR. [Engine.New] refused a
	// company that runs the log on one ([config.CheckTiers]): its first
	// restart recreates the log empty and the node never serves again, so
	// the error line once logged here was printed on the way to that state.
	if !c.Config.RunsStateLog() {
		return nil
	}
	runTracker := c.Config.TrackerBackendFor() == config.TrackerNative
	wiki := c.Config.KnowledgeBackendFor() == config.KnowledgeNative

	// THE RESOLVED ID, not the raw field. `node.id` may be absent, a
	// `${VAR}` reference, or come from the environment — and the value
	// three durable things are named after (this node's own consumer, the
	// writer stamped on its records, the eviction gate's key) must be the
	// same one the broker's server name and the presence row already use.
	//
	// READ BEFORE the context below rather than after, so the one failure
	// in this function that has started nothing has nothing to unwind.
	nodeID, err := config.ResolveNodeID(boot, config.EnvOnly())
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &native{run: runCtx, stop: cancel, nodeID: nodeID}
	// ONE FAILURE PATH FOR EVERYTHING BELOW, armed before the first thing
	// that outlives this call and stood down only once the runtime is the
	// engine's.
	//
	// Everything below either starts a goroutine or registers this node
	// somewhere, and both DETACH from runCtx deliberately: the state log's
	// apply loops, its position heartbeat and its snapshot donor run under
	// a context of the log's own, and a slice answerer's registration is
	// copied with [context.WithoutCancel] inside the queue, because a
	// registration scope is not a process one. So a bare `cancel()` on the
	// way out reaches NEITHER — it ends this node's own loops, which at
	// that point have not started — and a return that left them behind
	// hands [Engine.New]'s failure path a store that an applier is still
	// committing into, which is the mid-batch write [Engine.Stop] orders
	// itself to avoid. A caller that retried would then be running two
	// appliers on one node's durable consumer, each deriving the same rows
	// into the same replicated database, and two answerers scanning an
	// index only one of them maintains.
	//
	// Deferred rather than written at each return: the list of things to
	// unwind grows down the function, and the two returns that did unwind
	// had already stopped matching the four that did not.
	started := false
	defer func() {
		if !started {
			n.shutdown(ctx)
		}
	}()

	// THE LOG COMES UP BEFORE ANYTHING READS IT, and its own context
	// outlives this call: an apply loop started under the caller's context
	// is one stopNative can never end.
	//
	// ONCE FOR THE NODE, not once per backend. The register is the node's
	// and every domain in it runs or none does — a node running half its
	// register serves rows derived from one log while another's records
	// pile up unapplied, and nothing above it can tell that from a node
	// that is merely behind.
	sl, err := e.startStateLog(ctx, boot, nodeID, c.Epoch())
	if err != nil {
		return err
	}
	n.log = sl

	if runTracker {
		running := sl.Domain(tracker.Domain{}.Name())
		writer, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: running.publisher,
			// THE APPLIER'S OWN MEASURED RATE, so a refusal's
			// `retry_after_seconds` is derived from what this node
			// actually applies rather than from the one-record-a-second
			// floor a nil reader falls back to — which reported a node
			// two thousand records behind as half an hour behind.
			Drain:  running.runner.Drain,
			DB:     e.backends.Store,
			Claims: e.backends.Coord,
			NodeID: nodeID,
			// THE CHART, read PER CALL. A project's lead is the one
			// fact a wake's fallback recipient needs that the rows
			// cannot give — `tracker_projects` carries no column for
			// it, because the applier may not read an org — and the
			// caller that needs it is a fleet singleton on a tick with
			// no tool arguments to carry a seam through.
			Leads: liveLeads{engine: e},
			// AND THE CHART AGAIN, for the one custom-field type whose
			// value is a colleague: a people field resolves through the
			// company's own roster, which belongs to the EPOCH rather
			// than to a row — so the write resolves it and the record
			// carries the handle, and no applier ever reads an org.
			World: liveSeats{engine: e},
			// THE NODE'S OWN WRITER ACTS AS THE SYSTEM, and every
			// surface derives its own from it with Writer.As: a seat's
			// tools act as that seat, an operator's session as that
			// credential. What is left acting as the system is what the
			// node itself does — a duty finishing an abandoned walk, a
			// repair telling somebody they were unblocked — and
			// attributing those to a person would make a machine's
			// housekeeping indistinguishable from somebody's decision.
			Actor:     nodeID,
			ActorKind: tracker.AuthorSystem,
			// The process's one recorder, so every write outcome and
			// every rejection is counted rather than merely declared.
			Metrics: e.metrics,
		})
		if err != nil {
			return fmt.Errorf("engine: tracker writer: %w", err)
		}
		n.writer = writer
		// THROUGH THE DOMAIN'S OWN READ AUTHORITY, so a level asked for
		// is a level served: the refusal ladder, the coverage probe and
		// the barrier a linearizable read waits through. Built beside
		// the runner rather than here, because the health it refuses on
		// is the same one seat admission reads.
		if n.trackerReader, err = tracker.NewReader(
			e.backends.Store, running.reader); err != nil {
			return fmt.Errorf("engine: tracker reader: %w", err)
		}
	}
	// THE LEXICAL INDEX COVERS BOTH CORPORA, so it is built under EITHER
	// backend rather than under the wiki's.
	//
	// It used to sit inside the block below, which was correct while the
	// index was the knowledge base's alone and silently wrong the moment
	// it stopped being: a company with `tracker.backend: native` and
	// Confluence for its knowledge indexed none of its own work items,
	// served no `search_work_items`, and went on paying the embedding
	// duty for a vector on every one of them — because that duty is armed
	// on `e.native != nil`, which is either backend.
	//
	// BEFORE the block, because the searcher built there takes it.
	n.indexer = search.NewIndexerOver(e.backends.Store,
		lexicalSources(runTracker, wiki))
	// AND THE TRACKER'S OWN SEARCH over it, with its own fan-out rather
	// than the knowledge searcher's: each verb's corpus filter is its own,
	// and neither can widen into the other's.
	n.itemSearch = tracker.NewSearcher(e.backends.Store, itemRanker{
		index: n.indexer,
		fan: &search.FanOut{
			Self:   nodeID,
			Local:  search.NodeScanner{Index: n.indexer},
			Peers:  e.searchPeers(),
			Roster: e.searchRoster,
			Corpus: n.indexer.Corpus,
			Report: e.reportSearch,
			Enter:  e.enterSearch,
		},
	})
	// AND THIS NODE ANSWERS FOR ITS PEERS. Registered here rather than
	// beside the coordinator because they are different jobs on one node:
	// every node with an index answers, whether or not anybody on it ever
	// searches.
	if e.backends.Queue != nil {
		stop, err := search.ServeSlices(runCtx, e.backends.Queue, nodeID,
			search.NodeScanner{Index: n.indexer})
		if err != nil {
			return fmt.Errorf("engine: serve search slices: %w", err)
		}
		n.stopSlices = stop
	}

	if wiki {
		running := sl.Domain(pages.Domain{}.Name())
		if running == nil {
			return fmt.Errorf("engine: this node runs no pages domain, so the " +
				"knowledge base has nowhere to write — the domain is in the " +
				"register and its stream failed to come up")
		}
		var err error
		if n.pages, err = pages.NewStore(pages.Options{
			Publisher: running.publisher, DB: e.backends.Store,
		}); err != nil {
			return fmt.Errorf("engine: pages store: %w", err)
		}
		if n.pageReader, err = pages.NewReader(pages.ReaderOptions{
			DB: e.backends.Store, Log: running.reader,
			Committed: running.runner.Committed,
		}); err != nil {
			return fmt.Errorf("engine: pages reader: %w", err)
		}
		// LIVE off the epoch, not off the company this node booted
		// with: `knowledge.skills_container` is Tier B, and this
		// searcher is built once per node while an apply can move the
		// key underneath it.
		//
		// THE INDEX ITSELF IS NOT BUILT HERE — see below. It covers the
		// tracker too, and gating it on the WIKI's backend left a
		// tracker-native company on Confluence indexing none of its own
		// work items.
		n.searcher = pages.NewSearcher(pages.SearcherOptions{
			Index: n.indexer, SkillsContainer: e.skillsContainer,
			Node:   nodeID,
			Peers:  e.searchPeers(),
			Roster: e.searchRoster,
			Report: e.reportSearch,
			Enter:  e.enterSearch,
		})
	}

	// NO PROJECTOR LOOP HERE ANY MORE. Both native backends are state-log
	// domains, and their apply loops are the state log's own — started
	// with the register above, stopped with it, and reporting their
	// position rather than a hydration flag.
	if n.indexer != nil {
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			n.indexer.Run(runCtx)
		}()
	}

	e.native = n
	// THE RUNTIME IS THE ENGINE'S FROM HERE, so the cleanup above stands
	// down and [Engine.stopNative] — the same shutdown — is what ends it.
	// Set before the two calls below because both reach through e.native:
	// a failure cleanup that ran after they had started would stop loops
	// the engine is about to be asked to stop again.
	started = true
	// AND THE CHART, so the projects this company's units name are objects
	// before any seat files into one. A create takes its key from its
	// project's own counter, so a project that does not exist refuses
	// every write into it — and the first thing a fresh company does is
	// file work. Best effort here for the reason [Engine.applyChart]
	// gives: a failure costs the projects that did not land and nothing
	// else, and the next apply retries them.
	e.applyChart(ctx, c)
	// AND THE CONTAINERS, for the same reason and on the same terms — see
	// [Engine.applyContainers], and the bug it fixes.
	e.applyContainers(ctx, c)
	log.InfoContext(ctx, "native_backends_started",
		"tracker", runTracker, "knowledge", wiki)
	return nil
}

// stopNative ends this node's native backends. Nil-safe, which is the node
// that runs none.
func (e *Engine) stopNative(ctx context.Context) { e.native.shutdown(ctx) }

// shutdown ends everything this runtime started and WAITS for it.
//
// ONE IMPLEMENTATION FOR TWO CALLERS — [Engine.stopNative] and the failure
// path in [Engine.startNative] — because a half-built runtime leaks precisely
// what a built one does: the log's apply loops do not know their node never
// finished booting, and an answerer registered on the broker is a claim on
// buckets a peer is counting on whether or not the node that made it went on
// to serve a seat. A second copy of this order is how one of them stops
// matching the other.
//
// Nil-safe throughout, because the failure path can reach it with the log not
// yet built, no answerer registered and nothing in the wait group.
//
// # Why it takes a context it then strips
//
// The withdrawal below is a call to the broker, so it needs a context that is
// not already cancelled — and BOTH of the contexts in reach routinely are. The
// caller's is a shutdown signal or a failed boot; this runtime's own is
// cancelled by the [native.stop] two lines further down, which is what makes a
// second shutdown a no-op rather than a hang. So the caller's travels here for
// its VALUES — the trace the teardown belongs to — and [context.WithoutCancel]
// is what makes it usable, which is the rule this tree states once: a cleanup
// that inherits a dead context does nothing at all.
func (n *native) shutdown(ctx context.Context) {
	if n == nil {
		return
	}
	if n.stopSlices != nil {
		// FIRST, and before the context that would cancel an answerer
		// mid-scan: a node that has decided to go away must stop
		// claiming buckets its peers are counting on before it stops
		// being able to scan them. Withdrawing costs a coordinator one
		// missing assignment on its next search, where a registration
		// that outlived the scan costs it a silent empty slice it
		// counts as answered.
		_ = n.stopSlices(context.WithoutCancel(ctx))
	}
	n.stop()
	n.done.Wait()
	// THE APPLY LOOPS LAST, after the feeds and the projector that read
	// what they write. A loop stopped first leaves a feed consuming a log
	// nothing is applying, which is not wrong so much as a shutdown that
	// looks like a stall in every log line it produces on the way out.
	n.log.Stop()
}

// NativeHydrated reports whether every native projection this node runs has
// caught up.
//
// THE GATE ON SEAT ACQUISITION. A seat whose mailbox attached first would
// answer "there is no such item" to its own tools — an answer it acts on by
// filing a duplicate or abandoning work it was told to do. A node with no
// native backend is trivially hydrated, which is what a company on Jira and
// Confluence has.
func (e *Engine) NativeHydrated() bool {
	if e.native == nil {
		return true
	}
	// STRICT, because this is seat admission rather than a read: a node
	// that is merely inside the trim floor still serves rows that are
	// behind, and a seat attaching to one acts on them.
	ok, refusal := e.native.log.Established(e.native.run, true)
	if !ok && refusal != "" {
		log.DebugContext(e.native.run, "seat_admission_withheld",
			"reason", string(refusal))
	}
	return ok
}

// SeatsServiceable reports whether this node may KEEP the seats it holds.
//
// THE OPPOSITE DIRECTION FROM [Engine.NativeHydrated], and they fire on
// different classes of fault. Hydration is about a copy that is BEHIND: it
// catches up, so withholding claims is the whole remedy and dropping work in
// hand would be pure loss. This is about a copy that is WRONG — an applier
// halted at a record it cannot decode, an eviction whose peers are dropping
// everything this node writes, rows below a trim floor (or a floor nobody
// could read) with a hole nothing will fill, a checkpoint naming a stream
// that is not this one, an applied prefix frozen past [statelog.StallGrace],
// or a record held past [statelog.DeferralGrace]. A seat left running on any
// of those answers its own tools out of a copy the fleet has already
// abandoned, and D122 is the rule that says it must not.
//
// A LAG IS NEVER ONE OF THEM, which is what the log line below means by
// "wrong rather than behind" — and for as long as the health underneath
// derived "has this node's copy ever been whole" from "is it level this
// instant", that line was false on every firing: one unapplied tracker record
// made a solo node unfit for a heartbeat and moved all seven of its seats.
//
// A node with no native backend is trivially serviceable, which is what a
// company on Jira and Confluence has.
func (e *Engine) SeatsServiceable() (bool, string) {
	if e.native == nil {
		return true, ""
	}
	ok, domain := e.native.log.Healthy(e.native.run)
	if !ok {
		log.WarnContext(e.native.run, "seats_unserviceable",
			"domain", domain,
			"hint", "this node's copy of that domain is wrong rather than "+
				"behind; its seats move to a peer until it recovers")
		return false, domain
	}
	return true, ""
}

// ReplicationStatus is one of this node's replication loops, as the fleet view
// counts them.
//
// # Why one type over two mechanisms
//
// This node runs two kinds of loop and they are genuinely different: a
// PROJECTOR follows a coordination bucket's change feed and holds a revision
// cursor, and a state-log APPLIER consumes an ordered stream and commits a
// checkpoint with the rows it derives. Nothing about their internals is
// shared, and forcing one into the other's status struct would put a revision
// where a position belongs.
//
// What the fleet view asks is not about either mechanism. It asks "how many of
// this node's copies of the company's state have caught up" — the question
// behind an operator's "why is the new node holding no seats" — and a count
// that included only one of the two kinds would answer 1 of 1 for a node whose
// tracker is hours behind. So the shared thing is the QUESTION, and this is
// its shape: a name, whether it is ready, and the detail an operator reads
// when it is not.
type ReplicationStatus struct {
	// Name is the family or the domain, which is what an operator sees.
	Name string

	// Kind is `projection` or `domain`, so the two mechanisms are
	// distinguishable when the detail below is not enough.
	Kind string

	// Ready is the same fact for both: this loop's copy is one a seat's
	// tools may be attached to. It is NOT strict readiness — see
	// [Engine.NativeHydrated], which asks the stricter question that
	// actually gates admission.
	Ready bool

	// Detail is why it is not ready, in the loop's own words. Empty for a
	// loop that is.
	Detail string
}

// NativeStatus is what this node reports about its replication loops, for the
// fleet view. Empty for a node running no native backend.
//
// IT TAKES THE CALLER'S CONTEXT, and that is load-bearing rather than
// idiomatic tidiness: a domain's row is assembled from the broker's own bounds
// and the fleet's published floor, which are two network calls per domain, and
// the caller is a heartbeat with a budget. Reading them under this node's
// long-lived run context instead would let one unreachable broker hold the
// heartbeat open past its own deadline — which is a node that stops
// advertising itself at all, reported by nothing, because it was assembling a
// report about being behind.
func (e *Engine) NativeStatus(ctx context.Context) []ReplicationStatus {
	if e.native == nil {
		return nil
	}
	// EVERY ROW IS A DOMAIN'S NOW. The wiki's projection row went with the
	// projector: a row that could only say hydrated or not has been
	// replaced by a position on a log, which is the same question answered
	// with a distance.
	out := e.native.log.Status(ctx)
	return out
}

// Domains is every state-log domain this build runs, in the fixed order
// [registeredDomains] declares.
//
// EXPOSED SO A CALLER DOES NOT WRITE THE LIST AGAIN. A second copy is what
// makes a fourth domain silently absent from whatever walks it — the shape
// that keeps a fleet comparison, an operator listing or a report certifying
// two domains after somebody added a third.
func (e *Engine) Domains() []statelog.Domain { return registeredDomains() }

// Tracker is this node's tracker read side, or nil.
func (e *Engine) Tracker() *tracker.Reader {
	if e.native == nil {
		return nil
	}
	return e.native.trackerReader
}

// TrackerWriter is this node's tracker write side, or nil.
func (e *Engine) TrackerWriter() *tracker.Writer {
	if e.native == nil {
		return nil
	}
	return e.native.writer
}

// Pages is this node's knowledge read side, or nil.
func (e *Engine) Pages() *pages.Reader {
	if e.native == nil {
		return nil
	}
	return e.native.pageReader
}

// PagesStore is this node's knowledge write side, or nil.
func (e *Engine) PagesStore() *pages.Store {
	if e.native == nil {
		return nil
	}
	return e.native.pages
}

// NativeSearcher is the native knowledge searcher, or nil.
func (e *Engine) NativeSearcher() *pages.Searcher {
	if e.native == nil {
		return nil
	}
	return e.native.searcher
}

// WaitCommitted blocks until this node's tracker applier has consumed through
// a position.
//
// THE READ-YOUR-WRITES PRIMITIVE ON THE LOG, and it takes a POSITION rather
// than a revision because that is what a write answers with: a bucket write
// returns a revision on one family, and a log write returns a place on a
// stream that only compares against the same stream and the same generation.
func (e *Engine) WaitCommitted(ctx context.Context, at statelog.Position) error {
	if e.native == nil || e.native.log == nil || at.Seq == 0 {
		return nil
	}
	// THE POSITION NAMES ITS OWN STREAM, so this resolves the domain from
	// it rather than taking one. That is what a bucket revision could never
	// do — it was a number on a family, and the caller had to say which —
	// and it is why both native backends now settle through one primitive.
	running := e.native.log.Domain(e.native.log.domainOf(at.Stream))
	if running == nil {
		return nil
	}
	return running.runner.WaitCommitted(ctx, at)
}

// startNativeFeeds starts the change feeds, per node.
//
// A FLEET-WIDE GROUP rather than a duty: every node pulls, so a change is
// handled by whichever gets there first and a lease flap on one node does not
// stall the company's notifications. Started here rather than per epoch,
// because a feed follows a DOMAIN and a domain does not change when a company
// revision does.
func (e *Engine) startNativeFeeds(ctx context.Context) {
	if e.native == nil || e.native.log == nil {
		return
	}

	// EVERY SOURCE IS A LOG NOW, and the coordination Feeder this function
	// used to require is gone with the last bucket family. A translator
	// says what it can read and a feed says how to run a durable consumer
	// over one domain's own stream; which estate the records are in was
	// the piece the domains replaced outright.
	type source struct {
		translator changefeed.Translator
		opener     changefeed.Opener
	}
	sources := []source{}
	if running := e.native.log.Domain(tracker.Domain{}.Name()); running != nil {
		// THE LOG IS THE SOURCE, and it is the piece the domain
		// replaced outright: a bucket feed needs a family and a key
		// class, and a log delivery has neither. Its own fleet-wide
		// group over the same stream the applier reads is what derives
		// a wake from a committed record.
		feed, err := trackerFeedSource(running)
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", tracker.Source, "error", err.Error())
		} else {
			sources = append(sources, source{
				translator: tracker.NewTranslator(),
				opener:     feed,
			})
		}
	}
	if running := e.native.log.Domain(pages.Domain{}.Name()); running != nil {
		// THE LOG IS THE SOURCE HERE TOO. A bucket feed needed a family
		// and a key class; a log delivery has neither, and its own
		// fleet-wide group over the same stream the applier reads is
		// what derives a wake from a committed record.
		feed, err := pagesFeedSource(running)
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", pages.Source, "error", err.Error())
		} else {
			sources = append(sources, source{
				translator: pages.NewTranslator(e.skillsContainer),
				opener:     feed,
			})
		}
	}
	for _, src := range sources {
		translator := src.translator
		feed, err := changefeed.New(changefeed.Options{
			Opener: src.opener, Publisher: e.backends.Queue,
			Claims: e.backends.Fleet, Translator: translator,
			Metrics: e.metrics,
		})
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", translator.Source().Name, "error", err.Error())
			continue
		}
		e.native.done.Add(1)
		go func() {
			defer e.native.done.Done()
			// THE NATIVE RUNTIME'S OWN CONTEXT, never the caller's: this
			// goroutine is joined by stopNative, which ends that one.
			//nolint:contextcheck // e.native.run is [context.WithoutCancel] of
			// the boot context: a feed started under the CALLER's would be one
			// [native.shutdown] can never end, and its wait would block for ever.
			if err := feed.Run(e.native.run); err != nil {
				log.ErrorContext(ctx, "changefeed_stopped",
					"source", translator.Source().Name, "error", err.Error(),
					"detail", "native writes still land; nothing is woken by them "+
						"until this node or a peer reopens the feed")
			}
		}()
	}
}

// nativeParsers is what the native backends contribute to the inbound edge.
//
// PER EPOCH, unlike everything else in this file: a parser's one
// company-derived input is the LEAD MAP, and that is the org chart — which
// is exactly what an apply changes. See [Engine.reconcileNative].
func (e *Engine) nativeParsers(c *Company) ([]notify.Parser, []notify.Prompt) {
	if e.native == nil || c == nil {
		return nil, nil
	}
	var (
		parsers []notify.Parser
		prompts []notify.Prompt
	)
	if e.native.writer != nil {
		// NO LEAD MAP AND NO BASE URL. The tracker's own parser reads a
		// record's routing snapshot, which the WRITER resolved at commit
		// — a mention resolved at read time names whoever holds the role
		// later, which is a different person.
		parsers = append(parsers, tracker.NewParser(tracker.ParserOptions{}))
		prompts = append(prompts, tracker.Prompt{})
	}
	if e.native.pages != nil {
		parsers = append(parsers, pages.NewParser(pages.ParserOptions{
			Leads: containerLeads(c.Org), BaseURL: e.publicBase(c),
		}))
		prompts = append(prompts, pages.Prompt{})
	}
	return parsers, prompts
}

// reconcileNative swaps the native parsers for a newly applied epoch.
//
// The same edge every vendor reconciler sits on, and for the same reason: a
// node that kept its boot-time parser would route the new revision's work by
// the old company's org chart — an item filed into a project whose lead
// moved would keep waking the seat that used to own it.
//
// NOT the projectors, the index, the stores or the feeds. Those follow a
// coordination FAMILY, which no company revision changes; rebuilding them
// here would drop this node's projection and re-run a boot reconcile on
// every configuration edit.
//
// There is no retirement branch. Switching `tracker.backend` away from
// native is not a live gesture — the tools, the projector and the feed are
// all built at boot — so a revision that changes it takes effect on
// restart, and the parser staying registered until then is the honest
// state: the records are still there and still reachable.
func (e *Engine) reconcileNative(ctx context.Context, c *Company) {
	if e.native == nil {
		return
	}
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return
	}
	parsers, prompts := e.nativeParsers(c)
	for i, parser := range parsers {
		if err := svc.Replace(parser, prompts[i]); err != nil {
			// THE PREVIOUS PARSER KEEPS RUNNING, the same posture every
			// vendor takes: routing by a stale org chart is worse than
			// the new one and much better than not routing at all.
			log.ErrorContext(ctx, "native_reconcile_failed",
				"source", parser.Source(), "error", err.Error(),
				"detail", "the previous routing is still current")
		}
	}
	e.applyChart(ctx, c)
	e.applyContainers(ctx, c)
}

// applyContainers makes the knowledge containers this company names exist.
//
// # The bug this fixes
//
// [pages.Store.EnsureContainer] HAD NO CALLER. Its own doc says it "runs on
// every boot for every unit's space" and nothing ever ran it, so
// `pages_containers` was empty in every deployment that ever existed: the
// Knowledge rail said "this node knows about no containers yet" beside a
// company whose agents had written pages, `GET /containers` answered `[]`
// for ever, and the one piece of the knowledge base an operator navigates by
// was unreachable from a running engine.
//
// A page merely NAMES its container — the string is on the page's own row —
// so the pages themselves were fine and only the thing that lists them was
// missing. Which is precisely why nothing caught it: every read that takes a
// container as a parameter worked, and only the read that ENUMERATES them was
// empty, which is indistinguishable from a company that has none.
//
// # Where the keys come from
//
// The org chart's spaces, plus the two RESERVED containers. A unit's `space:`
// is its knowledge identity and a seat's own `space:` is the same thing one
// level down — the same pair [chartProjects] reads for the tracker. The
// reserved two are the engine's own: tool skills, and the org root every
// seat's onboarding chain starts at. Both are materialised because the engine
// writes into them itself, and a container the engine writes into and cannot
// list is the same defect one layer in.
//
// # Best effort, and idempotent
//
// A failure is LOGGED rather than raised, exactly as [Engine.applyChart]
// explains: this is one clause of an epoch apply and the rest of it stands
// without it. Running on every apply and every boot is free after the first,
// because EnsureContainer decides nothing when the row it finds already says
// what the chart says — which is the guard its own doc was written around.
func (e *Engine) applyContainers(ctx context.Context, c *Company) {
	store := e.PagesStore()
	if store == nil || c == nil {
		return
	}
	var wrote []string
	for _, want := range chartContainers(c) {
		_, changed, err := store.EnsureContainer(ctx, want.Key, want.Name, want.Purpose)
		if err != nil {
			// EVERY CONTAINER IS ATTEMPTED. One key's refusal must not
			// leave the rest of a company's knowledge base unlistable,
			// and the caller is a reconcile that runs again.
			log.ErrorContext(ctx, "knowledge_container_not_applied",
				"container", want.Key, "error", err.Error(),
				"detail", "pages in it are readable by address and the "+
					"container will not appear in a listing; the next config "+
					"apply or restart retries it")
			continue
		}
		// ONLY WHAT WAS WRITTEN. This runs on every boot and every apply,
		// so the ordinary outcome is that every row already says what the
		// chart says — and a line naming all of them on every restart is
		// a log that reports a company nobody edited as one that changed.
		if changed {
			wrote = append(wrote, want.Key)
		}
	}
	if len(wrote) > 0 {
		log.InfoContext(ctx, "knowledge_containers_applied", "containers", wrote)
	}
}

// chartContainer is one container as the company declares it.
type chartContainer struct {
	Key     string
	Name    string
	Purpose string
}

// chartContainers is every knowledge container this company names.
//
// DE-DUPLICATED ON THE KEY and FIRST DECLARATION WINS, for the reason
// [chartProjects] gives for the same shape: two units legitimately share a
// space, and one container written twice in one pass would contend with
// itself at the broker and log a failure for a configuration that is fine.
//
// The reserved two come LAST, so a company that has somehow named one of them
// as a unit's space keeps the unit's own name on it rather than having the
// engine's generic label overwrite what a founder wrote. The config loader
// refuses that arrangement, so this is the belt to its braces.
func chartContainers(c *Company) []chartContainer {
	if c == nil || c.Config == nil {
		return nil
	}
	var out []chartContainer
	seen := map[string]bool{}
	add := func(key, name, purpose string) {
		// UPPER, which is what a container key is everywhere else: a page
		// carries `ENG` and a chart that wrote `eng` would create a second
		// container no page is in.
		key = strings.ToUpper(strings.TrimSpace(key))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, chartContainer{Key: key, Name: name, Purpose: purpose})
	}
	if c.Org != nil {
		for unit := range c.Org.AllUnits() {
			add(unit.Space, unit.Name, unit.Purpose)
		}
		for role := range c.Org.AllRoles() {
			// A SEAT'S OWN SPACE takes the SEAT's name, which is what a
			// founder naming one on a seat meant — a container for that
			// seat's own writing rather than for its unit's.
			add(role.Space, role.Name, "")
		}
	}
	add(c.Config.RootSpaceKey(), "Company",
		"The organisation's own pages, starting with the Onboarding page every seat reads first.")
	add(c.Config.SkillsContainerKey(), "Tool skills",
		"Prompt fragments the engine injects into a phase. Excluded from knowledge search.")
	return out
}

// applyChart makes the projects this company's chart names exist.
//
// # Why it runs here and not at the first write
//
// A create is a SEQUENCE — it takes the next number from its project's own
// counter and then writes the task — so the project has to be an object before
// anything can be filed in it. Creating it inside the create instead is the
// shape that gives two nodes two projects, two counters and two ENG-1s when
// they file at once. Deriving it from the chart makes it one arbitrated write
// per project, on that project's own subject.
//
// # And on every apply rather than only at boot
//
// A founder adds a unit with a new `project:` key and the seats in it start
// filing immediately. A reconcile that only ran at boot would leave every one
// of those refused with "project X is not on this node" until somebody
// restarted the fleet — a failure whose remedy is invisible from the message.
// After the first node has done it the apply is free: the operation id is
// derived from the revision and the key, so the losers of the broker's
// arbitration write nothing.
//
// # Best effort, and what that costs
//
// A failure is LOGGED rather than raised, because this is one clause of an
// epoch apply and the rest of it — the routing, the models, the tools — is
// still correct without it. What a failure costs is exactly the projects that
// did not land, and the next apply or the next boot retries them.
func (e *Engine) applyChart(ctx context.Context, c *Company) {
	writer := e.TrackerWriter()
	if writer == nil || c == nil || c.Org == nil {
		return
	}
	chart := chartProjects(c.Org)
	if len(chart) == 0 {
		return
	}
	wrote, err := writer.ApplyChart(ctx, tracker.ChartEpochOf(time.Now()), chart)
	if err != nil {
		log.ErrorContext(ctx, "tracker_chart_not_applied",
			"error", err.Error(), "wrote", wrote,
			"detail", "a project the chart names may not exist yet, and work "+
				"filed into it is refused until it does; the next config apply "+
				"or restart retries it")
		return
	}
	if len(wrote) > 0 {
		log.InfoContext(ctx, "tracker_chart_applied", "projects", wrote)
	}
}

// chartProjects is every project the org chart names, with the three fields
// the chart owns.
//
// DE-DUPLICATED ON THE KEY, because two units legitimately share a project —
// that is what [projectLeads] reports on — and one project written twice in
// one pass would contend with itself at the broker and log a failure for a
// configuration that is fine. The FIRST declaration wins, which matches how
// the lead is chosen for the same key.
func chartProjects(o *org.Organization) []tracker.ChartProject {
	var out []tracker.ChartProject
	seen := map[string]bool{}
	add := func(key, name, purpose, unit string) {
		// NORMALIZED THE WAY EVERY OTHER SCOPE IS, through the org's own
		// helper: a project key is compared upper everywhere — a task's
		// key is `ENG-42` and the column is an exact match — and a chart
		// that wrote `eng` would create a project no board could find.
		key = org.NormalizeScope(key)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, tracker.ChartProject{
			Key: key, Name: name, Purpose: purpose, Unit: unit,
		})
	}
	// THE UNIT COLUMN IS THE UNIT'S KEY, never its name. The project's
	// unit is what a task filed into it is filed under, and a task's filed
	// unit is never rewritten — so writing the NAME here filed every item
	// in the company under a spelling that moves the day somebody renames
	// the team, which is exactly what `id:` exists to prevent. The name is
	// the project's own display name beside it, and a reader resolves the
	// key back to the team's current name through the chart.
	for unit := range o.AllUnits() {
		add(unit.Project, unit.Name, unit.Purpose, unit.Key())
	}
	for role := range o.AllRoles() {
		// EVERY seat, not just the root-level ones: `Organization.Roles`
		// holds only the seats declared outside a unit, and a seat
		// nested in one that names its own project would otherwise get
		// no project at all — and file nothing, for ever.
		//
		// A ROLE'S OWN PROJECT takes the ROLE's name, because that is
		// what a founder naming one on a seat meant — a project for that
		// seat's work rather than for its unit's — and its home unit,
		// so the project still says where in the company it sits.
		var home string
		if unit := o.UnitFor(role); unit != nil {
			home = unit.Key()
		}
		add(role.Project, role.Name, "", home)
	}
	return out
}

// projectLeads maps a tracker project to the handle that owns it.
//
// THROUGH [org.Organization.LeadsBy], which is where "who owns this scope"
// lives: the tracker's only contribution is naming the field. The ambiguity
// report is logged rather than refused, because two units sharing a project
// is an ordinary arrangement and only a DISAGREEMENT about the lead matters.
func projectLeads(o *org.Organization) map[string]string {
	if o == nil {
		return nil
	}
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Project },
		OfRole: func(r *org.Role) string { return r.Project },
	})
	for _, unled := range report.Unled {
		// LOUD, because the consequence is invisible: every item in that
		// project naming nobody routes to nobody, which looks exactly like
		// an item nobody filed.
		log.Warn("work_project_has_no_lead", "unit", unled.Unit, "project", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("work_project_lead_ambiguous", "project", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return leads
}

// containerLeads maps a knowledge container to the handle that owns it.
func containerLeads(o *org.Organization) pages.Leads {
	if o == nil {
		return nil
	}
	leads, report := o.LeadsBy(org.Scope{
		OfUnit: func(u *org.Unit) string { return u.Space },
		OfRole: func(r *org.Role) string { return r.Space },
	})
	for _, unled := range report.Unled {
		log.Warn("pages_container_has_no_lead", "unit", unled.Unit, "container", unled.Scope)
	}
	for _, conflict := range report.Ambiguous {
		log.Warn("pages_container_lead_ambiguous", "container", conflict.Scope,
			"declared_by", conflict.DeclaredBy, "chose", conflict.Chose,
			"candidates", conflict.Candidates)
	}
	return pages.Leads(leads)
}

// ProjectOfSeat is one seat's home project, from a chart.
//
// EXPORTED because two surfaces need the same answer and a second walk is how
// one stops matching the other: the seat tools resolve it to default a create's
// container, and the API resolves it to scope `preset=my_queue`'s second arm —
// and a queue scoped to a different project from the one a create files into
// is the shape a person cannot diagnose from either screen.
func ProjectOfSeat(o *org.Organization, handle string) string {
	return scopeOfSeat(o, handle,
		func(u *org.Unit) string { return u.Project },
		func(r *org.Role) string { return r.Project })
}

// scopeOfSeat is the project or container a seat files into: its own, else
// its unit's, else its nearest ancestor's.
//
// THE WALK IS UPWARD, so a seat in a team with no space of its own writes in
// its department's rather than being told to name one. A seat with none
// anywhere returns empty, and the tool refuses rather than guessing — which
// is right: a page filed into a container nobody chose is one nobody finds.
func scopeOfSeat(o *org.Organization, handle string, of func(*org.Unit) string,
	own func(*org.Role) string) string {
	if o == nil || handle == "" {
		return ""
	}
	for role := range o.AllRoles() {
		if role.Handle() != handle {
			continue
		}
		if scope := strings.TrimSpace(own(role)); scope != "" {
			return scope
		}
		break
	}
	for unit := range o.AllUnits() {
		for _, role := range unit.Roles {
			if role.Handle() != handle {
				continue
			}
			if scope := ancestorScope(o, unit, of); scope != "" {
				return scope
			}
			return ""
		}
	}
	return ""
}

// ancestorScope walks a unit and its ancestors for the first scope declared.
func ancestorScope(o *org.Organization, unit *org.Unit, of func(*org.Unit) string) string {
	for u := unit; u != nil; u = parentOf(o, u) {
		if scope := strings.TrimSpace(of(u)); scope != "" {
			return scope
		}
	}
	return ""
}

// parentOf finds a unit's parent, or nil at the top.
func parentOf(o *org.Organization, child *org.Unit) *org.Unit {
	for unit := range o.AllUnits() {
		for _, candidate := range unit.Children {
			if candidate == child {
				return unit
			}
		}
	}
	return nil
}

// skillDetector answers whether a page body is a tool skill, for the
// projection's derived flag.
type skillDetector struct{}

// IsSkill reports a body that parses as a tool skill.
//
// THROUGH THE SKILLS PACKAGE'S OWN ADMISSION TEST, so the projected flag
// means exactly what the registry means by it: a page the sync would admit.
// A second heuristic here would let one page be a skill to the projection
// and prose to the loader — and the disagreement is silent, because each
// half is self-consistent.
func (skillDetector) IsSkill(body string) bool { return skills.IsSkill(body) }

// ---- what a seat's tools are given ------------------------------------- //

// workDeps is the tracker half of a seat's builtin surface, per epoch.
//
// The READER AND WRITER are this node's, and do not change with a revision;
// the DEFAULT PROJECT is the org chart's, and does. Both halves nil omits
// all five tools, which is what a company on Jira has — and omitting them is
// the point: a seat offered a tool against a tracker its company does not
// run would reach for it and fail at the call.
func (e *Engine) workDeps(c *Company) builtin.WorkDeps {
	if e.native == nil || e.native.trackerReader == nil || e.native.writer == nil {
		return builtin.WorkDeps{}
	}
	return builtin.WorkDeps{
		Reader: e.native.trackerReader,
		// ONE WRITER PER ACTOR, derived from the turn's own seat: the
		// tracker's rule is that a writer acts as exactly one party, and
		// the party here is the immutable seat the tool surface bound
		// rather than anything a model can name.
		Writer: func(actor builtin.Actor) builtin.WorkWriter {
			return e.native.writer.As(actor.Handle, actor.Kind, tracker.Provenance{
				TurnID: actor.TurnID, Chain: actor.Chain,
			})
		},
		// AND THE PROJECT SETTINGS, which every surface has rather than
		// the operator's alone: declaring a tag is open to every seat by
		// design, and a create refuses a label the project has not
		// declared — so a seat without this writer could never use the
		// `labels` argument on the tools it already holds. The authority
		// for every other facet is resolved per call.
		ProjectWriter: func(actor builtin.Actor) builtin.ProjectWriter {
			return e.native.writer.As(actor.Handle, actor.Kind, tracker.Provenance{
				TurnID: actor.TurnID, Chain: actor.Chain,
			})
		},
		// AND THE DEPENDENCY SEQUENCE, which is the same writer in its
		// third shape: a dependency is two commits on two subjects, so
		// it needs the replicated estate to check its counterparties
		// before the first of them — and this writer has one.
		Dependencies: func(actor builtin.Actor) builtin.WorkDepender {
			return e.native.writer.As(actor.Handle, actor.Kind, tracker.Provenance{
				TurnID: actor.TurnID, Chain: actor.Chain,
			})
		},
		Merges: func(actor builtin.Actor) builtin.WorkMerger {
			return e.native.writer.As(actor.Handle, actor.Kind, tracker.Provenance{
				TurnID: actor.TurnID, Chain: actor.Chain,
			})
		},
		// THE RANKED SEARCH, which reads and therefore takes no actor:
		// the corpus is the same for everybody and there is nothing to
		// attribute. Nil where this node has no index, and the tool is
		// then not advertised at all.
		Search:   WorkSearcher(e),
		Mentions: seatMentions{org: c.Org},
		// THE ROSTER, read PER CALL for the reason the default project
		// and the unit seam are: a seat's tools are cloned into its
		// lease, an apply does not rebuild the clone, and a captured
		// chart would refuse a colleague who joined this morning and
		// admit one who left.
		Seats: func() []colleague.Seat {
			return builtin.Corpus(e.Company().Org)
		},
		// AND THE UNIT SEAM, read per call for the reason the default
		// project is: a seat's tools are cloned into its lease, an apply
		// does not rebuild the clone, and a captured chart would render
		// a project's unit against an org that has since moved.
		Units: liveUnits{engine: e},
		// PER CALL against the epoch current when the tool runs, not
		// against the one that equipped it: a seat's tools are cloned
		// into its lease and an apply does not rebuild the clone, so a
		// captured project would outlive the org chart that named it.
		DefaultProject: func(handle string) string {
			return ProjectOfSeat(e.Company().Org, handle)
		},
		// THE LEAD MAP IS READ PER CALL against the epoch current when the
		// tool runs, for the reason the default project is: a seat's
		// tools are cloned into its lease, an apply does not rebuild the
		// clone, and a captured map would route a change by the org chart
		// that has since moved.
		Leads: liveLeads{engine: e},
		Now:   func() time.Time { return time.Now().UTC() },
		Zone:  c.Config.Tracker.Native.Location(),
		Await: e.WaitCommitted,
	}
}

// ChartUnits is an org chart as the tracker's unit seam.
//
// THE ONE IMPLEMENTATION, here because this package is where a concrete thing
// is matched to a seam: the tracker holds no org — the applier may not read
// one, since two nodes briefly on different epochs would write different rows
// — so a stored unit is resolved at READ time, and every surface that renders,
// filters or writes one has to reach the same answer.
//
// THROUGH [org.Organization.UnitByRef], which is that answer: a stored unit
// carries either spelling of a unit's identity — its id where the chart gave
// it one, its name where it did not, and whichever was current when the row
// was written. Resolving through [org.Organization.Unit] instead matched the
// name EXACTLY, so `unit: engineering` on a company with a unit named
// "Engineering" was refused with "This company has no team", and a company
// that gave its units ids could not resolve one at all.
func ChartUnits(o *org.Organization) tracker.Units { return chartUnits{org: o} }

type chartUnits struct{ org *org.Organization }

func (c chartUnits) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	if c.org == nil {
		return tracker.ChartUnit{}, false
	}
	unit := c.org.UnitByRef(ref)
	if unit == nil {
		return tracker.ChartUnit{}, false
	}
	lead := tracker.LeadRef{}
	// THE EFFECTIVE LEAD, which is the one inherited from an ancestor
	// where this unit declares none — because that is who actually hears
	// about the project's work, and rendering `none` beside a unit whose
	// parent has a lead sends a founder looking for a gap there is not.
	if role := c.org.EffectiveLead(unit); role != nil {
		// THE SEAT'S OWN HANDLE, through the accessor every other namer
		// of a seat goes through. Slugifying the display name here was a
		// SECOND derivation, and it disagreed with the first on every
		// seat whose operator declared a handle: a unit led by "Ada
		// Okonkwo" with `handle: ada` resolved to `ada-okonkwo`, which
		// is nobody — so the lead could not be looked up in the chart,
		// filtered on, asked, or opened as a person.
		lead.Handle = role.Handle()
		lead.Kind = tracker.AuthorAgent
		if role.IsHuman() {
			lead.Kind = tracker.AuthorHuman
		}
	}
	// THE KEY AND THE NAME, because the callers ask in both directions:
	// what a write stores is the key ([org.Unit.Key]), so that a rename
	// does not move the work, and what a screen reads is the name.
	return tracker.ChartUnit{Key: unit.Key(), Name: unit.Name, Lead: lead}, true
}

// AllUnits is the whole chart, for the board — see [tracker.Units].
//
// EVERY UNIT, in the org's own walk order, each with the lead its rows would
// route to: this is the same answer [chartUnits.ResolveUnit] gives one
// reference at a time, and two derivations of one unit's identity is how the
// board and the filter come to disagree about which team a row belongs to.
func (c chartUnits) AllUnits() []tracker.ChartUnit {
	if c.org == nil {
		return nil
	}
	var out []tracker.ChartUnit
	for unit := range c.org.AllUnits() {
		// THROUGH THE RESOLVER, by the unit's own key, so the pair
		// cannot drift: whatever ResolveUnit says a unit's key, name and
		// lead are is what the enumeration says too.
		if resolved, found := c.ResolveUnit(unit.Key()); found {
			out = append(out, resolved)
		}
	}
	return out
}

// liveUnits resolves against the epoch current when the tool RUNS.
type liveUnits struct{ engine *Engine }

func (l liveUnits) ResolveUnit(ref string) (tracker.ChartUnit, bool) {
	return ChartUnits(l.engine.Company().Org).ResolveUnit(ref)
}

func (l liveUnits) AllUnits() []tracker.ChartUnit {
	return ChartUnits(l.engine.Company().Org).AllUnits()
}

// liveSeats resolves a people field's value to exactly one handle, against the
// CURRENT epoch.
//
// EXACTLY ONE, and an ambiguous spelling is the same answer as an unknown one:
// both mean "this does not name a person", which is the only thing a stored
// value can be written from. A field holding a handle nobody has is a field
// every filter on it misses, silently, for as long as the value is there.
//
// Per call for the reason every other live seam here is: the writer outlives a
// revision, and a captured chart would admit a colleague who has left.
type liveSeats struct{ engine *Engine }

func (l liveSeats) ResolveSeat(ref string) (string, bool) {
	found := colleague.Resolve(ref, builtin.Corpus(l.engine.Company().Org))
	if len(found) != 1 {
		return "", false
	}
	return found[0].Seat.Handle, true
}

// liveLeads resolves a wake's two fallbacks against the CURRENT epoch.
type liveLeads struct{ engine *Engine }

// ProjectLead is who hears about unassigned work in a project.
func (l liveLeads) ProjectLead(project string) string {
	if project == "" {
		return ""
	}
	return projectLeads(l.engine.Company().Org)[strings.ToUpper(project)]
}

// UnitLead is who hears about work routed to a unit.
func (l liveLeads) UnitLead(unit string) string {
	return UnitLeadOf(l.engine.Company().Org, unit)
}

// UnitLeadOf is who hears about work routed to a unit, named by the unit's ID
// or by its NAME.
//
// BOTH SPELLINGS, which is the set [org.Unit.ID]'s own doc promises a unit is
// matched by. Matching the id ALONE — which this did — resolved a lead on no
// company that had not given its units ids: `id` is optional, the shipped
// example sets none, and `u.ID` is then the empty string, which equals no
// routing unit any writer has ever stored. So the fallback that reaches a
// unit's lead when a change named nobody else reached nobody, on every
// default company, and looked exactly like a unit whose lead is unset.
//
// The name is what a row holds today and the id is what it holds the moment a
// founder adds one; a task filed before that keeps the name, which is what
// [tracker.Task.FiledUnit] being a record of what was true means.
//
// THROUGH [org.Organization.UnitByRef] rather than a walk of its own, which is
// what makes "both spellings" one rule rather than a claim each reader
// repeats. The private loop this had folded with [strings.EqualFold] while the
// chart claims a unit key under a different fold, so the two disagreed over
// characters that are real in a team name.
//
// A UNIT THAT EXISTS AND LEADS NOBODY answers empty, which is not the same as
// a unit nothing names — and the resolver draws that line once, for every
// caller.
func UnitLeadOf(o *org.Organization, unit string) string {
	if o == nil {
		return ""
	}
	found := o.UnitByRef(unit)
	if found == nil {
		return ""
	}
	if lead := o.EffectiveLead(found); lead != nil {
		return lead.Handle()
	}
	return ""
}

// pageDeps is the knowledge half, on the same terms.
func (e *Engine) pageDeps(c *Company) builtin.PageDeps {
	if e.native == nil || e.native.pageReader == nil || e.native.pages == nil {
		return builtin.PageDeps{}
	}
	return builtin.PageDeps{
		Reader:   e.native.pageReader,
		Writer:   e.native.pages,
		Mentions: seatMentions{org: c.Org},
		DefaultContainer: func(handle string) string {
			return scopeOfSeat(e.Company().Org, handle,
				func(u *org.Unit) string { return u.Space },
				func(r *org.Role) string { return r.Space })
		},
		// THE TWO CONTAINERS A SEAT MAY NOT WRITE TO DIRECTLY: the
		// tool-skills container, whose pages are machinery the sync
		// publishes, and the org root, which holds the onboarding tree.
		// Read off the CURRENT epoch for the reason the defaults are —
		// and refused by name at the call rather than silently landing
		// somewhere every search excludes.
		Reserved: reservedContainers(c.Config),
		Await:    e.WaitCommitted,
	}
}

// reservedContainers are the containers a seat's own writes may not target.
func reservedContainers(cfg *config.Company) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, key := range []string{cfg.SkillsContainerKey(), cfg.RootSpaceKey()} {
		if key = strings.TrimSpace(key); key != "" {
			out = append(out, key)
		}
	}
	return out
}

// seatMentions resolves the handles a body names to the seats that exist.
//
// THE INTERSECTION IS THE WHOLE JOB. [notify.Mentions] is deliberately
// permissive — it yields `@here`, `@all` and every handle nobody has —
// because filtering there would make the grammar know the company. Here is
// where the company is known, so here is where a name that is not a seat is
// dropped: a comment naming an outsider must not produce a notification
// nobody can deliver.
type seatMentions struct{ org *org.Organization }

// LiveLeads and LiveUnits are the chart seams a surface outside this package
// needs, resolved PER CALL against the epoch current when the tool runs.
//
// EXPORTED BECAUSE THE OPERATOR SURFACE WENT WITHOUT THEM, and their absence
// was invisible rather than harmless: with no Leads an operator filing an
// unassigned task woke nobody at all — the lead fallback is precisely what
// catches a task naming nobody — and with no Units every project that surface
// listed rendered as belonging to no team.
func LiveLeads(e *Engine) tracker.Leads { return liveLeads{engine: e} }

// LiveUnits is the tracker's unit seam over the engine's current chart.
func LiveUnits(e *Engine) tracker.Units { return liveUnits{engine: e} }

// LiveMentions resolves @-mentions against the chart CURRENT when the comment
// is written, rather than the one that built the caller.
//
// The seat path captures its org deliberately — a seat's tools are cloned into
// its lease and rebuilt on an apply — but a surface built once at startup has
// no such rebuild, so it reads the engine per call. It is exported because the
// OPERATOR MCP needs the same rule and there must not be a second copy of it:
// built without one, that surface wrote comments whose @-mentions resolved to
// nothing and woke nobody, while its own tool description promised otherwise.
func LiveMentions(e *Engine) builtin.MentionResolver {
	return liveMentions{engine: e}
}

type liveMentions struct{ engine *Engine }

func (m liveMentions) Mentions(text string) []string {
	c := m.engine.Company()
	if c == nil {
		return nil
	}
	return seatMentions{org: c.Org}.Mentions(text)
}

// Mentions returns the handles this text addresses that are seats here.
func (m seatMentions) Mentions(text string) []string {
	if m.org == nil {
		return nil
	}
	var out []string
	for _, name := range notify.Mentions(text) {
		// A HUMAN SEAT COUNTS. `crewlet` is a transport like any other
		// on this backend — a person mentioned on a work item is
		// notified through whatever surface their contact declares —
		// so the lookup is over every seat, not only the agents.
		if m.org.SeatByHandle(name) != nil {
			out = append(out, name)
		}
	}
	return out
}

// nudgeSkills asks the skill sync to re-read the tool-skill container.
//
// # Why it is a nudge and not a read
//
// It is called from the page projection's post-commit hook, which runs on the
// projector's own loop, so doing the read here would hold every subsequent
// change behind a page walk and a registry replace. So it is a request to the
// node's one sync loop, which coalesces however many arrive into one walk: the
// read is wholesale, and one re-read after N changes is the same answer as N
// of them.
//
// # Why this backend needs no fleet nudge
//
// Every node applies the page log itself, so every node's own projection sees
// a skill page move and calls this. The broadcast the Confluence path needs
// exists because a vendor webhook reaches one node; a log every node applies
// already reaches all of them.
//
// NAMED AS THE NATIVE BACKEND'S, because the projection outlives an apply that
// moves the company's skills to Confluence, and the loop ignores a refresh
// from a backend its source is not on.
func (e *Engine) nudgeSkills() { e.skillSync.Refresh(string(config.KnowledgeNative)) }

// walkNativeSkills reads the tool-skill container out of this node's own page
// projection.
//
// # It waits for hydration first
//
// A walk over a container this node has not applied through is a PARTIAL set,
// and the registry replaces wholesale, so reading early would silently delete
// every skill the walk did not reach. The wait is cancelled with the walk: a
// source change or a stop ends it.
func (e *Engine) walkNativeSkills(ctx context.Context, container string) ([]skills.Page, error) {
	if !e.awaitHydration(ctx) {
		return nil, ctx.Err()
	}
	// STALE, and it is the strongest level this walk could HONESTLY name,
	// not a saving.
	//
	// What the walk must not do is rebuild the registry without the change
	// that asked for the rebuild. It cannot: [Engine.nudgeSkills] is called
	// from the applier's POST-COMMIT hook, so by the time the sync loop
	// runs, the record that woke it is already in this node's own committed
	// prefix, which is exactly what a stale read serves. The causality is
	// LOCAL, so no barrier and no high-water mark buys anything here.
	found, err := e.native.pageReader.SkillPages(ctx, container,
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return nil, err
	}
	out := make([]skills.Page, 0, len(found))
	for _, page := range found {
		out = append(out, skills.Page{
			ID: page.ID, Title: page.Title,
			Version: page.Version, Text: page.Body,
		})
	}
	return out, nil
}

// awaitHydration blocks until this node's own applied rows have caught up,
// reporting false if ctx ended first.
func (e *Engine) awaitHydration(ctx context.Context) bool {
	ticker := time.NewTicker(hydrationPoll)
	defer ticker.Stop()
	for !e.NativeHydrated() {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
	return true
}

// hydrationPoll is how often the first skill read checks whether the
// projection has caught up.
//
// A quarter-second. The wait is bounded by a boot reconcile, which is
// hundreds of milliseconds on an ordinary company and minutes on a large one
// — so the cost of polling is a handful of atomic reads either way, and a
// condition variable here would be a second thing to keep correct for a wait
// that happens once per process.
const hydrationPoll = 250 * time.Millisecond

// searchPeers is the fleet half of the knowledge search's fan-out.
//
// NIL WHEN THERE IS NO QUEUE, which is a legal deployment rather than a
// degradation: an embedded engine with no broker holds the whole corpus and
// takes every bucket, exactly as a single node does.
func (e *Engine) searchPeers() search.Peers {
	if e.backends == nil || e.backends.Queue == nil {
		return nil
	}
	return search.Broker{Queue: e.backends.Queue}
}

// searchRoster answers which nodes may be given a bucket range.
//
// FROM THE LEASE VIEW rather than from the positions register, and the two
// differ in exactly the way that matters here: a position row is held by every
// node the trim has to wait for, INCLUDING one that has been gone for hours,
// while a lease expires. A dead node in the roster costs every search on this
// node a partial answer for as long as its row survives.
func (e *Engine) searchRoster(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, nil
	}
	leases, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(leases))
	for _, lease := range leases {
		if profile, ok := placement.FromLease(lease); ok {
			out = append(out, profile.ID)
		}
	}
	return out, nil
}

// reportSearch counts what one answer covered.
//
// THE ONLY THING THAT MAKES THREE ALARMS ABLE TO FIRE. `search_slow`,
// `search_degraded` and `search_scoped` are each a property of the answers a
// node gave, and an alarm whose input nobody records is permanently silent —
// which looks exactly like a system with nothing wrong.
func (e *Engine) reportSearch(answer search.Answer, took time.Duration) {
	if e.metrics == nil {
		return
	}
	e.metrics.Observe(metrics.TrackerSearchScanDuration, took,
		metrics.Attrs{"path": "interactive", "rung": "hybrid"})
	coverage := "complete"
	if answer.Partial() {
		coverage = "scoped"
	}
	semantic := "full"
	if answer.SemanticSkipped {
		semantic = "skipped"
	}
	e.metrics.Add(metrics.TrackerSearchAnswers, 1,
		metrics.Attrs{"coverage": coverage, "semantic": semantic})
}

// enterSearch counts one scan in, and its return counts it out.
//
// THE LEVEL NOTHING ELSE CAN SAMPLE. Every scan figure this engine publishes —
// the budget the prefetch is held to, the interactive target, the whole
// supported-corpus table — was measured with ONE reader on an idle node, and a
// node answering nine at once is on a different row of that table. A duration
// histogram cannot say which row: it records what the scans cost without
// recording how many were competing for the disk while they did.
//
// SET RATHER THAN ADDED, on both edges, because it is a gauge: the value is
// the count in flight at the moment a collector reads it, and the rolling
// window's peak over a day is the busiest this node got.
func (e *Engine) enterSearch() func() {
	if e.metrics == nil {
		return func() {}
	}
	e.metrics.Set(metrics.TrackerSearchConcurrency,
		float64(e.searching.Add(1)), nil)
	return func() {
		e.metrics.Set(metrics.TrackerSearchConcurrency,
			float64(e.searching.Add(-1)), nil)
	}
}

// LeadsProjectOf answers whether a handle leads the unit that owns a project.
//
// THE UNIT'S EFFECTIVE LEAD, inherited from an ancestor where the unit
// declares none — because that is who actually answers for the project's work,
// and refusing somebody whose parent unit's lead they are would send them
// looking for an authority nobody holds.
//
// A PROJECT THIS BUILD CANNOT RESOLVE ANSWERS FALSE, which is the conservative
// direction: the policy edit is then refused naming the project rather than
// made by whoever asked.
//
// HERE RATHER THAN AT EITHER CALLER, because both surfaces ask it — a seat's
// write_project and an operator's — and two copies of "who leads this" is two
// chances for the seat surface and the operator surface to answer the same
// question differently about the same person.
func LeadsProjectOf(e *Engine) builtin.LeadsProject {
	return func(_ context.Context, actor, project string) bool {
		c := e.Company()
		if c == nil || c.Org == nil || actor == "" || project == "" {
			return false
		}
		key := tracker.ProjectKey(project)
		for unit := range c.Org.AllUnits() {
			if tracker.ProjectKey(unit.Project) != key {
				continue
			}
			if lead := c.Org.EffectiveLead(unit); lead != nil &&
				lead.Handle() == actor {

				return true
			}
		}
		// A ROLE'S OWN PROJECT IS LED BY THAT ROLE. A seat that names
		// its own project decides how it is filed, which is the only
		// reading of "the lead" a one-seat project has.
		for role := range c.Org.AllRoles() {
			if tracker.ProjectKey(role.Project) == key &&
				role.Handle() == actor {

				return true
			}
		}
		return false
	}
}
