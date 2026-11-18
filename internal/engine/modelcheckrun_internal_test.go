package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// checkNode is a node named node recording its model checks in fleet, running
// checks until the case ends.
func checkNode(t *testing.T, fleet coord.Fleet, node string) *Engine {
	t.Helper()
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: node}
	e.startModelChecks(t.Context())
	t.Cleanup(e.stopModelChecks)
	return e
}

// heldCheck is a check the case ends: it answers what the case sends, or stops
// with its context.
type heldCheck struct {
	started chan struct{}
	finish  chan json.RawMessage
}

func newHeldCheck() *heldCheck {
	return &heldCheck{started: make(chan struct{}), finish: make(chan json.RawMessage)}
}

func (h *heldCheck) run(ctx context.Context) (json.RawMessage, error) {
	close(h.started)
	select {
	case answer := <-h.finish:
		return answer, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// A CHECK IS ANSWERED RUNNING, AND ENDS ON ITS NODE'S TIME. Past its hold the
// start answers the check running (its node, its deadline the budget plus the
// end's write), and the check records what it found for any node to read. One
// that ends within the hold is answered ended.
//
// Mutation: run the check on the request's context, or answer only once it
// ended, and this is red.
func TestACheckIsAnsweredRunningAndEndsOnItsNodesTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fleet := coordmem.NewFleet()
		e := checkNode(t, fleet, "node-a")
		other := checkNode(t, fleet, "node-b")
		held := newHeldCheck()
		request, cancel := context.WithCancel(t.Context())
		start := time.Now()
		running, err := e.StartModelCheck(request, ModelCheckAsk{Entry: "sonnet", Smoke: true, By: checkingJane,
			Budget: 3 * time.Minute}, held.run)
		cancel()
		if err != nil {
			t.Fatalf("StartModelCheck: %v", err)
		}
		if !running.Running() || running.Node != "node-a" || running.Entry != "sonnet" || !running.Smoke ||
			running.Draft || running.By != "jane.doe" || running.ByKind != iam.ActorHuman || running.Version == 0 ||
			!running.Deadline.Equal(start.Add(3*time.Minute+modelCheckWriteBudget)) {
			t.Fatalf("the start answered %+v, want the check running on node-a under its budget", running)
		}
		<-held.started
		held.finish <- json.RawMessage(`{"ok":true,"lines":[]}`)
		synctest.Wait()
		read, found, err := other.ModelCheck(t.Context(), "sonnet")
		if err != nil || !found || read.ID != running.ID || read.Running() || string(read.Answer) != `{"ok":true,"lines":[]}` ||
			read.Failure != "" {
			t.Fatalf("node-b read %+v (found %v, %v), want the check ended with what it found", read, found, err)
		}

		quick, err := e.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "opus", By: checkingJane,
			Budget: time.Minute}, func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true,"lines":[]}`), nil
		})
		if err != nil || quick.Running() || string(quick.Answer) != `{"ok":true,"lines":[]}` {
			t.Fatalf("a check that ended within the hold = %+v (%v), want it answered ended", quick, err)
		}
	})
}

// ONE CHECK OF AN ENTRY AT A TIME, ACROSS THE FLEET. The same ask is handed the
// running check and runs nothing; a draft or the other smoke setting is
// refused naming it. An ended check, or one lost with its node, is replaced.
//
// Mutation: run every ask, hand back a draft's, or keep a lost check, and this
// is red.
func TestOneCheckOfAnEntryRunsAtATimeAcrossTheFleet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fleet := coordmem.NewFleet()
		a, b := checkNode(t, fleet, "node-a"), checkNode(t, fleet, "node-b")
		held := newHeldCheck()
		first, err := a.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "sonnet", Smoke: true, By: checkingJane,
			Budget: 3 * time.Minute}, held.run)
		if err != nil {
			t.Fatalf("StartModelCheck: %v", err)
		}
		ran := false
		again, err := b.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "sonnet", Smoke: true, By: checkingJane,
			Budget: 3 * time.Minute}, func(context.Context) (json.RawMessage, error) {
			ran = true
			return json.RawMessage(`{}`), nil
		})
		if err != nil || again.ID != first.ID || again.Node != "node-a" || ran {
			t.Fatalf("the same ask on node-b = %+v (%v, ran %v), want node-a's check handed back", again, err, ran)
		}
		for name, ask := range map[string]ModelCheckAsk{
			"a draft":                 {Entry: "sonnet", Draft: true, Smoke: true, By: checkingJane, Budget: time.Minute},
			"the other smoke setting": {Entry: "sonnet", By: checkingJane, Budget: time.Minute},
		} {
			_, err := b.StartModelCheck(t.Context(), ask, held.run)
			var running *ModelCheckInProgressError
			if !errors.As(err, &running) || running.Running.ID != first.ID {
				t.Errorf("%s while a check runs = %v, want it refused naming the running check", name, err)
			}
		}
		<-held.started
		held.finish <- json.RawMessage(`{"ok":false,"lines":[]}`)
		synctest.Wait()
		next, err := b.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "sonnet", Draft: true, By: checkingJane,
			Budget: time.Minute}, func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true,"lines":[]}`), nil
		})
		if err != nil || next.ID == first.ID || next.Node != "node-b" || !next.Draft {
			t.Fatalf("a check over an ended one = %+v (%v), want a new check on node-b", next, err)
		}

		// A check whose node stopped without ending it is lost at its
		// deadline, and the next ask replaces it.
		gone := coord.ModelCheck{ID: uuid.New(), Entry: "opus", Node: "node-c", Smoke: true, StartedAt: time.Now(),
			Deadline: time.Now().Add(time.Minute), By: "jane.doe", ByKind: iam.ActorHuman}
		if _, created, err := fleet.CreateModelCheck(t.Context(), gone); err != nil || !created {
			t.Fatalf("CreateModelCheck: %v, %v", created, err)
		}
		time.Sleep(2 * time.Minute)
		replaced, err := a.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "opus", Smoke: true, By: checkingJane,
			Budget: time.Minute}, func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true,"lines":[]}`), nil
		})
		if err != nil || replaced.ID == gone.ID || replaced.Node != "node-a" {
			t.Fatalf("an ask over a lost check = %+v (%v), want it replaced", replaced, err)
		}
	})
}

// A CHECK OUTLIVES NOTHING IT SHOULD NOT. A stopping node ends its checks
// saying so, not with what the harness made of the cancellation, and takes no
// check after; a check answering nothing, or past what its record keeps, ends
// saying why; and no reader reads a record past its linger.
//
// Mutation: record a stopped check's own answer, take a check after the stop,
// or read a lapsed record, and this is red.
func TestACheckOutlivesNothingItShouldNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fleet := coordmem.NewFleet()
		e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a"}
		e.startModelChecks(t.Context())
		cut := func(ctx context.Context) (json.RawMessage, error) {
			<-ctx.Done()
			// The doctor's reading of a cancelled run: the vendor did not answer.
			return json.RawMessage(`{"ok":false,"failure":"timeout","lines":[]}`), nil
		}
		running, err := e.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "sonnet", Smoke: true, By: checkingJane,
			Budget: 3 * time.Minute}, cut)
		if err != nil || !running.Running() {
			t.Fatalf("StartModelCheck = %+v, %v", running, err)
		}
		e.stopModelChecks()
		read, found, err := e.ModelCheck(t.Context(), "sonnet")
		if err != nil || !found || read.Running() || len(read.Answer) != 0 ||
			read.Failure != errModelCheckNodeStopping.Error() {
			t.Fatalf("a check its node's stop cut off = %+v (found %v, %v), want it ended saying the node stopped",
				read, found, err)
		}
		if _, err := e.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "opus", By: checkingJane, Budget: time.Minute},
			cut); !errors.Is(err, ErrModelChecksStopped) {
			t.Errorf("a check asked of a stopped node = %v, want it refused", err)
		}

		live := checkNode(t, fleet, "node-b")
		for name, tc := range map[string]struct {
			run  func(context.Context) (json.RawMessage, error)
			says string
		}{
			"an answer of nothing": {run: func(context.Context) (json.RawMessage, error) { return nil, nil },
				says: "answered nothing"},
			"an answer past its bound": {run: func(context.Context) (json.RawMessage, error) {
				return json.RawMessage(`"` + strings.Repeat("x", coord.MaxModelCheckAnswerBytes) + `"`), nil
			}, says: "past the"},
			"a check that could not run": {run: func(context.Context) (json.RawMessage, error) {
				return nil, errors.New("the company no longer declares \"gone\"")
			}, says: "no longer declares"},
		} {
			ended, err := live.StartModelCheck(t.Context(), ModelCheckAsk{Entry: "e-" + name, By: checkingJane,
				Budget: time.Minute}, tc.run)
			if err != nil || ended.Running() || len(ended.Answer) != 0 || !strings.Contains(ended.Failure, tc.says) {
				t.Errorf("%s = %+v (%v), want it ended saying %q", name, ended, err, tc.says)
			}
		}

		time.Sleep(coord.ModelCheckLinger + time.Minute)
		if _, found, err := live.ModelCheck(t.Context(), "e-an answer of nothing"); err != nil || found {
			t.Errorf("a record past its linger read (found %v, %v), want it absent", found, err)
		}
	})
}

// A MODEL'S CHECK GOES WITH THE MODEL: the apply that removes an entry deletes
// its check's record, and only the fleet's current company does.
func TestAModelsCheckGoesWithIt(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	current, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	for _, entry := range []string{"plan", "removed"} {
		if _, created, err := fleet.CreateModelCheck(t.Context(), coord.ModelCheck{ID: uuid.New(), Entry: entry,
			Node: "node-a", StartedAt: time.Now(), Deadline: time.Now().Add(time.Minute), By: "jane.doe",
			ByKind: iam.ActorHuman}); err != nil || !created {
			t.Fatalf("CreateModelCheck(%s): %v, %v", entry, created, err)
		}
	}
	next, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e.clearRemovedModelChecks(t.Context(), next)
	if all, _ := fleet.ListModelChecks(t.Context()); len(all) != 2 {
		t.Fatalf("a company no activation named cleared checks: %+v", all)
	}
	next.ActivatedAt = current.At
	e.clearRemovedModelChecks(t.Context(), next)
	all, err := fleet.ListModelChecks(t.Context())
	if err != nil || len(all) != 1 || all[0].Entry != "plan" {
		t.Fatalf("checks kept %+v (%v), want the model the company still declares", all, err)
	}
}
