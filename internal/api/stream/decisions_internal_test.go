package stream

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// registered waits until n sockets are listening on svc.
func registered(t *testing.T, svc *Service, n int) {
	t.Helper()
	waitUntil(t, func() bool {
		svc.listeners.mu.Lock()
		defer svc.listeners.mu.Unlock()
		return len(svc.listeners.open) >= n
	}, "the sockets never started listening")
}

// SOCKETS ARE DECIDED IN THE NODE'S TURN, never all at once.
//
// A decision is an identity read, and every identity read on a node — every
// REST request's own authentication among them — runs on the ONE connection
// the store reserves for them. A move that decides every socket (a published
// company, a move naming everyone) used to put one read per open tab on that
// connection at the same instant, ahead of every request that arrived after
// it. So twelve sockets on twelve credentials are decided again by a move
// naming everyone, each read held open long enough that any two running
// together would overlap, and no two may.
//
// Mutation: let decisions run beside each other and several overlap.
func TestSocketDecisionsTakeTheNodesTurn(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	var running, most atomic.Int64
	read := func(r *http.Request) (*http.Request, *auth.Refusal) {
		now := running.Add(1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		return resolvedAs(person("ana"))(r)
	}
	const tabs = 12
	sockets := make([]*served, tabs)
	for i := range sockets {
		ana := person("ana")
		sockets[i] = serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
			decide: read})
	}
	for _, s := range sockets {
		s.settled(t, 1)
	}
	most.Store(0)

	svc.CredentialsMoved(Moved{Everyone: true})
	for _, s := range sockets {
		s.settled(t, 2)
	}
	if got := most.Load(); got != 1 {
		t.Fatalf("%d socket decisions ran at once, want 1: every one is a read "+
			"on the store's one identity connection", got)
	}
}

// SOCKETS ON ONE CREDENTIAL SHARE ONE DECISION PER MOVE.
//
// A person with six tabs open on one cookie holds six sockets, and their
// handshakes present the same credential, which is all the guard resolves
// them from: six reads of their rows per move would answer one question six
// times. So the six take one decision when they start listening and one more
// when a move ends the session, and every one of them closes on it.
//
// Mutation: start a decision for every socket that asks, and the move reads
// the rows once per tab.
func TestSocketsOnOneCredentialShareADecision(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana := person("ana")
	session := sessionOf(ana)
	var (
		mu    sync.Mutex
		ended bool
		reads atomic.Int64
	)
	decide := func(r *http.Request) (*http.Request, *auth.Refusal) {
		mu.Lock()
		over := ended
		mu.Unlock()
		defer reads.Add(1)
		if over {
			return r.WithContext(iam.WithAnonymous(r.Context())), nil
		}
		return resolvedAs(ana)(r)
	}
	const tabs = 6
	sockets := make([]*served, tabs)
	for i := range sockets {
		sockets[i] = serve(t, svc, socketCase{principal: ana, opened: session,
			decide: decide, key: "one-cookie"})
	}
	registered(t, svc, tabs)
	waitUntil(t, func() bool { return reads.Load() >= 1 },
		"the sockets never decided their credential")
	if got := reads.Load(); got != 1 {
		t.Fatalf("six sockets on one cookie read its rows %d times as they "+
			"started listening, want once", got)
	}

	mu.Lock()
	ended = true
	mu.Unlock()
	svc.CredentialsMoved(Moved{Sessions: []string{session.lineage}})
	for i, s := range sockets {
		if got := s.closedWith(t); got != CloseUnauthenticated {
			t.Fatalf("tab %d closed %d, want %d", i, got, CloseUnauthenticated)
		}
	}
	if got := reads.Load(); got != 2 {
		t.Fatalf("a move ending one cookie's session read its rows %d times in "+
			"all, want twice: once as the tabs listened and once for the move", got)
	}
}

// A CREDENTIAL'S OWN END IS NEVER ANSWERED FROM AN EARLIER DECISION.
//
// No move is counted at a deadline, so a decision taken before it covers every
// wake since — and answered from one, a socket at its session's absolute
// deadline would be told the session is live, re-arm, and be told so again a
// second later, for ever. Two tabs on one cookie share every decision until
// then; at the deadline the guard refuses the session and both must close.
//
// Mutation: answer the end with the count a wake reads, and both tabs stay
// open past it.
func TestACredentialsOwnEndIsNeverAnsweredFromAnEarlierDecision(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana := person("ana")
	session := sessionOf(ana)
	ends := time.Now().Add(400 * time.Millisecond)
	untilEnds := func(r *http.Request) (*http.Request, *auth.Refusal) {
		if time.Now().Before(ends) {
			return resolvedAs(ana)(r)
		}
		return r.WithContext(iam.WithAnonymous(r.Context())), nil
	}
	first := serve(t, svc, socketCase{principal: ana, opened: session, ends: ends,
		decide: untilEnds, key: "one-cookie"})
	second := serve(t, svc, socketCase{principal: ana, opened: session, ends: ends,
		decide: untilEnds, key: "one-cookie"})
	for _, s := range []*served{first, second} {
		if got := s.closedWith(t); got != CloseUnauthenticated {
			t.Fatalf("a tab at its session's deadline closed %d, want %d", got,
				CloseUnauthenticated)
		}
	}
}

// A DECISION ITS STARTER ABANDONED IS NOT ANOTHER SOCKET'S ANSWER.
//
// A decision is read in the node's turn, and the socket that started it may
// close while it waits for that turn: nothing was read, so a socket on the
// same credential must take its own rather than close on an answer nobody
// gave — whether it was already waiting on that decision when its starter
// left, or asks after. Both orders: the first is reached by giving the waiter
// time to reach its wait before the starter leaves, the second by asking once
// the starter has gone.
//
// Mutations: hand a waiter the abandoned decision's empty answer and the
// waiter that was waiting is given none; treat an abandoned decision as still
// covering its moves and the later asker waits on it for ever.
func TestADecisionItsStarterAbandonedIsNotAnAnswer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		waitingFirst bool
	}{
		{"a socket already waiting on it", true},
		{"a socket asking after it", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d := newDecisions()
			held, release := d.hold("one-cookie")
			defer release()
			// THE TURN IS TAKEN, so the first decision waits for it.
			d.turn <- struct{}{}

			gone, leave := context.WithCancel(t.Context())
			abandoned := make(chan struct{})
			go func() {
				defer close(abandoned)
				d.decide(gone, held, 0,
					func(context.Context) (*http.Request, *auth.Refusal) {
						t.Error("a decision whose socket left before its turn was read")
						return nil, nil
					})
			}()
			waitUntil(t, func() bool {
				d.mu.Lock()
				defer d.mu.Unlock()
				return held.latest != nil
			}, "the first decision never started")

			answer := make(chan bool, 1)
			ask := func() {
				r, _, ok := d.decide(t.Context(), held, 0,
					func(ctx context.Context) (*http.Request, *auth.Refusal) {
						r, err := http.NewRequestWithContext(ctx, http.MethodGet, "/", nil)
						if err != nil {
							t.Error(err)
							return nil, nil
						}
						return r.WithContext(iam.WithAnonymous(ctx)), nil
					})
				answer <- ok && r != nil
			}
			if c.waitingFirst {
				go ask()
				// LONG ENOUGH TO REACH ITS WAIT: nothing it does
				// before that blocks.
				time.Sleep(50 * time.Millisecond)
			}
			// THE STARTER LEAVES, and only once it has gone is the turn
			// free: handed back sooner, the starter could take it as it
			// left.
			leave()
			<-abandoned
			if !c.waitingFirst {
				go ask()
			}
			<-d.turn
			select {
			case got := <-answer:
				if !got {
					t.Fatal("a socket on the credential of an abandoned " +
						"decision was given no answer")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a socket on the credential of an abandoned decision " +
					"never decided")
			}
		})
	}
}
