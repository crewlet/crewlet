package duty_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
)

// A RULE PASS THAT COULD NOT READ SAYS WHY ON ITS RECORD: the run is failed,
// and every class reads unread with the reason, a deadline's included.
// Mutation: record it failed with no classes, and the page says it failed but
// not why.
func TestARulePassThatCouldNotReadSaysWhyOnItsRecord(t *testing.T) {
	t.Parallel()
	l := &fakeLog{}
	step(t, newDuty(l, &fakeReader{signature: "r1", observeErr: errors.New("the ledger is down")}, nil,
		&clock{now: start}))
	if len(l.runs) != 1 || l.runs[0].Status != duty.RunFailed || len(l.runs[0].Classes) == 0 {
		t.Fatalf("runs = %+v, want one failed run naming its classes", l.runs)
	}
	for _, c := range l.runs[0].Classes {
		if c.Outcome != advisor.OutcomeUnread || !strings.Contains(c.Reason, "the ledger is down") {
			t.Errorf("class %s = %s %q, want unread saying why", c.Class, c.Outcome, c.Reason)
		}
	}
}
