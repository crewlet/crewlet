package statelogtest

import (
	"context"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// suiteLog is the suite's one broker: an ordered log of subjects, kept whole
// and handed to every seam that reads or writes a log — the publisher's
// appender and record reader, and the applier's fetcher and checkpoint log.
//
// It keeps what a conditional append is judged by — each subject's last
// SURVIVING sequence — and refuses an expectation that is not it with the
// broker's own error, so the publisher classifies the refusal as the lost race
// or trimmed anchor it would be. A purge removes everything below a sequence:
// LastSeq, At and Bounds no longer see it, and the fetcher steps over it, as a
// stream does.
type suiteLog struct {
	mu sync.Mutex

	// records is every record ever appended, record i at sequence i+1;
	// first is the lowest sequence a purge has not removed, and next the
	// next one the fetcher delivers.
	records []suiteMessage
	first   uint64
	next    uint64

	// expects is every expectation an append carried, in order — nil for
	// none.
	expects []*uint64
}

type suiteMessage struct {
	subject string
	body    []byte
}

func newSuiteLog() *suiteLog { return &suiteLog{first: 1, next: 1} }

// suiteStoredAt is the broker instant the suite's log stamps sequence seq with:
// deterministic, so two runs agree on every record's instant.
func suiteStoredAt(seq uint64) time.Time {
	return time.Unix(1_700_000_000, 0).UTC().Add(time.Duration(seq) * time.Second)
}

// lastOn is subject's last surviving sequence, or zero. The caller holds mu.
func (l *suiteLog) lastOn(subject string) uint64 {
	for i := len(l.records) - 1; i >= 0; i-- {
		seq := uint64(i + 1)
		if seq < l.first {
			return 0
		}
		if l.records[i].subject == subject {
			return seq
		}
	}
	return 0
}

func (l *suiteLog) Append(_ context.Context, subject, _ string, expect *uint64,
	body []byte) (uint64, bool, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	l.expects = append(l.expects, expect)
	if expect != nil && *expect != l.lastOn(subject) {
		// THE BROKER'S OWN REFUSAL, so the publisher classifies it as the
		// lost race it would be rather than as no answer at all.
		return 0, false, &jetstream.APIError{
			ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequence,
			Description: "wrong last sequence",
		}
	}
	l.records = append(l.records, suiteMessage{subject: subject, body: append([]byte(nil), body...)})
	return uint64(len(l.records)), false, nil
}

func (l *suiteLog) LastSeq(_ context.Context, subject string) (uint64, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seq := l.lastOn(subject)
	return seq, seq > 0, nil
}

// At is the record the log holds at seq, which a resolution reads to learn
// whose a record it did not append itself is, and the applier verifies its
// checkpoint by.
func (l *suiteLog) At(_ context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < l.first || seq > uint64(len(l.records)) {
		return "", nil, time.Time{}, false, nil
	}
	m := l.records[seq-1]
	return m.subject, m.body, suiteStoredAt(seq), true, nil
}

// Bounds is the log's first surviving sequence and its last.
func (l *suiteLog) Bounds(context.Context) (uint64, uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.first, uint64(len(l.records)), nil
}

// Ends is Bounds in the shape the zero fence reads.
func (l *suiteLog) Ends(ctx context.Context) (statelog.LogEnds, error) {
	first, last, err := l.Bounds(ctx)
	return statelog.LogEnds{First: first, Last: last}, err
}

// Fetch delivers the next records in order, waiting up to wait for the first
// the way a pull consumer waits on its stream — an empty answer at once would
// turn the apply loop into a spin.
func (l *suiteLog) Fetch(ctx context.Context, maxMessages, _ int, wait time.Duration) ([]statelog.Message, error) {
	deadline := time.Now().Add(wait)
	for {
		l.mu.Lock()
		l.next = max(l.next, l.first)
		var out []statelog.Message
		for l.next <= uint64(len(l.records)) && len(out) < maxMessages {
			out = append(out, statelog.Message{
				Seq: l.next, StoredAt: suiteStoredAt(l.next), Payload: l.records[l.next-1].body,
			})
			l.next++
		}
		l.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
}

func (l *suiteLog) Pending(context.Context) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := max(l.next, l.first)
	if last := uint64(len(l.records)); next <= last {
		return last - next + 1, nil
	}
	return 0, nil
}

// purge removes every record below seq, as the trim's purge does.
func (l *suiteLog) purge(seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.first = max(l.first, seq)
}

// appended is every payload the log was handed, in order.
func (l *suiteLog) appended() [][]byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]byte, 0, len(l.records))
	for _, m := range l.records {
		out = append(out, m.body)
	}
	return out
}

// last is the log's last sequence.
func (l *suiteLog) last() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return uint64(len(l.records))
}

// expectations is every expectation an append carried, in order.
func (l *suiteLog) expectations() []*uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*uint64(nil), l.expects...)
}
