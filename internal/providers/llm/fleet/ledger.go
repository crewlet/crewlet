package fleet

import (
	"sync"
	"time"
)

// Job is one seat job's work on one fleet: the key its home is kept under.
type Job struct {
	// Fleet is the fleet's key.
	Fleet string
	// Subject is the seat's handle, or a helper's name.
	Subject string
	// Job is the phase (or helper) the work is for.
	Job string
}

// Ledger is the balancer's memory on this node: each seat job's home and
// when each member last took work. Created once by the engine and handed to
// every registry, so an apply keeps it; lost on restart (see the package doc).
type Ledger struct {
	now    func() time.Time
	mu     sync.Mutex
	homes  map[Job]string
	served map[string]time.Time
}

// NewLedger makes an empty ledger on clock, time.Now where nil.
func NewLedger(clock func() time.Time) *Ledger {
	if clock == nil {
		clock = time.Now
	}
	return &Ledger{now: clock, homes: map[Job]string{}, served: map[string]time.Time{}}
}

// Now is the ledger's clock.
func (l *Ledger) Now() time.Time { return l.now() }

// Home is the member that took job's last piece of work, "" for none.
func (l *Ledger) Home(job Job) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.homes[job]
}

// LastServed is when member last took a piece of work, zero for never.
func (l *Ledger) LastServed(member string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.served[member]
}

// serve records that member takes job's piece of work now: its new home,
// and the instant ties are broken by, which never moves back.
func (l *Ledger) serve(job Job, member string) {
	at := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.homes[job] = member
	if at.After(l.served[member]) {
		l.served[member] = at
	}
}

// Pick is one step of a piece of work's walk on a fleet: the member, why it
// is there (the decision record), and the seat job's home before it.
type Pick struct {
	Choice
	Fleet    string
	Strategy Strategy
	// Home is the member that took the seat job's last piece of work, ""
	// for none, and HomeWhy why it does not take this one, where it does not.
	Home, HomeWhy string

	job    Job
	ledger *Ledger
}

// Take records that the member takes the work, before it runs: the seat
// job's new home, and its serve for the next tie.
func (p *Pick) Take() { p.ledger.serve(p.job, p.Key) }

// Moved reports whether the work goes to a member other than the home.
func (p *Pick) Moved() bool { return p.Home != "" && p.Home != p.Key }

// Share is the member's share at the pick, nil where unknown.
func (p *Pick) Share() *int {
	if !p.Known {
		return nil
	}
	s := p.Member.Share
	return &s
}

// Walk is f's walk for job ([Fleet.Walk]) from job's home, each step a Pick
// whose Take records its serve here. Empty for a fleet with no members.
func (l *Ledger) Walk(f Fleet, job Job) []*Pick {
	home := l.Home(job)
	why := ""
	if i := indexOf(f.Members, home); i >= 0 {
		why = f.Members[i].Why
		if why == "" {
			why = "Balanced to the least-used member"
		}
	}
	walk := f.Walk(home)
	out := make([]*Pick, len(walk))
	for i, c := range walk {
		out[i] = &Pick{Choice: c, Fleet: f.Key, Strategy: f.Strategy, Home: home, HomeWhy: why,
			job: job, ledger: l}
	}
	return out
}

// indexOf is the index of the member keyed key, -1 for none.
func indexOf(members []Member, key string) int {
	for i, m := range members {
		if m.Key == key {
			return i
		}
	}
	return -1
}
