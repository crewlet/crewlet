package duty_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// fakeDeep is a deep run that waits until released, or until its context ends.
type fakeDeep struct {
	assigned bool
	release  chan struct{}
	mu       sync.Mutex
	runs     int
	canceled bool
}

func (f *fakeDeep) Assigned() bool { return f.assigned }

func (f *fakeDeep) Deep(ctx context.Context, _ time.Time, _, _ []advisor.Stored) (duty.ModelRun, error) {
	f.mu.Lock()
	f.runs++
	f.mu.Unlock()
	select {
	case <-f.release:
		return duty.ModelRun{Summary: map[string]any{"status": "done"}, Concluded: true}, nil
	case <-ctx.Done():
		f.mu.Lock()
		f.canceled = true
		f.mu.Unlock()
		return duty.ModelRun{}, ctx.Err()
	}
}

// A DEEP RUN RUNS BESIDE THE TICK: a person's Run deep starts it, the ticks
// while it runs renew the lease and run nothing else, and when it ends it is
// recorded under the press as a deep run. Mutation: run it inside the tick,
// and the lease lapses three ticks into a twenty-minute run.
func TestADeepRunRunsBesideTheTick(t *testing.T) {
	t.Parallel()
	l, r, c := &fakeLog{}, &fakeReader{signature: "r1"}, &clock{now: start}
	d := newDuty(l, r, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	d.Deep = deep
	l.requested = []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}
	claims := 0
	d.Claim = func(context.Context) (bool, error) { claims++; return true, nil }
	step(t, d)
	step(t, d)
	if r.reads != 0 || len(l.runs) != 0 || claims != 2 {
		t.Errorf("ticks beside a deep run read %d times and recorded %d runs, claims %d; want only the claims",
			r.reads, len(l.runs), claims)
	}
	close(deep.release)
	d.WaitDeep()
	if len(l.runs) != 1 || l.runs[0].ID != "pressed" || l.runs[0].Kind != advisordomain.RunKindDeep ||
		l.runs[0].Trigger != duty.TriggerPerson {
		t.Fatalf("runs = %+v, want the press recorded as a deep run", l.runs)
	}
	if deep.runs != 1 {
		t.Errorf("the deep run ran %d times, want once", deep.runs)
	}
}

// A HOLDER THAT LOSES THE LEASE STOPS ITS DEEP RUN, recording nothing, so the
// next holder runs the press again. Mutation: let it run on, and two nodes
// investigate at once.
func TestAHolderThatLosesTheLeaseStopsItsDeepRun(t *testing.T) {
	t.Parallel()
	l, r, c := &fakeLog{}, &fakeReader{signature: "r1"}, &clock{now: start}
	d := newDuty(l, r, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	d.Deep = deep
	l.requested = []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}
	held := true
	d.Claim = func(context.Context) (bool, error) { return held, nil }
	step(t, d)
	held = false
	step(t, d)
	d.WaitDeep()
	if !deep.canceled || len(l.runs) != 0 {
		t.Errorf("after the lease was lost: canceled %v, runs %+v; want it stopped and nothing recorded",
			deep.canceled, l.runs)
	}
}

// A DEEP RUN IS DUE NIGHTLY AND EARLY ON MONDAY, once per slot, and a press
// with no model assigned is recorded as failed, saying why. Mutation: run it
// on every tick after its hour, and the company pays for one an hour.
func TestADeepRunIsDueNightlyAndEarlyOnMonday(t *testing.T) {
	t.Parallel()
	l, r := &fakeLog{}, &fakeReader{signature: "r1"}
	c := &clock{now: time.Date(2030, 1, 9, 3, 30, 0, 0, time.UTC)}
	d := newDuty(l, r, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep.release)
	d.Deep = deep
	step(t, d)
	d.WaitDeep()
	c.now = c.now.Add(time.Hour)
	step(t, d)
	d.WaitDeep()
	if deep.runs != 1 {
		t.Errorf("the night's slot ran %d deep runs, want one", deep.runs)
	}
	off := newDuty(&fakeLog{requested: []duty.Request{{Run: "p", By: "owner", Kind: advisordomain.RunKindDeep}}},
		&fakeReader{signature: "r1"}, nil, &clock{now: start})
	off.Deep = &fakeDeep{}
	logOff := off.Log.(*fakeLog)
	step(t, off)
	if len(logOff.runs) != 1 || logOff.runs[0].Status != duty.RunFailed || logOff.runs[0].Kind != advisordomain.RunKindDeep {
		t.Errorf("a press with no model = %+v, want it recorded failed", logOff.runs)
	}
}
