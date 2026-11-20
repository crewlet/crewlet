package duty_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// refusingLog is the advisor log refusing a run's record: every one that is
// not failed, or, while down, every one.
type refusingLog struct {
	*fakeLog
	mu      sync.Mutex
	down    bool
	refused int
}

func (l *refusingLog) RecordRun(ctx context.Context, r duty.Run) error {
	l.mu.Lock()
	refuse := l.down || r.Status != duty.RunFailed
	if refuse {
		l.refused++
	}
	l.mu.Unlock()
	if refuse {
		return errors.New("advisordomain: invalid: model is at most 4096 bytes")
	}
	return l.fakeLog.RecordRun(ctx, r)
}

// A RUN WHOSE RECORD IS REFUSED CONCLUDES AS FAILED, and the press that
// started it is never run again: the record is written again as failed with
// a short reason, the record that cannot be refused, and a record the log
// could not take at all is written on the next tick, which does not run the
// press again from the start. Mutation: log the refusal and go on, and every
// tick runs the press again, spending its budget each time.
func TestARunWhoseRecordIsRefusedConcludesAsFailedAndIsNotRunAgain(t *testing.T) {
	t.Parallel()
	l, r, c := &fakeLog{}, &fakeReader{signature: "r1"}, &clock{now: start}
	d := newDuty(l, r, nil, c)
	refusing := &refusingLog{fakeLog: l}
	d.Log = refusing
	deep := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep.release)
	d.Deep = deep
	l.requested = []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}
	step(t, d)
	d.WaitDeep()
	if len(l.runs) != 1 || l.runs[0].ID != "pressed" || l.runs[0].Status != duty.RunFailed ||
		l.runs[0].Kind != advisordomain.RunKindDeep {
		t.Fatalf("runs = %+v, want the press recorded as failed", l.runs)
	}
	reason, _ := l.runs[0].Model.(map[string]any)["reason"].(string)
	if reason == "" || len(reason) > 240 {
		t.Errorf("the failed record says %q, want why in a few words", reason)
	}
	step(t, d)
	d.WaitDeep()
	if deep.runs != 1 {
		t.Errorf("the press ran %d times, want once", deep.runs)
	}

	// THE LOG DOWN AS THE RUN ENDS: nothing is recorded, the press stays
	// waiting, and the next tick records it rather than running it again.
	l2, c2 := &fakeLog{}, &clock{now: start}
	d2 := newDuty(l2, &fakeReader{signature: "r1"}, nil, c2)
	down := &refusingLog{fakeLog: l2, down: true}
	d2.Log = down
	deep2 := &fakeDeep{assigned: true, release: make(chan struct{})}
	close(deep2.release)
	d2.Deep = deep2
	l2.requested = []duty.Request{{Run: "pressed", By: "owner", Kind: advisordomain.RunKindDeep}}
	step(t, d2)
	d2.WaitDeep()
	if len(l2.runs) != 0 || len(l2.requested) != 1 {
		t.Fatalf("with the log down: runs %+v, requested %+v; want nothing recorded and the press waiting", l2.runs, l2.requested)
	}
	down.mu.Lock()
	down.down = false
	down.mu.Unlock()
	step(t, d2)
	d2.WaitDeep()
	if deep2.runs != 1 || len(l2.runs) != 1 || l2.runs[0].Status != duty.RunFailed {
		t.Errorf("the next tick ran the press %d times and recorded %+v, want it recorded, not run again",
			deep2.runs, l2.runs)
	}
}
