package projection

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// follow opens the live watch from the cursor and applies what arrives, until
// the context ends or the watch closes.
//
// TWO GOROUTINES, and the split is not stylistic. The watch delivers on the
// read loop of this node's single NATS connection, so a consumer that blocks
// stops that loop and with it every seat's mailbox and every coordination
// read in the process. A store transaction blocks for milliseconds at a time.
// So the drain takes changes off the watch and touches nothing else, and the
// writer does the writing — see [buffer] for why a full buffer drops rather
// than waits.
func (p *Projector) follow(ctx context.Context) error {
	from := p.Cursor()
	watcher, err := p.docs.WatchDocuments(ctx, p.family, from)
	if err != nil {
		return fmt.Errorf("projection: follow %s from %d: %w", p.family, from, err)
	}
	p.mu.Lock()
	p.watch = watcher
	p.mu.Unlock()
	defer func() { _ = watcher.Stop() }()

	drained := make(chan error, 1)
	go func() { drained <- p.drain(ctx, watcher) }()

	writerDone := make(chan error, 1)
	go func() { writerDone <- p.write(ctx) }()

	select {
	case <-ctx.Done():
		p.buf.Close()
		<-writerDone
		return nil
	case err := <-drained:
		// The watch ended. Close the buffer so the writer finishes what is
		// already in it — those changes are applied and their revisions
		// are real, and throwing them away would only make the next
		// reconcile re-fetch them.
		p.buf.Close()
		if werr := <-writerDone; werr != nil {
			return werr
		}
		return err
	case err := <-writerDone:
		// The writer failed, which means the store is refusing writes. The
		// watch is stopped by the deferred Stop and the cycle restarts
		// through the reconcile.
		return err
	}
}

// drain moves changes from the watch into the buffer, touching nothing else.
//
// It returns when the watch closes or the context ends. A DROP DOES NOT STOP
// IT: the projector rebuilds on the next cycle, and stopping here would leave
// the watch un-consumed while that happened, which is the NATS read-loop stall
// this split exists to prevent.
func (p *Projector) drain(ctx context.Context, watcher coord.Watcher) error {
	changes := watcher.Changes()
	for {
		select {
		case <-ctx.Done():
			return nil
		case change, open := <-changes:
			if !open {
				return nil
			}
			if change == nil {
				// The caught-up marker. On a resumed watch it means the
				// replay is done and what follows is live; nothing here
				// branches on it, because the reconcile already
				// established hydration and a marker cannot.
				continue
			}
			if !p.buf.Push(change) {
				log.WarnContext(ctx, "projection_buffer_full",
					"family", string(p.family), "key", change.Key,
					"revision", change.Revision,
					"detail", "the writer is behind and the change was dropped; "+
						"this projection rebuilds rather than serving rows it "+
						"cannot account for")
			}
		}
	}
}

// write applies buffered changes in batches, advancing the cursor after each
// commit.
//
// It returns nil when the buffer closes, which is the ordinary end of a
// follow: the caller has already decided why.
func (p *Projector) write(ctx context.Context) error {
	for {
		batch := p.buf.Take(applyBatch)
		if batch == nil {
			return nil
		}
		// A partial batch waits briefly for company, and only a partial
		// one: a busy fleet fills the batch and never pays the linger.
		if len(batch) < applyBatch {
			batch = p.lingerFor(ctx, batch)
		}
		if err := p.applyBatch(ctx, batch); err != nil {
			return err
		}
		if err := p.setCursor(ctx, highest(batch), true); err != nil {
			return err
		}
		p.advance(highest(batch), time.Now().UTC())
	}
}

// lingerFor tops a partial batch up for at most [applyLinger].
func (p *Projector) lingerFor(ctx context.Context, batch []*coord.Change) []*coord.Change {
	deadline := time.NewTimer(applyLinger)
	defer deadline.Stop()
	for len(batch) < applyBatch {
		if more := p.buf.TryTake(applyBatch - len(batch)); len(more) > 0 {
			batch = append(batch, more...)
			continue
		}
		select {
		case <-ctx.Done():
			return batch
		case <-deadline.C:
			return batch
		case <-time.After(lingerPoll):
		}
	}
	return batch
}

// lingerPoll is how often the linger checks for more.
//
// Five milliseconds: fine enough that the linger fills a batch from a burst
// rather than waiting out its whole window, coarse enough that an idle
// projector wakes fifty times a second rather than thousands.
const lingerPoll = 5 * time.Millisecond

// highest is the newest revision in a batch.
//
// A MAXIMUM RATHER THAN THE LAST ENTRY'S. Coordination delivers a family's
// changes in revision order today, and a cursor that assumed it would move
// BACKWARDS on the day that stopped being true — silently, re-applying a
// prefix of the stream on every restart.
func highest(batch []*coord.Change) uint64 {
	var out uint64
	for _, c := range batch {
		if c != nil && c.Revision > out {
			out = c.Revision
		}
	}
	return out
}
