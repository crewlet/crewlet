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
// read from), each answered from this node's own copy — and a copy that answers
// no request ([statelog.Health.Answers]: neither level this instant nor drained
// and within the snapshot slack) is passed over for another holder, by this
// node's router and by every other node's alike.

// Every placement the router may be handed.
var (
	_ estate.Placement = partmap.Whole{}
	_ estate.Placement = (*partmap.View)(nil)
)

// servingRecheck is how long a copy's verdict on whether it answers requests is
// trusted by the request gate ([localEstate.For]).
//
// ONE SECOND. The verdict reads every log's bounds from the broker — a
// JetStream API request per log — and a gate that asked per request would put
// three round trips in front of every tool call a data node answers. Refreshed
// at most once a second, a node pays at most three reads a second however many
// requests it answers; and a verdict a second old errs only about a copy that
// crossed the snapshot slack (a thousand records) inside that second, which the
// floors still hold to every write the node's own seats made.
const servingRecheck = time.Second

// servingRead bounds one verdict's reads of the broker, whatever the deadline
// of the request that happened to find the verdict stale: the verdict is
// shared by every request after it, and one request's cancellation must not
// become everybody's "not serving" for a second.
//
// statelog.ReadBudget, the budget a floor wait takes — the same "is this copy
// fit to answer" question, bounded the same way.
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

// newLocalEstate is this node's own backends, per partition it serves.
func newLocalEstate(e *Engine, boot *config.Bootstrap) *localEstate {
	return &localEstate{e: e, holding: holdingOf(boot, LayoutZero()), now: time.Now,
		read: func(ctx context.Context, n *native, p statelog.PartitionID) (bool, statelog.ReadRefusal) {
			return n.log.PartitionAnswers(ctx, p)
		},
		verdicts: map[statelog.PartitionID]servingVerdict{}}
}

// localEstate is [estate.LocalBackends] over this node's own copy.
type localEstate struct {
	e       *Engine
	holding statelog.Holding
	now     func() time.Time

	// read measures whether a copy answers requests — the state log's own
	// judgement in production, a parameter for the tests.
	read func(ctx context.Context, n *native, p statelog.PartitionID) (bool, statelog.ReadRefusal)

	mu       sync.Mutex
	verdicts map[statelog.PartitionID]servingVerdict
}

// servingVerdict is one partition's last serving verdict.
type servingVerdict struct {
	at      time.Time
	serving bool
}

// For implements [estate.LocalBackends]: p's backend where this node serves p.
//
// A PARTITION HELD WITH NO RUNTIME YET — a data node that booted with no
// company — is served with no halves, so every operation on it is answered
// "no native backend here" and moves on, while the node still answers for it.
func (l *localEstate) For(ctx context.Context, p statelog.PartitionID) (estate.Backend, bool) {
	serving, err := l.holding.Serving(p)
	if err != nil || !serving {
		return estate.Backend{}, false
	}
	n := l.e.native.Load()
	if n == nil {
		return estate.Backend{}, true
	}
	b := l.e.partitionBackend(n, p)
	b.Answers = func(ctx context.Context) bool { return l.answers(ctx, n, p) }
	return b, true
}

// answers is p's verdict on whether it answers requests, read again when it is
// older than [servingRecheck] — see [statelog.Health.Answers]. A verdict that
// could not be read keeps the one before it, and a copy never judged answers
// nothing: a broker blip must not send every request away from a copy that was
// answering, and a copy nobody has measured must not answer as though it had
// been.
func (l *localEstate) answers(ctx context.Context, n *native, p statelog.PartitionID) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	held, known := l.verdicts[p]
	if known && now.Sub(held.at) < servingRecheck {
		return held.serving
	}
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), servingRead)
	defer cancel()
	serving, refusal := l.read(read, n, p)
	if refusal == statelog.RefuseBrokerUnreachable {
		serving = known && held.serving
	}
	l.verdicts[p] = servingVerdict{at: now, serving: serving}
	return serving
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
	return b
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

// admissionPartitions is the partitions a seat on a company with these native
// halves needs a serving holder of under layout l: the tracker's catalogue
// partition, and the knowledge base's — under layout 0 both the one partition.
func admissionPartitions(l statelog.Layout, runTracker, wiki bool) []statelog.PartitionID {
	var out []statelog.PartitionID
	add := func(p statelog.PartitionID) {
		if p.Valid() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	if runTracker {
		add(l.OnlyPartition(tracker.Domain{}.Name()))
	}
	if wiki {
		add(l.OnlyPartition(pages.Domain{}.Name()))
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
