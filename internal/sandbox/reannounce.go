package sandbox

import "time"

// # A job is announced once, and again on a schedule
//
// A job its holder does not resume at once stays running on its row, and each
// poll finds it over again; announcing every time would write an event record
// per poll. As with the provider park notices (parknotice.go in the engine), a
// notice goes out once per change, here a new job ([PendingRun.LaunchID]): the
// events copy of a completion is published once per job. The seat still hears
// of a job it did not resume, since what made it wait may clear silently: the
// holder's own conditions re-fire it ([Coordinator.Readmit]), and the repeat
// is the backstop, on a doubling schedule from one poll interval to
// [reannounceCeilingPolls], to the seat's control topic alone.

// reannounceCeilingPolls is the most poll intervals a job its holder has not
// resumed waits between two announcements: twenty, five minutes at
// [DefaultPollInterval]. In polls, so a faster waiter keeps the same shape at
// its own scale. A condition that clears unsignalled resumes its job within
// five minutes, and [ResumeGiveUp] still sees several attempts. And inside the
// broker's delivery budget (internal/queue/jetstream's LeastTimeToDeadLetter),
// which TestAJobsNextAnnouncementIsOutBeforeItsLastCanDeadLetter holds, so a
// store's silence never leaves a completion whose only copy is its dead letter.
// See the package doc, "Collection failures".
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
