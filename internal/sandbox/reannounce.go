package sandbox

import "time"

// # A job is announced once, and again on a schedule
//
// The waiter announces a job's completion when its poll finds the job over,
// and the seat's holder claims it, collects it and resumes its turn. A job
// its holder does not resume at once (its gate refused it, its resume was
// held for a model's limit or handed back for a retry) stays running on its
// row, and the waiter's next poll finds it over again. It used to be
// announced again on every poll: on a live company a finished health check
// whose review met a model's weekly limit was announced 6280 times in 27
// hours, each one a record in the event store and on the dashboard's feed.
//
// The pattern is the provider park notices' (parknotice.go in the engine): a
// notice goes out once per change, not once per pass. Here the change is a
// new job ([PendingRun.LaunchID]), so the events copy of a completion, the
// record that the job finished, is published once per job. The seat still has
// to hear about a job it did not resume, because what made it wait may clear
// with nothing to say so; but the condition that will re-fire it when it does
// is the holder's own ([Coordinator.Readmit]: a provider park's release, a
// budget window's turn, a pause lifted), and the waiter's repeat is only the
// backstop for a clearing nothing signals. So it repeats on a doubling
// schedule, from one poll interval to [reannounceCeilingPolls] of them, to the
// seat's control topic alone.

// reannounceCeilingPolls is the most poll intervals a job its holder has not
// resumed waits between two announcements: twenty, which at
// [DefaultPollInterval] is five minutes.
//
// IN POLLS, NOT A DURATION, for the reason [ConnectGiveUp] gives the other
// way round: the schedule is the poll's, and a waiter driven at a faster
// cadence (the e2e suite's 100 ms) keeps the same shape at its own scale,
// where a fixed five minutes would leave a job its condition released
// unannounced for longer than any of its tests run. At the default cadence a
// condition that clears with nothing to signal it resumes its job within five
// minutes, and a job held for a day is announced a few hundred times rather
// than on every poll. The resume give-up ([ResumeGiveUp]) still sees several
// attempts inside its half hour.
//
// AND INSIDE THE BROKER'S DELIVERY BUDGET, which nothing else keeps it. A
// completion whose claim the holder's own store does not answer is handed back
// NAK'd and nothing renews it ([Coordinator.collectFailedAt]), so its next
// announcement has to be out before its last can be dead-lettered: at the
// default cadence the ceiling, the poll that makes the announcement and that
// poll's own bound come to under seven minutes, and the broker carries a
// failing message at least about eight (internal/queue/jetstream's
// LeastTimeToDeadLetter). TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter
// holds the two apart, and a ceiling raised past it would leave a completion
// whose only copy is its dead letter while a store is silent.
const reannounceCeilingPolls = 20

// announced is one turn's last announced job and its schedule.
type announced struct {
	launch string
	at     time.Time
	times  int
}

// announcing reports whether run's completion is due to be announced now, and
// whether this is its job's first announcement, recording it as made. The
// first is due at once; each one after it waits twice as long as the one
// before, from the waiter's interval to [reannounceCeilingPolls] of them. A new
// job of the same turn starts again.
func (w *Waiter) announcing(run PendingRun) (first, due bool) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	last, seen := w.announced[run.TurnID]
	if !seen || last.launch != run.LaunchID {
		w.announced[run.TurnID] = announced{launch: run.LaunchID, at: now, times: 1}
		return true, true
	}
	ceiling := w.interval * reannounceCeilingPolls
	wait := w.interval << min(last.times-1, 16)
	if wait <= 0 || wait > ceiling {
		wait = ceiling
	}
	if now.Sub(last.at) < wait {
		return false, false
	}
	w.announced[run.TurnID] = announced{launch: run.LaunchID, at: now, times: last.times + 1}
	return false, true
}

// unannounce forgets an announcement that did not go out, so the next poll
// makes it again.
func (w *Waiter) unannounce(turnID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	last, seen := w.announced[turnID]
	if !seen {
		return
	}
	if last.times <= 1 {
		delete(w.announced, turnID)
		return
	}
	last.times--
	last.at = time.Time{}
	w.announced[turnID] = last
}
