package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE GATE RESERVE ON THE ENGINE'S OWN LOGS: an eviction still lands on a log
// its ordinary writes can no longer reach, and one refused even there says to
// raise the ceiling rather than to run the gesture again.

// fullForOrdinaryWrites fills each identity log, with records every build
// decodes and no gate drops, until it holds at least minBytes, and then sets
// its ceiling to ceiling(bytes held) — answering the ceiling each got, by log.
//
// THE CEILING IS SET ON THE STREAM ITSELF, past the capacity window, because
// what is under test is what a log at its ceiling takes, not how it came to be
// there; the reserve reads the ceiling from the broker on every admission, as
// it would after any resize.
func fullForOrdinaryWrites(t *testing.T, identity []*runningDomain, minBytes uint64,
	ceiling func(held uint64) uint64) map[string]uint64 {

	t.Helper()
	out := map[string]uint64{}
	for _, running := range identity {
		name := running.domain.Name()
		for {
			stats, err := running.log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read %s's usage: %v", name, err)
			}
			if stats.Bytes >= minBytes {
				break
			}
			barrierOn(t, running)
		}
		waitApplied(t, running)
		stats, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read %s's usage: %v", name, err)
		}
		limit := ceiling(stats.Bytes)
		if err := running.log.SetMaxBytes(t.Context(), limit); err != nil {
			t.Fatalf("set %s's ceiling to %d: %v", name, limit, err)
		}
		out[name] = limit
	}
	return out
}

// ordinaryCeilingAt is the smallest ceiling whose ordinary writes are held to
// no less than held — a log exactly full for everything but a gate record.
func ordinaryCeilingAt(held uint64) uint64 {
	limit := held
	for statelog.OrdinaryCeiling(limit, true) < held {
		limit++
	}
	return limit
}

// refusedFull reports whether err is the full-log refusal.
func refusedFull(err error) bool {
	var refusal *statelog.Unavailable
	return errors.As(err, &refusal) && refusal.Reason == statelog.ReasonLogFull
}

// requireOrdinaryRefused asserts that an ordinary append — a linearizable
// read's barrier, admitted through the log's own reserve as the engine's read
// index admits it — is refused `log_full` and never reaches the log.
func requireOrdinaryRefused(t *testing.T, running *runningDomain) {
	t.Helper()
	name := running.domain.Name()
	index, err := statelog.NewReadIndex(running.domain, running.log, running.reserve,
		barrierEncoder(running.domain),
		func() uint32 { return running.runner.Committed().Generation }, nil)
	if err != nil {
		t.Fatalf("build %s's read index: %v", name, err)
	}
	_, before, err := running.log.Bounds(t.Context())
	if err != nil {
		t.Fatalf("read %s's end: %v", name, err)
	}
	if _, err := index.Read(t.Context()); !refusedFull(err) {
		t.Fatalf("a linearizable read on %s, full for ordinary writes, answered "+
			"%v — want log_full", name, err)
	}
	if _, after, err := running.log.Bounds(t.Context()); err != nil || after != before {
		t.Fatalf("the refused barrier moved %s's end from %d to %d (%v)",
			name, before, after, err)
	}
}

// AN EVICTION LANDS ON A LOG FULL FOR ORDINARY WRITES, AND COMPLETES.
//
// The commonest way a log fills is a node that is gone and still counted,
// pinning the trim — and the one gesture that unpins it is an eviction, a
// record on that log. Refused like any other append, it left an operator
// running it again for ever. Here every identity log is at its ordinary
// ceiling, so a linearizable read's barrier is refused, and the eviction still
// lands on every log, past that ceiling, and is applied.
func TestAnEvictionLandsOnALogFullForOrdinaryWrites(t *testing.T) {
	t.Parallel()
	e, back, _ := trimmedTracker(t)
	identity := identityLogs(t, e.native.Load().log)
	// A HUNDRED AND TWENTY-EIGHT KIBIBYTES HELD keeps a reserve of about
	// eight: room for many eviction records of a few hundred bytes each,
	// and more than one barrier is counted at — so a reserve that admitted
	// ordinary appends up to the broker's own ceiling would let the barrier
	// below through, and the case would say so.
	ceilings := fullForOrdinaryWrites(t, identity, 128<<10, ordinaryCeilingAt)
	for _, running := range identity {
		requireOrdinaryRefused(t, running)
	}

	res, err := e.native.Load().gate.Evict(t.Context(), GateRequest{
		Node: "node-gone", OpID: "op-full", By: "operator"})
	if err != nil {
		t.Fatalf("evict on full logs: %v", err)
	}
	if !res.Complete() {
		t.Fatalf("an eviction on logs full for ordinary writes did not complete: "+
			"%+v — it is the record that unpins them", res)
	}
	for _, running := range identity {
		name := running.domain.Name()
		stats, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read %s's usage: %v", name, err)
		}
		soft := statelog.OrdinaryCeiling(ceilings[name], true)
		if stats.Bytes <= soft || stats.Bytes > ceilings[name] {
			t.Fatalf("%s holds %d bytes with the eviction landed, want it in the "+
				"reserve between %d and %d", name, stats.Bytes, soft, ceilings[name])
		}
		waitApplied(t, running)
		rows, err := running.domain.(evictionLister).Evictions(t.Context(), back.Store)
		if err != nil {
			t.Fatalf("read %s's evictions: %v", name, err)
		}
		if len(rows) != 1 || rows[0].NodeID != "node-gone" || rows[0].Back {
			t.Fatalf("%s holds evictions %+v, want node-gone evicted", name, rows)
		}
		// AND THE RESERVE IS STILL NOT ROOM FOR ORDINARY WRITES.
		requireOrdinaryRefused(t, running)
	}
}

// AN EVICTION REFUSED EVEN FROM THE RESERVE SAYS TO RAISE THE CEILING.
//
// With the reserve spent the broker refuses the gate record too, and that
// refusal is the one no retry clears: the answer is `crewlet retention
// set-capacity`, and advising the same gesture again under the same operation
// id would send an operator round a loop the refusal already knows the end of.
func TestAnEvictionPastTheReserveIsSentToSetCapacity(t *testing.T) {
	t.Parallel()
	e, _, _ := trimmedTracker(t)
	identity := identityLogs(t, e.native.Load().log)
	// SIXTY-FOUR BYTES OF ROOM, less than any eviction record.
	fullForOrdinaryWrites(t, identity, 48<<10, func(held uint64) uint64 { return held + 64 })

	res, err := e.native.Load().gate.Evict(t.Context(), GateRequest{
		Node: "node-gone", OpID: "op-past", By: "operator"})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if res.Complete() {
		t.Fatalf("an eviction with no room anywhere on its logs reported itself "+
			"complete: %+v", res)
	}
	for _, d := range res.Domains {
		if !refusedFull(d.Err) {
			t.Fatalf("%s answered %+v, want log_full", d.Domain, d)
		}
		if d.Retry() {
			t.Errorf("%s advises running the gesture again on a log whose reserve "+
				"is spent: %+v", d.Domain, d.Remedy())
		}
		if !d.Remedy().Offers(statelog.GateSetCapacity) {
			t.Errorf("%s's remedy does not offer raising the ceiling: %+v",
				d.Domain, d.Remedy())
		}
		if !strings.Contains(d.Err.Error(), "gate record") {
			t.Errorf("%s's refusal does not say the gate record was refused past "+
				"its reserve: %v", d.Domain, d.Err)
		}
	}
}

// THE REPORT AND THE GAUGE MEASURE A RESERVED LOG AGAINST ITS ORDINARY CEILING.
//
// Both read the running logs the engine built, so this is what an operator's
// `crewlet retention status` and a collector's graph show: on the tracker and
// pages logs the reserve beside the ceiling and a headroom that reaches zero
// where ordinary writes start being refused, and on the vector changelog —
// which keeps no reserve — neither. Either surface measuring against the
// broker's ceiling would show room on a log refusing every write.
func TestTheReportAndTheGaugeMeasureHeadroomAgainstTheOrdinaryCeiling(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.native.Load().log
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	r := &retention{fleet: e.backends.Fleet, state: s, nodeID: "node-a", metrics: recorder}
	rows := map[string]statelog.DomainReport{}
	for _, d := range r.Report(t.Context()).Domains {
		rows[d.Domain] = d
	}
	for _, name := range s.order {
		running := s.domains[name]
		reserved := statelog.KeepsGateReserve(running.domain)
		stats, err := running.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read %s's usage: %v", name, err)
		}
		want := *statelog.Headroom(stats.Bytes, stats.MaxBytes, reserved)
		var wantReserve uint64
		if reserved {
			wantReserve = statelog.GateReserve(stats.MaxBytes)
		}
		got := rows[name]
		if got.ReserveBytes != wantReserve {
			t.Errorf("%s reports a reserve of %d bytes, want %d", name,
				got.ReserveBytes, wantReserve)
		}
		if got.HeadroomFraction == nil || *got.HeadroomFraction != want {
			t.Errorf("%s reports a headroom of %v, want %v — of the ceiling its "+
				"ordinary writes are held to", name, got.HeadroomFraction, want)
		}

		r.gauges(name, reserved, stats, statelog.TrimDecision{}, fleetInputs{})
		var gauged *float64
		for _, snap := range recorder.Read() {
			if snap.Name == metrics.StatelogLogHeadroomFraction && snap.Attrs["domain"] == name {
				gauged = &snap.Value
			}
		}
		if gauged == nil || *gauged != want {
			t.Errorf("%s's headroom gauge reads %v, want %v", name, gauged, want)
		}
	}
	for _, name := range []string{"tracker", "pages"} {
		if rows[name].ReserveBytes == 0 {
			t.Errorf("%s claims identity and reports no reserve", name)
		}
	}
}
