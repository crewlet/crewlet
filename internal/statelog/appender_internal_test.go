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
// only as the text of its own store error. And a record too large meets one
// of three limits, each moved by a different hand, so its detail names the
// limit and that limit's remedy and never another's. The rows are the server's own
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

		// says and never are what a record too large's detail must and
		// must not name: the limit that refused it and what moves THAT
		// limit, and never another limit's knob.
		says, never []string
	}{
		{"the log at its byte ceiling",
			wire(server.NewJSStreamStoreFailedError(server.ErrMaxBytes,
				server.Unless(server.ErrMaxBytes))), faultFull, nil, nil},
		{"a message past the server's max_payload",
			fmt.Errorf("append: 9000000 bytes exceeds the 8388608-byte limit "+
				"(the server's max_payload): %w", nats.ErrMaxPayload), faultTooLarge,
			[]string{"max_payload refused it", "raise max_payload",
				"split the change", "8388608-byte limit"},
			[]string{"max_msg_size", "file store"}},
		{"a message too large for the file store",
			wire(server.NewJSStreamStoreFailedError(server.ErrMsgTooLarge)), faultTooLarge,
			[]string{"file store", "no server setting raises", "split the change",
				server.ErrMsgTooLarge.Error()},
			[]string{"max_payload", "max_msg_size"}},
		{"a message past the stream's own per-message limit",
			wire(server.NewJSStreamMessageExceedsMaximumError()), faultTooLarge,
			[]string{"max_msg_size", "reconfigured", "unlimited (-1)",
				server.NewJSStreamMessageExceedsMaximumError().Description},
			[]string{"max_payload", "file store"}},
		{"a message-count limit nobody declared on a state log",
			wire(server.NewJSStreamStoreFailedError(server.ErrMaxMsgs,
				server.Unless(server.ErrMaxMsgs))), faultRefused, nil, nil},
		{"a JetStream store with no resources left",
			wire(server.NewJSInsufficientResourcesError()), faultRefused, nil, nil},
		{"a sealed stream", wire(server.NewJSStreamSealedError()), faultRefused, nil, nil},
		{"a lost race, solo",
			wire(server.NewJSStreamWrongLastSequenceError(7)), faultRejected, nil, nil},
		{"a lost race, clustered",
			wire(server.NewJSStreamWrongLastSequenceConstantError()), faultRejected, nil, nil},
		// NEITHER IS A REFUSAL, though the broker named both: each says
		// this operation's record may yet land.
		{"this operation's own record in flight",
			wire(server.NewJSStreamDuplicateMessageConflictError()), faultUnsettled, nil, nil},
		{"a store closed under an entry raft committed",
			wire(server.NewJSStreamStoreFailedError(server.ErrStoreClosed,
				server.Unless(server.ErrStoreClosed))), faultUnsettled, nil, nil},
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
			for _, want := range c.says {
				if !strings.Contains(detail, want) {
					t.Errorf("the detail does not say %q: %s", want, detail)
				}
			}
			for _, wrong := range c.never {
				if strings.Contains(detail, wrong) {
					t.Errorf("the detail names %q, another limit's knob: %s",
						wrong, detail)
				}
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

// EVERY BROKER ANSWER IS READ AS THE FACT IT IS, AND ONLY A FULL LOG AS A FULL
// LOG.
//
// Two answers were read as something they were not. Every API error the
// framework had no case for was a full log, so a stream deleted under the node
// or a cluster without a leader told the caller to raise a byte ceiling. And
// the client's own refusal of an oversized record — a plain error, sent
// nowhere — was "no answer", which sent the write round its ambiguous path
// sixteen times and reported a colleague editing the object.
func TestEveryBrokerAnswerIsClassifiedAsWhatItIs(t *testing.T) {
	t.Parallel()
	api := func(code jetstream.ErrorCode, words string) error {
		return fmt.Errorf("publish: %w", &jetstream.APIError{ErrorCode: code,
			Description: words})
	}
	for name, tc := range map[string]struct {
		err  error
		want fault
	}{
		"an acknowledgement":           {err: nil, want: faultNone},
		"a wrong last sequence":        {err: api(jetstream.JSErrCodeStreamWrongLastSequence, "wrong last sequence"), want: faultRejected},
		"a clustered wrong sequence":   {err: api(jetstream.JSErrCodeStreamWrongLastSequenceConstant, "wrong last sequence"), want: faultRejected},
		"maximum bytes exceeded":       {err: api(codeStreamStoreFailed, storeFailedMaxBytes), want: faultFull},
		"a message over the stream's":  {err: api(codeStreamMessageExceedsMaximum, "message size exceeds maximum allowed"), want: faultTooLarge},
		"a payload the client refused": {err: fmt.Errorf("publish: %w", nats.ErrMaxPayload), want: faultTooLarge},
		"a stream that is gone":        {err: api(jetstream.JSErrCodeStreamNotFound, "stream not found"), want: faultRefused},
		"jetstream not enabled":        {err: api(jetstream.JSErrCodeJetStreamNotEnabled, "jetstream not enabled"), want: faultRefused},
		"no responders":                {err: nats.ErrNoResponders, want: faultUnknown},
		"a timeout":                    {err: errors.New("context deadline exceeded"), want: faultUnknown},
	} {
		if got, _ := classify(tc.err); got != tc.want {
			t.Errorf("%s: classify = %d, want %d", name, got, tc.want)
		}
	}
}

// A LINEARIZABLE READ ON A FULL LOG IS REFUSED `log_full`, NOT `no_quorum`.
//
// The barrier handed its append's error on raw, and the read path knows a full
// log only by its refusal reason — so a full log's reads were refused as a
// majority that did not agree, which is worth coming back for, when the truth
// was a ceiling somebody has to raise.
func TestABarrierOnAFullLogRefusesReadsAsAFullLog(t *testing.T) {
	t.Parallel()
	full := &jetstream.APIError{ErrorCode: codeStreamStoreFailed,
		Description: storeFailedMaxBytes}
	var refused *Refused
	if err := barrierRefusal(ReadLinearizable, "crewlet.probe.barrier",
		barrierAppendError("crewlet.probe.barrier", full)); !errors.As(err, &refused) ||
		refused.Code != RefuseLogFull {
		t.Fatalf("a full log's barrier refused the read with %v, want %s", err, RefuseLogFull)
	}
	if err := barrierRefusal(ReadLinearizable, "crewlet.probe.barrier",
		barrierAppendError("crewlet.probe.barrier", nats.ErrNoResponders)); !errors.As(err, &refused) ||
		refused.Code != RefuseNoQuorum {
		t.Fatalf("an unanswered barrier refused the read with %v, want %s", err, RefuseNoQuorum)
	}
}
