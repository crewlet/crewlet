package stream

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// waitingAt is how many of who's questions wait at g for a slot.
func waitingAt(g *queryGate, who uuid.UUID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if l := g.lanes[who]; l != nil {
		return l.waiting.Len()
	}
	return 0
}

// blockingQueries is a query surface whose every question says who asked it
// and then waits for one release, or for its socket to go.
type blockingQueries struct {
	entered chan string
	release chan struct{}
}

func newBlockingQueries() *blockingQueries {
	return &blockingQueries{entered: make(chan string, 128), release: make(chan struct{})}
}

func (b *blockingQueries) query(ctx context.Context, _ string, _ map[string]any) (any, error) {
	p, _ := iam.From(ctx)
	b.entered <- p.Login
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return nil, nil
}

// next is who asked the next question to run.
func (b *blockingQueries) next(t *testing.T) string {
	t.Helper()
	select {
	case who := <-b.entered:
		return who
	case <-time.After(10 * time.Second):
		t.Fatal("no question ran")
		return ""
	}
}

// none proves no further question runs for a while.
func (b *blockingQueries) none(t *testing.T, why string) {
	t.Helper()
	select {
	case who := <-b.entered:
		t.Fatalf("%s: %s's question ran", why, who)
	case <-time.After(250 * time.Millisecond):
	}
}

// ask writes n questions on s.
func (s *served) ask(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		s.write(t, map[string]any{"kind": "query", "id": i + 1, "what": "events"})
	}
}

// A SECOND PRINCIPAL IS ADMITTED ON THE NEXT FREE SLOT, however many questions
// the first has queued.
//
// The node's ceiling on every socket's questions together is one more thing a
// caller who opens sockets could take whole: served first come first served,
// one person's three tabs at four questions each sat ahead of every colleague's
// first, and on a host at the store's reader floor the ceiling is four — one
// tab's standing reads fill it. So the ceiling is shared out by principal: here
// it is one slot, ana holds it and has eleven more waiting across three tabs,
// and ben's one question is the next to run when ana's ends — ahead of all
// eleven, which arrived first.
//
// Mutations: serve the line first come first served, or in the order the
// principals joined it, and ana's next question runs instead.
func TestASecondPrincipalIsAdmittedOnTheNextFreeSlot(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	svc.queries = newQueryGate(1)
	questions := newBlockingQueries()
	ana, ben := person("ana"), person("ben")

	for range 3 {
		tab := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
			decide: resolvedAs(ana), query: questions.query})
		tab.ask(t, MaxInFlightQueries)
	}
	if got := questions.next(t); got != "ana" {
		t.Fatalf("the first question to run was %s's, want ana's", got)
	}
	waitUntil(t, func() bool { return waitingAt(svc.queries, ana.ID) == 11 },
		"ana's other eleven questions never reached the ceiling")

	colleague := serve(t, svc, socketCase{principal: ben, opened: sessionOf(ben),
		decide: resolvedAs(ben), query: questions.query})
	colleague.ask(t, 1)
	waitUntil(t, func() bool { return waitingAt(svc.queries, ben.ID) == 1 },
		"ben's question never reached the ceiling")

	questions.release <- struct{}{}
	if got := questions.next(t); got != "ben" {
		t.Fatalf("the slot ana's question gave back went to %s, want ben: one "+
			"principal's tabs held the node's ceiling against a colleague", got)
	}
	questions.none(t, "a ceiling of one ran a second question")
}

// A SOCKET WHOSE QUESTIONS WAIT AT THE CEILING STILL READS ITS FRAMES.
//
// The four a socket runs wait at the node's ceiling for OTHER sockets'
// questions, so a bound taken on the read loop — a fifth question stopping the
// reader until one of the four ended — held the socket's ping and its watch
// behind somebody else's burst. Here ben holds the only slot, ana's four wait
// for it and three more are behind them, and ana's ping is answered.
//
// Mutation: take the socket's bound on its read loop again, and the ping waits
// until ben's question ends.
func TestASocketWaitingAtTheCeilingStillReadsItsFrames(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	svc.queries = newQueryGate(1)
	questions := newBlockingQueries()
	ana, ben := person("ana"), person("ben")

	holder := serve(t, svc, socketCase{principal: ben, opened: sessionOf(ben),
		decide: resolvedAs(ben), query: questions.query})
	holder.ask(t, 1)
	if got := questions.next(t); got != "ben" {
		t.Fatalf("the first question to run was %s's, want ben's", got)
	}

	tab := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: resolvedAs(ana), query: questions.query})
	tab.ask(t, MaxInFlightQueries+3)
	waitUntil(t, func() bool { return waitingAt(svc.queries, ana.ID) == MaxInFlightQueries },
		"ana's questions never reached the ceiling")
	tab.open(t)
	questions.none(t, "a ceiling ben holds ran ana's question")
}

// A SOCKET'S FULL BACKLOG IS TOLD WHEN TO ASK AGAIN, and the socket stays open.
//
// A question beyond a socket's four waits in its backlog, which bounds what a
// client sending faster than it is answered makes the node hold. One beyond
// THAT is answered at once — `unavailable`, with the one-second hint, under its
// own id — rather than read off the wire into a queue with no end, or held on
// the read loop, which would stop the keepalive. The socket is still open
// afterwards: a client that sent too much has done nothing that ends it.
//
// Mutation: drop the backlog's bound and no answer comes.
func TestASocketsFullBacklogIsToldWhenToAskAgain(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	questions := newBlockingQueries()
	ana := person("ana")
	tab := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: resolvedAs(ana), query: questions.query})

	tab.ask(t, MaxInFlightQueries+MaxQueuedQueries+1)
	refused := int64(MaxInFlightQueries + MaxQueuedQueries + 1)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		_, raw, err := tab.conn.Read(ctx)
		if err != nil {
			t.Fatalf("no answer refused the question past the backlog: %v", err)
		}
		var env struct {
			Kind       Kind   `json:"kind"`
			ID         int64  `json:"id"`
			Error      string `json:"error"`
			RetryAfter *int   `json:"retry_after"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if env.Kind != KindError {
			continue
		}
		if env.ID != refused || env.Error != CodeUnavailable ||
			env.RetryAfter == nil || *env.RetryAfter != 1 {
			t.Fatalf("the question past the backlog was answered %s, want "+
				"unavailable for id %d with retry_after 1", raw, refused)
		}
		break
	}
	tab.open(t)
}

// THE CEILING KEEPS NOTHING FOR A PRINCIPAL WHO HOLDS NOTHING.
//
// A lane is keyed on a principal, and principals are whoever can authenticate:
// a lane left behind by a question that ran, or by one whose socket went away
// while it waited — granted in the same instant or not — is one per person who
// ever opened a tab, held for the life of the process. And a slot left held is
// a ceiling one smaller for good.
func TestTheQueryCeilingKeepsNothingForAPrincipalWhoHoldsNothing(t *testing.T) {
	t.Parallel()
	g := newQueryGate(1)
	ana, ben := uuid.New(), uuid.New()

	release, err := g.take(t.Context(), ana)
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	gone, leave := context.WithCancel(t.Context())
	left := make(chan error, 1)
	go func() {
		_, err := g.take(gone, ben)
		left <- err
	}()
	waitUntil(t, func() bool { return waitingAt(g, ben) == 1 },
		"ben's question never reached the ceiling")
	leave()
	if err := <-left; err == nil {
		t.Fatal("a question whose socket went away was granted a slot")
	}
	release()
	release()
	if slots, lanes := g.held(); slots != 0 || lanes != 0 {
		t.Fatalf("the ceiling holds %d slots for %d principals after every "+
			"question ended, want none", slots, lanes)
	}
	if again, err := g.take(t.Context(), ben); err != nil {
		t.Fatalf("the slot given back twice is not free once: %v", err)
	} else {
		again()
	}
	g.mu.Lock()
	free := g.free
	g.mu.Unlock()
	if free != 1 {
		t.Fatalf("a release called twice gave back %d slots of a ceiling of one",
			free)
	}
}
