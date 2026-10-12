package code

import "time"

// SetPageLimits has r end every page at rows rows or bytes of encoded rows —
// the cases that read a snapshot across several pages without seeding tens of
// thousands of rows. The shipped limits are MaxSnapshotItems and MaxPageBytes.
func SetPageLimits(r *Reader, rows, bytes int) { r.pageRows, r.pageBytes = rows, bytes }

// SetCommitterIdle has c's repository goroutines exit after d with nothing
// queued, rather than CommitterIdle — set before c's first proposal.
func SetCommitterIdle(c *Committer, d time.Duration) { c.idle = d }

// SetCommitterClock has c measure how long a push has waited in its queue by
// clock rather than the wall clock — set before c's first proposal.
func SetCommitterClock(c *Committer, clock func() time.Time) { c.clock = clock }

// CommitterRepos is how many repositories c holds a goroutine for.
func CommitterRepos(c *Committer) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.repos)
}

// CommitterStopping reports a committer Stop has been called on.
func CommitterStopping(c *Committer) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopped
}

// CommitterQueued is how many proposals wait in repoID's queue on c.
func CommitterQueued(c *Committer, repoID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if rc := c.repos[repoID]; rc != nil {
		return len(rc.inbox)
	}
	return 0
}

// The committer's batching bounds, for the limits test.
const (
	RecordOverheadBytes = recordOverheadBytes
	CommandResultBound  = commandResultBound
)

// EntryBound is the most bytes p's entry takes in a record.
func EntryBound(p Proposal) (int, error) { return entryBound(p) }

// RowBound is the most rows p's entry makes its record write.
func RowBound(p Proposal) int { return rowBound(p) }

// RecordRows is the row bound of a record whose entries bound rows rows.
func RecordRows(rows int) int { return recordRows(rows) }

// EntryOf is the record entry p becomes with its commands as decided.
func EntryOf(p Proposal) PushEntry { return p.entryWith(p.Commands) }
