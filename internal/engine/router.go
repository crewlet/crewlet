package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// THE ESTATE'S ROUTER, ON EVERY NODE.
//
// Every seat tool on every node reaches the tracker and the knowledge base
// through one [estate.Router]: a data node holds the whole estate and answers
// its own seats' calls from its own copy — with its floors enforced exactly as
// a remote data node enforces them — while a node without `data` holds nothing
// and asks for everything. A tool cannot tell which, and must not: a tool that
// behaved differently on the two kinds of node would be two tools, and only
// one of them tested.
//
// # Who holds the estate, as this node routes it
//
// Every live data node, as this node's WATCHED presence view names them
// ([wholeEstate] over [coord.LeaseView]) — the view the fleet's other
// per-request questions read, never a second one, and never a listing of the
// fleet per call. Who serves a repository's GIT work is read off the same
// view — every live node with the git role ([wholeGit], ADR-0059) — and so is
// what each of them advertises it can give its background work, read together
// with them off one listing ([wholeGit.Listing]), which places a repository's
// maintenance; this node's own git half is asked per request ([engineGit]).
//
// # What this node answers
//
// [localEstate]: on a data node, its own copy — the core's logs and the
// identity directory from boot, and the native halves once a company keeps
// them — and a copy that LAGS
// ([statelog.Health.Answers] false: neither level this instant nor drained and
// within the snapshot slack) is asked only once every data node whose copy
// does not has run nothing, by this node's router and by every other node's
// alike: a worse node to ask, never no node (estate's [estate.Router] doc). A
// domain's copy that is WRONG is out of service for that domain's operations
// alone ([localEstate.For], ADR-0049), and the node keeps its seats.

// The placement the router is handed.
var _ estate.Placement = wholeEstate{}

// servingRecheck is how long a copy's verdict — whether it is wrong, and
// whether it answers requests — is trusted by the request gate
// ([localEstate.For]) before it is read again.
//
// ONE SECOND. The verdict reads the broker for every log — a JetStream API
// request per log at least — and the published floors once
// ([stateLog.readFloors]), and a gate that asked per request would put those
// round trips in front of every tool call a data node answers. Refreshed
// at most once a second, and never on a request's own path once it has been
// read — see [localEstate.verdict] — a node pays at most one verdict's reads a
// second however many requests it answers; and a verdict a second old errs
// only about a copy that crossed the snapshot slack (a thousand records)
// inside that second, which the floors still hold to every write the node's
// own seats made. It is counted from when the read LANDED, so a read slower
// than this is not already stale when it is stored.
const servingRecheck = time.Second

// servingRead bounds one verdict's reads of the broker, whatever the deadline
// of the request that happened to find the verdict stale: the verdict is
// shared by every request after it, and one request's cancellation must not
// become everybody's "not serving" for a second.
//
// statelog.ReadBudget, the budget a floor wait takes — the same "is this copy
// fit to answer" question, bounded the same way. It may exceed
// [servingRecheck]: nobody waits on a read but a request that finds the copy
// never judged, and that request waits no longer than its own context.
const servingRead = statelog.ReadBudget

// newRouter builds this node's router over the fleet's queue, routing by the
// presence view and answering in-process from this node's own copy, and
// signing what it asks and verifying what it is answered with envelopes —
// the same ones [Engine.serveEstate] answers peers with.
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
	// THIS NODE'S OWN GIT HALF, where it has the git role: asked per request
	// ([engineGit]), since the code host is built at boot or at the first
	// apply that hosts code — after this router exists.
	if e.profile.Roles.Has(placement.RoleGit) {
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

// gitPlacement is who serves git as this node routes a repository's git work
// (ADR-0059): every live node whose presence profile names the git role, from
// the same watched view as the estate's placement — and, read together with
// them off one listing of that view, what each of them advertises it can give
// its background work, which places a repository's maintenance
// ([wholeGit.Listing]).
func (e *Engine) gitPlacement() estate.GitPlacement {
	return wholeGit{roster: presenceRoster{view: e.dataView}}
}

// wholeGit is the router's git placement: every live node with the git role,
// as this node's watched presence view names them — [wholeEstate]'s twin over
// the git role, for its reasons: answered from memory on every git read, and
// listed again at once when a node it named went silent.
type wholeGit struct {
	// roster is every live git node, from memory.
	roster gitRoster
}

// gitRoster is every live git node, as the router's git placement reads them.
type gitRoster interface {
	// LiveGitNodes is every live git node; an error is UNKNOWN.
	LiveGitNodes() ([]string, error)

	// GitListing is every live git node and what each advertises it can
	// give its background work, off ONE listing; an error is UNKNOWN.
	GitListing() (estate.GitListing, error)

	// Invalidate asks for the nodes to be listed again: one it named went
	// silent.
	Invalidate()
}

// Holders is every live git node, sorted. An error from the roster is
// UNKNOWN, never "no git node".
func (w wholeGit) Holders() ([]string, error) {
	nodes, err := w.roster.LiveGitNodes()
	if err != nil {
		return nil, fmt.Errorf("engine: who serves git: %w", err)
	}
	out := slices.Clone(nodes)
	slices.Sort(out)
	return out, nil
}

// Listing is every live git node, sorted as Holders sorts them, and what each
// one's presence row says its git cache gives its background work
// ([coord.GitBackgroundKey]), off ONE listing of the view: the nodes and the
// figures read together, so a node is weighed by the listing that names it
// ([estate.GitPlacement]). An error from the roster is UNKNOWN, never "no git
// node".
func (w wholeGit) Listing() (estate.GitListing, error) {
	listing, err := w.roster.GitListing()
	if err != nil {
		return estate.GitListing{}, fmt.Errorf("engine: who serves git, and what each git node can give "+
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

// engineGit is [estate.LocalGit] over the PUBLISHED native runtime: its git
// half ([gitHalf]) — the reads by object id of the query server and the merge
// previews [Engine.buildCodeHost] built beside the code host, and the engine
// writes of the code host itself.
//
// LOADED ON EVERY CALL, never captured when the router or the server is built,
// because the code host — and the query server and the previews with it — is
// built at boot or at the first apply that sets code.native, after both exist:
// captured, a git node that applied its company after boot would answer "no
// code host" to every git read and every engine write until it restarted.
type engineGit struct{ e *Engine }

// Git is the runtime's git half, or false where no runtime is published or it
// built no code host.
func (g engineGit) Git() (estate.GitServer, bool) {
	n := g.e.native.Load()
	if n == nil || n.gitReads == nil || n.mergePreviews == nil || n.codeHost == nil {
		return nil, false
	}
	return gitHalf{reads: n.gitReads, writes: n.codeHost, previews: n.mergePreviews}, true
}

// estateEnvelopes is how this node signs and verifies every envelope the
// estate's router and server exchange — under the fleet keyring every state-log
// record is signed under (ADR-0031), with domains of the estate's own.
//
// ONE PER NODE, built in [New] before the router, and handed to both halves: a
// node is a router and, where it holds data, a server, and a node that could
// sign a request it could not verify the answer to would believe nothing it
// was told. The keyring is required on every node, so this fails only on a
// bootstrap [config.Bootstrap.Validate] would already have refused.
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
// domain — or of the seat copy, for [estate.SeatCopy] — or false while that
// copy is out of service.
//
// A DOMAIN'S COPY THAT IS WRONG IS NOT SERVED — for that domain's operations
// alone (ADR-0049). An applier halted at a record it cannot decode or held one
// past the deferral grace, an eviction, rows below the log's first record, a
// checkpoint on another stream, a stalled prefix: a domain's copy in any of
// those states goes OUT OF SERVICE — this node's router sends its own seats'
// calls on that domain to the other data nodes, and another node asking for
// one is told `out_of_service` — while every other domain's copy keeps
// answering here: a tracker applier halted at a record sends the node's
// tracker calls to a peer and its knowledge-base calls are still answered
// from its own pages. It never sheds the node's seats, which are served by a
// data node whose copy is sound. That is the whole of the remedy a fault
// needs: the seats were never the problem, the copy was, and moving them cost
// every one of them its processes and its memory to be served by the very
// same peers.
//
// THE COPY IS THE CORE'S, judged from boot: a data node runs every log from
// its first second, company or none, so each domain's copy is judged — and its
// identity directory and its usage rows served ([Engine.estateBackend]) —
// before its first company arrives. Only the native halves wait for one: a
// data node that booted with no company, or whose company keeps its tracker
// and knowledge base on vendors, is served with the core's halves alone, so a
// tracker or page operation on it is answered "no native backend here" and
// moves on. A data node whose core is not up yet — the moment between [New]
// building the router and starting the core — is served with nothing at all,
// the same "not here".
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
// only as [localEstate.For] does — on a copy never judged, up to ctx. For a
// reader of the copy that is not an estate operation ([searchCopies]), which
// must not answer from a copy the router has stopped serving.
func (l *localEstate) serves(ctx context.Context, domain string) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return true
	}
	return l.verdict(ctx, c).serves(domain)
}

// copyOutOfService is the first of domains, in the order given, whose copy on
// this node is out of service ([localEstate.serves], ADR-0049) — or false
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

// verdict is the copy's verdict, read again once it is older than
// [servingRecheck].
//
// # Never on a request's path, and never under the lock
//
// A read of the broker can take its whole [servingRead] — a leader election, a
// quorum lost, a broker that does not answer — and every tool call this node's
// seats make and every request another node sends it asks this first. Held
// under one lock across that read, every one of them queued behind it, each
// found the verdict it waited for already older than the recheck and read
// again in turn, and none could leave when its own caller gave up: twenty
// calls behind an unreachable broker waited forty seconds. So the read runs
// OFF the request path, one at a time (the [verdictSlot]'s reading), and a
// request meanwhile answers from the verdict before it — nobody waits on a
// read but a request that finds the copy never judged, and that one waits no
// longer than its own context.
//
// # A reading that could not reach the broker changes nothing it could not see
//
// A domain's copy that was answering keeps answering, and one that was wrong
// stays so, unless its own log was read and says otherwise
// ([copyVerdict.carried]) — a broker blip must not send every request away from
// a sound copy, nor bring a wrong one back into service on no information, and
// one domain's unread health says nothing about any other's. A copy never
// judged answers nothing: nobody has measured it.
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

// servesUnasked reports whether this node answers its SEATS from its own copy
// as far as it can tell WITHOUT WAITING: it runs its core, and the last
// verdict read of the copy — where one has been — found every domain a seat
// depends on in service ([estate.SeatCopy]). A wrong copy of any other domain
// costs that domain's calls a peer and never the seats (ADR-0049).
//
// For serviceability, which asks it on every sweep and may not wait on the
// broker. A copy nobody has judged yet is not read as a wrong one — its first
// read is started here, and the next sweep has the answer — and a verdict held
// past the recheck answers while the read that replaces it runs, exactly as a
// request is answered.
func (l *localEstate) servesUnasked(ctx context.Context) bool {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return false
	}
	v, known, _ := l.peek(ctx, c)
	return !known || v.serves(estate.SeatCopy)
}

// outOfService is which of domains this node's copy holds OUT OF SERVICE as far
// as it can tell without waiting — a verdict read found that domain's copy
// wrong (ADR-0049), or found the estate file shut, which takes every domain's
// copy with it — in the order domains names them, and nil for none.
//
// FOR A WATCH OVER ONE SUBSYSTEM'S APPLIERS — the inbox relay's sources, the
// artifact rooms' two logs — each of which withholds what those appliers
// announce while their copy is out, and so NAMES THE DOMAINS IT WATCHES: a
// wrong copy of any other domain leaves their announcements complete, and a
// watch that read another domain's verdict withheld a sound copy's pushes and
// delivered a wrong one's. A copy nobody has judged yet and a node whose core
// is not up are not out of service — [localEstate.For] serves both — and a
// stale verdict answers while the read that replaces it runs, exactly as a
// request is answered.
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

// refresh reads the copy's verdict once and stores it, stamped with the
// instant the read LANDED: stamped with the instant it began, a read slower
// than [servingRecheck] would be stale the moment it was stored, and the next
// request would read again at once.
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

	// ONE LINE PER DOMAIN that went out of service, naming it and the fault
	// that holds: which log's copy is wrong, and why, is what an operator
	// acts on, and every other domain's copy is still serving.
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
// core's — its logs, the identity directory and the usage domain's rows, from
// boot — and the native halves over them where the company keeps them (n, nil
// before the first company).
//
// THE USAGE DOMAIN'S ROWS ARE THE CORE'S, read off the file a data node holds
// open from boot whatever its company runs ([HoldsEstate]), never the native
// runtime's: a company on vendors for both its tracker and its knowledge base,
// hosting no code, builds no native half, and with the reads tied to one every
// data node answered every spend, activity and readers screen "no native
// backend here", so the router refused each as an estate nobody served — a 503
// asking the screen to retry what no retry could ever answer.
//
// WRITES ONLY WHILE THIS NODE PUBLISHES. A node restarted into a maintenance
// mode is the evidence a capacity operation is established from, and a write
// it took on anybody's behalf would be the publish the mode exists to rule
// out — so its writers are absent ([Engine.writeSide] hands none out) and
// every write is answered "not here".
//
// EVERY BACKEND CARRIES THE NODE'S CONFIGURATION POSTURE ([estate.Backend.
// Current]) through the one gate this node's trigger admission, scheduler,
// inbound consumer and integration reconcile already read ([Engine.admits],
// which the reconciler fills): a node confirmed behind the company's
// configuration is asked last for every estate operation, so what it supplies
// from its own epoch — the chart, the units, the scope, the code limits —
// answers only where no current data node does. The gate is read when an
// operation runs, never when the backend is built, since a backend outlives
// the posture it was built under.
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
	// THE IDENTITY DIRECTORY, whatever the company keeps — what a node
	// without `data` asks of whose record a login names and who holds each
	// seat ([routedIdentity]). A typed nil would read as a reader that is
	// there, so it is set only where one is.
	if c.iamReader != nil {
		b.Identity = c.iamReader
	}
	// THE ARTIFACTS ESTATE, which every data node runs from boot whatever its
	// company keeps, so a node without `data` reaches it through any data
	// node's copy. Set only where both sides are, for the identity
	// directory's reason: a typed nil reads as a half that is there.
	if c.artifactsReader != nil && c.artifactsWriter != nil {
		b.Artifacts = estate.ArtifactsHalf{Reader: c.artifactsReader, Writer: c.artifactsWriter}
	}
	// AND ITS DATASTORE, run beside it on every data node.
	if c.artifactDataReader != nil && c.artifactDataWriter != nil {
		b.ArtifactData = estate.ArtifactDataHalf{Reader: c.artifactDataReader, Writer: c.artifactDataWriter}
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
	w := e.writeSide()
	if writer := w.tracker; writer != nil {
		b.Writer = func(a estate.Actor) estate.TrackerWriter {
			// A NIL INTERFACE, never a typed nil: the server reads nil
			// as "the tracker refused to act as this party".
			if w := writer.As(a.Handle, a.Kind, a.Provenance); w != nil {
				return w
			}
			return nil
		}
	}
	if w.pages != nil {
		b.PageWriter = w.pages
	}
	// THE CODE HALF, where the runtime was built for it: the reader always,
	// the committer, the change writer, the checks writer, the marks writer
	// and the lifecycle store only where this node publishes — each nil in a maintenance mode,
	// and code.Served then refuses a proposal, a conversation's or a check's
	// write and a lifecycle request as a node that ran nothing, so the router
	// asks the next data node with the same push id or operation rather than
	// this node appending it.
	if n.built.runsCode() {
		b.Code = code.Served{Committer: w.committer, ChangeWriter: w.changes, ChecksWriter: w.checks,
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
// it: every live data node, and every live git node, from memory — one view
// for both, so the two placements can never disagree about who is live — and,
// read together with the git nodes off one listing of it, what each of them
// advertises it can give its background work ([presenceRoster.GitListing]).
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

// LiveGitNodes is every live node with the git role. A node with no view has
// no coordination and so no fleet: nobody else to ask, which is an answer
// rather than an unknown — its own git half, where it has one, is asked in
// process ([estate.RouterOptions.LocalGit]).
func (r presenceRoster) LiveGitNodes() ([]string, error) {
	if r.view == nil {
		return nil, nil
	}
	leases, _, err := r.view.Leases()
	if err != nil {
		return nil, fmt.Errorf("engine: which nodes serve git: %w", err)
	}
	return gitNodesOf(leases), nil
}

// GitListing is every live node with the git role, by the rule
// [presenceRoster.LiveGitNodes] lists them by ([gitNodeOf]), and what each
// one's presence row says its git cache gives its background work
// ([coord.GitBackgroundKey], which every git node's seat host writes on every
// beat beside its status, from [Engine.gitBackgroundBudget]) — off ONE read of
// the view, each lease decoded once, so a node is weighed by the very listing
// that names it. A row that names a figure says it, zero included: a git node
// with nothing to give — no code host built yet, or transfers that keep all of
// its memory. A row that names none is NOT SAYING — a build or a host that
// writes none — and has no entry. A beat whose status read overran says it
// all the same: the figure is not in the status. A node with no view has no
// fleet: no git node, and nobody saying anything. An error is UNKNOWN: the
// view could not be read.
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
		node, ok := gitNodeOf(lease)
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

// Invalidate lists again: a node the view named went silent,
// so it lists again rather than waiting out its heartbeat.
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
