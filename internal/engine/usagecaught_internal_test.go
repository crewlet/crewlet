package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE USAGE PUBLISHER WAITS FOR THE USAGE APPLIER, AND FOR NO OTHER.
//
// Every day a node publishes is joined with the usage rows its applier wrote,
// which right after a restore are the copy's until that applier has replayed
// what the fleet published since — so the publisher derives nothing until the
// USAGE runner has drained its log ([stateLog.usageCaught]). The usage
// package's own cases hand the publisher a gate of their own, so nothing there
// reaches the engine's: read off another domain's runner, or not read at all,
// a restored node would publish days joined with rows the fleet has since
// moved past, and every one of those cases would stay green.
//
// Mutation: open the gate whatever the runner says, or read the tracker's
// runner rather than the usage domain's, and the gate reads open before the
// usage runner has drained; let it never open, and the control goes red.
func TestTheUsagePublisherWaitsForTheUsageApplierAlone(t *testing.T) {
	t.Parallel()
	s := seamsStateLog(t)
	entries := map[string]registration{}
	for _, entry := range register() {
		entries[entry.Domain.Name()] = entry
	}
	trackerEntry := entries[worklet.Domain{}.Name()]
	usageEntry := entries[usage.Domain{}.Name()]
	trackerRunner := seamsRunner(t, s, trackerEntry)
	usageRunner := seamsRunner(t, s, usageEntry)
	s.logs = newLogSet([]*runningLog{
		{domain: trackerEntry.Domain, runner: trackerRunner},
		{domain: usageEntry.Domain, runner: usageRunner},
	})
	drain := func(name string, run *statelog.Runner) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = run.Run(ctx)
		}()
		t.Cleanup(func() {
			cancel()
			<-done
		})
		deadline := time.Now().Add(10 * time.Second)
		for !run.Drained() {
			if time.Now().After(deadline) {
				t.Fatalf("%s's runner never drained an empty log", name)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	if s.usageCaught() {
		t.Fatal("the usage publisher derives before any applier has run")
	}
	// ANOTHER DOMAIN'S APPLIER DRAINED, this one's not: the gate stays shut.
	drain("the tracker", trackerRunner)
	if s.usageCaught() {
		t.Fatal("the usage publisher derives while its own applier has not drained, " +
			"because another domain's has")
	}
	// THE CONTROL: once its own applier drains, the gate opens.
	drain("the usage domain", usageRunner)
	if !s.usageCaught() {
		t.Fatal("the usage publisher stays shut once its own applier has drained")
	}
}

// A NODE RUNNING NO USAGE LOG DERIVES NO DAY: there are no applied rows to join
// a derivation with.
func TestANodeRunningNoUsageLogIsNeverCaughtUp(t *testing.T) {
	t.Parallel()
	var none *stateLog
	if none.usageCaught() {
		t.Error("a node running no state log reads as caught up with its usage log")
	}
	if (&stateLog{}).usageCaught() {
		t.Error("a node running no usage log reads as caught up with it")
	}
}
