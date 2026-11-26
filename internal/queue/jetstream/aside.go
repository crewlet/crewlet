package jetstream

import (
	"cmp"
	"context"
	"slices"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
)

// An aside delivery ([queue.OutcomeAside]) is the message itself, kept by the
// attachment that fetched it: unacknowledged, its ack window restarted by an
// in-progress acknowledgement every third of it ([attachment.keepHeld]), its
// delivery count untouched. Readmit moves it to the attachment's readmitted
// list, which the next drain takes first. Nothing is copied or republished, so
// the broker's own state is the record: a node that dies holding it leaves it
// to the ack window, and a release or a stop hands it back at one delivery as
// fetched-and-undispatched mail is ([attachment.handBackAside]).

// asideDelivery is one delivery an attachment holds aside, under the key a
// Readmit names it by.
type asideDelivery struct {
	delivery
	key string
}

// beginDispatch marks the start of one handler call: a Readmit from here on
// readmits what the call sets aside ([attachment.setAside]). An attachment
// dispatches one call at a time, so the keys readmitted before decide nothing.
func (a *attachment) beginDispatch() {
	a.asideMu.Lock()
	defer a.asideMu.Unlock()
	a.readmits = nil
}

// asideRoom is how many more deliveries a takes aside ([queue.MaxAside]).
func (a *attachment) asideRoom() int {
	a.asideMu.Lock()
	defer a.asideMu.Unlock()
	return queue.MaxAside - len(a.aside)
}

// Why an aside is handed back rather than kept, as its line says should the
// hand-back spend the delivery's budget.
const (
	asideNoKey = "set aside under no key"
	asidePast  = "set aside past the attachment's bound"
)

// setAside keeps items aside under key ([queue.OutcomeAside]). A Readmit of
// key since their handler began readmits them at once; a partition naming no
// key, or one that would take a past [queue.MaxAside], is handed back. Called
// inside the handler's in-flight span, as every outcome is applied.
func (a *attachment) setAside(ctx context.Context, items []delivery, key string) {
	a.asideMu.Lock()
	if key == "" || len(a.aside)+len(items) > queue.MaxAside {
		a.asideMu.Unlock()
		reason := asidePast
		if key == "" {
			reason = asideNoKey
		}
		a.nakAll(ctx, items, reason)
		return
	}
	if _, again := a.readmits[key]; again {
		a.readmitted = bySeq(a.readmitted, items)
		a.asideMu.Unlock()
		return
	}
	for _, d := range items {
		a.aside = append(a.aside, asideDelivery{delivery: d, key: key})
	}
	a.asideMu.Unlock()
}

// bySeq is held with more added, every one marked readmitted, in the order the
// stream holds them: the order a drain takes them in.
func bySeq(held, more []delivery) []delivery {
	out := slices.Clone(held)
	for _, d := range more {
		d.readmitted = true
		out = append(out, d)
	}
	slices.SortStableFunc(out, func(x, y delivery) int { return cmp.Compare(x.seq, y.seq) })
	return out
}

// readmit moves every delivery a holds aside under key to its readmitted list
// and remembers key for the dispatch running, answering how many moved.
func (a *attachment) readmit(key string) int {
	a.asideMu.Lock()
	defer a.asideMu.Unlock()
	if a.readmits == nil {
		a.readmits = map[string]struct{}{}
	}
	a.readmits[key] = struct{}{}
	var back []delivery
	kept := a.aside[:0]
	for _, d := range a.aside {
		if d.key == key {
			back = append(back, d.delivery)
			continue
		}
		kept = append(kept, d)
	}
	a.aside = kept
	if len(back) > 0 {
		a.readmitted = bySeq(a.readmitted, back)
	}
	return len(back)
}

// Readmit hands back every delivery this client's attachments of the pair
// hold aside under key, at the head of each one's next drain
// ([queue.EventQueue.Readmit]). The loop takes them at its next cycle, at most
// one poll away ([Queue.fetchWait]), as the lane's mail is.
func (q *Queue) Readmit(_ context.Context, topic, group, key string) (int, error) {
	if q.isClosed() {
		return 0, ErrClosed
	}
	n := 0
	for _, a := range q.lookup(topic, group) {
		n += a.readmit(key)
	}
	return n, nil
}

// takeReadmitted takes up to n readmitted deliveries, oldest first.
func (a *attachment) takeReadmitted(n int) []delivery {
	a.asideMu.Lock()
	defer a.asideMu.Unlock()
	n = min(n, len(a.readmitted))
	if n <= 0 {
		return nil
	}
	out := slices.Clone(a.readmitted[:n])
	a.readmitted = slices.Clone(a.readmitted[n:])
	return out
}

// readmittedOne hands the oldest readmitted delivery's message, if any, to
// take on the calling goroutine, as a fetch hands what it takes.
func (a *attachment) readmittedOne(take func(jetstream.Msg)) {
	for _, d := range a.takeReadmitted(1) {
		take(d.msg)
	}
}

// setBack returns what a cycle took and will not dispatch, met by a hold, a
// pause or a quiesce: readmitted deliveries go back to the readmitted list,
// spending nothing, and fetched ones are handed back for reason
// ([attachment.handBackUndispatched]).
func (a *attachment) setBack(ctx context.Context, items []delivery, reason string) {
	var fetched, readmitted []delivery
	for _, d := range items {
		if d.readmitted {
			readmitted = append(readmitted, d)
			continue
		}
		fetched = append(fetched, d)
	}
	if len(readmitted) > 0 {
		a.asideMu.Lock()
		a.readmitted = bySeq(a.readmitted, readmitted)
		a.asideMu.Unlock()
	}
	if len(fetched) > 0 {
		a.handBackUndispatched(ctx, fetched, reason)
	}
}

// handBackAside hands back everything a holds aside or readmitted once its
// consume loop has ended, a release or a stop: at one delivery each, as
// fetched-and-undispatched mail is, so the successor has it now rather than
// after the ack window.
func (a *attachment) handBackAside(ctx context.Context) {
	a.asideMu.Lock()
	held := a.readmitted
	for _, d := range a.aside {
		held = append(held, d.delivery)
	}
	a.aside, a.readmitted, a.readmits = nil, nil, nil
	a.asideMu.Unlock()
	if len(held) > 0 {
		a.handBackUndispatched(ctx, held, a.blockedBy())
	}
}

// heldMsgs is every message a holds aside or readmitted.
func (a *attachment) heldMsgs() []jetstream.Msg {
	a.asideMu.Lock()
	defer a.asideMu.Unlock()
	out := make([]jetstream.Msg, 0, len(a.aside)+len(a.readmitted))
	for _, d := range a.aside {
		out = append(out, d.msg)
	}
	for _, d := range a.readmitted {
		out = append(out, d.msg)
	}
	return out
}

// asideHeld is what this client holds of a pair's mail aside, for a backlog
// read ([Queue.Backlog]): the ids, which the read never returns, and per
// mailbox how many of the consumer's unacknowledged messages they are, -1 when
// the client holds others in hand (a drain collecting, a handler running, a
// readmitted delivery waiting for one), which a read from the floor still
// returns: a reader settling on an empty mailbox waits for those.
type asideHeld struct {
	ids              map[uuid.UUID]struct{}
	lane, background int
}

// heldOf is [asideHeld] for the pair key, over this client's attachments.
func (q *Queue) heldOf(key attachKey) asideHeld {
	held := asideHeld{ids: map[uuid.UUID]struct{}{}}
	for _, a := range q.lookup(key.topic, key.group) {
		var lane, background int
		a.asideMu.Lock()
		if a.windowed.Load() > 0 || len(a.readmitted) > 0 || a.running.priority.Load() > 0 ||
			a.running.background.Load() > 0 {
			held.lane, held.background = -1, -1
		}
		count := func(d delivery) {
			if a.lane != nil && queue.Prioritized(d.ev) {
				lane++
			} else {
				background++
			}
		}
		for _, d := range a.aside {
			held.ids[d.ev.ID] = struct{}{}
			count(d.delivery)
		}
		a.asideMu.Unlock()
		if held.lane >= 0 {
			held.lane += lane
			held.background += background
		}
	}
	return held
}
