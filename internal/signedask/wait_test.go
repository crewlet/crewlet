package signedask

import (
	"testing"
	"time"
)

// A CARRIED WAIT IS COUNTED FROM ARRIVAL, and no clock but the taking node's
// runtime is asked: the answer is due exactly the wait after the request
// reached it, however long it then waited for a slot, and one whose wait was
// spent before it started is nobody's. So is a request carrying no wait, or
// one that is no duration: no sender of this build sends either, and an older
// unreleased build's asker gets no compatibility.
//
// Mutations: give a request carrying no wait a due instant, or count the wait
// from the start rather than the arrival, red.
func TestACarriedWaitIsCountedFromArrivalAndNoWaitIsNobodys(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		waitMS int64
		start  time.Duration // after arrival
		due    time.Time
		nobody bool
	}{
		"a fifteen-second wait":            {waitMS: 15_000, due: instant.Add(15 * time.Second)},
		"held ten seconds for a slot":      {waitMS: 15_000, start: 10 * time.Second, due: instant.Add(15 * time.Second)},
		"held past its wait":               {waitMS: 15_000, start: 15 * time.Second, nobody: true},
		"a millisecond's wait":             {waitMS: 1, due: instant.Add(time.Millisecond)},
		"no wait":                          {nobody: true},
		"a wait below zero":                {waitMS: -1, nobody: true},
		"a wait too long to be a duration": {waitMS: 1 << 62, nobody: true},
	} {
		due, why := Due(instant, tc.waitMS, instant.Add(tc.start))
		switch {
		case tc.nobody && why == "":
			t.Errorf("%s: due %v, want it refused as a wait nobody is waiting out", name, due)
		case !tc.nobody && (why != "" || !due.Equal(tc.due)):
			t.Errorf("%s: due %v (%q), want %v", name, due, why, tc.due)
		}
	}
}

// A WAIT CROSSES ROUNDED UP TO THE MILLISECOND, so a wait under one is never
// carried as none, which is refused; one already spent is carried as none, and
// no sender asks with it.
//
// Mutation: truncate (wait.Milliseconds()), and a 0.4 ms wait crosses as none, red.
func TestAWaitCrossesRoundedUpToTheMillisecond(t *testing.T) {
	t.Parallel()
	for wait, want := range map[time.Duration]int64{
		400 * time.Microsecond:                  1,
		time.Millisecond:                        1,
		15*time.Second - 10*time.Microsecond:    15_000,
		15 * time.Second:                        15_000,
		0:                                       0,
		-time.Second:                            0,
		time.Duration(1<<62) + time.Microsecond: int64(time.Duration(1<<62)/time.Millisecond) + 1,
	} {
		if got := WaitMS(wait); got != want {
			t.Errorf("WaitMS(%v) = %d, want %d", wait, got, want)
		}
	}
}
