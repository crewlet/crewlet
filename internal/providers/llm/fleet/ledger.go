package fleet

import (
	"sync"
	"time"
)

// Job is one seat job's work on one fleet: the key its home is kept under.
type Job struct {
	// Fleet is the fleet's key.
	Fleet string
	// Subject is the seat's id, or a helper's name.
	Subject string
	// Job is the phase (or helper) the work is for.
	Job string
}

// Ledger is the balancer's memory on this node: each seat job's home and
// when each member last took work. Created once by the engine and handed to
// every registry, so an apply keeps it; lost on restart (see the package doc).
type Ledger struct {
	mu     sync.Mutex
	homes  map[Job]string
	served map[string]time.Time
}

// NewLedger makes an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{homes: map[Job]string{}, served: map[string]time.Time{}}
}

// Home is the member that took job's last piece of work, "" for none.
func (l *Ledger) Home(job Job) string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.homes[job]
}

// LastServed is when member last took a piece of work, zero for never.
func (l *Ledger) LastServed(member string) time.Time {
	if l == nil {
		return time.Time{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.served[member]
}

// Serve records that member takes job's piece of work at `at`: its new home,
// and the instant ties are broken by.
func (l *Ledger) Serve(job Job, member string, at time.Time) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.homes[job] = member
	if at.After(l.served[member]) {
		l.served[member] = at
	}
}
