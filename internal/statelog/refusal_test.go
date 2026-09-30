package statelog_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD LARGER THAN THE BROKER CARRIES IS REFUSED ONCE, AS TOO LARGE — on a
// real broker, whose client refuses it before sending anything.
//
// That refusal is a plain error rather than the broker's, and it was read as
// no answer: the write asked the log what landed, found nothing, retook its
// snapshot, decided the same record and was refused again — sixteen times —
// and then told its caller a colleague kept editing the object.
//
// TWO PATHS TO ONE ANSWER. On a log that keeps a gate reserve the reserve
// refuses the record before it is sent, since no record above the budget it
// admits against can be counted in it; on a log that keeps none the client
// refuses it, and that refusal is what has to be read as too large.
func TestAnOversizedRecordIsRefusedOnceAsTooLarge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain statelog.Domain
		sent   int64
	}{
		"on a log that keeps a gate reserve": {probeDomain{}, 0},
		"on a log that keeps none":           {unreservedDomain{}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarnessFor(t, tc.domain)
			_, err := h.write(probeSubject("a"), "op-large",
				strings.Repeat("x", queue.MaxPayloadBytes))
			if !errors.Is(err, queue.ErrTooLarge) {
				t.Fatalf("an oversized record answered %v, want queue.ErrTooLarge", err)
			}
			if errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("an oversized record was reported as a conflict: %v", err)
			}
			if n := h.appends.appends.Load(); n != tc.sent {
				t.Fatalf("an oversized record was sent %d times, want %d — the "+
					"same record is refused the same way on every round", n, tc.sent)
			}
		})
	}
}

// unreservedDomain is the probe log claiming no identity, which is what keeps
// no gate reserve — the vector changelog's shape.
type unreservedDomain struct{ probeDomain }

func (unreservedDomain) ClaimsIdentity() bool { return false }

// A BROKER REFUSAL THAT IS NOT A FULL LOG IS NOT REPORTED AS ONE.
//
// Every API error the framework had no case for came back `log_full`, with a
// remedy to raise the stream's byte ceiling. A stream that is not there has
// room to spare; the refusal now carries the broker's own error.
func TestABrokerRefusalThatIsNotAFullLogIsNotReportedAsOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.appends.fail(&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound,
		Description: "stream not found"}, true)
	_, err := h.write(probeSubject("a"), "op-refused", "hello")
	var refusal *statelog.Unavailable
	if errors.As(err, &refusal) && refusal.Reason == statelog.ReasonLogFull {
		t.Fatalf("a missing stream was reported as a full log: %v", err)
	}
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != jetstream.JSErrCodeStreamNotFound {
		t.Fatalf("the refusal %v does not carry the broker's own error", err)
	}
	if n := h.appends.appends.Load(); n != 1 {
		t.Fatalf("a refused record was sent %d times, want once", n)
	}
}
