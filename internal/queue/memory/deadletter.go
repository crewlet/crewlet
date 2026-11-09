package memory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// deadLetterRetention is how long this twin keeps a dead letter: the JetStream
// backend's own default, the event stream's age bound its dead-letter stream
// shares (internal/queue/jetstream's defaultEventRetention). If it moves, this
// moves with it — a twin that kept a dead letter longer, or for ever, would
// certify a caller that reads an entry the broker had already let go.
const deadLetterRetention = 30 * 24 * time.Hour

// letter is one entry a dead-letter subject holds.
type letter struct {
	seq      uint64
	storedAt time.Time
	body     []byte
}

// KeepDeadLetter implements queue.EventQueue: body kept on subject until the
// retention lets it go or DropDeadLetter removes it, in the broker rather than
// in this client, so a peer lists it and a stop does not lose it.
func (q *Queue) KeepDeadLetter(_ context.Context, subject string, body []byte) error {
	if err := deadLetterSubject(subject); err != nil {
		return err
	}
	// THE SAME CEILING THE BROKER ENFORCES, for Publish's reason: an entry
	// kept here and refused there is the one direction a twin must never be
	// wrong in.
	if len(body) > queue.MaxPayloadBytes {
		return fmt.Errorf("memory: keep a dead letter on %s: %d bytes exceeds the %d-byte limit: %w",
			subject, len(body), queue.MaxPayloadBytes, queue.ErrTooLarge)
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return err
	}
	q.broker.keepLocked(subject, slices.Clone(body))
	return nil
}

// DeadLettersOn implements queue.EventQueue: every entry subject holds, oldest
// first, each with the instant its retention lets it go.
func (q *Queue) DeadLettersOn(_ context.Context, subject string) ([]queue.Kept, error) {
	if err := deadLetterSubject(subject); err != nil {
		return nil, err
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return nil, err
	}
	letters := q.broker.lettersLocked(subject)
	if len(letters) == 0 {
		return nil, nil
	}
	out := make([]queue.Kept, 0, len(letters))
	for _, l := range letters {
		out = append(out, queue.Kept{Seq: l.seq, StoredAt: l.storedAt,
			Expires: l.storedAt.Add(deadLetterRetention), Body: slices.Clone(l.body)})
	}
	return out, nil
}

// DropDeadLetter implements queue.EventQueue: the entry seq removed from
// subject, an entry already gone answered nil, and a seq that names an entry
// of another subject refused with nothing removed.
func (q *Queue) DropDeadLetter(_ context.Context, subject string, seq uint64) error {
	if seq == 0 {
		return fmt.Errorf("memory: drop a dead letter on %s: sequence 0 names no entry", subject)
	}
	if err := deadLetterSubject(subject); err != nil {
		return err
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return err
	}
	named := func(l letter) bool { return l.seq == seq }
	for _, other := range slices.Collect(maps.Keys(q.broker.deadLetters)) {
		if other != subject && slices.ContainsFunc(q.broker.lettersLocked(other), named) {
			return fmt.Errorf("memory: dead letter %d is on %s, not %s; nothing was dropped",
				seq, other, subject)
		}
	}
	if kept := slices.DeleteFunc(q.broker.lettersLocked(subject), named); len(kept) > 0 {
		q.broker.deadLetters[subject] = kept
	} else {
		delete(q.broker.deadLetters, subject)
	}
	return nil
}

// reachLocked is the answer every verb that reaches the broker gives before it
// does: this client not started, then the broker gone — the order Publish asks
// them in, so a client stopped while its broker was down answers that it is
// not live.
func (q *Queue) reachLocked() error {
	switch {
	case !q.running:
		return ErrNotStarted
	case q.broker.down:
		return ErrBrokerDown
	}
	return nil
}

// keepLocked appends body to subject under the next sequence, stamped on the
// broker's clock.
func (b *Broker) keepLocked(subject string, body []byte) {
	b.lastLetter++
	b.deadLetters[subject] = append(b.lettersLocked(subject),
		letter{seq: b.lastLetter, storedAt: b.now(), body: body})
}

// lettersLocked is what subject holds now: the entries the retention has let
// go are gone first, as the broker this twin stands for expires a message by
// its age whether or not anybody reads the stream.
func (b *Broker) lettersLocked(subject string) []letter {
	letters := b.deadLetters[subject]
	horizon := b.now().Add(-deadLetterRetention)
	kept := slices.DeleteFunc(letters, func(l letter) bool { return !l.storedAt.After(horizon) })
	if len(kept) == 0 {
		delete(b.deadLetters, subject)
		return nil
	}
	b.deadLetters[subject] = kept
	return kept
}

// deadLetterSubject reports why subject cannot hold a dead letter, or nil when
// it can: a literal subject inside the dead-letter namespace. The contract's
// rule ([queue.EventQueue.KeepDeadLetter]); the JetStream backend states the
// same one and the conformance suite holds the two to one answer.
func deadLetterSubject(subject string) error {
	if !strings.HasPrefix(subject, topics.DeadLetterPrefix) {
		return fmt.Errorf("memory: %q is not a dead-letter subject (topics.DeadLetter names one)", subject)
	}
	return queue.LiteralSubject(subject)
}
