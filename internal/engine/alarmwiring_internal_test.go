package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// EVERY ALARM WHOSE INPUT A NODE ASSEMBLES FOR ITSELF FIRES THROUGH THE WIRING
// A RUNNING NODE HAS — not through helpers a case calls directly.
//
// Each has three links that a helper-level test cannot see: the retention loop
// the engine builds has to hold the watch or the measurement, the evaluation it
// runs has to take them, and the report every surface reads has to carry them
// to the table. With any one of those gone an alarm is dead on a real node
// while every case beside it passes — which is how `deferred_old` and
// `floor_unknown` shipped unable to fire, and `trim_blocked` able to fire on
// every fresh fleet.

// A BINDING TO AN AGENT'S SEAT FIRES `iam_binding_dangling` — through the real
// directory, the real chart view and the real alarm tracker.
func TestTheDanglingBindingAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	if r.bindings == nil {
		t.Fatal("a node running the identity domain armed no binding watch")
	}

	// A MACHINE BOUND TO THE CEO'S SEAT, which is an agent's: a settled
	// residue, refused 403 on every request it makes.
	bot := uuid.Must(uuid.NewV7()).String()
	writer := e.IAMWriter()
	if _, err := writer.Enrol(ctx, iamdomain.Enrolment{
		PersonID: bot, Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:bot", OpID: "op-enrol-bot", Reason: "a pipeline",
	}); err != nil {
		t.Fatalf("enrol: %v", err)
	}
	if _, err := writer.Claim(ctx, iamdomain.KindSeat, "ceo", bot, "op-bind-bot"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	awaitBinding(t, e, bot)

	t0 := time.Now().UTC()
	r.now = func() time.Time { return t0 }
	r.beat(ctx)
	if alarmed(r.Report(ctx), statelog.KindBindingDangling) {
		t.Fatal("a residue seen once fired before it outlived the stall grace")
	}
	r.now = func() time.Time { return t0.Add(statelog.StallGrace + statelog.AlarmInterval) }
	r.beat(ctx)
	if !alarmed(r.Report(ctx), statelog.KindBindingDangling) {
		t.Fatal("a binding to an agent's seat, a beat past the stall grace, " +
			"raised no iam_binding_dangling")
	}
	if got := alarmGauge(t, e, statelog.KindBindingDangling); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindBindingDangling, got)
	}
}

// A LOG WHOSE CEILING CANNOT HOLD ITS REPLAY WINDOW FIRES `log_ceiling_short`
// — and the evaluation measures every strict log's intake on the way.
//
// The intake cannot be made real here: a stream is created now, and the
// measurement waits out a log's first two days by design. So the clock is put
// three days ahead — which makes every strict log MEASURED (at nothing, the
// day behind the clock holding none of its records), proving the evaluation
// took the measurement and the report carries it — and then one log's intake
// is the figure a busy company would have. The ceiling and the replay window
// are the node's own.
func TestTheCeilingAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)

	r.now = func() time.Time { return time.Now().UTC().Add(72 * time.Hour) }
	r.evaluate(ctx)
	report := r.Report(ctx)
	strict := 0
	for _, d := range report.Domains {
		if d.Replay != statelog.ReplayStrict {
			continue
		}
		strict++
		if d.BytesPerDay == nil {
			t.Errorf("the %s log was not measured by the evaluation", d.Domain)
		}
	}
	if strict == 0 {
		t.Fatal("the report carries no strict log at all")
	}
	if alarmed(report, statelog.KindLogCeilingShort) {
		t.Fatal("logs that took in nothing fired log_ceiling_short")
	}

	// A BUSY COMPANY'S INTAKE on the tracker's log: a tebibyte a day, which
	// no ceiling this node sized holds for the replay window.
	r.mu.Lock()
	r.rates["tracker"] = statelog.PerDay(1 << 40)
	r.mu.Unlock()
	r.beat(ctx)
	var fired statelog.Alarm
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind == statelog.KindLogCeilingShort {
			fired = a
		}
	}
	if fired.Kind == "" {
		t.Fatal("a tebibyte a day on a log sized for gibibytes raised no " +
			"log_ceiling_short")
	}
	if !strings.HasPrefix(fired.Detail, "tracker: ") {
		t.Errorf("the alarm reads %q and does not name the log", fired.Detail)
	}
	if got := alarmGauge(t, e, statelog.KindLogCeilingShort); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindLogCeilingShort, got)
	}
}

// A FRESH NODE'S BLOCKED TRIM IS SILENT, AND A BLOCK THAT HAS KEPT RECORDS
// PAST THE WINDOW FIRES `trim_blocked` — through the trim's own tick, the floor
// it publishes and the report every surface reads.
//
// A fresh node is the young fleet: its trim is blocked from the first tick,
// because nothing has been backed up. The report's clock is then put a whole
// window and more ahead — so the block is older than the threshold by every
// measure the clock has — and the alarm must stay down, because the age term
// the tick published keeps every record the log holds. Then the published
// floor is what the duty would publish once the records had aged — the same
// block, its age term past the log's head — and the alarm must rise, naming
// the log. A floor published at another generation must not be read as this
// log's at all.
func TestTheTrimBlockedAlarmFiresOnARunningNodeOnlyPastTheWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)

	r.tick(ctx)
	report := r.Report(ctx)
	var name string
	var at domainAt
	for _, d := range report.Domains {
		if d.BlockedBy != "" && d.LastSeq > 0 {
			name, at = d.Domain, domainAt{First: d.FirstSeq, Last: d.LastSeq}
			break
		}
	}
	if name == "" {
		t.Fatalf("no domain with records is blocked on a fresh node (%v), so this "+
			"case is not exercising the young fleet it names", report.Blocked())
	}

	later := time.Now().UTC().Add(r.cfg.MinAge() + statelog.TrimInterval + time.Hour)
	r.now = func() time.Time { return later }
	r.beat(ctx)
	if alarmed(r.Report(ctx), statelog.KindTrimBlocked) {
		t.Fatal("a fresh node's trim, blocked on its first backup with nothing in " +
			"its log past the window, raised trim_blocked")
	}

	// THE SAME BLOCK, AS THE DUTY PUBLISHES IT ONCE THE LOG HAS AGED: its
	// age term keeps nothing the log holds.
	floors, err := r.fleet.Floors(ctx)
	if err != nil {
		t.Fatalf("read the published floors: %v", err)
	}
	var aged coord.TrimFloor
	for _, f := range floors {
		if f.Domain == name {
			aged = f
		}
	}
	for i := range aged.Terms {
		if aged.Terms[i].Name == string(statelog.TermAgeFloor) {
			aged.Terms[i].Seq = at.Last + 1
		}
	}
	if err := r.fleet.PutFloor(ctx, aged); err != nil {
		t.Fatalf("publish the aged floor: %v", err)
	}
	r.beat(ctx)
	var fired statelog.Alarm
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind == statelog.KindTrimBlocked {
			fired = a
		}
	}
	if fired.Kind == "" {
		t.Fatalf("a trim blocked past its window with %d record(s) past it raised "+
			"no trim_blocked", at.Last-at.First+1)
	}
	if !strings.HasPrefix(fired.Detail, name+": ") {
		t.Errorf("the alarm reads %q and does not name the %s log", fired.Detail, name)
	}
	if got := alarmGauge(t, e, statelog.KindTrimBlocked); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1", statelog.KindTrimBlocked, got)
	}

	// A FLOOR FROM ANOTHER GENERATION IS NOT THIS LOG'S: its terms and its
	// clock describe a sequence space this log does not have.
	aged.Generation++
	if err := r.fleet.PutFloor(ctx, aged); err != nil {
		t.Fatalf("publish the other generation's floor: %v", err)
	}
	r.beat(ctx)
	after := r.Report(ctx)
	if alarmed(after, statelog.KindTrimBlocked) {
		t.Error("a floor published at another generation raised trim_blocked " +
			"against this log's sequences")
	}
	for _, d := range after.Domains {
		if d.Domain == name && d.BlockedBy != "" {
			t.Errorf("the %s row renders a block from generation %d as its own",
				name, aged.Generation)
		}
	}
}

// A RECORD FROM A NEWER BUILD, HELD PAST THE DEFERRAL GRACE, FIRES
// `deferred_old` — from a real deferral on a real applier, dated by the real
// position heartbeat — and says whether this node's seats move for it.
//
// The alarm could not fire before: the report stood the grace in for the age
// whenever a record was held, and the rule fires past the grace. The record
// here is what a peer one build ahead would publish — signed under this
// fleet's keyring, at a record version this build does not read — so the
// applier retains it exactly as a rolling upgrade makes it. And it said "its
// seats move" of every log, where only the logs that gate seat admission move
// any: the identity estate's is held on the request path instead.
func TestTheDeferredOldAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain  string
		version int
		record  func(generation uint32) (statelog.Subject, []byte, error)
		says    string
	}{
		"a log that gates seat admission": {
			domain: tracker.Domain{}.Name(), version: tracker.RecordVersion,
			record: func(generation uint32) (statelog.Subject, []byte, error) {
				subject := tracker.TaskSubject("t-from-a-newer-build")
				payload, err := tracker.MutationRecord{
					RecordEnvelope: tracker.RecordEnvelope{
						V: tracker.RecordVersion + 1, OpID: "op-from-a-newer-build",
						Subject: subject, Op: tracker.OpCreate, Gen: generation,
						CreatedAt: time.Now().UTC(), Writer: "node-b",
						Scope: tracker.ScopeSet{Subject: true, Container: "ENG"},
					},
					Mutation: []byte(`{}`), Actor: "ana", ActorKind: tracker.AuthorHuman,
				}.Encode()
				return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
					payload, err
			},
			says: "and this node's seats move at",
		},
		"a log that gates none": {
			domain: iamdomain.Domain{}.Name(), version: iamdomain.RecordVersion,
			record: func(generation uint32) (statelog.Subject, []byte, error) {
				person := uuid.Must(uuid.NewV7()).String()
				subject := iamdomain.PersonSubject(person)
				payload, err := iamdomain.Encode(iamdomain.MutationRecord{
					RecordEnvelope: iamdomain.RecordEnvelope{
						V: iamdomain.RecordVersion + 1, OpID: "op-from-a-newer-build",
						Subject: subject, Op: iamdomain.OpEnrol, Gen: generation,
						Writer: "node-b", Scope: iamdomain.PeopleScope(person),
					},
					Mutation: []byte(`{}`), Person: person,
					Actor: "ana.admin", ActorKind: iam.KindPerson,
				})
				return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
					payload, err
			},
			says: "that log does not gate seat admission, so this record moves no seats",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			e := bootDirectoryNode(t, nil)
			r := quietRetention(t, e)
			running := r.state.domains[tc.domain]
			if running == nil {
				t.Fatalf("the node runs no %s domain", tc.domain)
			}
			subject, payload, err := tc.record(running.runner.Committed().Generation)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			signer, err := r.state.signerFor(running.domain)
			if err != nil {
				t.Fatalf("signer: %v", err)
			}
			address := running.domain.Stream().SubjectPrefix + "." + subject.String()
			if _, _, err := running.log.Append(ctx, address, "op-from-a-newer-build",
				nil, signer.Seal(payload)); err != nil {
				t.Fatalf("append: %v", err)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, held := running.runner.Deferred(); held {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the applier never retained a record written at a newer version")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// THE HEARTBEAT DATES IT, as it does on every node every ten
			// seconds.
			r.state.publishPositions(ctx)
			sighted := time.Now().UTC()

			r.now = func() time.Time { return sighted.Add(statelog.DeferralGrace - time.Minute) }
			r.beat(ctx)
			if alarmed(r.Report(ctx), statelog.KindDeferredOld) {
				t.Fatal("a deferral inside the grace raised deferred_old")
			}
			r.now = func() time.Time {
				return sighted.Add(statelog.DeferralGrace + statelog.AlarmInterval)
			}
			r.beat(ctx)
			var fired statelog.Alarm
			for _, a := range r.Report(ctx).Alarms {
				if a.Kind == statelog.KindDeferredOld {
					fired = a
				}
			}
			if fired.Kind == "" {
				t.Fatal("a record from a newer build held a beat past the grace " +
					"raised no deferred_old")
			}
			want := fmt.Sprintf("the %s log at %s@", tc.domain, running.domain.Stream().Name)
			version := fmt.Sprintf("written at record version %d against the %d "+
				"this build reads", tc.version+1, tc.version)
			if !strings.Contains(fired.Detail, want) ||
				!strings.Contains(fired.Detail, version) {
				t.Errorf("the alarm reads %q; it names the log, the position and "+
					"the version an operator upgrades to", fired.Detail)
			}
			if !strings.Contains(fired.Detail, tc.says) {
				t.Errorf("the alarm reads %q; want it to say %q", fired.Detail, tc.says)
			}
			if got := alarmGauge(t, e, statelog.KindDeferredOld); got != 1 {
				t.Errorf("the alarm gauge for %s reads %v, want 1",
					statelog.KindDeferredOld, got)
			}
		})
	}
}

// A FLOOR THIS NODE CANNOT USE FOR FOUR HEARTBEATS FIRES `floor_unknown` —
// observed on the heartbeat, through the fleet's own published floors.
//
// The floor is one the fleet published at a LATER generation than this node's
// applier, which every read on this node refuses on: what a fleet looks like
// to a node that was not re-anchored with it. It could not fire before: the
// branch that set its input was unreachable behind a health read that failed on
// the same floor.
func TestTheFloorUnknownAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	if r.floors == nil {
		t.Fatal("the node's retention loop keeps no floor watch")
	}
	name := tracker.Domain{}.Name()
	generation := r.state.domains[name].runner.Committed().Generation
	ahead := coord.TrimFloor{Domain: name, Generation: generation + 1,
		At: time.Now().UTC(), By: "node-b"}
	if err := r.fleet.PutFloor(ctx, ahead); err != nil {
		t.Fatalf("publish a floor ahead of this node: %v", err)
	}

	t0 := time.Now().UTC()
	r.now = func() time.Time { return t0 }
	r.beat(ctx)
	if alarmed(r.Report(ctx), statelog.KindFloorUnknown) {
		t.Fatal("one beat that could not use the floor raised floor_unknown")
	}
	r.now = func() time.Time { return t0.Add(statelog.FloorCacheStale + statelog.AlarmInterval) }
	r.beat(ctx)
	var fired statelog.Alarm
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind == statelog.KindFloorUnknown {
			fired = a
		}
	}
	if fired.Kind == "" {
		t.Fatal("a floor this node could not use for five heartbeats raised no " +
			"floor_unknown")
	}
	if !strings.Contains(fired.Detail, name+": ") ||
		!strings.Contains(fired.Detail, fmt.Sprintf("generation %d", generation+1)) {
		t.Errorf("the alarm reads %q; it names the log and why its floor is unusable",
			fired.Detail)
	}
	if got := alarmGauge(t, e, statelog.KindFloorUnknown); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1", statelog.KindFloorUnknown, got)
	}

	// THE FLEET PUBLISHES AT THIS NODE'S GENERATION AGAIN, and the next beat
	// clears it.
	ahead.Generation = generation
	if err := r.fleet.PutFloor(ctx, ahead); err != nil {
		t.Fatalf("publish the floor at this node's generation: %v", err)
	}
	r.now = func() time.Time { return t0.Add(statelog.FloorCacheStale + 2*statelog.AlarmInterval) }
	r.beat(ctx)
	if alarmed(r.Report(ctx), statelog.KindFloorUnknown) {
		t.Error("a floor this node can use again still raises floor_unknown")
	}
}

// domainAt is a log's bounds as a case read them.
type domainAt struct{ First, Last uint64 }

// THE TABLE IS EVALUATED ON THE HEARTBEAT, not on the trim's quarter-hour.
//
// Every alarm there fires at a threshold of a minute or more, and an evaluation
// paced by the trim silently raised each of them to fifteen minutes: the
// binding watch this node runs is observed on every beat, so a second
// observation arriving within one [statelog.AlarmInterval] of the first is the
// heartbeat running.
func TestTheAlarmTableIsEvaluatedOnTheHeartbeat(t *testing.T) {
	t.Parallel()
	e := bootDirectoryNode(t, nil)
	r := e.retention
	if r == nil || r.bindings == nil {
		t.Fatal("this node runs no retention loop or no binding watch")
	}
	first := awaitObservation(t, r, time.Time{}, statelog.AlarmInterval+10*time.Second)
	second := awaitObservation(t, r, first, statelog.AlarmInterval+10*time.Second)
	if gap := second.Sub(first); gap > statelog.AlarmInterval+5*time.Second {
		t.Errorf("two evaluations were %s apart, want one heartbeat", gap)
	}
}

// quietRetention is the node's own retention loop with its ticking stopped, so
// a case can drive the same evaluations on its own clock without racing them.
func quietRetention(t *testing.T, e *Engine) *retention {
	t.Helper()
	r := e.retention
	if r == nil {
		t.Fatal("the node started no retention loop")
	}
	r.stop()
	r.done.Wait()
	if r.alarms == nil {
		t.Fatal("the node's retention loop keeps no alarm tracker")
	}
	return r
}

// awaitBinding waits for this node's identity applier to hold a person's
// binding.
func awaitBinding(t *testing.T, e *Engine, person string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		bindings, err := e.IAM().SeatBindings(t.Context())
		if err == nil {
			for _, b := range bindings {
				if b.Person == person && b.Seat != "" {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the binding never applied (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitObservation waits for the binding watch's latest observation to move
// past after, and answers it.
func awaitObservation(t *testing.T, r *retention, after time.Time,
	within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r.bindings.mu.Lock()
		at := r.bindings.at
		r.bindings.mu.Unlock()
		if at.After(after) {
			return at
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observation after %s within %s", after, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// alarmed reports whether a report carries an alarm of a kind.
func alarmed(report statelog.Report, kind statelog.Kind) bool {
	for _, a := range report.Alarms {
		if a.Kind == kind {
			return true
		}
	}
	return false
}

// alarmGauge is the process recorder's alarm gauge for one kind.
func alarmGauge(t *testing.T, e *Engine, kind statelog.Kind) float64 {
	t.Helper()
	if e.metrics == nil {
		t.Fatal("the node has no recorder")
	}
	for _, s := range e.metrics.Read() {
		if s.Name == metrics.AlarmActive && s.Attrs["kind"] == string(kind) {
			return s.Value
		}
	}
	t.Fatalf("no %s series for %s", metrics.AlarmActive, kind)
	return -1
}
