package executor

import (
	"crypto/sha256"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT CLAUDE CODE ESTIMATES A SESSION'S NEXT REQUEST AT, read off the
// session's own main thread ([nextReading], [sandbox.NextRequest]): the last
// counted reply at its input, cache and output ([countedReplyOf]), and the
// harness's own estimate of every main-thread record after it, restarted at a
// main compact boundary from the stretch the compaction kept. The floor reads
// it rather than the last request's input, which lacks the reply and what
// came after it, and past a compaction is the compacted-away conversation's;
// where it makes no figure, the floor reads that input. The package doc ("What
// Claude Code estimates a session's next request at") has the rules, the
// cases with no figure, and the bound on what it keeps.

// nextReading is the estimate of a session's next request as a reading of the
// one walk of its main thread ([threadReading]): handed every whole line in
// file order, it keeps the harness's anchor and its estimate of what lies after
// its place, and answers the figure ([nextReading.next]).
type nextReading struct {
	// anchor is the anchor's four counts; run the estimate since the first
	// record of the newest run of one message id (runID, that id's digest),
	// outside that record's own; tail the estimate of everything after the
	// anchor's place. kept is, past a boundary with no anchor after it yet,
	// the estimate of the stretch that boundary kept. runVoid and tailVoid
	// are a record whose estimate does not read, carried as run and tail are.
	anchor            int64
	run, tail, kept   threadEstimate
	runID             [sha256.Size]byte
	runVoid, tailVoid bool
	// keptVoid is a boundary whose kept stretch could not be charged, until a
	// counted reply follows it; threadVoid a reply whose usage does not read
	// since the last boundary.
	keptVoid, threadVoid bool
	// chained is whether any chained line was read; unread a thread not read
	// whole.
	chained, unread bool

	// THE RECORDS SINCE THE LAST BOUNDARY, from which the stretch the next
	// boundary kept is charged: sum is the estimate of every chained
	// main-thread record since it, in file order; ring each record with a
	// canonical uuid, its uuid and sum through it, the oldest overwritten
	// past ringMax entries (start the oldest held), evicted the sum through
	// the last entry overwritten; ringVoid a record since the boundary whose
	// estimate does not read.
	sum      threadEstimate
	ring     []nextEntry
	start    int
	evicted  threadEstimate
	ringVoid bool
	// ringMax is the most entries the ring holds, [threadChainMaxRecords]
	// where it is zero: the most records a thread's chains file, past which no
	// boundary's kept stretch is one the resume relinks.
	ringMax int
}

// nextEntry is one record of the ring ([nextReading.ring]): its uuid's 16
// bytes and the running estimate through it.
type nextEntry struct {
	uuid [16]byte
	sum  threadEstimate
}

var (
	_ threadReading = (*nextReading)(nil)
	_ lineAsker     = (*nextReading)(nil)
)

// ask asks reader for every line's estimate ([threadReader.Estimating]), which
// the figure reads: a line read unasked answers no estimate, so a walk that
// forgot to ask makes no figure rather than an estimate of nothing.
func (r *nextReading) ask(reader *threadReader) { reader.Estimating() }

// line reads one whole line.
func (r *nextReading) line(l threadLine) {
	if r.unread || !l.Head.Chained() || !l.Head.Main() {
		return
	}
	r.chained = true
	if l.Head.Type == recordSystem && l.Head.Subtype == subtypeCompactBoundary {
		// A MAIN COMPACT BOUNDARY: no anchor before it counts, and what the
		// compaction kept before it is relinked after its summary.
		kept, ok := r.preserved(l)
		*r = nextReading{chained: true, ring: r.ring[:0], ringMax: r.ringMax, kept: kept, keptVoid: !ok}
		return
	}
	charge, ok := nextCharge(l)
	r.record(l, charge, ok)
	if l.Head.Type != recordAssistant {
		r.after(charge, ok)
		return
	}
	reply, err := countedReplyOf(l)
	switch {
	case err != nil:
		r.threadVoid = true
		r.after(charge, ok)
		return
	case reply.id == "":
		r.after(charge, ok)
		return
	}
	if digest := sha256.Sum256([]byte(reply.id)); digest != r.runID {
		r.runID, r.run, r.runVoid = digest, threadEstimate{}, false
	} else {
		r.run, r.runVoid = r.run.plus(charge), r.runVoid || !ok
	}
	if reply.anchors {
		r.anchor = reply.tokens
		r.tail, r.tailVoid = r.run, r.runVoid
		r.kept, r.keptVoid = threadEstimate{}, false
		return
	}
	r.tail, r.tailVoid = r.tail.plus(charge), r.tailVoid || !ok
}

// after charges a chained line that is no record of a counted reply's message
// id to both the tail and the newest run; ok false where its estimate does not
// read.
func (r *nextReading) after(charge threadEstimate, ok bool) {
	r.tail, r.tailVoid = r.tail.plus(charge), r.tailVoid || !ok
	r.run, r.runVoid = r.run.plus(charge), r.runVoid || !ok
}

// record adds l's charge to the running estimate since the last boundary, and
// l to the ring where its uuid is a canonical one, the only kind a
// boundary's list names a record by ([threadPreserved.UUIDs]).
func (r *nextReading) record(l threadLine, charge threadEstimate, ok bool) {
	r.sum = r.sum.plus(charge)
	r.ringVoid = r.ringVoid || !ok
	id, err := l.UUID()
	if err != nil {
		return
	}
	uuid, canonical := uuidBytes(id)
	if !canonical {
		return
	}
	entry := nextEntry{uuid: uuid, sum: r.sum}
	if len(r.ring) < r.capacity() {
		r.ring = append(r.ring, entry)
		return
	}
	r.evicted = r.ring[r.start].sum
	r.ring[r.start] = entry
	r.start = (r.start + 1) % len(r.ring)
}

// capacity is the most entries the ring holds.
func (r *nextReading) capacity() int {
	if r.ringMax > 0 {
		return r.ringMax
	}
	return threadChainMaxRecords
}

// nextCharge is the harness's estimate of l, a chained main-thread record
// after the anchor ([nextReading]'s rules), and false where it does not read:
// a line read unasked ([errNotEstimated]), a content the harness's own
// estimate throws on or nested past [estimateNestMax], and a PDF read whose
// pages do not read, which no bytes of its line bound.
func nextCharge(l threadLine) (threadEstimate, bool) {
	switch l.Head.Type {
	case recordUser, recordAssistant:
		e, err := l.Estimate()
		return e, err == nil
	case recordAttachment:
		e, rendered, err := l.AttachmentEstimate()
		switch {
		case err != nil:
			return threadEstimate{}, false
		case rendered:
			return e, true
		}
		if typ, _ := l.AttachmentType(); typ == attachmentPDFReads {
			return threadEstimate{}, false
		}
		return estimateFlat(sandbox.ClaudeCharge(l.Size)), true
	}
	return threadEstimate{}, true
}

// preserved is the estimate of the stretch the compaction l's boundary kept
// before it, which the harness relinks after its summary
// ([threadLine.Preserved]): nothing for a boundary that kept nothing; the
// running estimate through its newest record less the running estimate before
// its oldest, for a list its first and last uuids and for a segment its head
// and tail ([nextReading.stretch]); and false where that cannot be read: a
// metadata the layer refuses, a list naming a record no chain carries or past
// what the layer holds, an end no record of the ring holds, or a record since
// the last boundary whose estimate does not read.
func (r *nextReading) preserved(l threadLine) (threadEstimate, bool) {
	p, err := l.Preserved()
	switch {
	case err != nil:
		return threadEstimate{}, false
	case !p.Kept:
		return threadEstimate{}, true
	case r.ringVoid:
		return threadEstimate{}, false
	}
	if p.Segment {
		head, okHead := uuidBytes(p.Head)
		tail, okTail := uuidBytes(p.Tail)
		if !okHead || !okTail {
			return threadEstimate{}, false
		}
		return r.stretch(head, tail)
	}
	if p.More || p.Nowhere || len(p.UUIDs) == 0 {
		return threadEstimate{}, false
	}
	return r.stretch(p.UUIDs[0], p.UUIDs[len(p.UUIDs)-1])
}

// stretch is the estimate of the records of the ring from the first that is
// either end to the last that is either, every record between them charged,
// the safe side for a floor where the stretch's records interleave others in
// the file; false where either end is in no entry, its own or one the ring
// overwrote.
func (r *nextReading) stretch(head, tail [16]byte) (threadEstimate, bool) {
	first, last := -1, -1
	var sawHead, sawTail bool
	for i := range r.ring {
		e := r.ring[(r.start+i)%len(r.ring)]
		isHead, isTail := e.uuid == head, e.uuid == tail
		if !isHead && !isTail {
			continue
		}
		sawHead, sawTail = sawHead || isHead, sawTail || isTail
		if first < 0 {
			first = i
		}
		last = i
	}
	if !sawHead || !sawTail {
		return threadEstimate{}, false
	}
	before := r.evicted
	if first > 0 {
		before = r.ring[(r.start+first-1)%len(r.ring)].sum
	}
	through := r.ring[(r.start+last)%len(r.ring)].sum
	return threadEstimate{At3: through.At3 - before.At3, At4: through.At4 - before.At4}, true
}

// refused is a whole line that is no record: the thread is not read whole.
func (r *nextReading) refused(*threadLineError) { r.unread = true }

// end is how the walk ended: an error leaves the thread unread.
func (r *nextReading) end(_ bool, err error) {
	if err != nil {
		r.unread = true
	}
}

// next is the figure the reading holds, zero where it holds none (the file
// comment).
func (r *nextReading) next() sandbox.NextRequest {
	if r.unread || !r.chained || r.threadVoid || r.tailVoid || r.keptVoid {
		return sandbox.NextRequest{}
	}
	figure := estimateFlat(r.anchor).plus(r.kept).plus(r.tail)
	return sandbox.NextRequest{Tokens: figure.At3, OlderTokens: figure.At4}
}
