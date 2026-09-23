package statelog

import (
	"errors"
	"fmt"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

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
	api := func(code jetstream.ErrorCode) error {
		return fmt.Errorf("publish: %w", &jetstream.APIError{ErrorCode: code,
			Description: fmt.Sprintf("code %d", code)})
	}
	for name, tc := range map[string]struct {
		err  error
		want fault
	}{
		"an acknowledgement":           {err: nil, want: faultNone},
		"a wrong last sequence":        {err: api(jetstream.JSErrCodeStreamWrongLastSequence), want: faultRejected},
		"a clustered wrong sequence":   {err: api(jetstream.JSErrCodeStreamWrongLastSequenceConstant), want: faultRejected},
		"maximum bytes exceeded":       {err: api(codeStreamStoreFailed), want: faultFull},
		"a message over the stream's":  {err: api(codeMessageExceedsMaximum), want: faultTooLarge},
		"a payload the client refused": {err: fmt.Errorf("publish: %w", nats.ErrMaxPayload), want: faultTooLarge},
		"a stream that is gone":        {err: api(jetstream.JSErrCodeStreamNotFound), want: faultRefused},
		"jetstream not enabled":        {err: api(jetstream.JSErrCodeJetStreamNotEnabled), want: faultRefused},
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
		Description: "maximum bytes exceeded"}
	var refused *Refused
	if err := barrierRefusal(ReadLinearizable,
		barrierAppendError("crewlet.probe.barrier", full)); !errors.As(err, &refused) ||
		refused.Code != RefuseLogFull {
		t.Fatalf("a full log's barrier refused the read with %v, want %s", err, RefuseLogFull)
	}
	if err := barrierRefusal(ReadLinearizable,
		barrierAppendError("crewlet.probe.barrier", nats.ErrNoResponders)); !errors.As(err, &refused) ||
		refused.Code != RefuseNoQuorum {
		t.Fatalf("an unanswered barrier refused the read with %v, want %s", err, RefuseNoQuorum)
	}
}
