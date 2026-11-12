package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/workkey"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Two heads of settledPull.
const (
	headA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	headB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// openPull is a pull request open at head, opened by the waiting seat's app,
// that nothing moves.
func openPull(head string) github.PullMovers {
	return github.PullMovers{State: "open", Head: head, Author: "acme-ceo[bot]",
		Requested: []string{}, Reviewed: []string{}}
}

// waitingTask files one task held by ceo in status, waiting on settledPull,
// and answers its id.
func waitingTask(t *testing.T, e *Engine, status worklet.Status) string {
	t.Helper()
	writer := e.native.Load().writer
	at := time.Now().UTC()
	key := fmt.Sprintf("D%05d", seededProjects.Add(1))
	mustApply(t, "the project", func() (worklet.WriteResult, error) {
		return writer.WriteDocument(t.Context(), statelog.NewOpID(at, "project"),
			worklet.ProjectSubject(key), "", worklet.Project{V: worklet.DocumentVersion, Key: key,
				Name: "Waits " + key, CreatedAt: at, UpdatedAt: at}, worklet.ChangeProjectCreated, nil)
	})
	id := key + "-t0"
	mustApply(t, "task "+id, func() (worklet.WriteResult, error) {
		return writer.CreateTask(t.Context(), statelog.NewOpID(at, "create"), worklet.Task{
			V: worklet.DocumentVersion, ID: id, Project: key, Type: "task", Title: "ship it",
			Assignee: "ceo", Status: status, StatusGroup: status.Group(),
			Priority: worklet.PriorityNormal, CreatedAt: at, UpdatedAt: at,
		}, nil)
	})
	waitAlsoOn(t, e, id, settledPull)
	return id
}

// deadWaitNode is a running node on GitHub whose ceo seat holds one task in
// status waiting on settledPull, the host and the held merge asks stubbed.
func deadWaitNode(t *testing.T, status worklet.Status) (*Engine, string) {
	t.Helper()
	e, _ := aRunningNodeOf(t, nativeCleanupCompany)
	e.githubPulls.wire(&github.SeatLookup{Opts: github.SeatAppOptions{WebBase: "https://github.com"}},
		[]string{"acme"})
	return e, waitingTask(t, e, status)
}

// deadWaitOf is what id's own record holds of settledPull, and whether it holds any.
func deadWaitOf(t *testing.T, e *Engine, id string) (worklet.DeadWait, bool) {
	t.Helper()
	got, err := e.native.Load().trackerReader.Task(t.Context(), id, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	d, ok := got.Task.DeadWaits[settledPull]
	return d, ok
}

// poll is one pass of the change poll at now that must not fail.
func poll(t *testing.T, e *Engine, now time.Time) {
	t.Helper()
	if _, err := e.pollChanges(t.Context(), now, time.Time{}); err != nil {
		t.Fatalf("poll at %s: %v", now.Format(time.RFC3339), err)
	}
}

// freshReady reads ready work at the level that has every write the case made.
type freshReady struct{ r *worklet.Reader }

func (f freshReady) ReadyWork(ctx context.Context, q worklet.ReadyWorkQuery, now time.Time) (worklet.ReadyWork, error) {
	q.Level = statelog.ReadLinearizable
	return f.r.ReadyWork(ctx, q, now)
}

// pickupFleet is the fleet's completion ledger and claims, and the seat's
// inbox, for a picker over the node's own tracker.
type pickupFleet struct {
	mu     sync.Mutex
	worked map[string]time.Time
	claims map[string]time.Time
	got    []*events.Event
}

func (f *pickupFleet) WorkedAt(_ context.Context, _ string, keys []string) map[string]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]time.Time{}
	for _, k := range keys {
		if at, ok := f.worked[k]; ok {
			out[k] = at
		}
	}
	return out
}

func (f *pickupFleet) Claim(_ context.Context, key string, ttl time.Duration, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if until, held := f.claims[key]; held && now.Before(until) {
		return false, nil
	}
	f.claims[key] = now.Add(ttl)
	return true, nil
}

func (f *pickupFleet) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, key)
	return nil
}

func (f *pickupFleet) Publish(_ context.Context, _ string, ev *events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, ev)
	return nil
}

// turnOn is the seat's turn on the newest pickup, as the ledger records it.
func (f *pickupFleet) turnOn(at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.worked[workkey.Derive([]string{f.got[len(f.got)-1].ID.String()})] = at
}

// idleSeat is a seat its node finds idle on every pass.
type idleSeat struct{}

func (idleSeat) Conditions(context.Context, pickup.Seat) pickup.Conditions {
	return pickup.Conditions{Owned: true, TurnEngineReady: true}
}

// handOut is one settled pickup decision for ceo at now: two passes, so the
// idle settle is behind it.
func handOut(e *Engine, f *pickupFleet, now time.Time) pickup.Outcome {
	p := &pickup.Picker{Holder: idleSeat{}, Ready: freshReady{e.native.Load().trackerReader},
		Ledger: f, Claims: f, Publisher: f, Now: func() time.Time { return now }, Cooldown: 10 * time.Minute}
	org := e.Company().Org
	id, _ := org.AgentIDFor(org.AgentSeatByHandle("ceo"))
	seat := pickup.Seat{ID: id, Handle: "ceo", Role: "CEO"}
	p.Pass(context.Background(), []pickup.Seat{seat})
	return p.Pass(context.Background(), []pickup.Seat{seat})[0]
}

// pickupOf is a published pickup's payload.
func pickupOf(t *testing.T, ev *events.Event) types.WorkPickup {
	t.Helper()
	p, ok := events.DataAs[*types.WorkPickup](ev)
	if !ok || p == nil {
		t.Fatalf("event %s carries no pickup", ev.ID)
	}
	return *p
}

// THE INCIDENT, REPRODUCED: a seat opened a pull request for its own item and
// answered its pickup with a wait on it, and nobody was asked to review or
// merge it. Before ADR-0128 the item left every wake for good. Now, once the
// wait has stood its grace, the engine records that nothing moves the pull
// request and hands the item back to its holder once; not again for the same
// head, however long it sits; again for a new head that nothing moves either;
// and not at all once somebody is asked to review it.
func TestAWaitOnAPullRequestNothingMovesIsHandedBackOncePerHead(t *testing.T) {
	t.Parallel()
	e, id := deadWaitNode(t, worklet.StatusInProgress)
	stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)}})
	f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
	start := time.Now()

	// INSIDE THE GRACE: a seat that asks for a review in the same turn is never woken.
	poll(t, e, start)
	if _, dead := deadWaitOf(t, e, id); dead {
		t.Fatal("a wait recorded moments ago was judged dead inside its grace")
	}
	if out := handOut(e, f, start); out.Woken {
		t.Fatalf("an item waiting inside its grace was handed out: %+v", out)
	}

	at := start.Add(deadWaitGrace + time.Minute)
	poll(t, e, at)
	if d, dead := deadWaitOf(t, e, id); !dead || d.Head != headA || d.Author != "acme-ceo[bot]" {
		t.Fatalf("after the grace the wait reads %+v (dead %v), want dead at %s", d, dead, headA)
	}
	out := handOut(e, f, at)
	if !out.Woken || out.Item.ID != id || out.Item.Why != types.PickupDeadWait {
		t.Fatalf("the dead wait's holder was handed %+v, want %s back as a dead wait", out, id)
	}
	p := pickupOf(t, f.got[0])
	if len(p.DeadWaits) != 1 || p.DeadWaits[0].Ref != settledPull || p.DeadWaits[0].Head != headA {
		t.Errorf("the pickup names %+v, want %s at %s", p.DeadWaits, settledPull, headA)
	}
	if wake := types.WakeOf(types.Trigger{Type: p.EventType(), Origin: p.TriggerOrigin()}); wake != "work_item:dead_wait" {
		t.Errorf("the turn ledger counts the hand-back as %q, want work_item:dead_wait", wake)
	}

	// THE SAME HEAD, A DAY AND MORE LATER, ON A RESTARTED NODE: never again.
	f.turnOn(at)
	later := at.Add(pickup.RecheckAfter + time.Hour)
	poll(t, e, later)
	if out := handOut(e, f, later); out.Woken {
		t.Errorf("the same head was handed back again: %+v", out)
	}

	// A NEW HEAD THAT NOTHING MOVES EITHER: handed back once more.
	stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headB)}})
	poll(t, e, later.Add(time.Minute))
	out = handOut(e, f, later.Add(time.Minute))
	if !out.Woken || len(f.got) != 2 || pickupOf(t, f.got[1]).DeadWaits[0].Head != headB {
		t.Fatalf("a new head nothing moves was handed %+v (%d published), want it back at %s",
			out, len(f.got), headB)
	}

	// SOMEBODY IS ASKED TO REVIEW IT: the wait is alive, and the item blocked again.
	f.turnOn(later.Add(time.Minute))
	moving := openPull(headB)
	moving.Requested = []string{"reviewer"}
	stubPulls(e, map[string]pullRead{settledPull: {m: moving}})
	poll(t, e, later.Add(2*time.Minute))
	if _, dead := deadWaitOf(t, e, id); dead {
		t.Error("a pull request somebody was asked to review still reads as a dead wait")
	}
	if out := handOut(e, f, later.Add(3*time.Minute)); out.Woken || out.Skip != pickup.SkipNothing {
		t.Errorf("an item waiting on a reviewed pull request was handed %+v, want nothing ready", out)
	}
}

// A WAIT WITH SOMEBODY OR SOMETHING TO MOVE IT IS LEFT ALONE: a reviewer asked,
// a review of the head, a merge armed on the host, or a merge asked and held.
func TestAWaitSomebodyMovesIsLeftAlone(t *testing.T) {
	t.Parallel()
	requested, reviewed, armed := openPull(headA), openPull(headA), openPull(headA)
	requested.Requested = []string{"team:web"}
	reviewed.Reviewed = []string{"reviewer"}
	armed.AutoMerge = true
	for _, tc := range []struct {
		name  string
		m     github.PullMovers
		asked bool
	}{
		{"a reviewer asked", requested, false},
		{"a review of the head", reviewed, false},
		{"a merge armed", armed, false},
		{"a merge asked and held", openPull(headA), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, id := deadWaitNode(t, worklet.StatusInProgress)
			e.changePoll.asked = func(context.Context) (map[string]bool, error) {
				return map[string]bool{settledPull: tc.asked}, nil
			}
			stubPulls(e, map[string]pullRead{settledPull: {m: tc.m}})
			at := time.Now().Add(deadWaitGrace + time.Minute)
			poll(t, e, at)
			if d, dead := deadWaitOf(t, e, id); dead {
				t.Errorf("a wait with %s was recorded dead: %+v", tc.name, d)
			}
			f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
			if out := handOut(e, f, at); out.Woken {
				t.Errorf("a wait with %s was handed back: %+v", tc.name, out)
			}
		})
	}
}

// AN ITEM SOMEBODY ELSE IS MOVING IS NOT HANDED BACK, though nothing moves its
// pull request: one in review that a reviewer holds, and one whose holder has
// asked somebody on it.
func TestAnItemSomebodyMovesIsNotHandedBack(t *testing.T) {
	t.Parallel()
	t.Run("in review held by a reviewer", func(t *testing.T) {
		t.Parallel()
		e, id := deadWaitNode(t, worklet.StatusInReview)
		// THE ITEM WAS FILED IN REVIEW BY NOBODY BUT THE ENGINE, so ceo holds a
		// review somebody else asked for: it is the reviewer.
		stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)}})
		at := time.Now().Add(deadWaitGrace + time.Minute)
		poll(t, e, at)
		if _, dead := deadWaitOf(t, e, id); !dead {
			t.Fatal("nothing moving the pull request was not recorded")
		}
		f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
		if out := handOut(e, f, at); out.Woken {
			t.Errorf("an item a reviewer holds in review was handed back: %+v", out)
		}
	})
	t.Run("an open ask of its holder", func(t *testing.T) {
		t.Parallel()
		e, id := deadWaitNode(t, worklet.StatusInProgress)
		askOn(t, e, id)
		stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)}})
		at := time.Now().Add(deadWaitGrace + time.Minute)
		poll(t, e, at)
		f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
		if out := handOut(e, f, at); out.Woken {
			t.Errorf("an item whose holder asked somebody on it was handed back: %+v", out)
		}
	})
}

// A FACT THE ENGINE CANNOT READ WAKES NOBODY: a pull request the host will not
// show, its reviews refused, or the held merge asks unread record nothing, and
// a dead wait already recorded is left as it was, so it is handed no more often.
func TestAWaitTheEngineCannotReadWakesNobody(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		read  pullRead
		asked error
	}{
		{"the host refuses the pull request", pullRead{err: &github.APIError{Status: 404}}, nil},
		{"the host refuses its reviews", pullRead{err: fmt.Errorf("github: read the reviews: %w",
			&github.APIError{Status: 403})}, nil},
		{"the held merge asks are unread", pullRead{m: openPull(headA)}, errors.New("the store did not answer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, id := deadWaitNode(t, worklet.StatusInProgress)
			e.changePoll.asked = func(context.Context) (map[string]bool, error) {
				return map[string]bool{}, tc.asked
			}
			stubPulls(e, map[string]pullRead{settledPull: tc.read})
			at := time.Now().Add(deadWaitGrace + time.Minute)
			poll(t, e, at)
			if d, dead := deadWaitOf(t, e, id); dead {
				t.Errorf("%s, and the wait was recorded dead: %+v", tc.name, d)
			}
			f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
			if out := handOut(e, f, at); out.Woken {
				t.Errorf("%s, and the item was handed back: %+v", tc.name, out)
			}
		})
	}

	// RECORDED, THEN UNREAD: the record stands and its head is handed once.
	e, id := deadWaitNode(t, worklet.StatusInProgress)
	stubPulls(e, map[string]pullRead{settledPull: {m: openPull(headA)}})
	at := time.Now().Add(deadWaitGrace + time.Minute)
	poll(t, e, at)
	f := &pickupFleet{worked: map[string]time.Time{}, claims: map[string]time.Time{}}
	if out := handOut(e, f, at); !out.Woken {
		t.Fatalf("a dead wait was not handed back: %+v", out)
	}
	f.turnOn(at)
	stubPulls(e, map[string]pullRead{settledPull: {err: &github.APIError{Status: 502}}})
	poll(t, e, at.Add(time.Hour))
	if d, dead := deadWaitOf(t, e, id); !dead || d.Head != headA {
		t.Errorf("an unread pass changed the record to %+v (dead %v), want it left at %s", d, dead, headA)
	}
	if out := handOut(e, f, at.Add(time.Hour)); out.Woken {
		t.Errorf("an unread pass handed the item back again: %+v", out)
	}
}

// askOn puts a question from ceo to the founder on id.
func askOn(t *testing.T, e *Engine, id string) {
	t.Helper()
	project, _, _ := strings.Cut(id, "-")
	at := time.Now().UTC()
	mustApply(t, "the ask", func() (worklet.WriteResult, error) {
		return e.native.Load().writer.UpdateTask(t.Context(), statelog.NewOpID(at, "ask"), id, project,
			worklet.NoIfMatch, worklet.TaskPatch{Comment: &worklet.Comment{ID: "c1", Author: "ceo",
				AuthorKind: worklet.AuthorAgent, Body: "Which head should ship?", Ask: "founder",
				CreatedAt: at}}, worklet.ChangeComment, nil)
	})
}
