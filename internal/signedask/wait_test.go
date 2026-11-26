package signedask

import (
	"testing"
	"time"
)

// A CARRIED WAIT IS COUNTED FROM ARRIVAL, WHATEVER EITHER CLOCK READS: a taking
// node whose clock stands three seconds, or an hour, ahead of or behind the
// asker's deadline is due exactly the wait after the request reached it. A
// request carrying none, an older asker's, is due at its deadline as the
// taking node's clock reads it; a carried wait that is no duration is nobody's.
//
// Mutation: count a carried wait to the deadline on now, and every skewed case
// is due early or late, red.
func TestACarriedWaitIsCountedFromArrivalWhateverEitherClockReads(t *testing.T) {
	t.Parallel()
	arrived := instant
	deadline := instant.Add(15 * time.Second) // as the asker's clock reads it
	for name, tc := range map[string]struct {
		waitMS int64
		now    time.Time
		due    time.Time
		nobody bool
	}{
		"in step":                          {waitMS: 15_000, now: instant, due: arrived.Add(15 * time.Second)},
		"three seconds ahead":              {waitMS: 15_000, now: instant.Add(3 * time.Second), due: arrived.Add(15 * time.Second)},
		"past the wait ahead":              {waitMS: 15_000, now: instant.Add(14500 * time.Millisecond), due: arrived.Add(15 * time.Second)},
		"an hour behind":                   {waitMS: 15_000, now: instant.Add(-time.Hour), due: arrived.Add(15 * time.Second)},
		"an older asker's, three ahead":    {now: instant.Add(3 * time.Second), due: arrived.Add(12 * time.Second)},
		"an older asker's, in step":        {now: instant, due: arrived.Add(15 * time.Second)},
		"a wait below zero":                {waitMS: -1, now: instant, nobody: true},
		"a wait too long to be a duration": {waitMS: 1 << 62, now: instant, nobody: true},
	} {
		due, why := Due(arrived, tc.waitMS, deadline, tc.now)
		switch {
		case tc.nobody && why == "":
			t.Errorf("%s: due %v, want it refused as a wait nobody is waiting out", name, due)
		case !tc.nobody && (why != "" || !due.Equal(tc.due)):
			t.Errorf("%s: due %v (%q), want %v", name, due, why, tc.due)
		}
	}
}

// A WAIT CROSSES ROUNDED UP TO THE MILLISECOND, so a wait under one is never
// carried as none, which reads as an older asker's request; one already spent
// is carried as none, and no sender asks with it.
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
