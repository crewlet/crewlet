package executor

import (
	"bufio"
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// countReadBytes is the most one read of a live count takes of its stream
// ([counter.Read]): [liveBacklog], the most a display read catches up. A
// stream grows by tens of KiB an event, so a tick's growth at the fastest
// measured pace is a few MiB at most and a running count keeps up; a backlog
// (a node restarted, a run first watched late) is taken 8 MiB a tick, never
// skipped, and no read holds a slot for longer than one that size takes.
const countReadBytes = liveBacklog

// Count implements [sandbox.Runner]: a reading of the job's event stream from
// its first byte, through a decoder of the harness's own kept for the reading's
// life and the line handling [Runner.Collect] reads with, so its count is the
// collection's of the same bytes; or, for a harness whose stream shows its
// requests only once it exits, of its own record of the session
// ([recordCounted]), of the largest of its records ([recordsCounted]), or of
// its session store ([storeCounted]).
func (r *Runner) Count(sandbox.RunHandle) sandbox.Counter {
	if sc, ok := r.harness.(storeCounted); ok {
		return &storeCount{harness: sc, inFlight: r.harness.Events().InFlight()}
	}
	if rc, ok := r.harness.(recordCounted); ok {
		return r.recordCounter(rc)
	}
	if rs, ok := r.harness.(recordsCounted); ok {
		m := &maxCount{}
		for _, rc := range rs.countedRecords() {
			m.parts = append(m.parts, r.recordCounter(rc))
		}
		return m
	}
	return &counter{harness: r.harness, bound: r.lineBound, readBytes: countReadBytes}
}

// recordCounter is one live count of the job off record rc.
func (r *Runner) recordCounter(rc recordCounted) *recordCount {
	return &recordCount{harness: rc, bound: r.lineBound, readBytes: countReadBytes,
		inFlight: r.harness.Events().InFlight()}
}

// counter is one live count of one job's stream ([Runner.Count]).
type counter struct {
	harness   Harness
	bound     int
	readBytes int64
	// dec reads the lines lines completes, its transcript taken and dropped
	// after every read ([Decoder.Entries]) so a reading kept for hours holds
	// none of it; lines.taken is how far into the stream the reading is.
	dec   Decoder
	lines streamLines
	// most is the most each figure of any reading of the stream came to, so
	// a stream rewritten shorter is read again from its start without the
	// count going down.
	most sandbox.RequestCount
}

// Read implements [sandbox.Counter].
func (c *counter) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	out := c.harness.Output(PathsFor(box))
	if !out.Events {
		return c.most, nil
	}
	if c.dec == nil {
		c.restart()
	}
	body, size, err := box.OpenRange(ctx, out.Stdout, c.lines.taken, c.readBytes)
	if err == nil && size < c.lines.taken {
		// SHRUNK: the stream was written anew, and is read again from its start.
		_ = body.Close()
		c.restart()
		body, _, err = box.OpenRange(ctx, out.Stdout, 0, c.readBytes)
	}
	if err != nil {
		return c.most, fmt.Errorf("executor: opening the event stream to count its requests: %w", err)
	}
	defer func() { _ = body.Close() }()
	// A LINE UNFINISHED AT THE READ'S END WAITS for the next read, held or
	// counted as [Runner.Collect]'s one read would hold it.
	err = c.lines.read(bufio.NewReaderSize(body, lineChunkBytes), false,
		func(line []byte) error {
			c.dec.Line(line)
			return nil
		},
		func(n int64, head, tail []byte) error {
			c.dec.Skipped(LongLine{Size: n, Bound: c.bound, Head: head, Tail: tail})
			return nil
		})
	_ = c.dec.Entries(false)
	c.most = sandbox.RequestCount{
		Requests:  max(c.most.Requests, c.dec.Requests()),
		InFlight:  max(c.most.InFlight, c.dec.InFlight()),
		Undecided: max(c.most.Undecided, c.dec.Undecided()),
	}
	if err != nil {
		return c.most, fmt.Errorf("executor: reading the event stream to count its requests: %w", err)
	}
	return c.most, nil
}

// restart begins the reading at the stream's first byte, with a fresh decoder
// read through Entries from its first line ([Decoder.Entries]).
func (c *counter) restart() {
	c.dec = c.harness.Events()
	_ = c.dec.Entries(false)
	c.lines = streamLines{bound: c.bound, ends: true}
}
