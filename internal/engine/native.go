package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/projection"
	"github.com/crewlet/crewlet/internal/work"
)

// The native backends, wired.
//
// # What is per-NODE and what is per-EPOCH, and why the split is not obvious
//
// The projectors, the indexer and the change feeds are per-NODE: they follow
// a coordination family, and a family does not change when a company
// revision does. Rebuilding them on an apply would drop every node's
// projection and re-run a boot reconcile on every configuration change,
// which for a company that edits its org chart twice a day is a projection
// that is never hydrated.
//
// The parsers, the prompts and the tool wiring are per-EPOCH, because all
// three depend on the org chart: the lead map a fallback routes through, the
// default project a seat files into, the reserved containers. Those are
// exactly what an apply changes.
//
// # Seat acquisition waits on hydration, and nothing else does
//
// A seat whose mailbox attached before its node's projection was hydrated
// would answer "there is no such item" to its own tools — which is an answer
// it acts on. So [Engine.NativeHydrated] gates the claim, and /ready
// deliberately does not: the control plane's rule is that lag alone never
// sheds a node, and a node that is behind should stop taking new seats
// rather than be declared unhealthy.

// native holds this node's native-backend runtime.
type native struct {
	mu sync.Mutex

	// tracker and wiki are the projectors. Nil when the company runs the
	// vendor backend instead, which is the whole switch: a company on Jira
	// has no work projector at all rather than an empty one.
	tracker *projection.Projector
	wiki    *projection.Projector

	// indexer keeps the lexical search index behind the page projection.
	indexer *projection.Indexer

	// work and pages are the write paths.
	work  *work.Store
	pages *pages.Store

	// workReader and pageReader are the read paths over the projection.
	workReader *work.Reader
	pageReader *pages.Reader

	// searcher answers the knowledge seam natively.
	searcher *pages.Searcher

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
	tracker := c.Config.TrackerBackendFor() == config.TrackerNative
	wiki := c.Config.KnowledgeBackendFor() == config.KnowledgeNative
	if !tracker && !wiki {
		return nil
	}
	e.warnIfEphemeral(ctx, boot, tracker, wiki)

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	n := &native{run: runCtx, stop: cancel}

	if tracker {
		p, err := projection.New(projection.Options{
			Documents: e.backends.Fleet, DB: e.backends.Store,
			Applier: work.NewApplier(),
		})
		if err != nil {
			cancel()
			return fmt.Errorf("engine: work projection: %w", err)
		}
		n.tracker = p
		if n.work, err = work.NewStore(work.Options{Documents: e.backends.Fleet}); err != nil {
			cancel()
			return fmt.Errorf("engine: work store: %w", err)
		}
		if n.workReader, err = work.NewReader(work.ReaderOptions{
			DB: e.backends.Store, Hydrated: p.Hydrated,
		}); err != nil {
			cancel()
			return fmt.Errorf("engine: work reader: %w", err)
		}
	}
	if wiki {
		p, err := projection.New(projection.Options{
			Documents: e.backends.Fleet, DB: e.backends.Store,
			Applier: pages.NewApplier(skillDetector{}),
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

	for _, p := range []*projection.Projector{n.tracker, n.wiki} {
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
	log.InfoContext(ctx, "native_backends_started",
		"tracker", tracker, "knowledge", wiki)
	return nil
}

// stopNative ends this node's native backends.
func (e *Engine) stopNative() {
	if e.native == nil {
		return
	}
	e.native.stop()
	e.native.done.Wait()
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
	for _, p := range []*projection.Projector{e.native.tracker, e.native.wiki} {
		if p != nil && !p.Hydrated() {
			return false
		}
	}
	return true
}

// NativeStatus is what this node reports about its projections, for the fleet
// view. Empty for a node running no native backend.
func (e *Engine) NativeStatus() []projection.Status {
	if e.native == nil {
		return nil
	}
	var out []projection.Status
	for _, p := range []*projection.Projector{e.native.tracker, e.native.wiki} {
		if p != nil {
			out = append(out, p.Status())
		}
	}
	return out
}

// Work is this node's tracker read side, or nil.
func (e *Engine) Work() *work.Reader {
	if e.native == nil {
		return nil
	}
	return e.native.workReader
}

// WorkStore is this node's tracker write side, or nil.
func (e *Engine) WorkStore() *work.Store {
	if e.native == nil {
		return nil
	}
	return e.native.work
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
	for _, p := range []*projection.Projector{e.native.tracker, e.native.wiki} {
		if p != nil && p.Family() == family {
			return p.WaitApplied(ctx, revision)
		}
	}
	return nil
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

	translators := []changefeed.Translator{}
	if e.native.work != nil {
		translators = append(translators, work.NewTranslator())
	}
	if e.native.pages != nil {
		translators = append(translators, pages.NewTranslator(e.skillsContainer))
	}
	for _, translator := range translators {
		feed, err := changefeed.New(changefeed.Options{
			Feeder: feeder, Publisher: e.backends.Queue,
			Claims: e.backends.Fleet, Translator: translator,
		})
		if err != nil {
			log.ErrorContext(ctx, "changefeed_unavailable",
				"source", translator.Source(), "error", err.Error())
			continue
		}
		e.native.done.Add(1)
		go func() {
			defer e.native.done.Done()
			// THE NATIVE RUNTIME'S OWN CONTEXT, never the caller's: this
			// goroutine is joined by stopNative, which ends that one.
			if err := feed.Run(e.native.run); err != nil {
				log.ErrorContext(ctx, "changefeed_stopped",
					"source", translator.Source(), "error", err.Error(),
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
	if e.native.work != nil {
		parsers = append(parsers, work.NewParser(work.ParserOptions{
			Leads: projectLeads(c.Org), BaseURL: e.publicBase,
		}))
		prompts = append(prompts, work.Prompt{})
	}
	if e.native.pages != nil {
		parsers = append(parsers, pages.NewParser(pages.ParserOptions{
			Leads: containerLeads(c.Org), BaseURL: e.publicBase,
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
}

// projectLeads maps a tracker project to the handle that owns it.
//
// THROUGH [org.Organization.LeadsBy], which is where "who owns this scope"
// lives: the tracker's only contribution is naming the field. The ambiguity
// report is logged rather than refused, because two units sharing a project
// is an ordinary arrangement and only a DISAGREEMENT about the lead matters.
func projectLeads(o *org.Organization) work.Leads {
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
	return work.Leads(leads)
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
	if e.native == nil || e.native.workReader == nil || e.native.work == nil {
		return builtin.WorkDeps{}
	}
	return builtin.WorkDeps{
		Reader:   e.native.workReader,
		Writer:   e.native.work,
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
		Await: func(ctx context.Context, revision uint64) error {
			return e.WaitApplied(ctx, coord.FamilyWork, revision)
		},
	}
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
