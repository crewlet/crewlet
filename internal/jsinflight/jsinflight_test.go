package jsinflight_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsinflight"
)

// budget is a publish's budget as every caller hands Decide one: the engine's
// own, over a client whose connection is never dialled — what the budget asks
// of it is its request term — each send of which crosses in crossing.
func budget(t *testing.T, crossing time.Duration) *jsapi.Sends {
	t.Helper()
	js, err := jsapi.Embedded().Client(&nats.Conn{})
	if err != nil {
		t.Fatalf("a client: %v", err)
	}
	return jsapi.Budget(js, crossing)
}

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
		if err := jsinflight.Decide(context.Background(), budget(t, 0), publish); err != nil {
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
		err := jsinflight.Decide(context.Background(), budget(t, 0), publish)
		if !errors.Is(err, jetstream.ErrKeyRevisionMismatch) || jsinflight.InFlight(err) {
			t.Fatalf("Decide = %v, want the leader's decided refusal: the write ahead "+
				"was somebody else's, and it landed first", err)
		}
	})

	t.Run("still in flight when the budget ends", func(t *testing.T) {
		t.Parallel()
		forever := func(context.Context) error { return inFlight }
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		err := jsinflight.Decide(ctx, budget(t, 0), forever)
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

	// WHERE THE CALLER SET NO DEADLINE the waits between attempts are held
	// to the budget, which ends the request term after it started for a
	// write that crosses nothing: exact, in a bubble.
	t.Run("still in flight when a budget of the caller's none ends", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			forever := func(context.Context) error { return inFlight }
			start := time.Now()
			err := jsinflight.Decide(t.Context(), budget(t, 0), forever)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Decide = %v, want the budget's deadline", err)
			}
			if ended := time.Since(start); ended != jsapi.RequestTerm {
				t.Errorf("an attempt still in flight was waited out for %v, want the "+
					"budget's request term, %v", ended, jsapi.RequestTerm)
			}
		})
	})
}

// AN ATTEMPT AFTER AN IN-FLIGHT ANSWER IS GIVEN ITS OWN CROSSING.
//
// The leader answers "in flight" only once it holds the whole message, so the
// attempt it refused has already spent the time its bytes took to cross the
// link, and the attempt after it sends them all again. Handed the budget the
// first attempt had, the second had the request term alone left for a
// crossing of its own, and a large record across a slow link read as no
// answer while it was still crossing. So each attempt's send waits in the
// context the budget gives it: the first the request term and a crossing, the
// second a crossing more. Exact, in a bubble.
//
// Mutation: give every send the first send's budget, and the second attempt is
// a crossing short.
func TestAnAttemptAfterAnInFlightAnswerIsGivenItsOwnCrossing(t *testing.T) {
	t.Parallel()
	const crossing = 30 * time.Second
	inFlight := refusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
	synctest.Test(t, func(t *testing.T) {
		var deadlines []time.Time
		sends := budget(t, crossing)
		publish := func(ctx context.Context) error {
			send, done := sends.Next(ctx)
			defer done()
			deadline, _ := send.Deadline()
			deadlines = append(deadlines, deadline)
			if len(deadlines) == 1 {
				return inFlight
			}
			return nil
		}
		if err := jsinflight.Decide(t.Context(), sends, publish); err != nil {
			t.Fatalf("Decide = %v once the write ahead settled, want it landed", err)
		}
		if len(deadlines) != 2 {
			t.Fatalf("%d attempts, want the one refused in flight and the one that landed",
				len(deadlines))
		}
		if gained := deadlines[1].Sub(deadlines[0]); gained != crossing {
			t.Errorf("the attempt after an in-flight answer was given %v more than the first, "+
				"want its own crossing, %v", gained, crossing)
		}
	})
}
