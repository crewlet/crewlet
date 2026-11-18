package gitlet

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BUSY ALARM'S READING: what this node's committer turned away for a
// backlog over CommitBusyWindow. A busy refusal carries one of five reasons
// ([busyReason]) on crewlet.code.commit.busy, since each points at a
// different remedy, but only the three of THIS committer's backlog raise
// code_commit_busy ([busyCounts.backlog]). [busyWaited] is the bound a slow
// committer meets: one committing a repository slower than it is pushed turns
// the excess away as it runs out of time in the queue, never at
// CommitterInbox's count, so filing it as late would silence the alarm on
// exactly that committer. [busyLate] and [busyContended] are counted and
// logged, never alarmed. Every kind a queue holds (a compaction, an expunge,
// a change's write) counts, since it waits under the same bounds as a push.

// The window is a ring of one-minute buckets expired by comparing each slot's
// minute, not by a sweep (statelog/metrics' Window), so nothing runs on a
// timer and a refusal stands fourteen to fifteen minutes. Every minute,
// before the epoch and the zero time's included, is one a clock can read, so
// none marks an empty slot: a slot is empty while it has no map
// ([busyMinute.filled]) and the window has no newest minute until
// [busyWindow.begun]; a minute-zero sentinel would panic on a refusal stamped
// at or before the epoch's first minute. No repository is a counter attribute
// (unbounded series), so the hot one is the reading's and the log's: one
// code_commit_busy warning a repository a closed minute, written at the next
// refusal or reading, whichever comes first.

// CommitBusyWindow is how long a backlog's refusal keeps code_commit_busy
// raised: statelog.TrimInterval, borrowed (ADR-0015), so the alarm and
// pool_starved's p95 beside it describe one stretch of time. A judgment call
// (fifteen minutes): shorter flaps between the ten-second evaluations of a
// bursty sustained backlog, longer outlives the person's fix. The constant
// itself, not a test-held copy, because the alarm's detail names the window
// by it.
const CommitBusyWindow = statelog.TrimInterval

// busyBucket is one bucket's span: a minute, the log line's cadence, so the
// window's edge moves by a minute at most.
const busyBucket = time.Minute

// busyBuckets is how many buckets hold CommitBusyWindow, rounded up so a
// window of no whole number of minutes is still held whole.
const busyBuckets = int((CommitBusyWindow + busyBucket - 1) / busyBucket)

// busyReason is why a proposal was refused busy: the busy counter's `reason`
// attribute and the minute's count it is filed under ([busyCounts]).
type busyReason string

const (
	// busyQueue is a repository's queue at CommitterInbox proposals or
	// CommitterInboxBytes: the repository takes proposals faster than one
	// committer commits them.
	busyQueue busyReason = "queue"
	// busyNode is the node at CommitterNodeProposals or CommitterNodeBytes
	// across its repositories: too many busy repositories at once.
	busyNode busyReason = "node"
	// busyWaited is a proposal that QUEUED with more than a batch and its
	// answer (CommitBudget and ReplyMargin) of its asker's attempt left and
	// lost it behind the repository's earlier proposals, or, asked with no
	// deadline, waited a whole CommitBudget: the queue's time bound.
	busyWaited busyReason = "waited"
	// busyLate is a proposal that ARRIVED with no more than a batch and its
	// answer left: the attempt was spent upstream of the committer.
	busyLate busyReason = "late"
	// busyContended is a record that lost every compare-and-set round the
	// publisher may take: a second committer during a handoff.
	busyContended busyReason = "contended"
)

// BusyReading is this committer's backlog refusals ([busyCounts.backlog])
// over the last CommitBusyWindow, which the engine carries into statelog's
// code_commit_busy. The zero value is a committer that turned nothing away.
type BusyReading struct {
	// Refused is how many proposals were refused for a backlog.
	Refused int
	// Repo is the repository with the most of them by id (the smaller id on
	// a tie), "" where none was refused.
	Repo string
	// OfRepo is how many of them were Repo's.
	OfRepo int
	// Node is how many met the node's whole bound ([busyNode]).
	Node int
	// Waited is how many met the queue's time bound ([busyWaited]).
	Waited int
}

// AlarmReading is this committer's [BusyReading] over the CommitBusyWindow
// ending at now. It also writes the code_commit_busy lines of every minute
// closed by now, once each, since a reading is the one thing that reaches a
// committer that has stopped refusing.
func (c *Committer) AlarmReading(now time.Time) BusyReading {
	reading, lines := c.busyWindow.reading(now)
	c.sayBusy(lines)
	return reading
}

// sayBusy writes one code_commit_busy warning for each closed minute's
// repository: the minute's refusals by reason.
func (c *Committer) sayBusy(lines []busyLine) {
	for _, l := range lines {
		c.log.Warn("code_commit_busy", "repo", l.repo, "minute", l.minute, "refused", l.counts.total(),
			"queue", l.counts.queue, "node", l.counts.node, "waited", l.counts.waited,
			"late", l.counts.late, "contended", l.counts.contended)
	}
}

// busyWindow is a committer's busy refusals, a bucket a minute over the last
// CommitBusyWindow. The zero value is a window nothing was filed in or read
// from.
type busyWindow struct {
	mu sync.Mutex
	// minutes is a ring: a minute's bucket is the slot at its minute modulo
	// busyBuckets, and a slot holding an older minute is that minute expired.
	minutes [busyBuckets]busyMinute
	// newest is the latest minute filed in or read at, once begun. A refusal
	// stamped before it (read before another's instant and filed after it,
	// or a clock stepped back) is filed in it instead, so a minute whose
	// lines were written never gains a refusal they did not count.
	newest int64
	// begun reports newest set; a newest that began at zero would move every
	// earlier refusal into the epoch's first minute.
	begun bool
}

// busyMinute is one minute's refusals, by repository. The zero value is a
// slot nothing has been filed in.
type busyMinute struct {
	// at is minutes since the Unix epoch (negative before it), meaningful
	// only in a filled slot.
	at int64
	// repos is each repository refused in the minute, by id; nil marks an
	// empty slot, since no minute can.
	repos map[string]*busyCounts
	// said reports the minute's lines written.
	said bool
}

// filled reports a slot a refusal has been filed in.
func (m *busyMinute) filled() bool { return m.repos != nil }

// busyCounts is one repository's refusals in one minute, by reason.
type busyCounts struct{ queue, node, waited, late, contended int }

// add counts n refusals for reason.
func (b *busyCounts) add(reason busyReason, n int) {
	switch reason {
	case busyQueue:
		b.queue += n
	case busyNode:
		b.node += n
	case busyWaited:
		b.waited += n
	case busyLate:
		b.late += n
	case busyContended:
		b.contended += n
	}
}

// total is every refusal counted, whatever its reason.
func (b busyCounts) total() int { return b.queue + b.node + b.waited + b.late + b.contended }

// backlog is the refusals of this committer's own backlog, the three reasons
// that raise code_commit_busy: late and contended are no part of it.
func (b busyCounts) backlog() int { return b.queue + b.node + b.waited }

// busyLine is one code_commit_busy warning: a repository's refusals in one
// closed minute.
type busyLine struct {
	repo   string
	minute time.Time
	counts busyCounts
}

// minuteOf is at's wall-clock minute since the Unix epoch, so the committer's
// clock and a reader's agree. FLOORED: Go's division truncates toward zero,
// which would file the half minute before the epoch in the minute after it.
func minuteOf(at time.Time) int64 {
	const perMinute = int64(busyBucket / time.Second)
	seconds := at.Unix()
	minute := seconds / perMinute
	if seconds%perMinute < 0 {
		minute--
	}
	return minute
}

// slot is minute's slot in the ring.
func (w *busyWindow) slot(minute int64) *busyMinute {
	i := minute % int64(busyBuckets)
	if i < 0 {
		i += int64(busyBuckets)
	}
	return &w.minutes[i]
}

// file counts n refusals of repo for reason at at, and answers the lines of
// every minute that closed before it, for its caller to write outside the
// lock.
func (w *busyWindow) file(at time.Time, repo string, reason busyReason, n int) []busyLine {
	w.mu.Lock()
	defer w.mu.Unlock()
	minute := minuteOf(at)
	if w.begun {
		minute = max(minute, w.newest)
	}
	w.newest, w.begun = minute, true
	lines := w.closeBefore(minute)
	s := w.slot(minute)
	if !s.filled() || s.at != minute {
		*s = busyMinute{at: minute, repos: map[string]*busyCounts{}}
	}
	counts := s.repos[repo]
	if counts == nil {
		counts = &busyCounts{}
		s.repos[repo] = counts
	}
	counts.add(reason, n)
	return lines
}

// reading is the backlog refusals of the window ending at now, and the lines
// of every minute that closed before now's.
func (w *busyWindow) reading(now time.Time) (BusyReading, []busyLine) {
	minute := minuteOf(now)
	w.mu.Lock()
	defer w.mu.Unlock()
	lines := w.closeBefore(minute)
	if !w.begun || minute > w.newest {
		w.newest, w.begun = minute, true
	}
	var out BusyReading
	var of map[string]int
	for i := range w.minutes {
		s := &w.minutes[i]
		// Past the window is expired; later than now's is a refusal stamped
		// just after the reading's clock was read, and stands.
		if !s.filled() || s.at <= minute-int64(busyBuckets) {
			continue
		}
		for repo, counts := range s.repos {
			backlog := counts.backlog()
			if backlog == 0 {
				continue
			}
			if of == nil {
				of = map[string]int{}
			}
			of[repo] += backlog
			out.Refused += backlog
			out.Node += counts.node
			out.Waited += counts.waited
		}
	}
	for repo, n := range of {
		if n > out.OfRepo || (n == out.OfRepo && repo < out.Repo) {
			out.Repo, out.OfRepo = repo, n
		}
	}
	return out, lines
}

// closeBefore marks every unsaid minute before minute said and answers its
// lines, ordered by minute then repository so every reader of the log reads
// one order.
func (w *busyWindow) closeBefore(minute int64) []busyLine {
	var closed []*busyMinute
	for i := range w.minutes {
		if s := &w.minutes[i]; s.filled() && s.at < minute && !s.said {
			closed = append(closed, s)
		}
	}
	slices.SortFunc(closed, func(a, b *busyMinute) int { return cmp.Compare(a.at, b.at) })
	var lines []busyLine
	for _, s := range closed {
		s.said = true
		at := time.Unix(s.at*int64(busyBucket/time.Second), 0).UTC()
		for _, repo := range slices.Sorted(maps.Keys(s.repos)) {
			lines = append(lines, busyLine{repo: repo, minute: at, counts: *s.repos[repo]})
		}
	}
	return lines
}
