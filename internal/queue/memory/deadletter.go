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

// letter is one entry a dead-letter subject holds: its header and its body
// KEPT APART, as the broker this twin stands for carries the header as a
// header of the message and the body as its payload — so a listing here hands
// over the header alone and moves no body, exactly as the broker's does.
type letter struct {
	seq      uint64
	storedAt time.Time
	header   []byte
	body     []byte
}

// KeepDeadLetter implements queue.EventQueue: body kept on subject under
// header until the retention lets it go or DropDeadLetter removes it, in the
// broker rather than in this client, so a peer lists it and a stop does not
// lose it.
func (q *Queue) KeepDeadLetter(_ context.Context, subject string, header, body []byte) error {
	if err := deadLetterSubject(subject); err != nil {
		return err
	}
	if err := queue.LetterHeader(header); err != nil {
		return fmt.Errorf("memory: keep a dead letter on %s: %w", subject, err)
	}
	// THE CONTRACT'S CEILING FOR A KEEP, the one the broker is held to as
	// well ([queue.LetterFraming]): an entry kept here and refused there is
	// the one direction a twin must never be wrong in.
	if size := len(header) + len(body) + queue.LetterFraming; size > queue.MaxPayloadBytes {
		return fmt.Errorf("memory: keep a dead letter on %s: %d bytes of header and body exceed "+
			"the %d-byte limit less the %d a keep is framed in: %w", subject, len(header)+len(body),
			queue.MaxPayloadBytes, queue.LetterFraming, queue.ErrTooLarge)
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return err
	}
	q.broker.keepLocked(subject, slices.Clone(header), slices.Clone(body))
	return nil
}

// DeadLettersOn implements queue.EventQueue: every entry subject holds, oldest
// first, each with the instant its retention lets it go and the header it was
// kept under — never its body.
//
// ALWAYS CURRENT, which the contract does not promise and a caller must not
// lean on: the broker this twin stands for may answer from a copy behind it,
// and the reader that has to know holds the listing to DeadLetterMarks.
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
			Expires: l.storedAt.Add(deadLetterRetention), Header: slices.Clone(l.header)})
	}
	return out, nil
}

// DeadLetterFrame implements queue.EventQueue: the body the entry seq of
// subject was kept with, not held where the entry is gone, and refused for a
// seq that names an entry of another subject.
func (q *Queue) DeadLetterFrame(_ context.Context, subject string, seq uint64) ([]byte, bool, error) {
	if seq == 0 {
		return nil, false, fmt.Errorf("memory: read a dead letter on %s: sequence 0 names no entry", subject)
	}
	if err := deadLetterSubject(subject); err != nil {
		return nil, false, err
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return nil, false, err
	}
	if other, held := q.broker.ownerLocked(seq); held && other != subject {
		return nil, false, fmt.Errorf("memory: dead letter %d is on %s, not %s; nothing was read",
			seq, other, subject)
	}
	for _, l := range q.broker.lettersLocked(subject) {
		if l.seq == seq {
			return slices.Clone(l.body), true, nil
		}
	}
	return nil, false, nil
}

// DeadLetterMarks implements queue.EventQueue: how many entries every
// dead-letter subject under under holds, and the last sequence the broker gave
// any entry.
func (q *Queue) DeadLetterMarks(_ context.Context, under string) (queue.Marks, error) {
	if err := deadLetterSubject(under); err != nil {
		return queue.Marks{}, err
	}
	q.broker.mu.Lock()
	defer q.broker.mu.Unlock()
	if err := q.reachLocked(); err != nil {
		return queue.Marks{}, err
	}
	marks := queue.Marks{Counts: map[string]uint64{}, Last: q.broker.lastLetter}
	for _, subject := range slices.Collect(maps.Keys(q.broker.deadLetters)) {
		if !strings.HasPrefix(subject, under+".") {
			continue
		}
		if held := len(q.broker.lettersLocked(subject)); held > 0 {
			marks.Counts[subject] = uint64(held)
		}
	}
	return marks, nil
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
	if other, held := q.broker.ownerLocked(seq); held && other != subject {
		return fmt.Errorf("memory: dead letter %d is on %s, not %s; nothing was dropped",
			seq, other, subject)
	}
	named := func(l letter) bool { return l.seq == seq }
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

// keepLocked appends body to subject under header and the next sequence,
// stamped on the broker's clock.
func (b *Broker) keepLocked(subject string, header, body []byte) {
	b.lastLetter++
	b.deadLetters[subject] = append(b.lettersLocked(subject),
		letter{seq: b.lastLetter, storedAt: b.now(), header: header, body: body})
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

// ownerLocked is the subject whose entry seq is, and whether any subject still
// holds it.
func (b *Broker) ownerLocked(seq uint64) (string, bool) {
	for _, subject := range slices.Collect(maps.Keys(b.deadLetters)) {
		if slices.ContainsFunc(b.lettersLocked(subject), func(l letter) bool { return l.seq == seq }) {
			return subject, true
		}
	}
	return "", false
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
