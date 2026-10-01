package jetstream

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// inFlightJS refuses its first n publishes with a clustered leader's "another
// write to this subject is still in flight" and then acknowledges.
//
// EMBEDS THE INTERFACE for the reason notYetVisibleJS gives: the two methods
// under test are two of dozens.
type inFlightJS struct {
	jetstream.JetStream
	refusals int
	calls    int
}

func (f *inFlightJS) Options() jetstream.JetStreamOptions {
	return jetstream.JetStreamOptions{DefaultTimeout: time.Minute}
}

func (f *inFlightJS) Publish(context.Context, string, []byte, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	f.calls++
	if f.calls <= f.refusals {
		return nil, &jetstream.APIError{Code: 400,
			ErrorCode:   jetstream.JSErrCodeStreamWrongLastSequenceConstant,
			Description: "wrong last sequence"}
	}
	return &jetstream.PubAck{Sequence: 42}, nil
}

// AN APPEND IS ANSWERED BY WHAT THE LEADER DECIDED, never by its "still in
// flight".
//
// A clustered leader refuses a conditional append without comparing anything
// while another write to the subject is in flight — and that write may be
// this node's own, already acknowledged. Handed up as it came, the state log
// read it as a rejection, re-decided against the anchor it already had, and
// was refused again for as long as the write ahead took to clear. So the
// append waits it out, and one still in flight at its deadline comes back with
// no API error on it, which the state log reads as the no-answer it is.
//
// Staged rather than raced: the window is a few hundred microseconds on an
// idle machine.
//
// Mutation: publish once and return, and the first case reads the refusal and
// the second a rejection the state log would discriminate as a lost race.
func TestAnAppendIsAnsweredByWhatTheLeaderDecided(t *testing.T) {
	t.Parallel()
	expect := uint64(41)

	js := &inFlightJS{refusals: 2}
	seq, _, err := (&DomainLog{js: js}).Append(t.Context(), "crewlet.log.x", "op-1",
		&expect, []byte("{}"))
	if err != nil || seq != 42 {
		t.Fatalf("Append = (%d, %v) once the write ahead cleared, want 42 appended", seq, err)
	}
	if js.calls != 3 {
		t.Errorf("%d publishes, want 3: two waited out and the one that landed", js.calls)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, _, err = (&DomainLog{js: &inFlightJS{refusals: 1 << 30}}).Append(ctx,
		"crewlet.log.x", "op-2", &expect, []byte("{}"))
	var api *jetstream.APIError
	switch {
	case err == nil:
		t.Fatal("Append succeeded while every answer said another write was in flight")
	case errors.As(err, &api):
		t.Fatalf("Append = %v, an API error the state log classifies as the broker's "+
			"answer; the leader decided nothing", err)
	case !errors.Is(err, context.DeadlineExceeded):
		t.Errorf("Append = %v, want it to carry the deadline that ended the wait", err)
	}
}
