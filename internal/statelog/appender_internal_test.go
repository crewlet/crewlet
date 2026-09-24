package statelog

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
)

// EVERY ANSWER A PUBLISH CAN GET READS AS WHAT IT IS.
//
// A record refused for its size is a refusal of its own, whichever side made
// it, and never the third value: the client's refusal carries no API error, so
// without the sentinel it reads as an append that may or may not have landed,
// and the broker's, filed with the full log, would send an operator to raise a
// byte ceiling that is not the limit in the way. The broker's two answers that
// are not a decision about the record — a full ingest queue, and a message id
// still being proposed — read as neither a refusal nor silence. Each other
// answer is the control, and must still read as itself.
func TestEveryPublishAnswerIsClassifiedAsWhatItIs(t *testing.T) {
	t.Parallel()
	api := func(code jetstream.ErrorCode, description string) error {
		// Wrapped the way the client returns a refused acknowledgement.
		return fmt.Errorf("nats: %w", &jetstream.APIError{
			Code: 400, ErrorCode: code, Description: description,
		})
	}
	for _, tc := range []struct {
		name string
		err  error
		want fault
		// says is what the detail must carry.
		says string
	}{
		{name: "the client's refusal, translated by the appender",
			err: fmt.Errorf("append: the record's 9 bytes are more than the "+
				"max_payload: %w: %w", queue.ErrTooLarge, nats.ErrMaxPayload),
			want: faultTooLarge, says: "max_payload"},
		{name: "the broker's refusal against the stream's max_msg_size",
			err:  api(codeMessageExceedsMaximum, "message size exceeds maximum allowed"),
			want: faultTooLarge, says: "max_msg_size"},
		{name: "a lost race", err: api(jetstream.JSErrCodeStreamWrongLastSequence,
			"wrong last sequence: 4"), want: faultRejected},
		{name: "a full log", err: api(codeStreamStoreFailed, "maximum bytes exceeded"),
			want: faultFull, says: "maximum bytes exceeded"},
		// EVERY OTHER API ERROR IS A REFUSAL OF ITS OWN, in the server's
		// words: filed with the full log it would send an operator to a
		// byte ceiling that is not what refused the record.
		{name: "a sealed stream", err: api(10109, "invalid operation on sealed stream"),
			want: faultRefused, says: "sealed stream"},
		{name: "a server at its storage limit", err: api(10023, "insufficient resources"),
			want: faultRefused, says: "insufficient resources"},
		// TWO API ERRORS ARE NOT A DECISION ABOUT THIS RECORD. A full ingest
		// queue stored nothing and clears on its own, so filed as refused it
		// would end a write the next attempt would land; a message id still
		// being proposed may yet commit, so filed as refused it would tell a
		// caller nothing landed about a record that will.
		{name: "a full ingest queue", err: api(codeStreamTooManyRequests, "too many requests"),
			want: faultBusy, says: "too many requests"},
		{name: "this message id still being proposed",
			err:  api(codeDuplicateInProcess, "duplicate message id is in process"),
			want: faultInFlight, says: "in process"},
		{name: "no answer", err: errors.New("nats: timeout"), want: faultUnknown},
		{name: "a PubAck", err: nil, want: faultNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, detail := classify(tc.err)
			if got != tc.want {
				t.Fatalf("classify(%v) = %d, want %d", tc.err, got, tc.want)
			}
			if !strings.Contains(detail, tc.says) {
				t.Errorf("the detail %q does not say %q", detail, tc.says)
			}
		})
	}
}
