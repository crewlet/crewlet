package issuereport

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fleetAlarms is what the shadow reads: the kinds standing, or a read failure.
type fleetAlarms struct {
	mu       sync.Mutex
	standing []string
	err      error
}

func (f *fleetAlarms) set(err error, standing ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.standing, f.err = standing, err
}

func (f *fleetAlarms) read(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.standing, f.err
}

// newShadowRig is a rig whose shadow reads alarms, read once before it returns.
func newShadowRig(t *testing.T, alarms *fleetAlarms) *rig {
	t.Helper()
	r := newRigOn(t, memory.NewFleet(), newFakeRelay(t), newClock(),
		func(c *Config) { c.Alarms = alarms.read })
	r.report.readAlarms(context.Background())
	return r
}

func failedTurn() types.AgentTurnCompleted {
	return types.AgentTurnCompleted{Agent: "seat", Failed: true, ErrorKind: kindError}
}

// A FAILURE AN OUTAGE CAN CAUSE IS NOT COUNTED WHILE ITS ALARM STANDS. A loop
// of unclassified failures during a full volume, a starved pool or a stopped
// feed is the environment the alarm already names, and the row starts again
// once it clears, so failures from before the alarm do not join ones after it.
// A panic is never shadowed. An alarm outside the shadow list shadows nothing.
//
// Mutation: drop the shadow check from Observe, or the mark from a wired
// failure path, and the first loop is reported; keep the tracker through the
// shadow and the row straddling it is; mark the panic path and its report
// never comes.
func TestAShadowedClassIsNotCountedWhileAnEnvironmentAlarmStands(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassIdenticalFailureLoop)
	for _, kind := range ShadowAlarms {
		alarms := &fleetAlarms{}
		alarms.set(nil, string(statelog.KindBackupAge), string(kind))
		r := newShadowRig(t, alarms)
		for range threshold + 1 {
			r.publish(failedTurn())
		}
		r.report.DeadLettered(context.Background(), "seat", OutcomeNak)
		r.idle()
		if got := r.relay.received(); len(got) != 0 {
			t.Errorf("a loop under %s was reported: %s", kind, got)
		}
		r.stop()
	}

	alarms := &fleetAlarms{}
	r := newShadowRig(t, alarms)
	for range threshold - 1 {
		r.publish(failedTurn())
	}
	alarms.set(nil, string(statelog.KindVolumeLow))
	r.report.readAlarms(context.Background())
	r.publish(failedTurn())
	r.publish(types.TurnGuardBreach{Agent: "seat", Kind: types.GuardUnhandledException,
		PanicFunction: "internal/tools.(*Surface).call"})
	waitFor(t, "the panic's report", func() bool { return len(r.relay.received()) == 1 })
	alarms.set(nil, string(statelog.KindBackupAge))
	r.report.readAlarms(context.Background())
	r.publish(failedTurn())
	r.idle()
	if got := r.relay.received(); len(got) != 1 {
		t.Fatalf("a row straddling an alarm was reported: %d reports", len(got))
	}
	for range threshold - 1 {
		r.publish(failedTurn())
	}
	waitFor(t, "the loop's report", func() bool { return len(r.relay.received()) == 2 })
}

// A FLEET WHOSE ALARMS COULD NOT BE READ SHADOWS, and so does one not yet read:
// the read failing is the coordination store not answering, and a reporter
// just turned on has no answer yet.
//
// Mutation: treat a failed read as a quiet fleet, or start unshadowed, and a
// loop is reported.
func TestAnUnreadFleetShadows(t *testing.T) {
	t.Parallel()
	threshold := entryThreshold(ClassIdenticalFailureLoop)
	alarms := &fleetAlarms{}
	alarms.set(errors.New("coord: unavailable"))
	r := newShadowRig(t, alarms)
	for range threshold + 1 {
		r.publish(failedTurn())
	}
	r.idle()
	if got := r.relay.received(); len(got) != 0 {
		t.Errorf("a loop on an unreadable fleet was reported: %s", got)
	}

	unread := New(Config{Store: memory.NewFleet(), Origin: releaseOrigin,
		Alarms: (&fleetAlarms{}).read})
	unread.SetEnabled(true)
	for range threshold + 1 {
		unread.Observe(context.Background(), Signal{Class: ClassIdenticalFailureLoop,
			Subject: "seat", Kind: kindError, Shadowed: true})
	}
	if len(unread.queue) != 0 {
		t.Error("a reporter that has not read the fleet counted a loop")
	}
}
