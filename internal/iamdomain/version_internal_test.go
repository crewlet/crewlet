package iamdomain

import (
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY RECORD IS WRITTEN AT THE LOWEST VERSION THAT CARRIES ITS MEANING.
//
// A record above a peer's decode ceiling is DEFERRED on that peer, and a
// deferred record blocks every later record in its scope there. So the ceiling
// is what this build reads and never what it writes: a build that wrote every
// record at the ceiling would have a rolling upgrade defer everything on every
// older node — and a BARRIER an older node deferred is a linearizable read on
// that node that waits for ever. The sweep is the one op whose meaning moved,
// and a gate is pinned for ever.
func TestEveryRecordIsWrittenAtTheLowestVersionThatCarriesIt(t *testing.T) {
	t.Parallel()
	if RecordVersion < SweepRecordVersion {
		t.Fatalf("this build writes sweeps at %d and reads only up to %d",
			SweepRecordVersion, RecordVersion)
	}
	for _, op := range OpKinds {
		want := BaseRecordVersion
		switch op {
		case OpSweep:
			want = SweepRecordVersion
		case OpRemove, OpEviction, OpInvalidate:
			want = GateRecordVersion
		}
		if got := writeVersion(op); got != want {
			t.Errorf("%s is written at version %d, want %d", op, got, want)
		}
	}

	payload, err := EncodeBarrier(statelog.Envelope{
		Kind: statelog.BarrierKind,
	})
	if err != nil {
		t.Fatalf("EncodeBarrier: %v", err)
	}
	env, err := DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("decode the barrier: %v", err)
	}
	if env.V != BaseRecordVersion {
		t.Errorf("a barrier is written at version %d, want %d — an older node "+
			"defers it, and its linearizable read waits for ever",
			env.V, BaseRecordVersion)
	}
}
