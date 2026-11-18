package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE ESTATE'S ROUTER, ON EVERY NODE: every seat tool reaches the estate through
// one [estate.Router]. A data node answers its own seats from its own copy, its
// floors enforced as a remote one's; a node without `data` asks for everything.
// A tool must not tell which: one that behaved differently would be two tools,
// one of them untested. Holders: every live data node in the WATCHED presence
// view ([wholeEstate]), the one view per-request questions read, never a
// listing per call; git goes to the live gitlet-role nodes off the same view
// ([wholeGit], ADR-0089), this node's own git half asked per request
// ([engineGit]). A LAGGING copy ([statelog.Health.Answers] false) is asked
// last, never skipped ([estate.Router]); a WRONG one is out of service for
// that domain alone ([localEstate.For], ADR-0079).

// The placement the router is handed.
var _ estate.Placement = wholeEstate{}

// servingRecheck is how long the request gate ([localEstate.For]) trusts a
// copy's verdict (wrong, and answering) before reading it again.
//
// ONE SECOND: a verdict costs a JetStream request per log plus the published
// floors ([stateLog.readFloors]), too much to pay per tool call. Read off the
// request path ([localEstate.verdict]), a node pays at most one verdict a
// second; one a second old errs only about a copy that crossed the snapshot
// slack (a thousand records) inside it, and the floors still hold the node's
// own seats' writes. Counted from when the read LANDED ([localEstate.refresh]).
const servingRecheck = time.Second

// servingRead bounds one verdict's broker reads whatever the deadline of the
// request that found it stale: the verdict is shared, so one request's
// cancellation must not become everybody's "not serving" for a second.
// statelog.ReadBudget, as a floor wait on the same question. It may exceed
// [servingRecheck]: only a request finding the copy never judged waits on a
// read, and no longer than its own context.
const servingRead = statelog.ReadBudget

// newRouter builds this node's router over the fleet's queue, routing by the
// presence view, answering in-process from this node's own copy, and signing
// and verifying with the envelopes [Engine.serveEstate] answers peers with.
func (e *Engine) newRouter(q estate.Asker, nodeID string,
	envelopes *estate.Envelopes) (*estate.Router, error) {

	opts := estate.RouterOptions{
		Self: nodeID, Queue: q, Placement: e.estatePlacement(), Git: e.gitPlacement(),
		Session: estate.NewSession(), Seams: e.serverSeams(),
		Timing:    estate.Production(),
		Envelopes: envelopes,
	}
	if e.local != nil {
		opts.Local = e.local
	}
	// THIS NODE'S OWN GIT HALF, asked per request ([engineGit]): Gitlet is built
	// at boot or at the first apply that hosts code, after this router exists.
	if e.profile.Roles.Has(placement.RoleGitlet) {
		opts.LocalGit = e.localGit()
	}
	r, err := estate.NewRouter(opts)
	if err != nil {
		return nil, fmt.Errorf("engine: the estate's router: %w", err)
	}
	return r, nil
}

// estatePlacement is who holds the estate as this node routes: every live data
// node, from the watched presence view.
func (e *Engine) estatePlacement() estate.Placement {
	return wholeEstate{roster: presenceRoster{view: e.dataView}}
}

// wholeEstate is the router's placement: every live data node holds the whole
// estate, as this node's watched presence view names them.
//
// PRESENCE AND NOT A LISTING PER CALL, because every seat tool asks it first:
// the view answers from memory, and a node it named that went silent is listed
// again at once ([wholeEstate.Unanswered]) rather than a heartbeat later.
type wholeEstate struct {
	// roster is every live data node, from memory.
	roster dataRoster
}

// dataRoster is every live data node, as the router's placement reads them.
type dataRoster interface {
	// LiveDataNodes is every live data node; an error is UNKNOWN.
	LiveDataNodes() ([]string, error)

	// Invalidate asks for the nodes to be listed again: one it named went
	// silent.
	Invalidate()
}

// Holders is every live data node, sorted. An error from the roster is
// UNKNOWN, never "no holder".
func (w wholeEstate) Holders() ([]string, error) {
	nodes, err := w.roster.LiveDataNodes()
	if err != nil {
		return nil, fmt.Errorf("engine: who holds the estate: %w", err)
	}
	out := slices.Clone(nodes)
	slices.Sort(out)
	return out, nil
}

// Unanswered asks the roster to list again, because a node it named went
// silent.
func (w wholeEstate) Unanswered(string) { w.roster.Invalidate() }

// gitPlacement is who serves a repository's git work as this node routes it
// (ADR-0089): every live gitlet-role node off the estate placement's watched
// view, with what each advertises for background work, which places
// maintenance, read off the same listing ([wholeGit.Listing]).
func (e *Engine) gitPlacement() estate.GitPlacement {
	return wholeGit{roster: presenceRoster{view: e.dataView}}
}

// wholeGit is the router's git placement: [wholeEstate]'s twin over the gitlet
// role, for its reasons.
type wholeGit struct {
	// roster is every live Gitlet node, from memory.
	roster gitRoster
}

// gitRoster is every live Gitlet node, as the router's git placement reads them.
type gitRoster interface {
	// LiveGitletNodes is every live Gitlet node; an error is UNKNOWN.
	LiveGitletNodes() ([]string, error)

	// GitListing is every live Gitlet node and what each advertises it can
	// give its background work, off ONE listing; an error is UNKNOWN.
	GitListing() (estate.GitListing, error)

	// Invalidate asks for the nodes to be listed again: one it named went
	// silent.
	Invalidate()
}

// Holders is every live Gitlet node, sorted. An error from the roster is
// UNKNOWN, never "no Gitlet node".
func (w wholeGit) Holders() ([]string, error) {
	nodes, err := w.roster.LiveGitletNodes()
	if err != nil {
		return nil, fmt.Errorf("engine: who serves git: %w", err)
	}
	out := slices.Clone(nodes)
	slices.Sort(out)
	return out, nil
}

// Listing is every live Gitlet node, sorted as Holders sorts them, with what
// each one's presence row says its git cache gives background work
// ([coord.GitBackgroundKey]), off ONE listing so a node is weighed by the
// listing that names it ([estate.GitPlacement]). An error from the roster is
// UNKNOWN, never "no Gitlet node".
func (w wholeGit) Listing() (estate.GitListing, error) {
	listing, err := w.roster.GitListing()
	if err != nil {
		return estate.GitListing{}, fmt.Errorf("engine: who serves git, and what each Gitlet node can give "+
			"its background work: %w", err)
	}
	slices.Sort(listing.Nodes)
	return listing, nil
}

// Unanswered asks the roster to list again, because a node it named went
// silent.
func (w wholeGit) Unanswered(string) { w.roster.Invalidate() }

// localGit is this node's git half as the router and the estate server ask for
// it: resolved on every request ([engineGit]), never once.
func (e *Engine) localGit() estate.LocalGit { return engineGit{e: e} }

// engineGit is [estate.LocalGit] over the PUBLISHED native runtime's git half
// ([gitHalf]): the query server's reads by object id, the merge previews
// [Engine.buildGitletHost] built, and the engine writes of Gitlet's git surface.
//
// LOADED ON EVERY CALL, never captured at build: that surface is built at boot
// or at the first apply that sets code.backend: gitlet, after the router and
// server exist, and a captured one would leave a Gitlet node that applied its
// company after boot unserved until it restarted.
type engineGit struct{ e *Engine }

// Git is the runtime's git half, or false where no runtime is published or it
// built no git surface.
func (g engineGit) Git() (estate.GitServer, bool) {
	n := g.e.native.Load()
	if n == nil || n.gitReads == nil || n.mergePreviews == nil || n.gitletHost == nil {
		return nil, false
	}
	return gitHalf{reads: n.gitReads, writes: n.gitletHost, previews: n.mergePreviews}, true
}

// estateEnvelopes signs and verifies every envelope the estate's router and
// server exchange, under the fleet keyring state-log records are signed under
// (ADR-0031), with the estate's own domains. ONE PER NODE, built in [New] and
// handed to both halves: a node that signed requests it could not verify the
// answers to would believe nothing. It fails only on a bootstrap
// [config.Bootstrap.Validate] already refuses.
func estateEnvelopes(boot *config.Bootstrap) (*estate.Envelopes, error) {
	envelopes, err := estate.NewEnvelopes(recordKeyring(boot))
	if err != nil {
		return nil, fmt.Errorf("engine: the estate's envelopes: %w", err)
	}
	return envelopes, nil
}

// newLocalEstate is this data node's own copy, as the router answers from it.
func newLocalEstate(e *Engine) *localEstate {
	return &localEstate{e: e, now: time.Now,
		read: func(ctx context.Context, c *core) copyVerdict {
			return c.log.judgeCopy(ctx)
		}}
}

// localEstate is [estate.LocalBackends] over this node's own copy.
type localEstate struct {
	e   *Engine
	now func() time.Time

	// read judges the copy — the state log's own judgement in production,
	// a parameter for the tests.
	read func(ctx context.Context, c *core) copyVerdict

	// mu guards slot, and is NEVER held across a read of the broker — see
	// [localEstate.verdict].
	mu   sync.Mutex
	slot verdictSlot
}

// verdictSlot is the copy's verdict: the last one read, and the read in flight
// to replace it.
type verdictSlot struct {
	// held is the last verdict read, valid once known is.
	held  heldVerdict
	known bool

	// reading is closed when the read in flight lands, and nil while none
	// is: ONE read at a time, however many requests found the verdict
	// stale.
	reading chan struct{}
}

// heldVerdict is the copy's last verdict, and when its read landed.
type heldVerdict struct {
	at time.Time
	copyVerdict
}

// For implements [estate.LocalBackends]: the backend over this node's copy of
// domain (or [estate.SeatCopy]), or false while that copy is out of service.
//
// A WRONG copy (an applier halted at an undecodable record or held past the
// deferral grace, an eviction, rows below the log's first record, a checkpoint
// on another stream, a stalled prefix) is out of service for its domain alone
// (ADR-0079): its calls go to other data nodes, a peer asking is told
// `out_of_service`, other domains keep answering, and the seats stay, since
// the copy is the fault and moving them would only cost their processes. The
// copy is the core's, judged from boot; only the native halves wait for a
// company that keeps them ("no native backend here" until then); a core not up
// serves nothing.
func (l *localEstate) For(ctx context.Context, domain string) (estate.Backend, bool) {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return estate.Backend{}, true
	}
	v := l.verdict(ctx, c)
	if !v.serves(domain) {
		return estate.Backend{}, false
	}
	b := l.e.estateBackend(c, l.e.native.Load())
	b.Answers = func(context.Context) bool { return v.answers(domain) }
	return b, true
}

// serves reports whether this node's copy of domain is in service, waiting
// only as [localEstate.For] does: on a copy never judged, up to ctx. For a
// reader of the copy outside estate operations ([searchCopies]), which must
// not answer from a copy the router has stopped serving.
func (l *localEstate) serves(ctx context.Context, domain string) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return true
	}
	return l.verdict(ctx, c).serves(domain)
}

// copyOutOfService is the first of domains, in the order given, whose copy on
// this node is out of service ([localEstate.serves], ADR-0079) — or false
// where every one serves, or where this node holds no estate to judge. A
// fleet duty that decides from those domains' rows asks it before it claims
// ([dutyDecline]).
func (e *Engine) copyOutOfService(ctx context.Context, domains ...string) (string, bool) {
	if e == nil || e.local == nil {
		return "", false
	}
	for _, domain := range domains {
		if !e.local.serves(ctx, domain) {
			return domain, true
		}
	}
	return "", false
}

// verdict is the copy's verdict, read again once older than [servingRecheck].
//
// NEVER ON A REQUEST'S PATH, NEVER UNDER THE LOCK: a broker read can take its
// whole [servingRead], and every seat tool call and peer request asks this
// first; under one lock each would queue, re-read in turn, and could not leave
// when its caller gave up. So one read at a time runs off the path
// ([verdictSlot].reading) while requests answer from the verdict before it;
// only a request finding the copy never judged waits, up to its own context.
//
// A read that did not reach a domain's log changes nothing for that domain
// ([copyVerdict.carried]): a broker blip must neither send requests away from a
// sound copy nor restore a wrong one. A copy never judged answers nothing.
func (l *localEstate) verdict(ctx context.Context, c *core) copyVerdict {
	v, known, reading := l.peek(ctx, c)
	if known {
		return v
	}
	select {
	case <-reading:
	case <-ctx.Done():
		// NEVER JUDGED, and the caller would not wait: a copy nobody
		// has measured answers nothing — and is wrong about nothing.
		return copyVerdict{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.slot.held.copyVerdict
}

// peek is the copy's verdict as held NOW, never waiting: the last one read and
// true, or false where none has been — and it starts a read wherever the
// verdict held is older than [servingRecheck] and none is in flight. reading
// is closed when that read lands.
func (l *localEstate) peek(ctx context.Context, c *core) (v copyVerdict, known bool,
	reading chan struct{}) {

	l.mu.Lock()
	defer l.mu.Unlock()
	slot := &l.slot
	if slot.known && l.now().Sub(slot.held.at) < servingRecheck {
		return slot.held.copyVerdict, true, nil
	}
	reading = slot.reading
	if reading == nil {
		reading = make(chan struct{})
		slot.reading = reading
		// DETACHED from the request that found the verdict stale, since
		// every request after it shares what it reads, and BOUNDED by
		// [servingRead], which is the goroutine's whole lifetime.
		go l.refresh(context.WithoutCancel(ctx), c, reading)
	}
	return slot.held.copyVerdict, slot.known, reading
}

// servesUnasked reports, WITHOUT WAITING, whether this node answers its SEATS
// from its own copy: its core runs and the last verdict, where one exists,
// found every domain a seat depends on in service ([estate.SeatCopy]); a wrong
// copy of another domain never costs the seats (ADR-0079). For
// serviceability, which asks every sweep and may not wait on the broker: an
// unjudged copy is not read as wrong (its first read starts here), and a stale
// verdict answers while its replacement is read.
func (l *localEstate) servesUnasked(ctx context.Context) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return false
	}
	v, known, _ := l.peek(ctx, c)
	return !known || v.serves(estate.SeatCopy)
}

// outOfService is which of domains, in their order, this node's copy holds OUT
// OF SERVICE as far as it can tell without waiting (a verdict found that
// domain's copy wrong, ADR-0079, or the estate file shut, which takes every
// domain), and nil for none. For a watch over one subsystem's appliers (the
// inbox relay's sources, the artifact rooms' two logs) that withholds their
// announcements while their copy is out, so it NAMES THE DOMAINS IT WATCHES:
// another domain's verdict would withhold a sound copy's pushes and deliver a
// wrong one's. An unjudged copy and a core not up are not out of service, as
// [localEstate.For] serves both; a stale verdict answers while it is re-read.
func (l *localEstate) outOfService(ctx context.Context, domains ...string) []string {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return nil
	}
	v, known, _ := l.peek(ctx, c)
	if !known {
		return nil
	}
	var out []string
	for _, domain := range domains {
		if !v.serves(domain) {
			out = append(out, domain)
		}
	}
	return out
}

// refresh reads the copy's verdict once and stores it, stamped when the read
// LANDED: stamped when it began, a read slower than [servingRecheck] would be
// stale on arrival and the next request would read again at once.
func (l *localEstate) refresh(ctx context.Context, c *core, done chan struct{}) {
	defer close(done)
	read, cancel := context.WithTimeout(ctx, servingRead)
	defer cancel()
	v := l.read(read, c)

	l.mu.Lock()
	known, held := l.slot.known, l.slot.held
	v = v.carried(held.copyVerdict, known)
	l.slot.held, l.slot.known, l.slot.reading = heldVerdict{at: l.now(), copyVerdict: v}, true, nil
	l.mu.Unlock()

	// ONE LINE PER DOMAIN newly out of service, naming its fault: which log's
	// copy is wrong, and why, is what an operator acts on.
	for _, out := range v.newlyWrong(held.copyVerdict, known) {
		if out.domain == shutEstate {
			log.WarnContext(ctx, "estate_copy_not_served", "domain", out.domain,
				"detail", "this node's copy of the estate is not open, so every "+
					"domain's copy goes out of service: their operations go to the "+
					"other data nodes until the file is open again, and the node's "+
					"seats stay")
			continue
		}
		log.WarnContext(ctx, "estate_copy_not_served", "domain", out.domain,
			"reason", string(out.why),
			"detail", "this node's copy of "+out.domain+" is wrong rather than behind, so it "+
				"goes out of service: that domain's operations go to the other data "+
				"nodes until the copy recovers, every other domain's copy keeps "+
				"serving, and the node's seats stay")
	}
}

// estateBackend is this data node's answer for its copy of the estate: the
// core's halves (logs, identity directory, usage rows) from boot, and the
// native halves where the company keeps them (n is nil before one). USAGE
// ROWS ARE THE CORE'S ([HoldsEstate]), never the native runtime's: a company on
// vendors hosting no code has none, so usage screens would get endless 503s.
// WRITES ONLY WHILE THIS NODE PUBLISHES: a maintenance mode is the evidence a
// capacity operation is established from, so every writer, the core's
// included, is absent ([Engine.writeSide]) and a write answers "not here".
// [estate.Backend.Current] is [Engine.admits], read per operation since a
// backend outlives its posture: a node behind the company's configuration is
// asked last, so its epoch's chart, units, scope and code limits answer only
// where no current data node does.
func (e *Engine) estateBackend(c *core, n *native) estate.Backend {
	b := estate.Backend{
		Usage: usage.Reader{Estate: e.usageEstate()}, Current: e.admits,
		Reached: e.WaitReached,
		Admits: func(ctx context.Context) bool {
			ok, refusal := c.log.Established(ctx, true)
			if !ok && refusal != "" {
				log.DebugContext(ctx, "seat_admission_withheld", "reason", string(refusal))
			}
			return ok
		},
	}
	// THE IDENTITY DIRECTORY, whatever the company keeps, for a node without
	// `data` ([routedIdentity]). Set only where one is: a typed nil reads as
	// present.
	if c.iamReader != nil {
		b.Identity = c.iamReader
	}
	// THE ARTIFACTS ESTATE, run on every data node from boot, so a node without
	// `data` reaches it through any data node. Set only where both sides are
	// (a typed nil reads as present); its WRITER only while this node publishes.
	publishes := e.mode.Publishes()
	if c.artifactsReader != nil && c.artifactsWriter != nil {
		b.Artifacts = estate.ArtifactsHalf{Reader: c.artifactsReader}
		if publishes {
			b.Artifacts.Writer = c.artifactsWriter
		}
	}
	// AND ITS DATASTORE, run beside it on every data node.
	if c.artifactDataReader != nil && c.artifactDataWriter != nil {
		b.ArtifactData = estate.ArtifactDataHalf{Reader: c.artifactDataReader}
		if publishes {
			b.ArtifactData.Writer = c.artifactDataWriter
		}
	}
	// AND THE SEATS' EXECUTOR SESSIONS, likewise from boot: a seat's box runs
	// wherever its seat is held, and what it did is recorded and read back at
	// the next launch through whichever data node answers (ADR-0073).
	if c.executorReader != nil && c.executorWriter != nil {
		b.Executor = estate.ExecutorHalf{Reader: c.executorReader}
		if publishes {
			b.Executor.Writer = c.executorWriter
		}
	}
	// AND THE ACCESS LOG, on the executor log's terms: a turn is dispatched
	// and told wherever its seat is held (ADR-0103).
	if c.accessReader != nil && c.accessWriter != nil {
		b.Access = estate.AccessHalf{Reader: c.accessReader}
		if publishes {
			b.Access.Writer = c.accessWriter
		}
	}
	// AND THE ATLAS LOG, read and written by whichever node asks (ADR-0126):
	// a person's write reaches the operator's catalogue on any node serving
	// the API, and is made by a data node that publishes.
	if c.atlasReader != nil && c.atlasWriter != nil {
		b.Atlas = estate.AtlasHalf{Reader: c.atlasReader}
		if publishes {
			b.Atlas.Writer = c.atlasWriter
		}
	}
	if n == nil {
		return b
	}
	if n.trackerReader != nil {
		b.Tracker = n.trackerReader
	}
	if n.itemSearch != nil {
		b.WorkSearch = n.itemSearch
	}
	if n.pageReader != nil {
		b.Pages = n.pageReader
	}
	if n.searcher != nil {
		b.Knowledge = n.searcher
	}
	if n.indexer != nil {
		b.Backlinks = n.indexer
	}
	if n.chatReader != nil {
		b.Chat = n.chatReader
	}
	if n.chatIndex != nil {
		b.ChatSearch = n.chatIndex
	}
	w := e.writeSide()
	if w.tracker != nil {
		b.Writer = func(a estate.Actor) estate.TrackerWriter {
			// A NIL INTERFACE, never a typed nil: the server reads nil
			// as "the tracker refused to act as this party".
			if tw := w.tracker.As(a.Handle, a.Kind, a.Provenance); tw != nil {
				return tw
			}
			return nil
		}
	}
	if w.pages != nil {
		b.PageWriter = w.pages
	}
	if w.chat != nil {
		b.ChatWriter = w.chat
	}
	// THE CODE HALF where the runtime runs code: the reader always, every writer
	// and the lifecycle store only where this node publishes. Nil in a
	// maintenance mode, gitlet.Served refuses those as a node that ran nothing,
	// so the router asks the next data node with the same push id or operation.
	if n.built.runsCode() {
		b.Code = gitlet.Served{Committer: w.committer, ChangeWriter: w.changes, ChecksWriter: w.checks,
			Reader: n.codeReader, Store: w.code, Marks: w.marks}
	}
	return b
}

// serverSeams is what this node supplies to every operation it answers: its
// current chart and org.
func (e *Engine) serverSeams() estate.ServerSeams {
	seams := estate.ServerSeams{
		Units: liveUnits{engine: e}, Leads: liveLeads{engine: e},
		Seat: func(handle string) (*org.Role, *org.Organization) {
			c := e.Company()
			if c == nil || c.Org == nil {
				return nil, nil
			}
			return c.Org.AgentSeatByHandle(handle), c.Org
		},
	}
	return seams
}

// presenceRoster is the fleet's presence view as the router's placements read
// it, from memory: one view for live data and Gitlet nodes, so the placements
// never disagree about who is live, plus what each Gitlet node advertises for
// background work ([presenceRoster.GitListing]).
type presenceRoster struct{ view *coord.LeaseView }

// LiveDataNodes is every live data node. A node with no view has no
// coordination and so no fleet: nobody else to ask, which is an answer rather
// than an unknown.
func (r presenceRoster) LiveDataNodes() ([]string, error) {
	if r.view == nil {
		return nil, nil
	}
	leases, _, err := r.view.Leases()
	if err != nil {
		return nil, fmt.Errorf("engine: which nodes hold the estate: %w", err)
	}
	return dataNodesOf(leases), nil
}

// LiveGitletNodes is every live node with the gitlet role. A node with no view has
// no coordination and so no fleet: nobody else to ask, which is an answer
// rather than an unknown — its own git half, where it has one, is asked in
// process ([estate.RouterOptions.LocalGit]).
func (r presenceRoster) LiveGitletNodes() ([]string, error) {
	if r.view == nil {
		return nil, nil
	}
	leases, _, err := r.view.Leases()
	if err != nil {
		return nil, fmt.Errorf("engine: which nodes serve git: %w", err)
	}
	return gitletNodesOf(leases), nil
}

// GitListing is every live gitlet-role node ([gitletNodeOf], as
// [presenceRoster.LiveGitletNodes] lists them) and what each one's presence row
// says its git cache gives background work ([coord.GitBackgroundKey], written
// every beat beside the status from [Engine.gitBackgroundBudget]), off ONE read
// of the view so a node is weighed by the listing that names it. A figure is
// said, zero included (Gitlet not built, or transfers keep all its memory),
// even when the beat's status read overran; a row naming none is NOT SAYING and
// gets no entry. No view is no fleet; an error is UNKNOWN.
func (r presenceRoster) GitListing() (estate.GitListing, error) {
	if r.view == nil {
		return estate.GitListing{}, nil
	}
	leases, _, err := r.view.Leases()
	if err != nil {
		return estate.GitListing{}, fmt.Errorf("engine: which nodes serve git: %w", err)
	}
	listing := estate.GitListing{Budgets: map[string]int64{}}
	for _, lease := range leases {
		node, ok := gitletNodeOf(lease)
		if !ok {
			continue
		}
		listing.Nodes = append(listing.Nodes, node)
		if budget, said := coord.GitBackgroundFromMeta(lease.Meta); said {
			listing.Budgets[node] = budget
		}
	}
	return listing, nil
}

// Invalidate lists again rather than waiting out the heartbeat of a node the
// view named that went silent.
func (r presenceRoster) Invalidate() {
	if r.view != nil {
		r.view.Invalidate()
	}
}

// routable is [Engine.SeatsServiceable] over one view at now.
func routable(view *coord.LeaseView, now time.Time) (bool, string) {
	if view == nil {
		return true, ""
	}
	_, _, err := view.Leases()
	listed := view.ListedAt()
	if err == nil || listed.IsZero() {
		return true, ""
	}
	if age := now.Sub(listed); age > statelog.FloorCacheStale {
		reason := fmt.Sprintf("the estate's router cannot say who serves the estate: "+
			"its view of the fleet was last read %s ago, past the %s bound: %v",
			age.Round(time.Second), statelog.FloorCacheStale, err)
		log.Warn("seats_unserviceable", "reason", reason,
			"hint", "this node cannot route its seats' calls to the estate; its seats "+
				"move to a peer until its view of the fleet can be read again")
		return false, reason
	}
	return true, ""
}
