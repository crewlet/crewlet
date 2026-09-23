package notify_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// wire is a chat backend with a NETWORK in front of it, which is the one thing
// the other posters in this package leave out and the thing the teardown's
// ordering is about.
//
// A request the client sends is the SERVER's from then on. The client can stop
// waiting for the answer — its context ends — but that abandons the request
// rather than withdrawing it: the server still applies it, whenever it gets
// to it, and a clear sent on another connection in the meantime can be
// applied first. So a raise here is held until the test answers it, and a
// client that gives up returns at once while the raise stays held, to be
// applied when the test answers — after anything the client sent since.
//
// Every case drives it inside a synctest bubble, so "the teardown has done
// whatever it was going to do before the server answered" is
// [synctest.Wait] rather than a sleep: the ordering is reproduced exactly,
// on every run, with nothing left to load.
type wire struct {
	text    bool // renders text, as Slack's indicator does
	clears  bool // has a clear operation at all, which a typing indicator lacks
	hang    bool // answers nothing: every request waits out its own context
	refresh time.Duration

	mu   sync.Mutex
	log  []string
	held []chan struct{}

	clearErr      error
	clearBudget   time.Duration
	clearDeadline bool
}

func (w *wire) StatusBackend() string        { return "chat" }
func (w *wire) SupportsStatusText() bool     { return w.text }
func (w *wire) StatusRefresh() time.Duration { return w.refresh }
func (w *wire) DMChannelPrefix() string      { return "" }

func (w *wire) SetStatus(ctx context.Context, _, _, _, _ string) bool {
	if w.hang {
		<-ctx.Done()
		return false
	}
	answered := make(chan struct{})
	w.mu.Lock()
	w.held = append(w.held, answered)
	w.mu.Unlock()
	select {
	case <-answered:
		return true
	case <-ctx.Done():
		// The client gave up. The request did not: see answer.
		return false
	}
}

func (w *wire) ClearStatus(ctx context.Context, _, _, _ string) bool {
	w.mu.Lock()
	w.clearErr = ctx.Err()
	if deadline, ok := ctx.Deadline(); ok {
		w.clearBudget, w.clearDeadline = time.Until(deadline), true
	}
	w.mu.Unlock()
	if w.hang {
		<-ctx.Done()
		return false
	}
	if w.clears {
		w.note("clear")
	}
	return true
}

// answer has the server apply every raise it holds, in the order it got them,
// and respond to whichever client is still waiting.
func (w *wire) answer() {
	w.mu.Lock()
	held := w.held
	w.held = nil
	for range held {
		w.log = append(w.log, "raise")
	}
	w.mu.Unlock()
	for _, answered := range held {
		close(answered)
	}
}

func (w *wire) holding() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.held)
}

func (w *wire) note(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, event)
}

func (w *wire) applied() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.log...)
}

// teardown is one way an indicator is taken down, ending the session s the
// case opened on d, on the caller's context.
type teardown func(ctx context.Context, d *notify.StatusDriver, s *notify.StatusSession)

// teardowns are all three, each on a live caller's context and on a dead one —
// the ending being reported is often the cancellation itself: a shed seat, a
// drained node, a turn that ran out of time.
func teardowns() map[string]teardown {
	ways := map[string]teardown{
		"a turn ending": func(ctx context.Context, _ *notify.StatusDriver, s *notify.StatusSession) {
			s.End(ctx, false)
		},
		"a seat handed on": func(ctx context.Context, d *notify.StatusDriver, _ *notify.StatusSession) {
			d.ClearFor(ctx, "swe")
		},
		"the node stopping": func(ctx context.Context, d *notify.StatusDriver, _ *notify.StatusSession) {
			d.Stop(ctx)
		},
	}
	out := map[string]teardown{}
	for name, way := range ways {
		out[name] = way
		out[name+" on a dead context"] = func(ctx context.Context, d *notify.StatusDriver, s *notify.StatusSession) {
			dead, cancel := context.WithCancel(ctx)
			cancel()
			way(dead, d, s)
		}
	}
	return out
}

// A REQUEST THE SERVER ALREADY HOLDS IS APPLIED BEFORE THE TEARDOWN IS OVER —
// and so before the clear, on the backend that has one, and before the turn is
// over, on the backend that has none.
//
// The teardown cancelled the session's context, which made the post in flight
// return at once while the server still held it; the clear then went out, and
// the held raise was applied after it. On Slack that is an indicator left up
// over a turn that had ended, until Slack's own expiry took it down; on a
// typing indicator it is "typing…" shown after the agent's reply. Stopping the
// loop between posts and letting the one in flight finish is what orders them.
//
// Both kinds of post are covered, because both can be in flight: the opening
// raise, and a heartbeat re-assertion made long after it.
func TestARequestTheServerHoldsLandsBeforeTheTeardownEnds(t *testing.T) {
	backends := map[string]struct{ text, clears bool }{
		"a backend that renders and clears text": {text: true, clears: true},
		"a typing indicator with no clear":       {text: false, clears: false},
	}
	inFlight := map[string]bool{"the raise": false, "a re-assertion": true}
	for backendName, backend := range backends {
		for postName, heartbeat := range inFlight {
			for teardownName, teardown := range teardowns() {
				t.Run(backendName+"/"+postName+"/"+teardownName, func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						w := &wire{text: backend.text, clears: backend.clears,
							refresh: 45 * time.Second}
						d := notify.NewStatusDriver(notify.StatusOptions{
							Poster: w, Mode: notify.StatusAlways,
						})
						s := d.Begin(context.Background(), "swe", "turn-1", "plan", chatMeta(nil))
						if s == nil {
							t.Fatal("no indicator was raised")
						}
						synctest.Wait()
						if heartbeat {
							w.answer()
							time.Sleep(w.refresh)
							synctest.Wait()
						}
						if w.holding() != 1 {
							t.Fatalf("the premise: one post in flight, have %d", w.holding())
						}

						ended := make(chan struct{})
						go func() {
							teardown(context.Background(), d, s)
							w.note("over")
							close(ended)
						}()
						// EVERYTHING THE TEARDOWN WILL DO BEFORE THE
						// SERVER ANSWERS, it has now done: returned,
						// or blocked waiting for that answer.
						synctest.Wait()
						w.answer()
						<-ended

						want := []string{"raise", "clear", "over"}
						if heartbeat {
							want = append([]string{"raise"}, want...)
						}
						if !w.clears {
							want = slices.DeleteFunc(want, func(e string) bool { return e == "clear" })
						}
						if got := w.applied(); !slices.Equal(got, want) {
							t.Fatalf("the server applied %v, want %v — a request the "+
								"teardown abandoned landed after it", got, want)
						}
						d.Stop(context.Background())
					})
				})
			}
		}
	}
}

// A TEARDOWN IS BOUNDED BY ONE REQUEST'S BUDGET TO WAIT AND ONE TO CLEAR,
// however many indicators it takes down and however dead the backend is.
//
// Waiting for the post in flight is what orders the clear after it, and it
// would also be a wait for as long as the vendor client cared to take. So the
// post runs out at statusRequestTimeout and is abandoned after all — the one
// case where its fate is unknowable — and the clear gets a budget of its own.
// Taken one after another, a stop with three indicators against a chat
// instance that had stopped answering waited out six budgets.
func TestATeardownIsBoundedByOnePostAndOneClear(t *testing.T) {
	for name, teardown := range teardowns() {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// NO HEARTBEAT, so the only clocks in the bubble are the
				// requests' own: a teardown left waiting on anything else is
				// a deadlock the bubble reports at once rather than an hourly
				// tick advancing for ever.
				w := &wire{text: true, clears: true, hang: true}
				d := notify.NewStatusDriver(notify.StatusOptions{Poster: w, Mode: notify.StatusAlways})
				var s *notify.StatusSession
				for _, thread := range []string{"1718.001", "1718.002", "1718.003"} {
					session := d.Begin(context.Background(), "swe", "turn-"+thread, "plan",
						chatMeta(func(m map[string]string) { m["ts"] = thread }))
					if session == nil {
						t.Fatal("no indicator was raised")
					}
					if s == nil {
						s = session
					}
				}
				synctest.Wait()

				start := time.Now()
				teardown(context.Background(), d, s)
				took := time.Since(start)

				if limit := 2 * notify.StatusRequestTimeout; took > limit {
					t.Fatalf("the teardown took %v against a backend that answers "+
						"nothing, want at most %v", took, limit)
				}
				w.mu.Lock()
				clearErr, budget, bounded := w.clearErr, w.clearBudget, w.clearDeadline
				w.mu.Unlock()
				if clearErr != nil {
					t.Errorf("the clear was made on a context already done (%v), so it "+
						"was never sent", clearErr)
				}
				if !bounded || budget <= 0 || budget > notify.StatusRequestTimeout {
					t.Errorf("the clear had %v to answer in (bounded: %v), want at most %v",
						budget, bounded, notify.StatusRequestTimeout)
				}
				d.Stop(context.Background())
			})
		})
	}
}

// A STOP THAT ARRIVES WITH A WAKE WINS, so a teardown never waits out a
// re-assertion nobody asked for.
//
// A phase change queued while a post is in flight and a stop arriving before
// that post returns are both ready when it does, and select picks between
// ready cases at random. Taken, the wake is one more request to the backend —
// ordered correctly, since it too is waited for, but one the teardown pays for
// and the reader gains nothing from. Repeated, because the choice it guards
// is a coin toss: fifty rounds leave a loop without the guard no way through.
func TestAStopArrivingWithAWakeEndsTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for range 50 {
			w := &wire{text: true, clears: true, refresh: time.Hour}
			d := notify.NewStatusDriver(notify.StatusOptions{Poster: w, Mode: notify.StatusAlways})
			s := d.Begin(context.Background(), "swe", "turn-1", "plan", chatMeta(nil))
			synctest.Wait()
			s.Phase("review")

			ended := make(chan struct{})
			go func() { s.End(context.Background(), false); close(ended) }()
			synctest.Wait()
			w.answer()
			synctest.Wait()
			if w.holding() != 0 {
				t.Fatal("the loop took a queued wake over the stop that came with it " +
					"and posted again for a teardown already under way")
			}
			<-ended
			if got, want := w.applied(), []string{"raise", "clear"}; !slices.Equal(got, want) {
				t.Fatalf("the server applied %v, want %v", got, want)
			}
		}
	})
}
