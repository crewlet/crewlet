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
type native struct {
	mu sync.Mutex

	// nodeID is who this node is: the name its own domain consumer takes,
	// the writer stamped on every record it publishes, and what the
	// eviction gate compares against. Held here because three subsystems
	// need it after boot and the bootstrap it came from is not kept.
	nodeID string

	// log is this node's state-log runtime: the tracker's domain and the
	// vector domain, each with its own apply loop and write authority.
	// Nil when the company runs the vendor tracker instead, which is the
	// whole switch — a company on Jira runs no domain at all rather than
	// an empty one.
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

	// stopSlices withdraws this node as an answerer for the fleet's
	// search fan-out. Nil when there is no queue to serve on, which is
	// every embedded engine and every test.
	stopSlices queue.Unsubscribe

	// skillNudge asks the sync worker to re-read the tool-skill
	// container. Buffered by ONE, because the slot means "re-read" rather
	// than "re-read once per change": the read is wholesale, so a second
	// pending nudge would buy a second identical walk.
	skillNudge chan struct{}

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
func (e *Engine) startNative(ctx context.Context, boot *config.Bootstrap, c *Company) error {
	if e.backends == nil || e.backends.Store == nil || e.backends.Fleet == nil {
		// A process with no store or no coordination runs no native
		// backend. That is the standalone API's shape, and it is not an
		// error: it serves what it can see.
		return nil
	}
	runTracker := c.Config.TrackerBackendFor() == config.TrackerNative
	wiki := c.Config.KnowledgeBackendFor() == config.KnowledgeNative
	if !runTracker && !wiki {
		return nil
	}
	e.warnIfEphemeral(ctx, boot, runTracker, wiki)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	// THE RESOLVED ID, not the raw field. `node.id` may be absent, a
	// `${VAR}` reference, or come from the environment — and the value
	// three durable things are named after (this node's own consumer, the
	// writer stamped on its records, the eviction gate's key) must be the
	// same one the broker's server name and the presence row already use.
	nodeID, err := config.ResolveNodeID(boot, config.EnvOnly())
	if err != nil {
		cancel()
		return err
	}
	n := &native{
		run: runCtx, stop: cancel, nodeID: nodeID,
		skillNudge: make(chan struct{}, 1),
	}

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
		cancel()
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
			// fact a sprint wake needs that the rows cannot give —
			// `tracker_projects` carries no column for it, because the
			// applier may not read an org — and the caller that needs
			// it is the sprint duty, a fleet singleton on a tick with
			// no tool arguments to carry a seam through.
			Leads: liveLeads{engine: e},
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
			sl.Stop()
			cancel()
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
			sl.Stop()
			cancel()
			return fmt.Errorf("engine: tracker reader: %w", err)
		}
	}
	if wiki {
		running := sl.Domain(pages.Domain{}.Name())
		if running == nil {
			cancel()
			return fmt.Errorf("engine: this node runs no pages domain, so the " +
				"knowledge base has nowhere to write — the domain is in the " +
				"register and its stream failed to come up")
		}
		var err error
		if n.pages, err = pages.NewStore(pages.Options{
			Publisher: running.publisher, DB: e.backends.Store,
		}); err != nil {
			cancel()
			return fmt.Errorf("engine: pages store: %w", err)
		}
		if n.pageReader, err = pages.NewReader(pages.ReaderOptions{
			DB: e.backends.Store, Log: running.reader,
			Committed: running.runner.Committed,
		}); err != nil {
			cancel()
			return fmt.Errorf("engine: pages reader: %w", err)
		}
		n.indexer = search.NewIndexer(e.backends.Store)
		// LIVE off the epoch, not off the company this node booted
		// with: `knowledge.skills_container` is Tier B, and this
		// searcher is built once per node while an apply can move the
		// key underneath it.
		n.searcher = pages.NewSearcher(pages.SearcherOptions{
			Index: n.indexer, SkillsContainer: e.skillsContainer,
			Node:   nodeID,
			Peers:  e.searchPeers(),
			Roster: e.searchRoster,
			Report: e.reportSearch,
			Enter:  e.enterSearch,
		})
		// AND THIS NODE ANSWERS FOR ITS PEERS. Registered here rather
		// than beside the coordinator because they are different jobs
		// on one node: every node with an index answers, whether or not
		// anybody on it ever searches.
		if e.backends.Queue != nil {
			stop, err := search.ServeSlices(runCtx, e.backends.Queue, nodeID,
				search.NodeScanner{Index: n.indexer})
			if err != nil {
				cancel()
				return fmt.Errorf("engine: serve search slices: %w", err)
			}
			n.stopSlices = stop
		}
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
	// AFTER e.native is set, because the worker reads through it.
	e.startNativeSkills()
	// AND THE CHART, so the projects this company's units name are objects
	// before any seat files into one. A create takes its key from its
	// project's own counter, so a project that does not exist refuses
	// every write into it — and the first thing a fresh company does is
	// file work. Best effort here for the reason [Engine.applyChart]
	// gives: a failure costs the projects that did not land and nothing
	// else, and the next apply retries them.
	e.applyChart(ctx, c)
	log.InfoContext(ctx, "native_backends_started",
		"tracker", runTracker, "knowledge", wiki)
	return nil
}

// stopNative ends this node's native backends.
func (e *Engine) stopNative() {
	if e.native == nil {
		return
	}
	if e.native.stopSlices != nil {
		// FIRST, and before the context that would cancel an answerer
		// mid-scan: a node that has decided to go away must stop
		// claiming buckets its peers are counting on before it stops
		// being able to scan them. Withdrawing costs a coordinator one
		// missing assignment on its next search, where a registration
		// that outlived the scan costs it a silent empty slice it
		// counts as answered.
		_ = e.native.stopSlices(context.WithoutCancel(e.native.run))
	}
	e.native.stop()
	e.native.done.Wait()
	// THE APPLY LOOPS LAST, after the feeds and the projector that read
	// what they write. A loop stopped first leaves a feed consuming a log
	// nothing is applying, which is not wrong so much as a shutdown that
	// looks like a stall in every log line it produces on the way out.
	e.native.log.Stop()
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
// everything this node writes, rows below a trim floor with a hole nothing
// will fill, or a record held past [statelog.DeferralGrace]. A seat left
// running on any of those answers its own tools out of a copy the fleet has
// already abandoned, and D122 is the rule that says it must not.
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
	for unit := range o.AllUnits() {
		add(unit.Project, unit.Name, unit.Purpose, unit.Name)
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
			home = unit.Name
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

// scopeOfSeat is the project or container a seat files into: its own, else
// its unit's, else its nearest ancestor's.
//
// THE WALK IS UPWARD, so a seat in a team with no space of its own writes in
// its department's rather than being told to name one. A seat with none
// anywhere returns empty, and the tool refuses rather than guessing — which
// is right: a page filed into a container nobody chose is one nobody finds.
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
// — so a project's chart-owned unit is resolved at READ time, and every
// surface that renders one has to reach the same answer.
func ChartUnits(o *org.Organization) tracker.Units { return chartUnits{org: o} }

type chartUnits struct{ org *org.Organization }

func (c chartUnits) ResolveUnit(name string) (string, tracker.LeadRef, bool) {
	if c.org == nil {
		return "", tracker.LeadRef{}, false
	}
	unit := c.org.Unit(name)
	if unit == nil {
		return "", tracker.LeadRef{}, false
	}
	lead := tracker.LeadRef{}
	// THE EFFECTIVE LEAD, which is the one inherited from an ancestor
	// where this unit declares none — because that is who actually hears
	// about the project's work, and rendering `none` beside a unit whose
	// parent has a lead sends a founder looking for a gap there is not.
	if role := c.org.EffectiveLead(unit); role != nil {
		lead.Handle = org.Slugify(role.Name)
		lead.Kind = tracker.AuthorAgent
		if role.IsHuman() {
			lead.Kind = tracker.AuthorHuman
		}
	}
	return unit.Name, lead, true
}

// liveUnits resolves against the epoch current when the tool RUNS.
type liveUnits struct{ engine *Engine }

func (l liveUnits) ResolveUnit(name string) (string, tracker.LeadRef, bool) {
	return ChartUnits(l.engine.Company().Org).ResolveUnit(name)
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
	if unit == "" {
		return ""
	}
	chart := l.engine.Company().Org
	if chart == nil {
		return ""
	}
	for u := range chart.AllUnits() {
		if !strings.EqualFold(u.ID, unit) {
			continue
		}
		if lead := chart.EffectiveLead(u); lead != nil {
			return lead.Handle()
		}
		return ""
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

// LiveMentions resolves @-mentions against the chart CURRENT when the comment
// is written, rather than the one that built the caller.
//
// The seat path captures its org deliberately — a seat's tools are cloned into
// its lease and rebuilt on an apply — but a surface built once at startup has
// no such rebuild, so it reads the engine per call. It is exported because the
// OPERATOR MCP needs the same rule and there must not be a second copy of it:
// built without one, that surface wrote comments whose @-mentions resolved to
// nothing and woke nobody, while its own tool description promised otherwise.
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

// warnIfEphemeral says out loud when the company's own records will not
// survive a restart.
//
// # Why a warning and not a refusal
//
// `stream.store_dir` unset selects an in-memory embedded broker, which is
// exactly what a test wants and what a stateless ingress-only node can use.
// The engine cannot tell one of those from an operator who left the field out
// of a deployment, so refusing here would break the two legitimate cases to
// catch the mistake.
//
// # Why it is worth a line at all
//
// The STAKES changed under this field. It has always meant "queued events do
// not survive a restart", which is recoverable — a vendor retries, a schedule
// fires again. With a native backend it means the company's tracker and its
// knowledge base are in that stream: every item ever filed, every page ever
// written, gone on the next restart, with nothing anywhere reporting a loss
// because from the engine's side the company simply has no work.
//
// So the line names what is at stake rather than restating the field, and it
// is an ERROR level rather than a warning: this is data loss on a timer, and
// the only thing standing between an operator and it is noticing.
func (e *Engine) warnIfEphemeral(ctx context.Context, boot *config.Bootstrap, tracker, wiki bool) {
	at := EphemeralRisk(boot, tracker, wiki)
	if at == "" {
		return
	}
	log.ErrorContext(ctx, "native_backend_on_an_ephemeral_stream",
		"at_risk", at,
		"detail", "stream.store_dir is unset, so this node's embedded broker "+
			"keeps its streams in memory — and the company's own records live "+
			"there. "+at+" this company writes is lost on the next restart, "+
			"with nothing reporting a loss because the company will simply "+
			"appear to have no work",
		"fix", "set stream.store_dir in crewlet.yaml, or run tracker.backend "+
			"and knowledge.backend against a vendor that keeps the record")
}

// EphemeralRisk names what an in-memory stream would lose, or "" for none.
//
// SPLIT FROM THE LOG LINE so the rule is testable as a value rather than by
// capturing a logger — which is what the codebase does everywhere the
// question "would this configuration lose data" has a yes/no answer somebody
// might change by accident.
func EphemeralRisk(boot *config.Bootstrap, tracker, wiki bool) string {
	switch {
	case boot == nil:
		return ""
	case boot.Stream.Type == config.StreamNATS:
		// An EXTERNAL cluster persists on its own terms, and this process
		// has no way to know them. Claiming a risk here would train an
		// operator to ignore the line.
		return ""
	case boot.Stream.StoreDir != "":
		// The operator answered the question.
		return ""
	case tracker && wiki:
		return "every work item and every page"
	case tracker:
		return "every work item"
	case wiki:
		return "every page"
	}
	return ""
}

// nudgeSkills asks the sync worker to re-read the tool-skill container.
//
// # Why a signal and not the work
//
// It is called from the projection's POST-COMMIT HOOK, which runs on the
// projector's own loop — so doing the read here would hold every subsequent
// change behind a page walk and a registry replace. And it must not spawn a
// goroutine per call either: an untracked one outlives [Engine.stopNative],
// which would leave it reading a store that is being closed.
//
// So it is a non-blocking send to the one tracked worker below. A send that
// finds the channel full is DROPPED, and that is correct rather than
// convenient: the buffered slot already means "re-read", the read is
// wholesale, and one re-read after N changes is the same answer as N of them.
func (e *Engine) nudgeSkills() {
	if e.native == nil || e.native.skillNudge == nil {
		return
	}
	select {
	case e.native.skillNudge <- struct{}{}:
	default:
	}
}

// startNativeSkills runs the tool-skill sync for the life of this node.
//
// # It reads at boot as well as on change
//
// A node joining a company whose skills were published months ago sees no
// change at all, so a registry fed only by changes would be empty on every
// restart until somebody happened to edit a page.
//
// # And it waits for hydration first
//
// A walk over a container this node has not applied through is a PARTIAL set,
// and [Engine.SyncSkills] replaces wholesale — so reading early would silently
// delete every skill the walk did not reach, and the next read is whenever
// somebody next edits one.
func (e *Engine) startNativeSkills() {
	if e.native == nil || e.native.pageReader == nil {
		return
	}
	e.native.done.Add(1)
	go func() {
		defer e.native.done.Done()
		ctx := e.native.run
		if !e.awaitHydration(ctx) {
			return
		}
		e.syncNativeSkills(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-e.native.skillNudge:
				e.syncNativeSkills(ctx)
			}
		}
	}()
}

// awaitHydration blocks until this node's own applied rows have caught up,
// reporting false if the node stopped first.
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

// syncNativeSkills reads the tool-skill container into the registry.
func (e *Engine) syncNativeSkills(ctx context.Context) {
	c := e.Company()
	if c == nil || e.native == nil || e.native.pageReader == nil {
		return
	}
	reader := e.native.pageReader
	e.syncSkillsFrom(ctx, c, func(ctx context.Context, container string) ([]skills.Page, error) {
		// STALE, and it is the strongest level this walk could
		// HONESTLY name — not a saving.
		//
		// What the walk must not do is rebuild the registry without the
		// change that asked for the rebuild. It cannot: [nudgeSkills]
		// is called from the applier's POST-COMMIT hook, so by the time
		// this worker runs, the record that woke it is already in this
		// node's own committed prefix — which is exactly what a stale
		// read serves. The causality is LOCAL, so no barrier and no
		// high-water mark buys anything here.
		//
		// It read `session` before, which named a wait that never
		// happened: nothing populated [statelog.Query.Session], so the
		// target was the zero position and the read served this same
		// prefix under a stronger name. The behaviour is unchanged and
		// the claim is now true.
		found, err := reader.SkillPages(ctx, container, statelog.ReadStale)
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
	})
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
	leases, err := e.backends.Coord.ListLive(ctx, coord.NodePrefix)
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
// direction: the sprint decision is then refused naming the project rather
// than made by whoever asked.
//
// HERE RATHER THAN AT EITHER CALLER, because both surfaces ask it — a seat's
// write_project and an operator's manage_sprint — and two copies of "who leads
// this" is two chances for the seat surface and the operator surface to answer
// the same question differently about the same person.
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
		// its own project plans its own sprints, which is the only
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
