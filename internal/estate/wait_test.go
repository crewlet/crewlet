package estate

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/worklet"
)

// serveAhead makes node answer on the fleet's broker with its clock standing
// ahead of its askers' by ahead, with git as its git half where it is not nil.
// Every node of the fleet is otherwise one machine; only the serving node's own
// clock ([ServerOptions.Now]) is moved.
func serveAhead(t *testing.T, f *fleet, node *fakeNode, git LocalGit, ahead time.Duration) {
	t.Helper()
	f.nodes[node.name] = node
	stop, err := Serve(t.Context(), silencer{q: f.start(t), node: node}, ServerOptions{
		Self: node.name, Local: node, Git: git, Seams: node.seams(), Envelopes: fleetEnvelopes,
		Now: func() time.Time { return time.Now().Add(ahead) },
	})
	if err != nil {
		t.Fatalf("serve %s: %v", node.name, err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

// dataNodes lists names as the routers' data nodes.
func (f *fleet) dataNodes(names ...string) {
	f.placement.set(func(p *fakePlacement) { p.nodes = slices.Sorted(slices.Values(names)) })
}

// A DATA NODE AHEAD OF ITS ASKER SERVES ITS ASKER'S WHOLE WAIT, and not a moment
// more: with its clock three seconds ahead, and ahead by more than the read's
// attempt less the reply's margin (still inside the attempt, so admitted), a
// read that waits for as long as it is let is cut exactly the router's read
// attempt after it arrived.
//
// Mutation: bound the work by the asker's deadline on the node's clock, and the
// read is cut short by the node's lead, red.
//
// In a synctest bubble ([inBubble]), so the request's trip takes no time and
// the cut is exact arithmetic.
func TestADataNodeAheadOfItsAskerServesItsAskersWholeWait(t *testing.T) {
	t.Parallel()
	attempt := Production().ReadAttempt
	for name, ahead := range map[string]time.Duration{
		"three seconds ahead":                        3 * time.Second,
		"ahead by more than the attempt less margin": attempt - gitlet.ReplyMargin + 500*time.Millisecond,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				f := newFleet(t)
				node := &fakeNode{name: "data-a", units: chartOf("data-a")}
				cut := make(chan time.Duration, 1)
				node.levelRead = func(ctx context.Context) error {
					begun := time.Now()
					<-ctx.Done()
					cut <- time.Since(begun)
					return ctx.Err()
				}
				serveAhead(t, f, node, nil, ahead)
				f.dataNodes("data-a")
				r := f.router(t, "agent-2", nil)

				_, _ = r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
				// The router's attempt and the node's work end at one instant:
				// the node's goroutine may not have reported yet.
				synctest.Wait()
				select {
				case got := <-cut:
					if got != attempt {
						t.Errorf("a read on a node %v ahead of its asker was cut %v after it arrived, want the "+
							"asker's whole %v read attempt", ahead, got, attempt)
					}
				default:
					t.Fatalf("the read never reached the node %v ahead of its asker", ahead)
				}
			})
		})
	}
}

// A GITLET NODE AHEAD OF ITS ASKER SERVES A GIT READ ITS ASKER'S WHOLE WAIT: the
// git half is served until a reply's margin before the router's git read
// attempt ends, counted from the read's arrival, whether the node's clock
// stands three seconds ahead or ahead by more than the attempt less the margin.
//
// Mutation: serve the git arm until the asker's deadline as the node's clock
// reads it, and the half's time is cut by the node's lead, red.
//
// In a synctest bubble ([inBubble]), for
// [TestADataNodeAheadOfItsAskerServesItsAskersWholeWait]'s reason.
func TestAGitletNodeAheadOfItsAskerServesAGitReadItsAskersWholeWait(t *testing.T) {
	t.Parallel()
	attempt := Production().GitReadAttempt
	for name, ahead := range map[string]time.Duration{
		"three seconds ahead":                        3 * time.Second,
		"ahead by more than the attempt less margin": attempt - gitlet.ReplyMargin + 500*time.Millisecond,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				f := newFleet(t)
				half := &fakeGit{name: "git-a"}
				serveAhead(t, f, &fakeNode{name: "git-a", units: chartOf("git-a")}, gitHalf{node: half}, ahead)
				f.git.set(func(p *fakePlacement) { p.nodes = []string{"git-a"} })
				r := f.router(t, "agent-2", nil)

				begun := time.Now()
				if _, err := r.Code().GitRead(t.Context(), treeOf(repoID(5))); err != nil {
					t.Fatalf("a git read of a node %v ahead of its asker: %v", ahead, err)
				}
				deadlines := half.readDeadlines()
				want := begun.Add(attempt - gitlet.ReplyMargin)
				if len(deadlines) != 1 || !deadlines[0].Equal(want) {
					t.Errorf("a git read of a node %v ahead of its asker was served until %v, want %v: the "+
						"asker's whole %v attempt less the reply's margin", ahead, deadlines, want, attempt)
				}
			})
		})
	}
}

// A REQUEST CARRYING NO WAIT RUNS NOTHING, a read or a write alike: nothing
// says how long its asker waits, and an older unreleased build's asker, the
// only one that sends none, gets no compatibility. It is answered stale, bound
// to its id, so the asker moves on.
//
// Mutation: bound a request carrying no wait by its deadline on the node's
// clock, as the build before did, and both run, red.
func TestARequestCarryingNoWaitRunsNothing(t *testing.T) {
	t.Parallel()
	f := newFleet(t, "data-a")
	args, err := json.Marshal(tasksArgs{})
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]request{
		"a read": {ID: "r-read", Op: opTasks.spec.name, Args: args, Deadline: time.Now().Add(time.Minute)},
		"a write": {ID: "r-write", Op: opCreateTask.spec.name, Actor: mustInput(t, swe),
			Args:     json.RawMessage(`{"OpID":"op-unwaited","Task":{"Project":"ENG"}}`),
			Deadline: time.Now().Add(time.Minute)},
	} {
		if rep := askSigned(t, f.start(t), "data-a", req); rep.Unserved != unservedStale || rep.Request != req.ID {
			t.Errorf("%s carrying no wait was answered %+v, want it %q, bound to %q", name, rep, unservedStale,
				req.ID)
		}
	}
	if node := f.nodes["data-a"]; node.askedFor("tasks") || node.askedFor("create") {
		t.Fatal("a request carrying no wait ran")
	}
}

// A DATA NODE THREE SECONDS AHEAD IS HEARD FOR ITS ASKER'S WHOLE WAIT, the one
// asked first of two. A read needing its whole read attempt there, less the
// reply's margin, is answered from it, never cut short; a caller with two
// seconds left is still refused there by admission, which judges the asker's
// deadline on the node's clock with no allowance (#1406's piece 3c), and
// served by the other node.
//
// Mutation: bound the work by the asker's deadline on the node's clock, and
// the long read is cut three seconds early and asked of the other node, red.
//
// In a synctest bubble ([inBubble]), for
// [TestADataNodeAheadOfItsAskerServesItsAskersWholeWait]'s reason.
func TestADataNodeThreeSecondsAheadIsHeardForItsAskersWholeWait(t *testing.T) {
	t.Parallel()
	const ahead = 3 * time.Second
	attempt := Production().ReadAttempt
	// twoNodes is a fleet of two data nodes and a router asking them, the node
	// it asks first standing ahead and running levelRead on a task read.
	twoNodes := func(t *testing.T, levelRead func(context.Context) error) (*Router, *fakeNode, *fakeNode) {
		f := newFleet(t)
		f.dataNodes("data-a", "data-b")
		r := f.router(t, "agent-2", nil)
		order := r.order(opTasks.spec, []string{"data-a", "data-b"})
		first := &fakeNode{name: order[0], units: chartOf(order[0]), levelRead: levelRead}
		second := &fakeNode{name: order[1], units: chartOf(order[1])}
		serveAhead(t, f, first, nil, ahead)
		serveAhead(t, f, second, nil, 0)
		return r, first, second
	}
	t.Run("a read needing its whole attempt", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			needs := attempt - gitlet.ReplyMargin
			r, first, second := twoNodes(t, func(ctx context.Context) error {
				select {
				case <-time.After(needs):
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			begun := time.Now()
			_, err := r.Work().Tasks(t.Context(), worklet.Query{}, time.Now())
			waited := time.Since(begun)
			if err != nil || !first.askedFor("tasks") || second.askedFor("tasks") {
				t.Fatalf("a read needing %v of a %v attempt, on %s %v ahead = %v (asked of %s: %t, of %s: "+
					"%t), want it answered by %s alone", needs, attempt, first.name, ahead, err, first.name,
					first.askedFor("tasks"), second.name, second.askedFor("tasks"), first.name)
			}
			if waited != needs {
				t.Errorf("the read was answered %v in, want the %v it needed", waited, needs)
			}
		})
	})
	t.Run("a caller with two seconds left", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			r, first, second := twoNodes(t, nil)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, err := r.Work().Tasks(ctx, worklet.Query{}, time.Now())
			first.mu.Lock()
			asked := first.requests
			first.mu.Unlock()
			if err != nil || asked != 1 || first.askedFor("tasks") || !second.askedFor("tasks") {
				t.Fatalf("a read with 2s left = %v (%s, %v ahead, asked %d times, ran it: %t; %s ran it: %t), "+
					"want %s's admission to refuse it and %s to serve it", err, first.name, ahead, asked,
					first.askedFor("tasks"), second.name, second.askedFor("tasks"), first.name, second.name)
			}
		})
	})
}
