package engine

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
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
	if !standsOn(fired, "tracker") {
		t.Errorf("the alarm stands on %+v and does not name the tracker's log alone",
			fired.Domains)
	}
	if got := alarmGauge(t, e, statelog.KindLogCeilingShort); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindLogCeilingShort, got)
	}
}

// A FRESH NODE'S BLOCKED TRIM IS SILENT, AND A BLOCK THAT HAS KEPT RECORDS
// PAST THE WINDOW IS NAMED: through the trim's own tick, the floor it publishes
// and the report every surface reads.
//
// A fresh node is the young fleet: its trim is blocked from the first tick,
// because nothing has been backed up. The report's clock is then put a whole
// window and more ahead, so the block is older than the threshold by every
// measure the clock has, and the log must be named nowhere, because the age
// term the tick published keeps every record the log holds. Then the published
// floor is what the duty would publish once the records had aged (the same
// block, its age term past the log's head) and the log's hold must be named,
// under the alarm whose remedy it is: the BACKUP's, since the backup term is
// what holds it and that log's own `backup_age` is up on a node that has never
// taken one, so it raises no `trim_blocked` of its own and the one cause reads
// as one problem.
// A floor published at another generation must not be read as this log's at
// all.
func TestABlockedTrimIsNamedOnARunningNodeOnlyPastTheWindow(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	// A RECORD ON A LOG, because a fresh node's boot writes none: the org
	// is part of the company document, so nothing lands on a log until
	// somebody acts — and a trim blocked over an empty log keeps nothing.
	if _, err := e.IAMWriter().InvalidateAll(ctx,
		statelog.NewOpID(time.Now(), "trim-blocked"), "a record to keep"); err != nil {
		t.Fatalf("write a record to the identity log: %v", err)
	}

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
	young := r.Report(ctx)
	if alarmed(young, statelog.KindTrimBlocked) {
		t.Fatal("a fresh node's trim, blocked on its first backup with nothing in " +
			"its log past the window, raised trim_blocked")
	}
	// THE PREMISES: the backup term holds the log, and the backup alarm is up,
	// which is what a node that has never taken a backup is.
	backup, up := alarmOf(young, statelog.KindBackupAge)
	if !up {
		t.Fatal("a node that has never taken a backup raised no backup_age, so " +
			"this case is not the young fleet it names")
	}
	for _, d := range young.Domains {
		if d.Domain == name && d.BlockedBy != statelog.TermBackupFloor {
			t.Fatalf("the %s log is held by %q, not by the backup it is waiting for",
				name, d.BlockedBy)
		}
	}
	// EVERY LOG IS NAMED FOR ITS MISSING BACKUP, and none with the hold: a
	// block that keeps nothing past the window raised no trim_blocked to fold.
	if !slices.ContainsFunc(backup.Domains, func(d statelog.AlarmDomain) bool {
		return d.Domain == name
	}) {
		t.Fatalf("backup_age stands on %+v and not on the %s log, which no "+
			"backup covers", backup.Domains, name)
	}
	for _, d := range backup.Domains {
		if d.Term != "" || !d.Since.IsZero() {
			t.Fatalf("backup_age names %+v with its trim's hold while nothing in "+
				"any log is past the window", d)
		}
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
	aging := r.Report(ctx)
	if alarmed(aging, statelog.KindTrimBlocked) {
		t.Errorf("a log the missing backup holds raised a trim_blocked of its own " +
			"beside backup_age, so one cause reads as two problems")
	}
	backup, _ = alarmOf(aging, statelog.KindBackupAge)
	var held *statelog.AlarmDomain
	for i, d := range backup.Domains {
		if d.Domain == name {
			held = &backup.Domains[i]
		}
	}
	if held == nil {
		t.Fatalf("a trim blocked past its window with %d record(s) past it is named "+
			"nowhere: backup_age stands on %+v", at.Last-at.First+1, backup.Domains)
	}
	if held.Term != statelog.TermBackupFloor || !strings.Contains(held.Detail, "record(s) older") {
		t.Errorf("backup_age names the %s log as %+v; want the backup term holding it "+
			"and what it is keeping", name, *held)
	}
	if got := alarmGauge(t, e, statelog.KindTrimBlocked); got != 0 {
		t.Errorf("the alarm gauge for %s reads %v, want 0", statelog.KindTrimBlocked, got)
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
	backup, _ = alarmOf(after, statelog.KindBackupAge)
	for _, d := range backup.Domains {
		if d.Term != "" {
			t.Errorf("a floor published at another generation is folded under "+
				"backup_age against this log's sequences: %+v", d)
		}
	}
	for _, d := range after.Domains {
		if d.Domain == name && d.BlockedBy != "" {
			t.Errorf("the %s row renders a block from generation %d as its own",
				name, aged.Generation)
		}
	}
}

// A RECORD FROM A NEWER BUILD, HELD PAST THE DEFERRAL GRACE, FIRES ONE
// `deferred_old` STANDING ON EVERY LOG HOLDING ONE, each with its own
// measurement — from a real deferral on a real applier, dated by the real
// position heartbeat — and each log's entry says whether this node's copy of
// the estate goes out of service for it.
//
// The alarm could not fire before: the report stood the grace in for the age
// whenever a record was held, and the rule fires past the grace. The records
// here are what a peer one build ahead would publish — signed under this
// fleet's keyring, at a record version this build does not read — so the
// appliers retain them exactly as a rolling upgrade makes them. And the node
// holds one on TWO logs at once, which is what a build that moved two record
// versions leaves behind: one judges this node's copy of the estate and one
// judges none, so the grace takes the copy out of service for the first and
// takes nothing out for the second — the seats stay where they are for both.
// An alarm carrying only the node's oldest record said one of those about both;
// each log the alarm stands on now carries its own.
func TestTheDeferredOldAlarmFiresOnARunningNode(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	for _, tc := range []struct {
		domain  string
		version int
		record  func(generation uint32) (statelog.Subject, []byte, error)
		says    string
	}{
		{
			domain: worklet.Domain{}.Name(), version: worklet.RecordVersion,
			record: func(generation uint32) (statelog.Subject, []byte, error) {
				subject := worklet.TaskSubject("t-from-a-newer-build")
				payload, err := worklet.MutationRecord{
					RecordEnvelope: worklet.RecordEnvelope{
						V: worklet.RecordVersion + 1, OpID: "op-from-a-newer-build",
						Subject: subject, Op: worklet.OpCreate, Gen: generation,
						CreatedAt: time.Now().UTC(), Writer: "node-b",
						Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
					},
					Mutation: []byte(`{}`), Actor: "ana", ActorKind: worklet.AuthorHuman,
				}.Encode()
				return statelog.Subject{Kind: string(subject.Kind), ID: subject.ID},
					payload, err
			},
			says: "at which this node stops serving its copy of the estate",
		},
		{
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
			says: "this log does not judge this node's copy of the estate, " +
				"so nothing is taken out of service",
		},
	} {
		running := r.state.Domain(tc.domain)
		if running == nil {
			t.Fatalf("the node runs no %s domain", tc.domain)
		}
		subject, payload, err := tc.record(running.runner.Committed().Generation)
		if err != nil {
			t.Fatalf("encode a %s record: %v", tc.domain, err)
		}
		signer, err := r.state.signerFor(running.domain)
		if err != nil {
			t.Fatalf("signer: %v", err)
		}
		address := running.domain.Stream().SubjectPrefix + "." + subject.String()
		if _, _, err := running.log.Append(ctx, address, "op-from-a-newer-build",
			nil, signer.Seal(payload)); err != nil {
			t.Fatalf("append to the %s log: %v", tc.domain, err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, held := running.runner.Deferred(); held {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the %s applier never retained a record written at a "+
					"newer version", tc.domain)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// THE HEARTBEAT DATES BOTH, as it does on every node every ten seconds.
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
	// ONE ALARM, standing on each log that holds a record, with each log's
	// own measurement.
	byLog := map[string]statelog.AlarmDomain{}
	raised := 0
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind != statelog.KindDeferredOld {
			continue
		}
		raised++
		for _, d := range a.Domains {
			byLog[d.Domain] = d
		}
	}
	if raised != 1 {
		t.Errorf("deferred_old was raised %d time(s), want once naming each log", raised)
	}
	for _, tc := range []struct {
		domain  string
		version int
		says    string
	}{
		{worklet.Domain{}.Name(), worklet.RecordVersion,
			"at which this node stops serving its copy of the estate"},
		{iamdomain.Domain{}.Name(), iamdomain.RecordVersion,
			"this log does not judge this node's copy of the estate, " +
				"so nothing is taken out of service"},
	} {
		fired, found := byLog[tc.domain]
		if !found {
			t.Errorf("a record the %s log's applier held a beat past the grace "+
				"raised no deferred_old of its own (raised: %v)", tc.domain, byLog)
			continue
		}
		stream := r.state.Domain(tc.domain).spec.Name
		version := fmt.Sprintf("written at record version %d against the %d "+
			"this build reads", tc.version+1, tc.version)
		if !strings.Contains(fired.Detail, "at "+stream+"@") ||
			!strings.Contains(fired.Detail, version) {
			t.Errorf("the alarm reads %q; it names the position and the version an "+
				"operator upgrades to", fired.Detail)
		}
		if !strings.Contains(fired.Detail, tc.says) {
			t.Errorf("the alarm reads %q; want it to say %q", fired.Detail, tc.says)
		}
	}
	if len(byLog) != 2 {
		t.Errorf("deferred_old stands on %d log(s), want the two holding a record", len(byLog))
	}
	if got := alarmGauge(t, e, statelog.KindDeferredOld); got != 1 {
		t.Errorf("the alarm gauge for %s reads %v, want 1",
			statelog.KindDeferredOld, got)
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
	name := worklet.Domain{}.Name()
	generation := r.state.Domain(name).runner.Committed().Generation
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
	var raised []statelog.Alarm
	for _, a := range r.Report(ctx).Alarms {
		if a.Kind == statelog.KindFloorUnknown {
			raised = append(raised, a)
		}
	}
	if len(raised) == 0 {
		t.Fatal("a floor this node could not use for five heartbeats raised no " +
			"floor_unknown")
	}
	// ONE ALARM, standing on THE ONE LOG WHOSE FLOOR IS AHEAD and none of the
	// others: every other log's floor is readable, and its reads are served.
	if len(raised) != 1 {
		t.Errorf("raised floor_unknown %d times for one unusable floor: %v", len(raised), raised)
	}
	fired := raised[0]
	if !standsOn(fired, name) ||
		!strings.Contains(fired.Detail, fmt.Sprintf("generation %d", generation+1)) {
		t.Errorf("the alarm stands on %+v and reads %q; it names the log and why "+
			"its floor is unusable", fired.Domains, fired.Detail)
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
// floor watch this node runs is observed on every beat, so a second
// observation arriving within one [statelog.AlarmInterval] of the first is the
// heartbeat running.
func TestTheAlarmTableIsEvaluatedOnTheHeartbeat(t *testing.T) {
	t.Parallel()
	e := bootDirectoryNode(t, nil)
	r := e.retention.Load()
	if r == nil || r.floors == nil || r.floors.floors == nil {
		t.Fatal("this node runs no retention loop or no floor watch")
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
	r := e.retention.Load()
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

// awaitObservation waits for the floor watch's latest observation to move
// past after, and answers it.
func awaitObservation(t *testing.T, r *retention, after time.Time,
	within time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		r.floors.mu.Lock()
		at := r.floors.at
		r.floors.mu.Unlock()
		if at.After(after) {
			return at
		}
		if time.Now().After(deadline) {
			t.Fatalf("no observation after %s within %s", after, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// standsOn reports whether an alarm stands on exactly one log, the named one,
// with that log's measurement as its own detail.
func standsOn(a statelog.Alarm, domain string) bool {
	return len(a.Domains) == 1 && a.Domains[0].Domain == domain &&
		a.Domains[0].Detail == a.Detail
}

// alarmOf is a report's alarm of a kind, and whether it carries one.
func alarmOf(report statelog.Report, kind statelog.Kind) (statelog.Alarm, bool) {
	for _, a := range report.Alarms {
		if a.Kind == kind {
			return a, true
		}
	}
	return statelog.Alarm{}, false
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
