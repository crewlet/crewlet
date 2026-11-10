package jetstream

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/queuetest"
)

// scriptedConsumer is a consumer whose broker answers each pull request with
// the next of its answers, the last one standing for every pull after it.
// fetches is the clock at each pull, read inside the test's bubble where it
// has one.
type scriptedConsumer struct {
	jetstream.Consumer // nil: nothing but Fetch is called

	t       *testing.T
	answers []pullAnswer
	fetches []time.Time

	// clock, where set, is moved on by step at every pull: a broker whose
	// every answer takes that long by a clock the case moves, which is the
	// one a drain handed it ends its linger window on (Config.clock).
	clock *queuetest.Clock
	step  time.Duration
}

// pullAnswer is the batch the vendored client makes of the broker's answer
// to one pull request: the messages it delivered, then the status it kept for
// Error, nil for an ordinary ending.
type pullAnswer struct {
	delivered []jetstream.Msg
	refusal   error
}

// spunPulls is more pulls than any case here asks for. A loop that asks again
// at once never lets a bubble's clock move, so past it the case fails from
// inside the loop rather than hanging the run that shows the old code red.
const spunPulls = 64

func (c *scriptedConsumer) Fetch(int, ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	c.fetches = append(c.fetches, time.Now())
	if len(c.fetches) > spunPulls {
		c.t.Fatalf("the broker was asked %d times and the loop never rested", len(c.fetches))
	}
	if c.clock != nil {
		c.clock.Advance(c.step)
	}
	answer := c.answers[min(len(c.fetches), len(c.answers))-1]
	msgs := make(chan jetstream.Msg, len(answer.delivered))
	for _, m := range answer.delivered {
		msgs <- m
	}
	close(msgs)
	return answeredBatch{msgs: msgs, err: answer.refusal}, nil
}

type answeredBatch struct {
	msgs <-chan jetstream.Msg
	err  error
}

func (b answeredBatch) Messages() <-chan jetstream.Msg { return b.msgs }
func (b answeredBatch) Error() error                   { return b.err }

// eventMsg is a delivered message holding an event, and nothing else a drain
// reads of it.
type eventMsg struct{ jetstream.Msg }

func (eventMsg) Data() []byte { return []byte(`{"type":"test.delivered"}`) }

// refusals are the errors the client keeps for a pull request the broker
// answered at once with a status (checkMsg in nats.go's jetstream package):
// every status it does not read as "nothing for you yet".
var refusals = map[string]error{
	"no responders (503)":        nats.ErrNoResponders,
	"consumer deleted (409)":     jetstream.ErrConsumerDeleted,
	"leadership changed (409)":   jetstream.ErrConsumerLeadershipChanged,
	"server shutting down (409)": jetstream.ErrServerShutdown,
	"bad request (400)":          jetstream.ErrBadRequest,
}

// A FETCH THE BROKER REFUSED IS A FAILED FETCH, after what it did deliver.
// The client closes a refused request's batch with nothing more in it and
// keeps the status for Error; a fetch that read only the messages took the
// refusal for an empty fetch (#765).
func TestAFetchTheBrokerRefusedIsAFailure(t *testing.T) {
	t.Parallel()
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cons := &scriptedConsumer{t: t, answers: []pullAnswer{
				{delivered: []jetstream.Msg{eventMsg{}}, refusal: refusal},
			}}
			a := &attachment{cons: cons, fetching: t.Context()}
			taken := 0
			err := a.fetch(1, time.Second, func(jetstream.Msg) { taken++ })
			if !errors.Is(err, refusal) {
				t.Errorf("a fetch the broker refused with %v returned %v", refusal, err)
			}
			if taken != 1 {
				t.Errorf("a refused fetch handed over %d of the 1 message delivered before the refusal", taken)
			}
		})
	}
}

// A CONSUME LOOP THE BROKER REFUSES REPORTS IT AND ASKS AGAIN A POLL LATER,
// rather than asking again at once for as long as the refusal lasts (#765).
// Held on the batch loop's own cycle, drain, run twice in a bubble, whose
// clock moves only when the loop waits: the second request is a poll after
// the first, and each refusal is logged once.
func TestALoopTheBrokerRefusesReportsItAndAsksAgainAPollLater(t *testing.T) {
	t.Parallel()
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				const poll = 7 * time.Second
				var logs bytes.Buffer
				cons := &scriptedConsumer{t: t, answers: []pullAnswer{{refusal: refusal}}}
				a := &attachment{
					q:        &Queue{cfg: Config{FetchWait: poll}},
					cons:     cons,
					log:      slog.New(slog.NewJSONHandler(&logs, nil)),
					fetching: t.Context(),
				}
				for range 2 {
					if got := a.drain(t.Context(), queue.DefaultBatchOptions()); len(got) != 0 {
						t.Fatalf("a refused fetch drained %d deliveries", len(got))
					}
				}
				if len(cons.fetches) != 2 {
					t.Fatalf("two cycles asked the broker %d times", len(cons.fetches))
				}
				if gap := cons.fetches[1].Sub(cons.fetches[0]); gap != poll {
					t.Errorf("the loop asked again %v after the broker refused it, want its poll, %v", gap, poll)
				}
				if n := strings.Count(logs.String(), `"msg":"fetch_failed"`); n != 2 {
					t.Errorf("two refused fetches logged fetch_failed %d times, want 2:\n%s", n, logs.String())
				}
			})
		})
	}
}

// A REFUSAL INSIDE A DRAIN'S WINDOW ENDS IT, and the drain hands over what it
// holds. A positive linger waits out a fetch that comes back empty, for the
// arrivals the window exists to collect, and a refusal read as one: the drain
// asked again at once until its window closed, a minute at the cap (#765).
func TestARefusalInsideADrainsWindowEndsIt(t *testing.T) {
	t.Parallel()
	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				cons := &scriptedConsumer{t: t, answers: []pullAnswer{
					{delivered: []jetstream.Msg{eventMsg{}}},
					{refusal: refusal},
				}}
				a := &attachment{
					q:        &Queue{},
					cons:     cons,
					log:      slog.New(slog.DiscardHandler),
					fetching: t.Context(),
				}
				got := a.drain(t.Context(), queue.NewBatchOptions(queue.MaxLingerSeconds, 20))
				if len(got) != 1 {
					t.Errorf("the drain handed over %d deliveries, want the 1 it held when the broker refused it", len(got))
				}
				if len(cons.fetches) != 2 {
					t.Errorf("the drain asked the broker %d times, want 2: its first fetch and the one refused", len(cons.fetches))
				}
			})
		})
	}
}
