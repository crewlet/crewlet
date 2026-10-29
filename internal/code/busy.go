package code

import (
	"cmp"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BUSY ALARM'S READING: what this node's committer turned away for a
// backlog, over the window code_commit_busy stands for.
//
// # Four reasons, two alarmed
//
// A proposal refused busy is decided by nobody and sent again by whoever made
// it, and it is refused for one of four reasons ([busyReason]) that point at
// different remedies, so the busy counter (crewlet.code.commit.busy) carries
// the reason beside the proposal's kind. Only a BACKLOG — a repository's queue
// full ([busyQueue]) or the node holding its whole bound across its
// repositories ([busyNode]) — says THIS committer turns proposals away, so only
// those two raise the alarm. A LATE proposal ([busyLate]) is a symptom whose
// cause is upstream of the committer: the asker's attempt was spent before a
// batch could answer it — by the receiving node, the router's walk or the
// serving node's floor wait, each with an instrument of its own. A CONTENDED
// one ([busyContended]) lost every compare-and-set round, which is a second
// committer deciding the repository during a handoff. Both are counted and
// logged, never alarmed.
//
// # A bucket a minute, expired by its minute
//
// The window is fifteen one-minute buckets of per-repository counts, a ring
// whose slots are expired by comparing their minute rather than by a sweep —
// the recorder's rolling window's rule (statelog/metrics' Window) — so nothing
// runs on a timer to keep it right, and it holds an entry for each repository
// refused in the window and nothing for one that was not. A refusal therefore
// stands between fourteen and fifteen minutes, by the minute it fell in, so the
// alarm's "in the last fifteen minutes" is never said of an older one. The
// window counts every kind a backlog turns away — a compaction, an expunge
// record or a change's write waits in the same queue under the same bounds as
// the pushes around it, and meeting a full queue is the same backlog.
//
// EVERY MINUTE IS ONE A CLOCK CAN READ — the epoch's first, every one before
// it, the zero time's — so no minute marks a slot nothing was filed in: a slot
// is empty while it has no map ([busyMinute.filled]), and the window has no
// newest minute until its first filing or reading ([busyWindow.begun]).
// Marked by minute zero, a refusal stamped in 1970's first minute — a node's
// clock reset to the epoch — or at any instant before it, moved there by a
// newest minute that began at zero, met a slot that seemed to hold its minute
// already, with no map to count it in, and panicked on the goroutine that
// refused it; and that minute was neither counted nor logged.
//
// No repository is an attribute of the counter (a time series a repository is
// how a metrics backend falls over), so which one is hot is the reading's and
// the log's: one code_commit_busy warning a repository a minute, with that
// minute's counts by reason, never one a refusal — written once the minute has
// closed, at the committer's next refusal or the next reading of its window,
// whichever comes first; a data node reads it at every alarm evaluation, every
// statelog.AlarmInterval.

// CommitBusyWindow is how long a backlog's refusal keeps code_commit_busy
// raised: the trim's measurement tick itself, BORROWED (ADR-0015) — the span
// the store's connection-pool waits are measured over for pool_starved's p95,
// so the alarm and the figures a person reads beside it describe one stretch
// of time. A JUDGMENT CALL (fifteen minutes):
// shorter flaps between the ten-second evaluations of a sustained backlog,
// whose refusals come in bursts as its queue drains and refills; longer
// outlives the person's fix. The trim's constant itself rather than a copy a
// test holds equal: the alarm's detail names the window by the same constant,
// which this package cannot hand to a framework that imports none of it.
const CommitBusyWindow = statelog.TrimInterval

// busyBucket is one bucket's span: a MINUTE, the cadence of the log line a
// repository, and fine enough that the window's edge moves by a minute at most.
const busyBucket = time.Minute

// busyBuckets is how many buckets hold CommitBusyWindow — fifteen at its
// fifteen minutes — rounded up, so a window that is no whole number of minutes
// is still held whole.
const busyBuckets = int((CommitBusyWindow + busyBucket - 1) / busyBucket)

// busyReason is why a proposal was refused busy: the busy counter's `reason`
// attribute, and which of a minute's counts the refusal is filed under
// ([busyCounts]) — two of which raise code_commit_busy ([busyCounts.backlog]).
type busyReason string

const (
	// busyQueue is a repository's queue at CommitterInbox proposals or
	// CommitterInboxBytes of entries: the repository takes proposals faster
	// than one committer commits them.
	busyQueue busyReason = "queue"
	// busyNode is the node holding CommitterNodeBytes of proposals across its
	// repositories: one node committing for too many busy repositories at
	// once.
	busyNode busyReason = "node"
	// busyLate is a proposal whose asker's attempt left no more than a batch
	// and its answer (CommitBudget and ReplyMargin) — as it arrived, while it
	// waited or as its batch formed — or, asked with no deadline, one that
	// waited a whole CommitBudget.
	busyLate busyReason = "late"
	// busyContended is a record that lost every compare-and-set round the
	// publisher may take: the repository kept changing under it.
	busyContended busyReason = "contended"
)

// BusyReading is this committer's BACKLOG refusals over the last
// CommitBusyWindow — the proposals it refused busy because a repository's
// queue or the node's whole bound could not hold them — which the engine
// carries into the alarm reading (statelog's code_commit_busy). A late or a
// contended refusal is no part of it. The zero value is a committer that
// turned nothing away.
type BusyReading struct {
	// Refused is how many proposals were refused for a backlog.
	Refused int
	// Repo is the repository with the most of them, by id — the smaller id
	// of two with as many — and "" where none was refused.
	Repo string
	// OfRepo is how many of them were Repo's.
	OfRepo int
	// Node is how many met the node's whole bound (CommitterNodeBytes)
	// rather than one repository's queue.
	Node int
}

// AlarmReading is this committer's backlog refusals over the CommitBusyWindow
// ending at now ([BusyReading]): what raises code_commit_busy on the data node
// that decides the repository's pushes. It writes the code_commit_busy lines of
// every minute that has closed by now, once each, since a reading is the one
// thing that reaches a committer that has stopped refusing.
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
			"queue", l.counts.queue, "node", l.counts.node, "late", l.counts.late,
			"contended", l.counts.contended)
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
	// newest is the latest minute a refusal was filed in or a reading was
	// taken at, once begun. A refusal stamped before it — an instant read
	// before a reading's, or another refusal's, and filed after it, or a
	// wall clock stepped back — is filed in it instead, so a minute whose
	// lines were written never gains a refusal they did not count.
	newest int64
	// begun reports newest set: before the first filing or reading there is
	// no newest minute, and the first refusal is filed in its own — where a
	// newest that began at zero, the epoch's first minute, moved every
	// earlier refusal into that minute.
	begun bool
}

// busyMinute is one minute's refusals, by repository. The zero value is a
// slot nothing has been filed in.
type busyMinute struct {
	// at is the minute, counted in minutes since the Unix epoch — negative
	// before it — and meaningful only in a filled slot.
	at int64
	// repos is each repository refused in the minute, by id: made when the
	// slot's first refusal is filed, and nil in a slot nothing has been filed
	// in — which is how the two are told apart, since every minute is one a
	// clock can read and none can mark an empty slot.
	repos map[string]*busyCounts
	// said reports the minute's lines written.
	said bool
}

// filled reports a slot a refusal has been filed in.
func (m *busyMinute) filled() bool { return m.repos != nil }

// busyCounts is one repository's refusals in one minute, by reason.
type busyCounts struct{ queue, node, late, contended int }

// add counts n refusals for reason.
func (b *busyCounts) add(reason busyReason, n int) {
	switch reason {
	case busyQueue:
		b.queue += n
	case busyNode:
		b.node += n
	case busyLate:
		b.late += n
	case busyContended:
		b.contended += n
	}
}

// total is every refusal counted, whatever its reason.
func (b busyCounts) total() int { return b.queue + b.node + b.late + b.contended }

// backlog is the refusals of this committer's own backlog — a repository's
// queue full, or the node's whole bound — the two reasons that raise
// code_commit_busy. A late or a contended refusal is no part of it.
func (b busyCounts) backlog() int { return b.queue + b.node }

// busyLine is one code_commit_busy warning: a repository's refusals in one
// closed minute.
type busyLine struct {
	repo   string
	minute time.Time
	counts busyCounts
}

// minuteOf is at's minute, in minutes since the Unix epoch — the wall clock's,
// so a refusal stamped by the committer's clock and a reading taken by another
// read the same minute. FLOORED: Go's division truncates toward zero, which
// would file the half minute before the epoch in the minute after it.
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
		// A MINUTE PAST THE WINDOW is expired; one later than now's is a
		// refusal stamped a moment after the reading's clock was read, and
		// stands.
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
		}
	}
	for repo, n := range of {
		if n > out.OfRepo || (n == out.OfRepo && repo < out.Repo) {
			out.Repo, out.OfRepo = repo, n
		}
	}
	return out, lines
}

// closeBefore marks every minute before minute whose lines are unwritten as
// said, and answers its lines — a repository a minute, in the order of the
// minutes and then of the repositories, so two readers of one log read one
// order.
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
