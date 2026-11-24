package fleet

import (
	"cmp"
	"slices"
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
	homes  map[Job]Home
	served map[string]time.Time
	// running is the work in flight on each member, by job.
	running map[Running]int
	// changed is told when work in flight begins or ends, nil for nobody.
	changed func()
}

// Running is one job's work in flight on a member since an instant: what a
// member serves now. Pinned work names no fleet.
type Running struct {
	Job
	Member string
	Since  time.Time
}

// Home is the member that took a seat job's last piece of work, and when.
type Home struct {
	Job
	Member string
	At     time.Time
}

// NewLedger makes an empty ledger on clock, time.Now where nil.
func NewLedger(clock func() time.Time) *Ledger {
	if clock == nil {
		clock = time.Now
	}
	return &Ledger{now: clock, homes: map[Job]Home{}, served: map[string]time.Time{},
		running: map[Running]int{}}
}

// OnRunning makes the ledger tell changed whenever work in flight begins or
// ends; set it before work runs.
func (l *Ledger) OnRunning(changed func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.changed = changed
}

// Begin records job's work running on member from now until end is called:
// the runtime's own record of what each member serves now.
func (l *Ledger) Begin(job Job, member string) (end func()) {
	at := l.now()
	l.mu.Lock()
	key, found := Running{Job: job, Member: member}, false
	for held := range l.running {
		if held.Job == job && held.Member == member {
			key, found = held, true
			break
		}
	}
	if !found {
		key.Since = at
	}
	l.running[key]++
	changed := l.changed
	l.mu.Unlock()
	if changed != nil {
		changed()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.running[key]--; l.running[key] <= 0 {
				delete(l.running, key)
			}
			changed := l.changed
			l.mu.Unlock()
			if changed != nil {
				changed()
			}
		})
	}
}

// Running is every job's work in flight, the longest running first.
func (l *Ledger) Running() []Running {
	l.mu.Lock()
	out := make([]Running, 0, len(l.running))
	for r := range l.running {
		out = append(out, r)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b Running) int {
		return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.Member, b.Member), cmp.Compare(a.Fleet, b.Fleet),
			cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Job.Job, b.Job.Job))
	})
	return out
}

// Now is the ledger's clock.
func (l *Ledger) Now() time.Time { return l.now() }

// Home is the member that took job's last piece of work, "" for none.
func (l *Ledger) Home(job Job) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.homes[job].Member
}

// Homes is every seat job's home, the most recently taken first, for the
// node's status record.
func (l *Ledger) Homes() []Home {
	l.mu.Lock()
	out := make([]Home, 0, len(l.homes))
	for _, h := range l.homes {
		out = append(out, h)
	}
	l.mu.Unlock()
	slices.SortFunc(out, func(a, b Home) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.Fleet, b.Fleet),
			cmp.Compare(a.Subject, b.Subject), cmp.Compare(a.Job.Job, b.Job.Job))
	})
	return out
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
	l.homes[job] = Home{Job: job, Member: member, At: at}
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
