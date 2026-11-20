package jetstream

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
)

// heldPullWindow is how long the loop's pull stays open at the broker here:
// long enough that the hold and the publish both land inside the one pull the
// loop opened before the hold, on a loaded runner too.
const heldPullWindow = 3 * time.Second

// heldPullStepWithin bounds each wait below. LIVENESS: each is a step the loop
// takes within a pull window or two, and one it never takes is what fails.
const heldPullStepWithin = 30 * time.Second

// A HOLD TAKEN OVER A PULL OPEN AT THE BROKER HANDS BACK WHAT LANDS IN IT,
// SPENDING A DELIVERY NO HANDLER SEES. dispatchOne reads the gate again before
// a handler runs, so a hold stops a message already fetched, and that return
// costs a delivery as every return does. A message published under a hold the
// loop has not come round to reaches its handler one delivery short; parked
// first ([Queue.Loops]), the pull expires empty and the handler sees the whole
// budget. queuetest's holdTakenIn parks before a case publishes for this.
//
// Mutation: run the handler under a hold in dispatchOne, and the first arm sees
// three calls; drop the park from the second, and it sees two.
func TestAHoldTakenOverAnOpenPullSpendsItsDeliveryUnseen(t *testing.T) {
	t.Parallel()
	topologies := map[string]func(*testing.T, Config) *Queue{
		"member": func(t *testing.T, cfg Config) *Queue { return openForTest(t, cfg) },
		"leaf":   func(t *testing.T, cfg Config) *Queue { return openLeafForTest(t, cfg) },
	}
	for name, open := range topologies {
		t.Run(name+"/published_over_the_open_pull", func(t *testing.T) {
			t.Parallel()
			if got := passUnderAHold(t, open, false); !slices.Equal(got, []int{1, 0}) {
				t.Fatalf("the handler's calls left %v deliveries, want [1 0]: the hold's "+
					"hand-back of the pull's delivery spends the first of three unseen", got)
			}
		})
		t.Run(name+"/published_once_parked", func(t *testing.T) {
			t.Parallel()
			if got := passUnderAHold(t, open, true); !slices.Equal(got, []int{2, 1, 0}) {
				t.Fatalf("the handler's calls left %v deliveries, want [2 1 0]: a loop parked "+
					"before the publish fetches nothing, so the handler sees the whole budget", got)
			}
		})
	}
}

// passUnderAHold takes a hold while the loop's pull is open at the broker,
// parks the loop first when park is set, publishes one event, lifts the hold
// once the loop stands at a shut top, and returns the deliveries left at each
// handler call, which passes the event until its budget of three retires it.
func passUnderAHold(t *testing.T, open func(*testing.T, Config) *Queue, park bool) []int {
	t.Helper()
	ctx := t.Context()
	const topic, group = "hold.pull", "grp"
	q := open(t, Config{MaxDeliver: 3, FetchWait: heldPullWindow})
	var mu sync.Mutex
	var lefts []int
	if err := q.Subscribe(ctx, topic, group, func(hctx context.Context, ev *events.Event) queue.Result {
		left, ok := queue.DeliveriesLeftFor(hctx, ev.ID)
		if !ok {
			t.Errorf("the handler was not told how many deliveries %s has left", ev.Type)
		}
		mu.Lock()
		lefts = append(lefts, left)
		mu.Unlock()
		return queue.Pass("newer_record", errors.New("a peer on a newer build reads it"))
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	a := attachmentOf(t, q, topic)

	// THE LOOP'S PULL IS OPEN AT THE BROKER: past an open top, and counted
	// waiting by the consumer itself.
	var cycle uint64
	heldPullStep(t, "the loop's pull to be open at the broker", func() ([]string, error) {
		l, err := loopOf(q, a)
		if err != nil {
			return nil, err
		}
		if l.Cycles == 0 || l.Parked {
			return []string{fmt.Sprintf("the loop at cycle %d, parked %v", l.Cycles, l.Parked)}, nil
		}
		info, err := a.cons.Info(ctx)
		if err != nil {
			return nil, fmt.Errorf("consumer info: %w", err)
		}
		if info.NumWaiting < 1 {
			return []string{"no pull waiting at the broker"}, nil
		}
		cycle = l.Cycles
		return nil, nil
	})

	at, err := q.Attachment(ctx, topic, group)
	if err != nil {
		t.Fatalf("Attachment: %v", err)
	}
	if err := q.PauseTopic(ctx, topic, group, at, "hold-over-pull"); err != nil {
		t.Fatalf("PauseTopic: %v", err)
	}
	if park {
		parking, cancel := context.WithTimeout(ctx, heldPullStepWithin)
		err := parkedLoops(parking, t, q, topic, group)
		cancel()
		if err != nil {
			t.Fatalf("the loop never parked behind the hold: %v", err)
		}
	}
	if err := q.Publish(ctx, topic, &events.Event{ID: uuid.New(), Type: "e1",
		Timestamp: time.Now().UTC(), Source: "hold-over-pull"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// THE LOOP AT A SHUT TOP SINCE THE PULL: what the pull took is dispatched
	// or handed back, so the gate was read while the hold stood.
	heldPullStep(t, "the loop to come round to its shut gate", func() ([]string, error) {
		l, err := loopOf(q, a)
		if err != nil {
			return nil, err
		}
		if l.Cycles <= cycle || !l.Parked {
			return []string{fmt.Sprintf("the loop at cycle %d, parked %v", l.Cycles, l.Parked)}, nil
		}
		return nil, nil
	})
	// e1 WENT INTO THE OPEN PULL, read off the broker; otherwise the pull
	// ended before the publish and this run did not set up what it is about.
	// Where it went once parked is the parked arm's claim, which its calls decide.
	if !park {
		info, err := a.cons.Info(ctx)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.Delivered.Consumer != 1 {
			t.Fatalf("the consumer delivered %d message(s) under the hold, want e1 "+
				"into the pull open at the hold", info.Delivered.Consumer)
		}
	}

	if err := q.ResumeTopic(ctx, topic, group, at, "hold-over-pull"); err != nil {
		t.Fatalf("ResumeTopic: %v", err)
	}
	heldPullStep(t, "e1 to be dead-lettered once its budget is spent", func() ([]string, error) {
		dead, err := inspector(q).DeadLetters(ctx, topic, group)
		if err != nil {
			return nil, fmt.Errorf("DeadLetters: %w", err)
		}
		switch {
		case len(dead) == 0:
			return []string{"no dead letter yet"}, nil
		case len(dead) != 1 || dead[0].Type != "e1":
			return nil, fmt.Errorf("dead letters %d, the first %q, want e1 alone", len(dead), dead[0].Type)
		}
		return nil, nil
	})
	mu.Lock()
	defer mu.Unlock()
	return slices.Clone(lefts)
}

// loopOf is a's consume loop as q reports it.
func loopOf(q *Queue, a *attachment) (Loop, error) {
	loops := q.Loops()
	i := slices.IndexFunc(loops, func(l Loop) bool { return l.ID == a.id })
	if i < 0 {
		return Loop{}, fmt.Errorf("the loop of (%s, %s) ended", a.key.topic, a.key.group)
	}
	return loops[i], nil
}

// heldPullStep waits, within heldPullStepWithin, until waiting names nothing.
func heldPullStep(t *testing.T, what string, waiting func() ([]string, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), heldPullStepWithin)
	defer cancel()
	if err := untilLoops(ctx, what, waiting); err != nil {
		t.Fatalf("waiting for %s: %v", what, err)
	}
}
