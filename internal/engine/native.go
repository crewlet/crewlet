package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/statelog"
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

	// wiki is the pages projector, which is still a coordination family.
	// It adopts the state log in its own step; until then this node runs
	// one projector and one log side by side, and that is visible here
	// rather than hidden behind a common name.
	wiki *projection.Projector

	// indexer keeps the lexical search index behind the page projection.
	indexer *projection.Indexer

	// writer is the tracker's write authority and pages the wiki's.
	writer *tracker.Writer
	pages  *pages.Store

	// trackerReader and pageReader are the read paths.
	trackerReader *tracker.Reader
	pageReader    *pages.Reader

	// searcher answers the knowledge seam natively.
	searcher *pages.Searcher

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

	if runTracker {
		// THE LOG COMES UP BEFORE ANYTHING READS IT, and its own
		// context outlives this call: an apply loop started under the
		// caller's context is one stopNative can never end.
		sl, err := e.startStateLog(ctx, boot, nodeID, c.Epoch())
		if err != nil {
			cancel()
			return err
		}
		n.log = sl
		writer, err := tracker.NewWriter(tracker.WriterDeps{
			Publisher: sl.Domain(tracker.Domain{}.Name()).publisher,
			DB:        e.backends.Store,
			Claims:    e.backends.Coord,
			NodeID:    nodeID,
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
		})
		if err != nil {
			sl.Stop()
			cancel()
			return fmt.Errorf("engine: tracker writer: %w", err)
		}
		n.writer = writer
		n.trackerReader = tracker.NewReader(e.backends.Store)
	}
	if wiki {
		p, err := projection.New(projection.Options{
			Documents: e.backends.Fleet, DB: e.backends.Store,
			// The registry re-reads its container when a skill page
			// moves. Natively there is no page webhook to hang that
			// off — the change feed deliberately drops those changes
			// — so the APPLY is what notices; see
			// [pages.NewApplier].
			Applier: pages.NewApplier(skillDetector{}, e.nudgeSkills),
		})
		if err != nil {
			cancel()
			return fmt.Errorf("engine: pages projection: %w", err)
		}
		n.wiki = p
		if n.pages, err = pages.NewStore(pages.Options{Documents: e.backends.Fleet}); err != nil {
			cancel()
			return fmt.Errorf("engine: pages store: %w", err)
		}
		if n.pageReader, err = pages.NewReader(pages.ReaderOptions{
			DB: e.backends.Store, Hydrated: p.Hydrated,
		}); err != nil {
			cancel()
			return fmt.Errorf("engine: pages reader: %w", err)
		}
		n.indexer = projection.NewIndexer(e.backends.Store)
		// LIVE off the epoch, not off the company this node booted
		// with: `knowledge.skills_container` is Tier B, and this
		// searcher is built once per node while an apply can move the
		// key underneath it.
		n.searcher = pages.NewSearcher(pages.SearcherOptions{
			Index: n.indexer, SkillsContainer: e.skillsContainer,
		})
	}

	for _, p := range []*projection.Projector{n.wiki} {
		if p == nil {
			continue
		}
		n.done.Add(1)
		go func() {
			defer n.done.Done()
			if err := p.Run(runCtx); err != nil {
				log.ErrorContext(runCtx, "projection_stopped", "family", string(p.Family()),
					"error", err.Error(),
					"detail", "this node stops claiming seats for that backend; "+
						"its projection is going stale")
			}
		}()
	}
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
	if e.native.wiki != nil && !e.native.wiki.Hydrated() {
		return false
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
	var out []ReplicationStatus
	if e.native.wiki != nil {
		got := e.native.wiki.Status()
		row := ReplicationStatus{
			Name: string(got.Family), Kind: "projection", Ready: got.Hydrated,
		}
		if !row.Ready {
			row.Detail = fmt.Sprintf("hydrating: %d change(s) buffered at "+
				"revision %d", got.Pending, got.Revision)
		}
		out = append(out, row)
	}
	out = append(out, e.native.log.Status(ctx)...)
	return out
}

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

// WaitApplied blocks until this node has applied a family's revision.
//
// The read-your-writes primitive a REST write and a tool call use before they
// answer. A caller with no projector for that family returns at once, which
// is correct: there is nothing to wait for.
func (e *Engine) WaitApplied(ctx context.Context, family coord.Family, revision uint64) error {
	if e.native == nil || revision == 0 {
		return nil
	}
	if p := e.native.wiki; p != nil && p.Family() == family {
		return p.WaitApplied(ctx, revision)
	}
	return nil
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
	running := e.native.log.Domain(tracker.Domain{}.Name())
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
// because the feed follows a family and a family does not change when a
// company revision does.
func (e *Engine) startNativeFeeds(ctx context.Context) {
	if e.native == nil {
		return
	}
	feeder, ok := e.backends.Fleet.(coord.Feeder)
	if !ok {
		// A coordination backend with no feeds. Every native write still
		// lands and every board still reads; nothing is woken by one,
		// which is a degradation worth saying out loud.
		log.WarnContext(ctx, "native_feeds_unavailable",
			"detail", "this coordination backend serves no change feeds, so a "+
				"native write reaches the record but wakes nobody")
		return
	}

	// THE FAMILY AND THE KEY CLASS ARE NAMED HERE, by the package that
	// wires estates to consumers. A translator says what it can read and a
	// feed says how to run a durable consumer; which bucket the records are
	// in is neither one's business, and it is the piece a log domain
	// replaces outright.
	//
	// The class is the CHANGE class in both cases, never the head: a bucket
	// keeps one revision per key, so rewriting a key terminates an un-acked
	// message with nothing anywhere saying a wake was lost.
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
	if e.native.pages != nil {
		sources = append(sources, source{
			translator: pages.NewTranslator(e.skillsContainer),
			opener:     changefeed.DocumentSource(feeder, coord.FamilyPages, pages.ClassChange),
		})
	}
	for _, src := range sources {
		translator := src.translator
		feed, err := changefeed.New(changefeed.Options{
			Opener: src.opener, Publisher: e.backends.Queue,
			Claims: e.backends.Fleet, Translator: translator,
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
		Mentions: seatMentions{org: c.Org},
		// PER CALL against the epoch current when the tool runs, not
		// against the one that equipped it: a seat's tools are cloned
		// into its lease and an apply does not rebuild the clone, so a
		// captured project would outlive the org chart that named it.
		DefaultProject: func(handle string) string {
			return scopeOfSeat(e.Company().Org, handle,
				func(u *org.Unit) string { return u.Project },
				func(r *org.Role) string { return r.Project })
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
		Await: func(ctx context.Context, revision uint64) error {
			return e.WaitApplied(ctx, coord.FamilyPages, revision)
		},
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
// A walk over a half-projected container is a PARTIAL set, and
// [Engine.SyncSkills] replaces wholesale — so reading early would silently
// delete every skill the walk did not reach, and the next read is whenever
// somebody next edits one.
func (e *Engine) startNativeSkills() {
	if e.native == nil || e.native.wiki == nil || e.native.pageReader == nil {
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

// awaitHydration blocks until this node's projections have caught up,
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
		found, err := reader.SkillPages(ctx, container)
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
