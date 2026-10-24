package jsinflight_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsinflight"
)

// refusal is a conditional publish's refusal as the KV client hands it back:
// the leader's API error, wrapped in the revision mismatch the client maps
// BOTH "wrong last sequence" codes to — which is exactly why the code, and
// not the sentinel, is what tells them apart.
func refusal(code jetstream.ErrorCode) error {
	return fmt.Errorf("%w: %w", &jetstream.APIError{
		Code: 400, ErrorCode: code, Description: "an entirely unrelated string",
	}, jetstream.ErrKeyRevisionMismatch)
}

// ONLY THE IN-FLIGHT ANSWER IS WAITED OUT.
//
// The decided refusal names the revision the subject is at and is somebody
// else's win; the in-flight one decides nothing. Both arrive wrapped in the
// same sentinel, so a classifier that read the sentinel would wait out real
// refusals for the whole budget, and one that read the words would break on
// a reworded message.
func TestOnlyTheInFlightAnswerIsWaitedOut(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"in flight":               {refusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant), true},
		"refused on a comparison": {refusal(jetstream.JSErrCodeStreamWrongLastSequence), false},
		"the sentinel alone":      {jetstream.ErrKeyRevisionMismatch, false},
		"the words alone":         {errors.New("wrong last sequence"), false},
		"another API error":       {&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound}, false},
		"no error":                {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := jsinflight.InFlight(tc.err); got != tc.want {
				t.Errorf("InFlight(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

// A PUBLISH IS ANSWERED BY WHAT THE LEADER DECIDED.
//
// Staged rather than raced, because the window is a few hundred microseconds
// on an idle machine and a race reproduces it only under load.
//
// Mutation: hand back the first answer instead of retrying, and the publish
// that landed reads as refused, and the one still in flight at its deadline
// reads as a race somebody else won.
func TestAPublishIsAnsweredByWhatTheLeaderDecided(t *testing.T) {
	t.Parallel()
	inFlight := refusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
	decided := refusal(jetstream.JSErrCodeStreamWrongLastSequence)
	script := func(answers ...error) (func(context.Context) error, *int) {
		calls := 0
		return func(context.Context) error {
			calls++
			if len(answers) == 0 {
				return nil
			}
			answer := answers[0]
			answers = answers[1:]
			return answer
		}, &calls
	}

	t.Run("in flight, then landed", func(t *testing.T) {
		t.Parallel()
		publish, calls := script(inFlight, inFlight, nil)
		if err := jsinflight.Decide(context.Background(), time.Minute, publish); err != nil {
			t.Fatalf("Decide = %v once the write ahead settled and this one landed, "+
				"want it landed", err)
		}
		if *calls != 3 {
			t.Errorf("%d publishes, want 3: two waited out and the one that landed", *calls)
		}
	})

	t.Run("in flight, then refused on a comparison", func(t *testing.T) {
		t.Parallel()
		publish, _ := script(inFlight, decided)
		err := jsinflight.Decide(context.Background(), time.Minute, publish)
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) || jsinflight.InFlight(err) {
			t.Fatalf("Decide = %v, want the leader's decided refusal: the write ahead "+
				"was somebody else's, and it landed first", err)
		}
	})

	t.Run("still in flight when the budget ends", func(t *testing.T) {
		t.Parallel()
		forever := func(context.Context) error { return inFlight }
		err := jsinflight.Decide(context.Background(), 20*time.Millisecond, forever)
		var undecided *jsinflight.Undecided
		switch {
		case !errors.As(err, &undecided):
			t.Fatalf("Decide = %v, want *Undecided", err)
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch):
			t.Fatalf("Decide = %v, which every compare-and-set caller reads as a race "+
				"another writer won; nothing about it says one did", err)
		case !errors.Is(err, context.DeadlineExceeded):
			t.Errorf("Decide = %v, want it to carry the deadline that ended the wait", err)
		}
	})

	t.Run("each publish runs inside the budget", func(t *testing.T) {
		t.Parallel()
		var deadline bool
		publish := func(ctx context.Context) error {
			_, deadline = ctx.Deadline()
			return nil
		}
		if err := jsinflight.Decide(context.Background(), time.Minute, publish); err != nil {
			t.Fatalf("Decide: %v", err)
		}
		if !deadline {
			t.Error("a caller with no deadline handed the publish a context with none, " +
				"so one attempt could outlast the whole budget")
		}
	})
}
