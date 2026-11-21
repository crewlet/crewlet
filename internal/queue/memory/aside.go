package memory

import (
	"cmp"
	"context"
	"slices"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// An aside delivery ([queue.OutcomeAside]) leaves the mailbox, as a delivered
// and unacknowledged message leaves a consumer's pending set on the broker,
// and waits on the CONSUMER that set it aside, spending nothing, until a
// Readmit of its key puts it back at the front of the mailbox in the order the
// subscription received it. A detach or a stop of that consumer hands it back
// at one delivery, as the broker's attachment does.

// asideEntry is one delivery a consumer holds aside under the key a Readmit
// names it by, with when its subscription received it.
type asideEntry struct {
	ev      *events.Event
	key     string
	arrival uint64
}

// Why an aside is handed back rather than kept, the words the JetStream
// backend's line says.
const (
	asideNoKey = "set aside under no key"
	asidePast  = "set aside past the attachment's bound"
)

// setAsideLocked keeps evs aside on m under key, or puts them back where a
// Readmit of key since their handler began, or the bound, says: at the front
// of the mailbox, readmitted and unspent; or charged as a hand-back, for a
// partition naming no key, one past [queue.MaxAside], or a consumer detached
// while it ran. It returns what a hand-back retired, to log once unlocked.
func (b *Broker) setAsideLocked(sub *subscription, m *consumer, evs []*events.Event, key string) []deadLetter {
	switch {
	case m.detached:
		return b.handBackLocked(sub, evs, m.client.maxRedeliveries, "detached")
	case key == "":
		return b.handBackLocked(sub, evs, m.client.maxRedeliveries, asideNoKey)
	case len(m.aside)+len(evs) > queue.MaxAside:
		return b.handBackLocked(sub, evs, m.client.maxRedeliveries, asidePast)
	}
	entries := make([]asideEntry, 0, len(evs))
	for _, ev := range evs {
		entries = append(entries, asideEntry{ev: ev, key: key, arrival: sub.arrived[ev]})
	}
	if _, again := m.readmits[key]; again {
		sub.mail = prepend(inArrival(entries), sub.mail)
		return nil
	}
	m.aside = append(m.aside, entries...)
	return nil
}

// inArrival is entries' events in the order their subscription received them.
func inArrival(entries []asideEntry) []*events.Event {
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(x, y asideEntry) int { return cmp.Compare(x.arrival, y.arrival) })
	out := make([]*events.Event, len(sorted))
	for i, e := range sorted {
		out[i] = e.ev
	}
	return out
}

// Readmit puts every delivery this client's consumers of the pair hold aside
// under key back at the front of the mailbox, in the order the subscription
// received them, and drains what it can reach ([queue.EventQueue.Readmit]).
func (q *Queue) Readmit(ctx context.Context, topic, group, key string) (int, error) {
	q.broker.mu.Lock()
	if q.notStartedLocked() {
		q.broker.mu.Unlock()
		return 0, ErrNotStarted
	}
	sub, ok := q.broker.subs[subKey{topic, group}]
	if !ok {
		q.broker.mu.Unlock()
		return 0, nil
	}
	var back []asideEntry
	for _, m := range sub.membersOf(q) {
		if m.readmits == nil {
			m.readmits = map[string]struct{}{}
		}
		m.readmits[key] = struct{}{}
		kept := m.aside[:0]
		for _, e := range m.aside {
			if e.key == key {
				back = append(back, e)
				continue
			}
			kept = append(kept, e)
		}
		m.aside = kept
	}
	sub.mail = prepend(inArrival(back), sub.mail)
	q.broker.mu.Unlock()
	if len(back) > 0 {
		q.broker.drain(ctx, sub)
	}
	return len(back), nil
}

// handBackAsideLocked hands back everything m holds aside, charged one
// delivery each as a hand-back, at the front of the mailbox in the order the
// subscription received it, for reason; it returns what the charge retired.
func (b *Broker) handBackAsideLocked(sub *subscription, m *consumer, reason string) []deadLetter {
	if len(m.aside) == 0 {
		return nil
	}
	held := inArrival(m.aside)
	m.aside, m.readmits = nil, nil
	return b.handBackLocked(sub, held, m.client.maxRedeliveries, reason)
}
