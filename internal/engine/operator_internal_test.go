package engine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// peerCopy is a peer data node's copy of the estate, answering what an
// operator's surfaces ask with values that say it answered.
type peerCopy struct {
	mu     sync.Mutex
	purged []estate.Actor
}

func (p *peerCopy) For(context.Context) (estate.Backend, bool) {
	return estate.Backend{
		Tracker: peerBoard{}, Pages: peerPages{},
		Writer: func(a estate.Actor) estate.TrackerWriter {
			return &peerWriter{peer: p, actor: a}
		},
	}, true
}

// peerBoard is the peer's tracker reads.
type peerBoard struct{ estate.TrackerReader }

func (peerBoard) Tasks(context.Context, worklet.Query, time.Time) (worklet.Answer, error) {
	return worklet.Answer{TotalHint: 42}, nil
}

func (peerBoard) Inbox(context.Context, worklet.InboxQuery, time.Time) (worklet.InboxAnswer, error) {
	return worklet.InboxAnswer{Unread: 3}, nil
}

// peerPages is the peer's knowledge-base reads.
type peerPages struct{ estate.PageReader }

func (peerPages) Containers(context.Context, statelog.Freshness) ([]doclet.ContainerListing, error) {
	return []doclet.ContainerListing{{Key: "ENG"}}, nil
}

// peerWriter is the peer's tracker writer, acting as one party.
type peerWriter struct {
	estate.TrackerWriter
	peer  *peerCopy
	actor estate.Actor
}

func (w *peerWriter) PurgeTask(_ context.Context, opID, _, _, _ string) (worklet.WriteResult, error) {
	w.peer.mu.Lock()
	w.peer.purged = append(w.peer.purged, w.actor)
	w.peer.mu.Unlock()
	return worklet.WriteResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID}}, nil
}

// twoDataNodes names this node and a peer as the data nodes.
type twoDataNodes struct{}

func (twoDataNodes) Holders() ([]string, error) { return []string{"data-peer", "data-self"}, nil }
func (twoDataNodes) Unanswered(string)          {}

// THE OPERATOR'S SURFACES ARE ANSWERED THROUGH THE ROUTER, AS THE SEATS' TOOLS
// ARE — the dashboard's and the REST routes' reads, the file routes, the purge
// and the operator's own MCP. Read straight off this node's own copy, a data
// node whose copy was out of service — wrong rather than behind, or its file
// shut — sent its seats' calls to a peer and went on answering its operator
// from the copy it had stopped serving, or refused outright where the file was
// shut. Here this node's copy is wrong, and every surface is answered by the
// peer — the operator's own writes included, attributed to the operator.
func TestTheOperatorsSurfacesAreAnsweredThroughTheRouter(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	start := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	peer := &peerCopy{}
	envelopes := routerEnvelopes(t)
	stop, err := estate.Serve(t.Context(), start(), estate.ServerOptions{
		Self: "data-peer", Local: peer, Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })

	var faulted atomic.Bool
	e := &Engine{backends: &Backends{}, mode: statelog.ModeNormal}
	// A DATA NODE: its core runs, and the copy it holds is judged.
	withCore(e)
	e.native.Store(&native{trackerReader: &worklet.Reader{}, pageReader: &doclet.Reader{},
		writer: &worklet.Writer{}, pages: &doclet.Store{}})
	e.local = localWith(e, time.Now, func(context.Context, *core) copyVerdict {
		if faulted.Load() {
			return copyVerdict{fault: "tracker", answers: true}
		}
		return copyVerdict{answers: true}
	})
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "data-self", Queue: start(), Placement: twoDataNodes{},
		Local: e.local, Session: estate.NewSession(), Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.router = router
	faulted.Store(true)
	if _, ok := judged(t.Context(), e.local); ok {
		t.Fatal("the premise: this node's wrong copy is still served")
	}

	work, ok := OperatorWork(e)
	if !ok {
		t.Fatal("a data node running Worklet hands its operator no Worklet")
	}
	if answer, err := work.Tasks(t.Context(), worklet.Query{}, time.Now()); err != nil ||
		answer.TotalHint != 42 {
		t.Fatalf("the board read (%+v, %v), want the peer's answer", answer, err)
	}
	if inbox, err := work.Inbox(t.Context(), worklet.InboxQuery{}, time.Now()); err != nil ||
		inbox.Unread != 3 {
		t.Fatalf("a person's inbox read (%+v, %v), want the peer's answer", inbox, err)
	}
	kb, ok := OperatorPages(e)
	if !ok {
		t.Fatal("a data node running Doclet hands its operator none")
	}
	if containers, err := kb.Containers(t.Context(), statelog.Freshness{}); err != nil ||
		len(containers) != 1 || containers[0].Key != "ENG" {
		t.Fatalf("the containers read (%+v, %v), want the peer's answer", containers, err)
	}
	as, ok := OperatorWorkWriter(e)
	if !ok {
		t.Fatal("a publishing data node hands its operator no tracker writer")
	}
	operator := estate.Actor{Handle: "founder", Kind: worklet.AuthorOperator,
		Provenance: worklet.Provenance{OperatorID: "founder"}}
	if _, err := as(operator).PurgeTask(t.Context(), "op-purge", "t-1", "ENG", ""); err != nil {
		t.Fatalf("an operator's purge on a node whose copy is wrong: %v", err)
	}
	peer.mu.Lock()
	purged := peer.purged
	peer.mu.Unlock()
	if len(purged) != 1 || purged[0].Handle != "founder" || purged[0].Kind != worklet.AuthorOperator ||
		purged[0].Provenance.OperatorID != "founder" {
		t.Fatalf("the peer purged as %+v, want the operator", purged)
	}

	// IN A MAINTENANCE MODE this node hands out no writer, as it hands its
	// seats none: the mode is the evidence nothing here publishes.
	e.mode = statelog.ModeMaintenance
	if _, ok := OperatorWorkWriter(e); ok {
		t.Error("a node in a maintenance mode handed its operator a tracker writer")
	}
	if _, ok := OperatorPageWriter(e); ok {
		t.Error("a node in a maintenance mode handed its operator a knowledge-base writer")
	}
	if _, ok := OperatorWork(e); !ok {
		t.Error("a node in a maintenance mode stopped answering its operator's reads")
	}

	// AND A COMPANY ON VENDORS is handed none of them.
	vendors := &Engine{router: router}
	vendors.remote.Store(&remoteNative{})
	if _, ok := OperatorWork(vendors); ok {
		t.Error("a company on a vendor tracker was handed Worklet")
	}
	if _, ok := OperatorPages(vendors); ok {
		t.Error("a company on a vendor knowledge base was handed Doclet")
	}
}
