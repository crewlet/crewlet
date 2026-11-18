package statelog_test

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD TOO LARGE IS REFUSED ONCE, AS TOO LARGE, on a real broker: never read
// as no answer, retried and reported as a conflict.
//
// Two paths: a record past its domain's declared largest is refused by the
// publisher before it is sent, with a gate reserve or without; one inside the
// declaration that the SERVER cannot carry is refused by the client. No
// declaration passes [statelog.MaxTransportRecordBytes] and a smaller server is
// refused when the queue opens, so the second path is a server that CHANGED
// under a running node (a reconnect to a member configured apart, a restart with
// a smaller max_payload); the case restarts the server under the log with less.
func TestAnOversizedRecordIsRefusedOnceAsTooLarge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain  statelog.Domain
		size    int
		payload int32
		sent    int64
	}{
		"past the declaration, on a log that keeps a gate reserve": {
			probeDomain{}, probeMaxRecord + 1, 0, 0},
		"past the declaration, on a log that keeps none": {
			unreservedDomain{}, statelog.MaxTransportRecordBytes + 1, 0, 0},
		"inside the declaration and past the server's max_payload": {
			probeDomain{}, probeMaxRecord - 1024, probeMaxRecord / 2, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var h *harness
			if tc.payload == 0 {
				h = newHarnessFor(t, tc.domain)
			} else {
				srv := externaltest.Start(t, queue.MaxPayloadBytes)
				h = newHarnessOn(t, tc.domain, js.Config{URL: srv.URL(), Token: externaltest.Token})
				srv.Restart(tc.payload, h.q.Conn())
			}
			_, err := h.writeSized(probeSubject("a"), "op-large", tc.size)
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
// no gate reserve — the vector changelog's shape, down to declaring the
// largest record the transport carries.
type unreservedDomain struct{ probeDomain }

func (unreservedDomain) ClaimsIdentity() bool { return false }

func (unreservedDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.MaxRecordBytes = statelog.MaxTransportRecordBytes
	return spec
}

// NO DECLARATION ADMITS A RECORD THE TRANSPORT CANNOT CARRY. The transport's
// limit is on the signed MESSAGE and a declaration on the payload, so a log
// declared at the transport's own number passes the publisher's refusal and is
// refused by the broker instead; the spec is refused, and a log wanting all the
// transport carries declares [statelog.MaxTransportRecordBytes].
//
// Mutation: hold the declaration to [queue.MaxPayloadBytes] again and the
// transport's own number validates.
func TestADeclarationPastWhatTheTransportCarriesIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		declared int64
		valid    bool
	}{
		{statelog.MaxTransportRecordBytes, true},
		{statelog.MaxTransportRecordBytes + 1, false},
		{queue.MaxPayloadBytes, false},
		{0, false},
	} {
		spec := probeDomain{}.Stream()
		spec.MaxRecordBytes = tc.declared
		err := spec.Validate()
		if (err == nil) != tc.valid {
			t.Errorf("a declared largest record of %d validates %v, want %v",
				tc.declared, err, tc.valid)
		}
		if tc.valid && spec.MaxAppendBytes() > queue.MaxPayloadBytes {
			t.Errorf("a valid declaration of %d appends %d bytes, past the "+
				"transport's %d", tc.declared, spec.MaxAppendBytes(),
				queue.MaxPayloadBytes)
		}
	}
}

// A BROKER REFUSAL THAT IS NOT A FULL LOG IS NOT REPORTED AS ONE: `log_full`
// sends an operator to raise the byte ceiling, and a missing stream has room to
// spare. The refusal carries the broker's own error.
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
