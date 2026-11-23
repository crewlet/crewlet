package duty_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	// resumed is the checkpoint each run was handed, "" for a fresh one.
	resumed []string
	// wrote, where set, is told each time a run has written its line and
	// its checkpoint.
	wrote chan struct{}
}

func (f *fakeDeep) Assigned() bool { return f.assigned }

func (f *fakeDeep) Deep(ctx context.Context, now time.Time, _, _ []advisor.Stored,
	io duty.DeepIO) (duty.ModelRun, error) {
	f.mu.Lock()
	f.runs++
	f.resumed = append(f.resumed, string(io.Resume))
	f.mu.Unlock()
	io.Progress(duty.Line{At: now, Step: advisordomain.ProgressRead, Text: "reading capacity", Detail: "capacity"})
	io.Checkpoint(now, json.RawMessage(fmt.Sprintf(`{"agenda":[{"id":"A1","verdict":"confirmed"}],"run":%d}`, f.runs)))
	if f.wrote != nil {
		f.wrote <- struct{}{}
	}
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
	if r.reads != 1 || len(l.runs) != 0 || claims != 2 {
		t.Errorf("ticks beside a deep run read %d times and recorded %d runs, claims %d; want the press's checks "+
			"read once, then only the claims", r.reads, len(l.runs), claims)
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
	if len(l.progress) != 2 || l.progress[0] != "pressed: Checks done: 0 new, 0 closed" ||
		l.progress[1] != "pressed: reading capacity" || l.lines[0].Step != advisordomain.ProgressChecks ||
		l.lines[1].Step != advisordomain.ProgressRead || l.lines[1].Detail != "capacity" {
		t.Errorf("progress on the log = %v (%+v), want the checks' step and the investigation's under the press",
			l.progress, l.lines)
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
// with no model assigned runs the checks and says its deep part could not run,
// and why. Mutation: run it on every tick after its hour, and the company pays
// for one an hour.
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
	if len(logOff.runs) != 1 || logOff.runs[0].Status != duty.RunDone || logOff.runs[0].Kind != advisordomain.RunKindDeep ||
		logOff.runs[0].Model.(map[string]any)["skipped"] != duty.DeepNoModel ||
		!strings.Contains(logOff.runs[0].Model.(map[string]any)["reason"].(string), "no model assigned") {
		t.Errorf("a press with no model = %+v, want its checks recorded and its deep part said not to run", logOff.runs)
	}
}

// A TAKEOVER RESUMES FROM THE CHECKPOINT THE LOST HOLDER WROTE: a holder that
// loses the lease mid-run leaves its checkpoint on the log, under the press,
// and the next holder starts the press from it rather than from nothing.
// Mutation: start the next holder's run fresh, and every answered item is
// answered again on the company's budget.
func TestATakeoverResumesFromTheCheckpointTheLostHolderWrote(t *testing.T) {
	t.Parallel()
	l, c := &fakeLog{}, &clock{now: start}
	first := newDuty(l, &fakeReader{signature: "r1"}, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{}), wrote: make(chan struct{}, 2)}
	first.Deep = deep
	l.requested = []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}
	held := true
	first.Claim = func(context.Context) (bool, error) { return held, nil }
	step(t, first)
	<-deep.wrote
	held = false
	step(t, first)
	first.WaitDeep()
	if len(l.runs) != 0 || len(l.checkpoints["pressed"]) == 0 {
		t.Fatalf("after the lease was lost: runs %+v, checkpoint %s; want nothing recorded and the checkpoint kept",
			l.runs, l.checkpoints["pressed"])
	}
	second := newDuty(l, &fakeReader{signature: "r1"}, nil, c)
	second.Deep = deep
	close(deep.release)
	step(t, second)
	second.WaitDeep()
	if len(deep.resumed) != 2 || deep.resumed[0] != "" || !strings.Contains(deep.resumed[1], `"run":1`) {
		t.Errorf("the runs were handed %q, want the first fresh and the second the first's checkpoint", deep.resumed)
	}
	if len(l.runs) != 1 || l.runs[0].ID != "pressed" {
		t.Errorf("runs = %+v, want the press recorded once", l.runs)
	}
}

// ONE PRESS IS ONE RUN: the checks run at once and their findings are
// recorded before the investigation ends, the investigation goes on under the
// same id, and the press is recorded once with both parts, begun when the
// checks began. A press within [duty.DeepFreshFor] of an investigation that
// concluded runs the checks and keeps that investigation, saying so and from
// when; one past it investigates again. Mutation: record the checks and the
// investigation as two runs, or investigate on every press, and a check fails.
func TestOnePressIsOneRunOfTheChecksAndTheInvestigation(t *testing.T) {
	t.Parallel()
	found := advisor.Observe(advisor.Finding{Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts,
		Source: advisor.SourceEvidence, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"},
		Evidence: []advisor.Fact{{Key: "runs", Value: 3}}, Fix: advisor.Fix{Kind: advisor.FixNone}},
		advisor.Span{From: start.Add(-advisor.Window), To: start}, "")
	l, c := &fakeLog{}, &clock{now: start}
	d := newDuty(l, &fakeReader{signature: "r1", observed: []advisor.Observation{found}}, nil, c)
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	d.Deep = deep
	l.requested = []duty.Request{{Run: "pressed", By: "owner"}}
	step(t, d)
	if len(l.recorded) != 1 || l.recorded[0].Step != advisor.StepOpen || len(l.runs) != 0 {
		t.Fatalf("while it investigates: recorded %+v, runs %+v; want the checks' finding opened, no run yet",
			l.recorded, l.runs)
	}
	c.now = c.now.Add(10 * time.Minute)
	close(deep.release)
	d.WaitDeep()
	if len(l.runs) != 1 || l.runs[0].ID != "pressed" || l.runs[0].Kind != advisordomain.RunKindDeep ||
		!l.runs[0].StartedAt.Equal(start) || deep.runs != 1 {
		t.Fatalf("runs = %+v, want the press recorded once, begun with its checks", l.runs)
	}
	scopes := map[string]bool{}
	for _, s := range l.runs[0].Scopes {
		scopes[s.Scope] = true
	}
	if !scopes[string(advisor.GroupEvidence)] || !scopes[duty.DeepScope] || len(l.runs[0].Classes) == 0 {
		t.Errorf("scopes %v, classes %d: want the checks' and the investigation's parts in one record",
			scopes, len(l.runs[0].Classes))
	}

	// A PRESS WITHIN THE WINDOW keeps that investigation.
	l.runs[0].StartedAt = start
	c.now = start.Add(2 * time.Hour)
	l.requested = []duty.Request{{Run: "again", By: "owner"}}
	step(t, d)
	d.WaitDeep()
	again := l.runs[len(l.runs)-1]
	model, _ := again.Model.(map[string]any)
	if again.ID != "again" || deep.runs != 1 || model["skipped"] != duty.DeepKept ||
		!model["kept_from"].(time.Time).Equal(start) || !strings.Contains(model["reason"].(string), "from 2 h ago is kept") {
		t.Errorf("a press 2 h after = %+v (deep runs %d), want the checks recorded and the investigation kept",
			again, deep.runs)
	}

	// A PRESS PAST IT investigates again.
	c.now = start.Add(duty.DeepFreshFor + time.Minute)
	deep.release = make(chan struct{})
	close(deep.release)
	l.requested = []duty.Request{{Run: "later", By: "owner"}}
	step(t, d)
	d.WaitDeep()
	if deep.runs != 2 || l.runs[len(l.runs)-1].ID != "later" {
		t.Errorf("a press past the window ran %d investigations, want a second", deep.runs)
	}
}
