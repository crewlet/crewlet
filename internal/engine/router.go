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
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ESTATE'S ROUTER, ON EVERY NODE.
//
// Every seat tool on every node reaches the tracker, the knowledge base and
// chat through one [estate.Router]: a data node holds the whole estate and
// answers its own seats' calls from its own copy — with its floors enforced
// exactly as a remote data node enforces them — while a node without `data`
// holds nothing and asks for everything. A tool cannot tell which, and must
// not: a tool that behaved differently on the two kinds of node would be two
// tools, and only one of them tested.
//
// # Who holds the estate, as this node routes it
//
// Every live data node, as this node's WATCHED presence view names them
// ([wholeEstate] over [coord.LeaseView]) — the view the fleet's other
// per-request questions read, never a second one, and never a listing of the
// fleet per call.
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
// copy that is WRONG is out of service ([localEstate.For]), and the node keeps
// its seats.

// The placement the router is handed.
var _ estate.Placement = wholeEstate{}

// servingRecheck is how long a copy's verdict — whether it is wrong, and
// whether it answers requests — is trusted by the request gate
// ([localEstate.For]) before it is read again.
//
// ONE SECOND. The verdict reads the broker for every log whose health judges a
// copy — a JetStream API request per log at least — and a gate that asked per
// request would put those round trips in front of every tool call a data node
// answers. Refreshed
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
		Self: nodeID, Queue: q, Placement: e.estatePlacement(),
		Session: estate.NewSession(), Seams: e.serverSeams(),
		Envelopes: envelopes,
	}
	if e.local != nil {
		opts.Local = e.local
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

// For implements [estate.LocalBackends]: the backend over this node's copy,
// or false while the copy is out of service.
//
// A COPY THAT IS WRONG IS NOT SERVED. An applier halted at a record it cannot
// decode or held one past the deferral grace, an eviction, rows below the
// log's first record, a checkpoint on another stream, a stalled prefix: a copy
// in any of those states goes OUT OF SERVICE — this node's router sends its
// own seats' calls to the other data nodes, and another node asking is told
// `out_of_service` — and never sheds the node's seats, which read the estate
// from a data node whose copy is sound. That is the whole of the remedy a
// fault needs: the seats were never the problem, the copy was, and moving them
// cost every one of them its processes and its memory to be served by the very
// same peers.
//
// THE COPY IS THE CORE'S, judged from boot: a data node runs every log from
// its first second, company or none, so its copy is judged — and its identity
// directory served — before its first company arrives. Only the native halves
// wait for one: a data node that booted with no company is served with the
// directory and no halves, so a tracker or page operation on it is answered
// "no native backend here" and moves on. A data node whose core is not up yet —
// the moment between [New] building the router and starting the core — is
// served with nothing at all, the same "not here".
func (l *localEstate) For(ctx context.Context) (estate.Backend, bool) {
	c := l.e.core.Load()
	if c == nil || c.log == nil {
		return estate.Backend{}, true
	}
	v := l.verdict(ctx, c)
	if v.fault != "" {
		return estate.Backend{}, false
	}
	b := l.e.estateBackend(c, l.e.native.Load())
	b.Answers = func(context.Context) bool { return v.answers }
	return b, true
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
// A copy that was answering keeps answering, and one that was faulted stays so
// unless a log that was read says otherwise — a broker blip must not send every
// request away from a sound copy, nor bring a wrong one back into service on no
// information. A copy never judged answers nothing: nobody has measured it.
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

// servesUnasked reports whether this node answers from its own copy as far as
// it can tell WITHOUT WAITING: it runs its core, and the last verdict read of
// the copy — where one has been — found it sound.
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
	return !known || v.fault == ""
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
	if v.refusal == statelog.RefuseBrokerUnreachable {
		v.answers = known && held.answers
	}
	// A FAULT IS CARRIED ONLY WHERE ITS OWN LOG WENT UNREAD. Carried
	// whenever any log went unread, a copy whose faulted log had just been
	// read and found sound stayed out of service for as long as some OTHER
	// log's health could not be read in its budget — on a loaded broker,
	// reading after reading — which is the opposite of what an unread
	// number may do in either direction.
	if v.fault == "" && known && slices.Contains(v.unread, held.fault) {
		v.fault = held.fault
	}
	l.slot.held, l.slot.known, l.slot.reading = heldVerdict{at: l.now(), copyVerdict: v}, true, nil
	l.mu.Unlock()

	if v.fault != "" && (!known || held.fault == "") {
		log.WarnContext(ctx, "estate_copy_not_served", "log", v.fault,
			"detail", "this node's copy of the estate is wrong rather than behind, "+
				"so it goes out of service; its seats read the estate from the "+
				"other data nodes until the copy recovers")
	}
}

// estateBackend is this data node's answer for its copy of the estate: the
// core's — its logs and the identity directory, from boot — and the native
// halves over them where the company keeps them (n, nil before the first
// company).
//
// WRITES ONLY WHILE THIS NODE PUBLISHES. A node restarted into a maintenance
// mode is the evidence a capacity operation is established from, and a write
// it took on anybody's behalf would be the publish the mode exists to rule
// out — so its writers are absent ([Engine.writeSide] hands none out) and
// every write is answered "not here". The CORE's writers included, which the
// core builds in every mode because its logs run in every mode: a reader of
// them is served there, and a writer handed out beside it would take a peer's
// artifacts write as surely as a native one.
func (e *Engine) estateBackend(c *core, n *native) estate.Backend {
	b := estate.Backend{
		Committed: e.WaitCommitted,
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
	// directory's reason: a typed nil reads as a half that is there. Its
	// WRITER only while this node publishes, so a write asked of a node in a
	// maintenance mode is answered "not here" and moves on to one that does.
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

// presenceRoster is the fleet's presence view as the router's placement reads
// it: every live data node, from memory.
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
