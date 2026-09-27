package statelog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// TestAnOversizedRecordRefusesAtRoundOne.
//
// nats.ErrMaxPayload is raised CLIENT-SIDE, before the append reaches the
// broker, so it is not an APIError and fell through to the unknown answer —
// which is the one the publisher must retry, because an unanswered append may
// or may not be on the stream. So a record too large for the broker was
// re-decided for all sixteen rounds and then reported as "the rows kept
// changing under this write": a sentence about contention, which sends whoever
// reads it looking for a peer that is not there.
//
// Nothing about the record changes between rounds. It is too big now and it
// will be too big in a millisecond — and it is TOO LARGE rather than FULL,
// which it was classified as next: that sent an operator to raise a ceiling
// the log was nowhere near.
func TestAnOversizedRecordRefusesAtRoundOne(t *testing.T) {
	t.Parallel()
	f, detail := classify(fmt.Errorf("append: %w", nats.ErrMaxPayload))
	if f != faultTooLarge {
		t.Fatalf("classify(ErrMaxPayload) = %v, want faultTooLarge — the unknown "+
			"answer is retried, no retry can make a record smaller, and a full "+
			"log is a different operator's knob", f)
	}
	if !strings.Contains(detail, nats.ErrMaxPayload.Error()) {
		t.Errorf("the detail does not carry what the client said: %q", detail)
	}

	// CONTROL: an error nobody answered is still the unknown answer, or
	// this arm would have swallowed the case the retry exists for.
	if f, _ := classify(errors.New("no responders available for request")); f != faultUnknown {
		t.Errorf("an unanswered append classified as %v, want faultUnknown — it may "+
			"be on the stream and may not, and only the ordered classification "+
			"can say which", f)
	}
	if f, _ := classify(nil); f != faultNone {
		t.Errorf("classify(nil) = %v, want faultNone", f)
	}
}

// EVERY REFUSAL THE BROKER NAMES IS CLASSIFIED BY WHAT FIXES IT.
//
// The server's one store-failure code, 10077, covers a log at its byte
// ceiling and a message too large for the file store alike, and carries which
// only as the text of its own store error. The rows are the server's own
// errors, built by the vendored server's own constructors, so what is
// classified is exactly what a broker sends — and a wording this build does
// not know is a refusal carrying its words, never a full log.
func TestEveryBrokerRefusalIsClassifiedByWhatFixesIt(t *testing.T) {
	t.Parallel()
	wire := func(e *server.ApiError) error {
		return fmt.Errorf("append: %w", &jetstream.APIError{
			Code: e.Code, ErrorCode: jetstream.ErrorCode(e.ErrCode),
			Description: e.Description,
		})
	}
	for _, c := range []struct {
		name string
		err  error
		want fault
	}{
		{"the log at its byte ceiling",
			wire(server.NewJSStreamStoreFailedError(server.ErrMaxBytes,
				server.Unless(server.ErrMaxBytes))), faultFull},
		{"a message too large for the file store",
			wire(server.NewJSStreamStoreFailedError(server.ErrMsgTooLarge)), faultTooLarge},
		{"a message past the stream's own per-message limit",
			wire(server.NewJSStreamMessageExceedsMaximumError()), faultTooLarge},
		{"a message-count limit nobody declared on a state log",
			wire(server.NewJSStreamStoreFailedError(server.ErrMaxMsgs,
				server.Unless(server.ErrMaxMsgs))), faultRefused},
		{"a JetStream store with no resources left",
			wire(server.NewJSInsufficientResourcesError()), faultRefused},
		{"a sealed stream", wire(server.NewJSStreamSealedError()), faultRefused},
		{"a lost race, solo",
			wire(server.NewJSStreamWrongLastSequenceError(7)), faultRejected},
		{"a lost race, clustered",
			wire(server.NewJSStreamWrongLastSequenceConstantError()), faultRejected},
		// NEITHER IS A REFUSAL, though the broker named both: each says
		// this operation's record may yet land.
		{"this operation's own record in flight",
			wire(server.NewJSStreamDuplicateMessageConflictError()), faultUnsettled},
		{"a store closed under an entry raft committed",
			wire(server.NewJSStreamStoreFailedError(server.ErrStoreClosed,
				server.Unless(server.ErrStoreClosed))), faultUnsettled},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, detail := classify(c.err)
			if got != c.want {
				t.Fatalf("classify(%v) = %v, want %v", c.err, got, c.want)
			}
			// A REFUSAL WITH NO REMEDY OF ITS OWN CARRIES THE BROKER'S:
			// its code and its words are all an operator has.
			var api *jetstream.APIError
			if got == faultRefused && errors.As(c.err, &api) &&
				(!strings.Contains(detail, fmt.Sprint(int(api.ErrorCode))) ||
					!strings.Contains(detail, api.Description)) {
				t.Errorf("the detail %q drops the broker's code or words", detail)
			}
		})
	}
}

// THE SERVER'S OWN WORDS ARE PINNED, because the classification above
// compares them whole and a reworded server would otherwise move a full log —
// or a record that may yet land — to an unnamed refusal with nothing going
// red. The codes are pinned for the same reason: the client exports none of
// them.
func TestTheStoreRefusalsMatchTheServersOwn(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, ours, theirs string }{
		{"a full log", storeFailedMaxBytes, server.ErrMaxBytes.Error()},
		{"a message too large", storeFailedTooLarge, server.ErrMsgTooLarge.Error()},
		{"a store closed", storeFailedClosed, server.ErrStoreClosed.Error()},
	} {
		if c.ours != c.theirs {
			t.Errorf("%s is compared as %q and the server says %q", c.name,
				c.ours, c.theirs)
		}
	}
	for _, c := range []struct {
		name   string
		ours   jetstream.ErrorCode
		theirs server.ErrorIdentifier
	}{
		{"a store failure", codeStreamStoreFailed, server.JSStreamStoreFailedF},
		{"a per-message limit", codeStreamMessageExceedsMaximum,
			server.JSStreamMessageExceedsMaximumErr},
		{"a message id in process", codeStreamDuplicateMessageConflict,
			server.JSStreamDuplicateMessageConflict},
	} {
		if got := server.ApiErrors[c.theirs].ErrCode; uint16(c.ours) != got {
			t.Errorf("%s is matched as code %d and the server sends %d", c.name,
				c.ours, got)
		}
	}
}
