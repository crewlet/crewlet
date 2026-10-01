package engine

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/estate/partmap"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE ESTATE'S ROUTER, ON EVERY NODE.
//
// Every seat tool on every node reaches the tracker and the knowledge base
// through one [estate.Router]: a node that serves the partition an operation
// addresses answers it in-process, and one that does not asks a node that
// does. Under layout 0 a data node serves the one partition, `estate.000`, and
// so answers all of its own seats' calls itself — with its floors enforced
// exactly as a remote holder enforces them — while a node without `data`
// serves nothing and asks for everything. A tool cannot tell which, and must
// not: a tool that behaved differently on the two kinds of node would be two
// tools, and only one of them tested.
//
// # Who serves what, as this node routes it
//
// Under layout 0 there is no estate map, and the one partition's servers are
// every live data node as this node's WATCHED presence view names them
// ([partmap.Whole] over [coord.LeaseView]) — the view the fleet's other
// per-request questions read, never a second one, and never a listing of the
// fleet per call. A partitioned layout routes by the estate map's view
// ([partmap.View]), which satisfies the same interface.
//
// # What this node serves
//
// [localEstate]: the partitions its write authority's gate 3 says it serves
// ([holdingOf], the one rule the files it opens and the logs it may write are
// read from), each answered from this node's own copy — and a copy that LAGS
// ([statelog.Health.Answers] false: neither level this instant nor drained and
// within the snapshot slack) is asked only once every holder whose copy does
// not has run nothing, by this node's router and by every other node's alike: a
// worse holder, never no holder (estate's [estate.Router] doc). A copy that is
// WRONG is not served at all ([localEstate.For]), and the node keeps its seats.

// Every placement the router may be handed.
var (
	_ estate.Placement = partmap.Whole{}
	_ estate.Placement = (*partmap.View)(nil)
)

// servingRecheck is how long a copy's verdict — whether it is wrong, and
// whether it answers requests — is trusted by the request gate
// ([localEstate.For]) before it is read again.
//
// ONE SECOND. The verdict reads every log's bounds from the broker — a
// JetStream API request per log — and a gate that asked per request would put
// three round trips in front of every tool call a data node answers. Refreshed
// at most once a second PER PARTITION, and never on a request's own path once
// it has been read — see [localEstate.verdict] — a node pays at most one
// verdict's reads a second per partition however many requests it answers; and
// a verdict a second old errs only about a copy that crossed the snapshot
// slack (a thousand records) inside that second, which the floors still hold
// to every write the node's own seats made. It is counted from when the read
// LANDED, so a read slower than this is not already stale when it is stored.
const servingRecheck = time.Second

// servingRead bounds one verdict's reads of the broker, whatever the deadline
// of the request that happened to find the verdict stale: the verdict is
// shared by every request after it, and one request's cancellation must not
// become everybody's "not serving" for a second.
//
// statelog.ReadBudget, the budget a floor wait takes — the same "is this copy
// fit to answer" question, bounded the same way. It may exceed
// [servingRecheck]: nobody waits on a read but a request for a partition never
// judged, and that request waits no longer than its own context.
const servingRead = statelog.ReadBudget

// newRouter builds this node's router over the fleet's queue, routing by the
// presence view and answering in-process for what this node serves.
func (e *Engine) newRouter(q estate.Asker, nodeID string) (*estate.Router, error) {
	opts := estate.RouterOptions{
		Self: nodeID, Queue: q, Placement: e.estatePlacement(),
		Session: estate.NewSession(), Seams: e.serverSeams(),
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

// estatePlacement is who serves which partition as this node routes: under
// layout 0 — the only layout this build runs — every live data node, from the
// watched presence view.
func (e *Engine) estatePlacement() estate.Placement {
	return partmap.Whole{Running: LayoutZero(), Roster: presenceRoster{view: e.dataView}}
}

// newLocalEstate is this node's own backends, per partition holding says it
// serves — [holdingOf] the layout it runs, the one rule its write authority's
// gate 3 reads too.
func newLocalEstate(e *Engine, holding statelog.Holding) *localEstate {
	return &localEstate{e: e, holding: holding, now: time.Now,
		read: func(ctx context.Context, n *native, p statelog.PartitionID) copyVerdict {
			return n.log.partitionVerdict(ctx, p)
		},
		verdicts: map[statelog.PartitionID]*verdictSlot{}}
}

// localEstate is [estate.LocalBackends] over this node's own copy.
type localEstate struct {
	e       *Engine
	holding statelog.Holding
	now     func() time.Time

	// read judges a copy — the state log's own judgement in production, a
	// parameter for the tests.
	read func(ctx context.Context, n *native, p statelog.PartitionID) copyVerdict

	// mu guards verdicts and every slot in it, and is NEVER held across a
	// read of the broker — see [localEstate.verdict].
	mu       sync.Mutex
	verdicts map[statelog.PartitionID]*verdictSlot

	// cpus is the places a gather's queries of this node's copies take —
	// ONE for the node, which its server and its router both read here
	// ([localEstate.CPUs]).
	cpus estate.CPUs
}

// CPUs implements [estate.LocalBackends]: the node's one, so the batches it
// answers for other nodes and its own seats' gathers share its processors
// rather than each running a query per CPU beside the other.
func (l *localEstate) CPUs() *estate.CPUs { return &l.cpus }

// verdictSlot is one partition's verdict: the last one read, and the read in
// flight to replace it.
type verdictSlot struct {
	// held is the last verdict read, valid once known is.
	held  heldVerdict
	known bool

	// reading is closed when the read in flight lands, and nil while none
	// is: ONE read per partition at a time, however many requests found the
	// verdict stale.
	reading chan struct{}
}

// heldVerdict is one partition's last verdict, and when its read landed.
type heldVerdict struct {
	at time.Time
	copyVerdict
}

// For implements [estate.LocalBackends]: p's backend where this node serves p.
//
// A COPY THAT IS WRONG IS NOT SERVED. An applier halted at a record it cannot
// decode or held one past the deferral grace, an eviction, rows below the
// log's first record, a checkpoint on another stream, a stalled prefix: a copy
// in any of those states stops SERVING its partition — this node's router
// sends its own seats' calls to the partition's other holders, and another
// node asking is told `not_holder` — and never sheds the node's seats, which
// read the partition from a holder whose copy is sound. That is the whole of
// the remedy a fault needs: the seats were never the problem, the copy was,
// and moving them cost every one of them its processes and its memory to be
// served by the very same peers.
//
// A PARTITION HELD WITH NO RUNTIME YET — a data node that booted with no
// company — is served with no halves, so every operation on it is answered
// "no native backend here" and moves on, while the node still answers for it.
func (l *localEstate) For(ctx context.Context, p statelog.PartitionID) (estate.Backend, bool, error) {
	serving, err := l.holding.Serving(p)
	switch {
	case err != nil:
		// CANNOT TELL, kept apart from "does not serve" — gate 3's own
		// two refusals — so another node asking is told
		// `holding_unknown` rather than `not_holder`.
		return estate.Backend{}, false, fmt.Errorf("engine: whether this node serves %s: %w", p, err)
	case !serving:
		return estate.Backend{}, false, nil
	}
	n := l.e.native.Load()
	if n == nil {
		return estate.Backend{}, true, nil
	}
	v := l.verdict(ctx, n, p)
	if v.fault != "" {
		return estate.Backend{}, false, nil
	}
	b := l.e.partitionBackend(n, p)
	b.Answers = func(context.Context) bool { return v.answers }
	return b, true, nil
}

// verdict is p's verdict, read again once it is older than [servingRecheck].
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
// OFF the request path, one per partition at a time (the [verdictSlot]'s
// reading), and a request meanwhile answers from the verdict before it —
// nobody waits on a read but a request for a partition never judged, and that
// one waits no longer than its own context.
//
// # A reading that could not reach the broker changes nothing it could not see
//
// A copy that was answering keeps answering, and one that was faulted stays so
// unless a log that was read says otherwise — a broker blip must not send every
// request away from a sound copy, nor bring a wrong one back into service on no
// information. A copy never judged answers nothing: nobody has measured it.
func (l *localEstate) verdict(ctx context.Context, n *native, p statelog.PartitionID) copyVerdict {
	slot, v, known, reading := l.peek(ctx, n, p)
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
	return slot.held.copyVerdict
}

// peek is p's verdict as held NOW, never waiting: the last one read and true,
// or false where none has been — and it starts a read wherever the verdict
// held is older than [servingRecheck] and none is in flight. reading is closed
// when that read lands.
func (l *localEstate) peek(ctx context.Context, n *native, p statelog.PartitionID) (
	slot *verdictSlot, v copyVerdict, known bool, reading chan struct{}) {

	l.mu.Lock()
	defer l.mu.Unlock()
	slot = l.verdicts[p]
	if slot == nil {
		slot = &verdictSlot{}
		l.verdicts[p] = slot
	}
	if slot.known && l.now().Sub(slot.held.at) < servingRecheck {
		return slot, slot.held.copyVerdict, true, nil
	}
	reading = slot.reading
	if reading == nil {
		reading = make(chan struct{})
		slot.reading = reading
		// DETACHED from the request that found the verdict stale, since
		// every request after it shares what it reads, and BOUNDED by
		// [servingRead], which is the goroutine's whole lifetime.
		go l.refresh(context.WithoutCancel(ctx), n, p, slot, reading)
	}
	return slot, slot.held.copyVerdict, slot.known, reading
}

// servesUnasked reports whether this node answers p from its own copy as far
// as it can tell WITHOUT WAITING: it serves p, runs its native runtime, and the
// last verdict read of the copy — where one has been — found it sound.
//
// For serviceability, which asks it on every sweep and may not wait on the
// broker. A copy nobody has judged yet is not read as a wrong one — its first
// read is started here, and the next sweep has the answer — and a verdict held
// past the recheck answers while the read that replaces it runs, exactly as a
// request is answered.
func (l *localEstate) servesUnasked(p statelog.PartitionID) bool {
	serving, err := l.holding.Serving(p)
	if err != nil || !serving {
		return false
	}
	n := l.e.native.Load()
	if n == nil {
		return false
	}
	_, v, known, _ := l.peek(context.Background(), n, p)
	return !known || v.fault == ""
}

// refresh reads p's verdict once and stores it in slot, stamped with the
// instant the read LANDED: stamped with the instant it began, a read slower
// than [servingRecheck] would be stale the moment it was stored, and the next
// request would read again at once.
func (l *localEstate) refresh(ctx context.Context, n *native, p statelog.PartitionID,
	slot *verdictSlot, done chan struct{}) {

	defer close(done)
	read, cancel := context.WithTimeout(ctx, servingRead)
	defer cancel()
	v := l.read(read, n, p)

	l.mu.Lock()
	known, held := slot.known, slot.held
	if v.refusal == statelog.RefuseBrokerUnreachable {
		v.answers = known && held.answers
		if v.fault == "" && known {
			v.fault = held.fault
		}
	}
	slot.held, slot.known, slot.reading = heldVerdict{at: l.now(), copyVerdict: v}, true, nil
	l.mu.Unlock()

	if v.fault != "" && (!known || held.fault == "") {
		log.WarnContext(ctx, "estate_partition_not_served", "partition", p.String(),
			"log", v.fault,
			"detail", "this node's copy of the partition is wrong rather than behind, "+
				"so it stops serving it; its seats read the partition from its other "+
				"holders until the copy recovers")
	}
}

// partitionBackend is this data node's answer for partition p — under layout 0
// the whole native runtime, since p is the estate.
//
// WRITES ONLY WHILE THIS NODE PUBLISHES. A node restarted into a maintenance
// mode is the evidence a capacity operation is established from, and a write
// it took on anybody's behalf would be the publish the mode exists to rule
// out — so its writers are absent ([Engine.writeSide] hands none out) and
// every write is answered "not here".
func (e *Engine) partitionBackend(n *native, p statelog.PartitionID) estate.Backend {
	b := estate.Backend{
		Committed: e.WaitCommitted,
		Admits: func(ctx context.Context) bool {
			ok, refusal := n.log.PartitionEstablished(ctx, p, true)
			if !ok && refusal != "" {
				log.DebugContext(ctx, "seat_admission_withheld",
					"partition", p.String(), "reason", string(refusal))
			}
			return ok
		},
	}
	if n.log != nil {
		// WHERE THIS COPY IS, and the barrier a linearizable gather is
		// read after — each per log, since a partition carries several.
		b.Applied = func(stream string) statelog.Position {
			if running := n.log.logOf(stream); running != nil {
				return running.runner.Committed()
			}
			return statelog.Position{}
		}
		b.Barrier = func(ctx context.Context, stream string) (statelog.Position, bool, error) {
			running := n.log.logOf(stream)
			if running == nil || running.reader == nil {
				// A LOG WITH NO READ AUTHORITY makes no freshness
				// claim — the vectors' derived log.
				return statelog.Position{}, false, nil
			}
			at, err := running.reader.Barrier(ctx)
			return at, true, err
		}
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
	writer, store := e.writeSide()
	if writer != nil {
		b.Writer = func(a estate.Actor) estate.TrackerWriter {
			// A NIL INTERFACE, never a typed nil: the server reads nil
			// as "the tracker refused to act as this party".
			if w := writer.As(a.Handle, a.Kind, a.Provenance); w != nil {
				return w
			}
			return nil
		}
	}
	if store != nil {
		b.PageWriter = store
	}
	if n.log != nil && e.backends != nil && e.backends.Store != nil {
		b.Gates = e.partitionGates(n.log, p)
	}
	return b
}

// partitionGates is p's half of the `statelog.gate` operation: for each of p's
// identity-claiming logs this node runs, a writer that publishes a node gate's
// record through the log's own write authority — on behalf of the node an
// operator asked, which judged the gesture once and does not write this log
// itself ([estate.OpStatelogGate]). The record is the gate's own
// ([gateLogFor]), the one this node publishes for a gesture it runs, so the
// two can never differ in what they write.
//
// NONE WHILE THIS NODE PUBLISHES NOTHING — a capacity window, whose
// maintenance and seal modes are evidence precisely because nothing here
// appends — and none for a log this node does not run right now: each is "no
// native backend here", and the request moves on to the next holder.
func (e *Engine) partitionGates(s *stateLog, p statelog.PartitionID) func(domain string) estate.GateWriter {
	return func(domain string) estate.GateWriter {
		if s.appends("a gate record") != nil {
			return nil
		}
		id := statelog.LogID{Domain: domain, Partition: p}
		running := s.Log(id.String())
		if running == nil || !running.domain.ClaimsIdentity() {
			return nil
		}
		layout := s.layout
		return func(ctx context.Context, a estate.GateArgs) (statelog.Result, error) {
			// THE RECORD IS FOR THIS LOG OR FOR NONE: the request was
			// sent here for p, so a record naming another partition, or
			// a log of another layout, is one the asker resolved by a
			// map this node does not run.
			if a.Partition != p.String() || a.Layout != layout.Number {
				return statelog.Result{}, fmt.Errorf("%w: it names %s@%s of layout %d, and "+
					"was sent to %s for %s of layout %d", estate.ErrGateArgs, a.Domain,
					a.Partition, a.Layout, s.nodeID, id, layout.Number)
			}
			readmit, err := readmits(a.Kind)
			if err != nil {
				return statelog.Result{}, err
			}
			gl, err := gateLogFor(running, running.publisher,
				e.backends.Store.PartitionHandle(p.String()).Reader(), s.nodeID, e.metrics)
			if err != nil {
				return statelog.Result{}, err
			}
			return gl.write(ctx, a.By, a.OpID, a.Node, readmit)
		}
	}
}

// serverSeams is what this node supplies to every operation it answers,
// whichever partition it is on: its current chart and org, and its own event
// log, which takes custody of a stateless node's records whatever backends the
// company runs.
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
	if e.backends != nil && e.backends.Store != nil {
		seams.Events = e.backends.Store.Events()
	}
	return seams
}

// seatNeed is a partition a seat needs a serving holder of before it is
// admitted, and which of the company's native halves it needs there.
type seatNeed struct {
	partition      statelog.PartitionID
	tracker, pages bool
}

// seatNeeds is what seat admission waits for under layout l: for a native
// tracker, the partition holding its CATALOGUE, which every create reads; for
// a native knowledge base, its one partition where the layout gives it one.
// Under layout 0 both are estate.000.
//
// A KNOWLEDGE BASE DIVIDED BY CONTAINER adds nothing to wait for: no one of its
// partitions is the one every seat reads, and a page operation on a partition
// nobody serves is refused naming it, as every operation is. A layout that
// gives a native half NO partition to wait for — the tracker with neither one
// partition nor a company space holding its catalogue, the knowledge base with
// no log at all — is refused ([estate.ErrUnaddressed]) rather than read as
// nothing to wait for, which would admit a seat onto a company nobody checked.
func seatNeeds(l statelog.Layout, runTracker, wiki bool) ([]seatNeed, error) {
	var out []seatNeed
	need := func(p statelog.PartitionID, tracker, pages bool) {
		for i := range out {
			if out[i].partition == p {
				out[i].tracker = out[i].tracker || tracker
				out[i].pages = out[i].pages || pages
				return
			}
		}
		out = append(out, seatNeed{partition: p, tracker: tracker, pages: pages})
	}
	if runTracker {
		p, err := trackerCatalogue(l)
		if err != nil {
			return nil, err
		}
		need(p, true, false)
	}
	if wiki {
		name := pages.Domain{}.Name()
		switch logs := l.LogsOf(name); len(logs) {
		case 0:
			return nil, fmt.Errorf("%w: layout %d carries no log of the %s domain, so a "+
				"seat on a company running its knowledge base natively has nowhere to "+
				"read it", estate.ErrUnaddressed, l.Number, name)
		case 1:
			need(logs[0].Partition, false, true)
		}
	}
	return out, nil
}

// trackerCatalogue is the partition holding the tracker's catalogue under l:
// the domain's one partition where the layout gives it one, and otherwise the
// company space's, where a divided layout keeps the company-wide objects every
// tracker partition must see.
func trackerCatalogue(l statelog.Layout) (statelog.PartitionID, error) {
	name := tracker.Domain{}.Name()
	if p := l.OnlyPartition(name); p.Valid() {
		return p, nil
	}
	company := statelog.PartitionID{Space: statelog.SpaceCompany}
	for _, log := range l.LogsOf(name) {
		if log.Partition == company {
			return company, nil
		}
	}
	return statelog.PartitionID{}, fmt.Errorf("%w: layout %d gives the %s domain neither "+
		"one partition nor a company space to keep its catalogue in, and every create "+
		"reads the catalogue", estate.ErrUnaddressed, l.Number, name)
}

// routedPartitions is every partition a seat's calls may address under l on a
// company running these halves natively: each partition carrying a log of the
// tracker's domain or the knowledge base's, beside which the vectors a search
// reads are kept.
func routedPartitions(l statelog.Layout, runTracker, wiki bool) []statelog.PartitionID {
	var out []statelog.PartitionID
	add := func(domain string) {
		for _, log := range l.LogsOf(domain) {
			if !slices.Contains(out, log.Partition) {
				out = append(out, log.Partition)
			}
		}
	}
	if runTracker {
		add(tracker.Domain{}.Name())
	}
	if wiki {
		add(pages.Domain{}.Name())
	}
	return out
}

// presenceRoster is the fleet's presence view as the layout-0 placement's
// roster: every live data node, from memory.
type presenceRoster struct{ view *coord.LeaseView }

// LiveDataNodes implements [partmap.Roster]. A node with no view has no
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

// Invalidate implements [partmap.Roster]: a node the view named went silent,
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
