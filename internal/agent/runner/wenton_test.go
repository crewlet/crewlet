package runner_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
)

// A RUN CONTINUING A SESSION ITS TURN WENT PAST IS LED SO. The session a
// conversation's last turn left was kept while that turn still worked, and the
// turn's end said it went on past it — and the turn may have been lost after,
// or lost with an end that said nothing of going on: the launcher's basis says
// which, with when the session was kept, and the lead the run is handed before
// the turn's own brief says where the session ends, in place of a turn
// boundary it does not reach — never that a session its turn went on past runs
// through the turn — and of a lost turn's, that it may not hold the last of
// what that turn did.
//
// Mutation: lead the run off the base's turn and its end alone, leaving out
// whether that turn went on past the session, was lost, or when the session
// was kept, and a check goes red.
func TestARunContinuingASessionItsTurnWentPastIsLedSo(t *testing.T) {
	t.Parallel()
	kept := time.Date(2026, 10, 1, 8, 15, 0, 0, time.UTC)
	for _, c := range []struct {
		name         string
		wentOn, lost bool
		says, never  []string
	}{
		{name: "its turn went on past it", wentOn: true,
			says: []string{"up to a session kept while turn `t-0` still worked, at 2026-10-01T08:15:00Z",
				"what that turn did after it is not above", "How turn `t-0` ended"},
			never: []string{"through turn `t-0`", "may not hold"}},
		{name: "its turn went on past it and was then lost", wentOn: true, lost: true,
			says: []string{"up to a session kept while turn `t-0` still worked, at 2026-10-01T08:15:00Z",
				"what that turn did after it is not above, and that turn was lost", "How turn `t-0` ended"},
			never: []string{"through turn `t-0`", "may not hold"}},
		{name: "its turn was lost", lost: true,
			says: []string{"through turn `t-0`, which was lost",
				"may not hold the last of what that turn did", "How turn `t-0` ended"},
			never: []string{"still worked", "box or host"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base := *baseBasis.Base
			base.CapturedAt, base.WentOn, base.Lost = kept, c.wentOn, c.lost
			basis := baseBasis
			basis.Base = &base
			entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
			launcher := &probingLauncher{basis: basis}
			first, _ := buildWith(t, entries, buildOpts{launcher: launcher, calls: turnctx.NewCallLog()})
			work, _, err := first.Execute(t.Context(), 1, "", nil)
			if err != nil || !work.Suspended {
				t.Fatalf("the first launch: %+v, %v", work, err)
			}
			s := launcher.last(t).Session
			if s == nil || !s.Basis.ContinuesBase() {
				t.Fatalf("the first launch continues %+v, want the conversation's session", s)
			}
			for _, want := range c.says {
				if !strings.Contains(s.Replaced, want) {
					t.Errorf("the lead does not say %q:\n%s", want, s.Replaced)
				}
			}
			for _, not := range c.never {
				if strings.Contains(s.Replaced, not) {
					t.Errorf("the lead says %q:\n%s", not, s.Replaced)
				}
			}
		})
	}
}
